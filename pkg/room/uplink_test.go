package room

import (
	"testing"
	"time"
)

func TestUplinkFeedback(t *testing.T) {
	var u uplink
	t0 := time.Unix(1_000_000, 0)
	offset := int64(5_000_000) // the publisher's clock runs 5s behind
	// 50 audio frames of 100 bytes in each half second, sent as captured.
	send := func(at time.Time, queued time.Duration, video bool) {
		for i := range 25 {
			capture := at.Add(time.Duration(i) * 20 * time.Millisecond)
			u.observe(capture.Add(queued), !video, 100, uint64(capture.UnixMicro()-offset))
		}
	}
	now := t0
	for range 4 {
		send(now, 10*time.Millisecond, false)
		now = now.Add(feedbackInterval)
		fb, ok := u.feedback(now)
		if !ok || fb.QueuingDelayMs != 0 {
			t.Fatalf("steady: %v, %v", fb, ok)
		}
	}
	// A queue builds up: the delay over the base shows it.
	send(now, 310*time.Millisecond, false)
	now = now.Add(feedbackInterval)
	fb, _ := u.feedback(now)
	if fb.QueuingDelayMs < 290 || fb.QueuingDelayMs > 310 {
		t.Errorf("queued 300ms: %d", fb.QueuingDelayMs)
	}
	if fb.ReceiveBitrate < 35_000 || fb.ReceiveBitrate > 45_000 {
		t.Errorf("bitrate %d; want 25 x 100 B per 0.5s = 40 kbps", fb.ReceiveBitrate)
	}
	// No media: no feedback.
	now = now.Add(feedbackInterval)
	if _, ok := u.feedback(now); ok {
		t.Error("feedback without media")
	}
	// The base forgets after two minutes, so a lasting change of path
	// stops reading as queueing.
	for range 300 {
		send(now, 200*time.Millisecond, false)
		now = now.Add(feedbackInterval)
		fb, _ = u.feedback(now)
	}
	if fb.QueuingDelayMs != 0 {
		t.Errorf("after two minutes at a new delay: %d", fb.QueuingDelayMs)
	}
}

func TestUplinkVideoOnly(t *testing.T) {
	var u uplink
	now := time.Unix(1_000_000, 0)
	window := func(queued time.Duration) uint32 {
		for i := range 15 {
			capture := now.Add(time.Duration(i) * 33 * time.Millisecond)
			u.observe(capture.Add(queued), false, 1000, uint64(capture.UnixMicro()))
		}
		now = now.Add(feedbackInterval)
		fb, _ := u.feedback(now)
		return fb.QueuingDelayMs
	}
	window(30 * time.Millisecond)
	if d := window(230 * time.Millisecond); d < 190 || d > 210 {
		t.Errorf("video queued 200ms: %d", d)
	}
}
