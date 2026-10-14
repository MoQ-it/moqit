package bench

import (
	"context"
	"sync"
	"time"

	"github.com/MoQ-it/moqit/pkg/room"
	"github.com/MoQ-it/moqt"
	v22 "github.com/MoQ-it/moqt/wire/v22"
)

// Synthetic media shaped like a browser's: Opus at 64 kbps in 20 ms
// frames, one Group each; video at 30 fps with a Group every 2 s.
const (
	audioFrame   = 20 * time.Millisecond
	audioBytes   = 160
	videoFPS     = 30
	framesPerGOP = 60
	locTimestamp = 0x10 // LOC capture timestamp, µs
)

var videoKbps = map[string]int{"camera-h": 1500, "camera-m": 600, "camera-l": 200}

// publisher answers the relay's SUBSCRIBEs for the synthetic tracks.
type publisher struct {
	moqt.RejectAll
	ctx context.Context

	mu      sync.Mutex
	writers map[string][]*moqt.TrackWriter // by track name
}

func (p *publisher) HandleSubscribe(req *moqt.SubscribeRequest) {
	name := req.Track.Name
	_, video := videoKbps[name]
	if name != room.TrackSignal && name != room.TrackMic && !video {
		req.Reject(v22.ReqErrDoesNotExist, "not published")
		return
	}
	w, err := req.Accept(moqt.AcceptOptions{})
	if err != nil {
		return
	}
	if name == room.TrackSignal {
		return // the server reads it; we never send requests
	}
	p.mu.Lock()
	first := len(p.writers[name]) == 0
	p.writers[name] = append(p.writers[name], w)
	p.mu.Unlock()
	if first {
		go p.produce(name)
	}
}

// live returns the track's writers, dropping ended ones.
func (p *publisher) live(name string) []*moqt.TrackWriter {
	p.mu.Lock()
	defer p.mu.Unlock()
	ws := p.writers[name][:0]
	for _, w := range p.writers[name] {
		if w.Context().Err() == nil {
			ws = append(ws, w)
		}
	}
	p.writers[name] = ws
	return append([]*moqt.TrackWriter(nil), ws...)
}

func (p *publisher) produce(name string) {
	interval, size, perGroup := audioFrame, audioBytes, 1
	if kbps, ok := videoKbps[name]; ok {
		interval, size, perGroup = time.Second/videoFPS, kbps*1000/8/videoFPS, framesPerGOP
	}
	payload := make([]byte, size)
	t := time.NewTicker(interval)
	defer t.Stop()
	var group, object uint64
	subgroups := map[*moqt.TrackWriter]*moqt.OutgoingSubgroup{}
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-t.C:
		}
		ws := p.live(name)
		if len(ws) == 0 {
			return
		}
		props := v22.KeyValues{{Type: locTimestamp, Int: uint64(time.Now().UnixMicro())}}
		last := object == uint64(perGroup-1)
		for _, w := range ws {
			sg := subgroups[w]
			if sg == nil || object == 0 {
				sg = w.OpenSubgroup(group, 0, moqt.SubgroupOptions{Properties: true, FirstObject: object == 0, EndOfGroup: perGroup == 1})
				subgroups[w] = sg
			}
			if sg.WriteObject(&v22.Object{ID: object, Properties: props, Payload: payload}) != nil || last {
				sg.Close()
				delete(subgroups, w)
			}
		}
		if object++; last {
			group, object = group+1, 0
		}
	}
}

// publish joins the room as the synthetic publisher.
func (b *bench) publish(ctx context.Context) (*moqt.Session, error) {
	tok, err := b.token(b.cfg.Publisher, false)
	if err != nil {
		return nil, err
	}
	p := &publisher{ctx: ctx, writers: map[string][]*moqt.TrackWriter{}}
	sess, err := b.dial(ctx, tok, p)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := sess.PublishNamespace(pctx, room.ParticipantNamespace(b.cfg.Room, b.cfg.Publisher), nil); err != nil {
		sess.Close()
		return nil, err
	}
	return sess, nil
}
