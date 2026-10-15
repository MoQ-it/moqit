package client

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// drain returns the queued objects as "group/id" strings.
func drain(s *subscriber) string {
	var out []string
	for {
		select {
		case o := <-s.q:
			out = append(out, fmt.Sprintf("%d/%d", o.Group, o.ID))
		default:
			return strings.Join(out, " ")
		}
	}
}

func attach(t *Track, size int) *subscriber {
	s := &subscriber{t: t, q: make(chan *object, size)}
	t.mu.Lock()
	t.subs[s] = struct{}{}
	t.mu.Unlock()
	return s
}

func TestVideoGroups(t *testing.T) {
	tr := newTrack("camera-720", false)
	s := attach(tr, 3)
	tr.WriteVideo(time.UnixMicro(1), false, nil) // before the first keyframe: dropped
	tr.WriteVideo(time.UnixMicro(2), true, nil)
	tr.WriteVideo(time.UnixMicro(3), false, nil)
	tr.WriteVideo(time.UnixMicro(4), false, nil)
	tr.WriteVideo(time.UnixMicro(5), false, nil) // queue full: the rest of the group is skipped
	tr.WriteVideo(time.UnixMicro(6), false, nil)
	if got := drain(s); got != "0/0 0/1 0/2" {
		t.Errorf("first group: %s", got)
	}
	tr.WriteVideo(time.UnixMicro(7), false, nil)
	tr.WriteVideo(time.UnixMicro(8), true, nil)
	tr.WriteVideo(time.UnixMicro(9), false, nil)
	if got := drain(s); got != "1/0 1/1" {
		t.Errorf("after falling behind: %s", got)
	}
	if len(tr.gop) != 2 || tr.gop[0].TS != 8 || tr.gop[1].TS != 9 {
		t.Errorf("cached GOP: %+v", tr.gop)
	}

	tr.setMuted(true)
	tr.WriteVideo(time.UnixMicro(10), false, nil)
	tr.WriteVideo(time.UnixMicro(11), true, nil)
	if got := drain(s); got != "" {
		t.Errorf("muted: %s", got)
	}
	tr.setMuted(false)
	tr.WriteVideo(time.UnixMicro(12), false, nil) // unmuted mid-group: wait for a keyframe
	tr.WriteVideo(time.UnixMicro(13), true, nil)
	if got := drain(s); got != "3/0" {
		t.Errorf("unmuted: %s", got)
	}

	tr.close()
	if _, ok := <-s.q; ok {
		t.Error("queue still open after close")
	}
	tr.WriteVideo(time.UnixMicro(14), true, nil) // after close: ignored, no panic
}

func TestAudioGroups(t *testing.T) {
	tr := newTrack("mic", true)
	s := attach(tr, 2)
	for i := range 4 {
		tr.WriteAudio(time.UnixMicro(int64(i)), 30, []byte{1})
	}
	if got := drain(s); got != "0/0 1/0" {
		t.Errorf("audio: %s", got)
	}
	tr.WriteAudio(time.UnixMicro(4), 30, nil) // audio drops single frames only
	if got := drain(s); got != "4/0" {
		t.Errorf("after drops: %s", got)
	}
	o := &object{ID: 0, TS: 1234, Level: 30, Payload: []byte{1}}
	w := o.wire()
	if len(w.Properties) != 2 || w.Properties[0].Type != locTimestamp || w.Properties[0].Int != 1234 || w.Properties[1].Type != locAudioLevel || w.Properties[1].Int != 30 {
		t.Errorf("audio properties: %+v", w.Properties)
	}
	if w := (&object{TS: 1, Level: -1}).wire(); len(w.Properties) != 1 {
		t.Errorf("video properties: %+v", w.Properties)
	}
}
