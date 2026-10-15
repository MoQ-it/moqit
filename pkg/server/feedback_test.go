package server_test

import (
	"testing"
	"time"

	"github.com/MoQ-it/moqit/internal/testclient"
	"github.com/MoQ-it/moqit/pkg/room"
	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"github.com/MoQ-it/moqt"
	v22 "github.com/MoQ-it/moqt/wire/v22"
)

// A publisher hears how its media arrives: the delay its uplink adds and
// the rate the node receives.
func TestSendFeedback(t *testing.T) {
	a := startNodeWith(t, nil, "A", nil)
	alice := testclient.Join(t, a.addr, "r1", "alice", token(t, "alice", "r1"))
	alice.Send(testclient.MicTracks())
	bob := testclient.Join(t, a.addr, "r1", "bob", token(t, "bob", "r1"))
	bob.Subscribe("alice", room.TrackMic)
	var w *moqt.TrackWriter
	select {
	case w = <-alice.Mic:
	case <-time.After(5 * time.Second):
		t.Fatal("mic not subscribed")
	}
	// 20ms frames of 80 bytes; from frame 75 they carry a capture time
	// 300ms in the past, as if they had queued that long.
	go func() {
		for g := uint64(0); g < 150; g++ {
			late := time.Duration(0)
			if g >= 75 {
				late = 300 * time.Millisecond
			}
			sg := w.OpenSubgroup(g, 0, moqt.SubgroupOptions{Properties: true, EndOfGroup: true})
			sg.WriteObject(&v22.Object{ID: 0, Properties: v22.KeyValues{{Type: 0x10, Int: uint64(time.Now().Add(-late).UnixMicro())}}, Payload: make([]byte, 80)})
			sg.Close()
			time.Sleep(20 * time.Millisecond)
		}
	}()
	var fbs []*moqitv1.SendFeedback
	deadline := time.After(6 * time.Second)
	for len(fbs) < 1 || fbs[len(fbs)-1].QueuingDelayMs < 250 {
		select {
		case m := <-alice.Inbox:
			if fb := m.GetSendFeedback(); fb != nil {
				fbs = append(fbs, fb)
			}
		case <-deadline:
			t.Fatalf("feedback %v; want a queuing delay of about 300ms", fbs)
		}
	}
	if fbs[0].QueuingDelayMs > 50 || fbs[0].ReceiveBitrate < 20_000 || fbs[0].ReceiveBitrate > 45_000 {
		t.Errorf("first feedback %v; want little delay and about 32 kbps", fbs[0])
	}
	if d := fbs[len(fbs)-1].QueuingDelayMs; d > 350 {
		t.Errorf("queuing delay %d; want about 300", d)
	}
	// Bob, who sends nothing, hears nothing.
	select {
	case m := <-bob.Inbox:
		if m.GetSendFeedback() != nil {
			t.Error("feedback to a participant sending no media")
		}
	case <-time.After(600 * time.Millisecond):
	}
}
