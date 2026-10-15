package room

import (
	"sync"
	"time"

	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
)

// Uplink feedback: the node a participant is connected to measures how its
// media arrives. An Object's one-way delay is its arrival time minus its LOC
// capture timestamp; the clocks differ by an unknown but steady offset, so
// the delay over the lowest seen recently is what a queue on the way adds.
// The participant's SDK fits what it sends to this (SendFeedback).

const (
	feedbackInterval = 500 * time.Millisecond
	baseBucket       = 10 * time.Second
	baseBuckets      = 12 // the base is the lowest delay of the last two minutes
)

// delayBase tracks the lowest one-way delay over the last baseBuckets
// buckets.
type delayBase struct {
	mins  [baseBuckets]time.Duration
	epoch [baseBuckets]int64 // which bucket period each slot holds
}

func (b *delayBase) add(now time.Time, d time.Duration) {
	e := now.UnixNano() / int64(baseBucket)
	i := e % baseBuckets
	if b.epoch[i] != e {
		b.epoch[i], b.mins[i] = e, d
	} else if d < b.mins[i] {
		b.mins[i] = d
	}
}

func (b *delayBase) min(now time.Time) (time.Duration, bool) {
	e := now.UnixNano() / int64(baseBucket)
	var m time.Duration
	ok := false
	for i := range baseBuckets {
		if b.epoch[i] > e-baseBuckets && b.epoch[i] != 0 && (!ok || b.mins[i] < m) {
			m, ok = b.mins[i], true
		}
	}
	return m, ok
}

// uplink measures one participant's media arriving at this node.
type uplink struct {
	mu        sync.Mutex
	bytes     int
	since     time.Time // start of the current window
	audioMin  time.Duration
	videoMin  time.Duration
	haveAudio bool
	haveVideo bool
	audioBase delayBase
	videoBase delayBase
}

// observe records one Object: its payload size and, when it carries a
// capture timestamp (µs), its one-way delay. Audio is preferred for delay:
// video's includes a varying encode time.
func (u *uplink) observe(now time.Time, audio bool, bytes int, capture uint64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.since.IsZero() {
		u.since = now
	}
	u.bytes += bytes
	if capture == 0 {
		return
	}
	d := time.Duration(now.UnixMicro()-int64(capture)) * time.Microsecond
	switch {
	case audio && (!u.haveAudio || d < u.audioMin):
		u.audioMin, u.haveAudio = d, true
	case !audio && (!u.haveVideo || d < u.videoMin):
		u.videoMin, u.haveVideo = d, true
	}
}

// feedback closes the current window and reports it; ok is false when no
// media arrived in it.
func (u *uplink) feedback(now time.Time) (*moqitv1.SendFeedback, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	elapsed := now.Sub(u.since)
	if u.bytes == 0 || elapsed <= 0 {
		u.since = now
		return nil, false
	}
	fb := &moqitv1.SendFeedback{ReceiveBitrate: uint32(float64(u.bytes*8) / elapsed.Seconds())}
	queued := func(win time.Duration, base *delayBase) {
		base.add(now, win)
		if m, ok := base.min(now); ok && win > m {
			fb.QueuingDelayMs = uint32((win - m) / time.Millisecond)
		}
	}
	switch {
	case u.haveAudio:
		queued(u.audioMin, &u.audioBase)
	case u.haveVideo:
		queued(u.videoMin, &u.videoBase)
	}
	if u.haveVideo && u.haveAudio {
		u.videoBase.add(now, u.videoMin)
	}
	u.bytes, u.since, u.haveAudio, u.haveVideo = 0, now, false, false
	return fb, true
}

// sendFeedback runs while the room is open, telling each local participant
// that sends media how it arrives.
func (r *Room) sendFeedback(stop <-chan struct{}) {
	t := time.NewTicker(feedbackInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			r.mu.Lock()
			ps := make([]*Participant, 0, len(r.participants))
			for _, p := range r.participants {
				ps = append(ps, p)
			}
			r.mu.Unlock()
			for _, p := range ps {
				if fb, ok := p.uplink.feedback(now); ok {
					p.inbox.send(&moqitv1.InboxMessage{Message: &moqitv1.InboxMessage_SendFeedback{SendFeedback: fb}})
				}
			}
		}
	}
}
