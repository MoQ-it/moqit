package server_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/MoQ-it/moqit/internal/testclient"
	"github.com/MoQ-it/moqit/pkg/dvr"
	"github.com/MoQ-it/moqit/pkg/room"
	"github.com/MoQ-it/moqit/pkg/server"
	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"github.com/MoQ-it/moqit/proto/moqit/v1/moqitv1connect"
	moqit "github.com/MoQ-it/moqit/sdk/go"
	"github.com/MoQ-it/moqt"
	v22 "github.com/MoQ-it/moqt/wire/v22"
	"github.com/alicebob/miniredis/v2"
)

func withDVR(t *testing.T, dir string) func(*server.Config) {
	return func(c *server.Config) {
		s, err := dvr.Open(dvr.Config{Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		c.DVR, c.DVRWindow, c.DVRFetchLimit = s, time.Hour, 4
	}
}

// recordedMic waits for the recorder's subscription to a participant's mic
// and writes n groups 250ms apart, each with a LOC timestamp.
func recordedMic(t *testing.T, c *testclient.Client, prefix string, n int) {
	t.Helper()
	var w *moqt.TrackWriter
	select {
	case w = <-c.Mic:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: mic never recorded", c.Identity)
	}
	if w.Context().Err() != nil {
		t.Fatal("recording ended")
	}
	for g := range uint64(n) {
		sg := w.OpenSubgroup(g, 0, moqt.SubgroupOptions{Properties: true})
		sg.WriteObject(&v22.Object{ID: 0, Properties: v22.KeyValues{{Type: 0x10, Int: uint64(time.Now().UnixMicro())}}, Payload: []byte(fmt.Sprintf("%s%d", prefix, g))})
		sg.Close()
		time.Sleep(250 * time.Millisecond)
	}
}

// micCheckpoints reads a participant's index until it lists at least n
// mic checkpoints.
func micCheckpoints(t *testing.T, c *testclient.Client, identity string, n int) ([]*moqitv1.DvrCheckpoint, []*moqitv1.DvrIndex) {
	t.Helper()
	var cps []*moqitv1.DvrCheckpoint
	var idx []*moqitv1.DvrIndex
	waitFor(t, "mic checkpoints", func() bool {
		var err error
		idx, err = c.DvrIndex(identity, 2)
		if err != nil {
			return false
		}
		cps = nil
		for _, i := range idx {
			for _, cp := range i.Checkpoints {
				if cp.Track == room.TrackMic {
					cps = append(cps, cp)
				}
			}
		}
		return len(cps) >= n
	})
	return cps, idx
}

func payloads(objs []*v22.FetchObject) []string {
	var out []string
	for _, o := range objs {
		out = append(out, string(o.Payload))
	}
	return out
}

func TestDVR(t *testing.T) {
	dir := t.TempDir()
	a := startNodeWith(t, nil, "A", withDVR(t, dir))
	alice := testclient.Join(t, a.addr, "r1", "alice", token(t, "alice", "r1"))
	alice.WaitRoom("dvr_window in room state", func(r *moqitv1.Room) bool { return r.DvrWindow == 3600 })
	alice.Send(testclient.MicTracks())
	recordedMic(t, alice, "a", 12)

	carol := testclient.Watch(t, a.addr, "r1", viewerToken(t, "carol", "r1"))
	cps, idx := micCheckpoints(t, carol, "alice", 2)
	last := idx[len(idx)-1]
	if len(last.Epochs) == 0 || last.Epochs[0].Codec != "opus" || last.Earliest == 0 || last.Latest < last.Earliest {
		t.Errorf("index %+v", last)
	}
	if cps[1].At-cps[0].At < 1e6 || cps[0].Loc == 0 {
		t.Errorf("checkpoints %+v", cps)
	}
	epoch, first := dvr.SplitGroup(cps[1].Group)
	objs, err := carol.Fetch("alice", room.TrackMic, cps[1].Group, cps[1].Group+2)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprint([]string{fmt.Sprintf("a%d", first), fmt.Sprintf("a%d", first+1), fmt.Sprintf("a%d", first+2)})
	if got := fmt.Sprint(payloads(objs)); got != want {
		t.Errorf("fetched %s; want %s (epoch %d)", got, want, epoch)
	}

	// Only viewers rewind; the index is fetch-only; FETCH needs an
	// absolute range.
	bob := testclient.Join(t, a.addr, "r1", "bob", token(t, "bob", "r1"))
	bob.Room = "r1"
	_, err = bob.Fetch("alice", room.TrackMic, cps[0].Group, cps[0].Group)
	wantCodeErr(t, "participant fetch", err, v22.ReqErrUnauthorized)
	_, err = carol.TrySubscribe("alice", room.TrackDvr)
	wantCodeErr(t, "subscribing to the index", err, v22.ReqErrDoesNotExist)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = carol.Sess.Fetch(ctx, v22.FullTrackName{Namespace: room.ParticipantNamespace("r1", "alice"), Name: room.TrackMic}, nil)
	wantCodeErr(t, "fetch without a range", err, v22.ReqErrInvalidRange)
	_, err = carol.Fetch("alice", room.TrackData, 0, 1)
	wantCodeErr(t, "fetching data", err, v22.ReqErrDoesNotExist)

	// History survives a restart of the node.
	a.Close()
	b := startNodeWith(t, nil, "B", withDVR(t, dir))
	dave := testclient.Watch(t, b.addr, "r1", viewerToken(t, "dave", "r1"))
	objs, err = dave.Fetch("alice", room.TrackMic, cps[1].Group, cps[1].Group+2)
	if err != nil || fmt.Sprint(payloads(objs)) != want {
		t.Errorf("after restart: %v, %v", payloads(objs), err)
	}
}

func TestDVROff(t *testing.T) {
	a := startNodeWith(t, nil, "A", func(c *server.Config) {
		withDVR(t, t.TempDir())(c)
		c.DVRWindow = 0
	})
	alice := testclient.Join(t, a.addr, "r1", "alice", token(t, "alice", "r1"))
	alice.Send(testclient.MicTracks())
	carol := testclient.Watch(t, a.addr, "r1", viewerToken(t, "carol", "r1"))
	_, err := carol.Fetch("alice", room.TrackDvr, 0, 1)
	wantCodeErr(t, "rewinding a room without history", err, v22.ReqErrUnauthorized)
	select {
	case <-alice.Mic:
		t.Error("a room without history was recorded")
	case <-time.After(500 * time.Millisecond):
	}
}

func TestDVRCluster(t *testing.T) {
	mr := miniredis.RunT(t)
	a := startNodeWith(t, mr, "A", withDVR(t, t.TempDir()))
	b := startNodeWith(t, mr, "B", withDVR(t, t.TempDir()))
	e := startNodeWith(t, mr, "E", func(c *server.Config) { c.Edge, c.Region = true, "eu" })
	leaf := startNodeWith(t, mr, "L", func(c *server.Config) { c.Edge, c.Tier, c.Region = true, 2, "eu" })

	// Alice records an epoch on A, then moves to B and records another.
	alice := testclient.Join(t, a.addr, "r1", "alice", token(t, "alice", "r1"))
	alice.Send(testclient.MicTracks())
	recordedMic(t, alice, "a", 6)
	alice.Sess.Close()
	alice = testclient.Join(t, b.addr, "r1", "alice", token(t, "alice", "r1"))
	alice.Send(testclient.MicTracks())
	recordedMic(t, alice, "b", 6)

	carol := testclient.Watch(t, e.addr, "r1", viewerToken(t, "carol", "r1"))
	cps, _ := micCheckpoints(t, carol, "alice", 1)
	epochB, _ := dvr.SplitGroup(cps[len(cps)-1].Group)
	objs, err := carol.Fetch("alice", room.TrackMic, dvr.ArchiveGroup(epochB, 0), dvr.ArchiveGroup(epochB, 1))
	if err != nil || fmt.Sprint(payloads(objs)) != "[b0 b1]" {
		t.Errorf("epoch on B through the edge: %v, %v", payloads(objs), err)
	}
	// The earlier epoch is on A.
	var epochA uint32
	waitFor(t, "an older epoch", func() bool {
		for ep := uint32(1); ep < epochB; ep++ {
			if objs, err := carol.Fetch("alice", room.TrackMic, dvr.ArchiveGroup(ep, 0), dvr.ArchiveGroup(ep, 1)); err == nil && len(objs) == 2 {
				epochA = ep
				return true
			}
		}
		return false
	})
	objs, _ = carol.Fetch("alice", room.TrackMic, dvr.ArchiveGroup(epochA, 0), dvr.ArchiveGroup(epochA, 1))
	if fmt.Sprint(payloads(objs)) != "[a0 a1]" {
		t.Errorf("epoch on A through the edge: %v", payloads(objs))
	}

	// Through a tier 2 edge, which asks its parent.
	dave := testclient.Watch(t, leaf.addr, "r1", viewerToken(t, "dave", "r1"))
	if cps, _ := micCheckpoints(t, dave, "alice", 1); cps[len(cps)-1].Group>>32 != uint64(epochB) {
		t.Errorf("index through the cascade: %+v", cps)
	}
	objs, err = dave.Fetch("alice", room.TrackMic, dvr.ArchiveGroup(epochA, 1), dvr.ArchiveGroup(epochA, 1))
	if err != nil || fmt.Sprint(payloads(objs)) != "[a1]" {
		t.Errorf("through the cascade: %v, %v", payloads(objs), err)
	}

	// History outlives the participant.
	alice.Sess.Close()
	waitFor(t, "alice gone", func() bool { return len(b.Manager.Room("r1").Participants()) == 0 })
	objs, err = carol.Fetch("alice", room.TrackMic, dvr.ArchiveGroup(epochB, 2), dvr.ArchiveGroup(epochB, 2))
	if err != nil || fmt.Sprint(payloads(objs)) != "[b2]" {
		t.Errorf("after alice left: %v, %v", payloads(objs), err)
	}
}

func TestCreateRoomDvrWindow(t *testing.T) {
	mr := miniredis.RunT(t)
	dvrNode := func(id string) (*node, moqitv1connect.RoomServiceClient) {
		n := startNodeWith(t, mr, id, func(c *server.Config) {
			withDVR(t, t.TempDir())(c)
			c.DVRWindow, c.DVRMaxWindow = 0, 2*time.Hour
		})
		api := httptest.NewServer(n.APIHandler())
		t.Cleanup(api.Close)
		return n, moqit.NewRoomServiceClient(api.URL, apiKey, apiSecret)
	}
	_, apiA := dvrNode("A")
	b, _ := dvrNode("B")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	created, err := apiA.CreateRoom(ctx, connect.NewRequest(&moqitv1.CreateRoomRequest{Name: "r1", DvrWindow: 99999}))
	if err != nil || created.Msg.Room.DvrWindow != 7200 {
		t.Fatalf("CreateRoom: %v, %v; want the window capped at 7200", created, err)
	}
	// The room's window holds on another node, and its participants are
	// recorded there.
	alice := testclient.Join(t, b.addr, "r1", "alice", token(t, "alice", "r1"))
	alice.WaitRoom("dvr_window from the cluster", func(r *moqitv1.Room) bool { return r.DvrWindow == 7200 })
	alice.Send(testclient.MicTracks())
	select {
	case <-alice.Mic:
	case <-time.After(5 * time.Second):
		t.Fatal("not recorded")
	}
	// Rooms created on join get the default: none here.
	created, _ = apiA.CreateRoom(ctx, connect.NewRequest(&moqitv1.CreateRoomRequest{Name: "r2"}))
	if created.Msg.Room.DvrWindow != 0 {
		t.Errorf("default window %d; want 0", created.Msg.Room.DvrWindow)
	}
}
