package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MoQ-it/moqit/internal/testclient"
	"github.com/MoQ-it/moqit/pkg/auth"
	"github.com/MoQ-it/moqit/pkg/room"
	"github.com/MoQ-it/moqit/pkg/server"
	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"github.com/MoQ-it/moqt"
	v22 "github.com/MoQ-it/moqt/wire/v22"
	"github.com/alicebob/miniredis/v2"
)

func viewerToken(t *testing.T, identity, rm string) string {
	tok, err := auth.NewAccessToken(apiKey, apiSecret).SetIdentity(identity).
		SetRoomGrant(&auth.RoomGrant{Viewer: true, Room: rm}).ToJWT()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// sendMic writes one object to the next subscription of a participant's mic.
func sendMic(t *testing.T, c *testclient.Client, payload string) *moqt.TrackWriter {
	t.Helper()
	select {
	case w := <-c.Mic:
		writeGroup(w, 0, payload)
		return w
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: mic never subscribed", c.Identity)
		return nil
	}
}

func writeGroup(w *moqt.TrackWriter, group uint64, payload string) {
	sg := w.OpenSubgroup(group, 0, moqt.SubgroupOptions{})
	sg.WriteObject(&v22.Object{ID: 0, Payload: []byte(payload)})
	sg.Close()
}

func wantCodeErr(t *testing.T, what string, err error, code v22.RequestErrorCode) {
	t.Helper()
	var re *moqt.RequestError
	if !errors.As(err, &re) || re.Code != code {
		t.Errorf("%s: %v; want request error %v", what, err, code)
	}
}

// rejected reports whether the node refuses a session with tok; the
// refusal may arrive just after setup.
func rejected(t *testing.T, n *node, tok string) bool {
	t.Helper()
	c, err := testclient.Dial(t, n.addr, "", tok)
	if err != nil {
		return true
	}
	select {
	case <-c.Sess.Context().Done():
		return true
	case <-time.After(time.Second):
		return false
	}
}

func TestViewer(t *testing.T) {
	a := startNodeWith(t, nil, "A", nil)
	alice := testclient.Join(t, a.addr, "r1", "alice", token(t, "alice", "r1"))
	alice.Send(testclient.MicTracks())

	carol := testclient.Watch(t, a.addr, "r1", viewerToken(t, "carol", "r1"))
	carol.WaitState("alice in the viewer's room state", func(s map[string]*moqitv1.ParticipantInfo) bool {
		return len(s["alice"].GetTracks()) == 1
	})
	sub := carol.Subscribe("alice", room.TrackMic)
	sendMic(t, alice, "opus")
	if got := testclient.NextObject(t, sub); got != "opus" {
		t.Errorf("viewer got %q", got)
	}

	alice.WaitRoom("viewer counted", func(r *moqitv1.Room) bool { return r.NumViewers == 1 })
	if s := alice.State(); s["carol"] != nil {
		t.Error("the viewer appears in room state")
	}
	if ps := a.Manager.Room("r1").Participants(); len(ps) != 1 {
		t.Errorf("participants %v; want alice only", ps)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := carol.Sess.PublishNamespace(ctx, room.ParticipantNamespace("r1", "carol"), nil); err == nil {
		t.Error("a viewer published a namespace")
	}
	_, err := carol.Sess.Subscribe(ctx, v22.FullTrackName{Namespace: room.InboxNamespace("r1", "alice"), Name: room.TrackInbox}, nil)
	wantCodeErr(t, "another's inbox", err, v22.ReqErrUnauthorized)
	_, err = carol.TrySubscribe("alice", room.TrackSignal)
	wantCodeErr(t, "signal track", err, v22.ReqErrUnauthorized)
	_, err = carol.Sess.Subscribe(ctx, v22.FullTrackName{Namespace: room.ParticipantNamespace("r2", "alice"), Name: room.TrackMic}, nil)
	wantCodeErr(t, "another room", err, v22.ReqErrUnauthorized)

	carol.Sess.Close()
	alice.WaitRoom("viewer gone", func(r *moqitv1.Room) bool { return r.NumViewers == 0 })
}

func TestViewerCap(t *testing.T) {
	a := startNodeWith(t, nil, "A", func(c *server.Config) { c.MaxViewers = 1 })
	first := testclient.Watch(t, a.addr, "r1", viewerToken(t, "v1", "r1"))
	if !rejected(t, a, viewerToken(t, "v2", "r1")) {
		t.Fatal("a second viewer was admitted past MaxViewers")
	}
	first.Sess.Close()
	waitFor(t, "viewer slot released", func() bool { return a.Manager.Viewers() == 0 })
	if rejected(t, a, viewerToken(t, "v3", "r1")) {
		t.Fatal("viewer refused after a slot freed")
	}
	// Participants don't count against MaxViewers.
	testclient.Join(t, a.addr, "r1", "alice", token(t, "alice", "r1"))
}

func TestEdge(t *testing.T) {
	mr := miniredis.RunT(t)
	a := startNode(t, mr, "A")
	e := startNodeWith(t, mr, "E", func(c *server.Config) { c.Edge = true })

	if !rejected(t, e, token(t, "bob", "r1")) {
		t.Fatal("an edge admitted a participant")
	}

	alice := join(t, a, "alice")
	alice.Send(testclient.MicTracks())
	tok, _ := auth.NewAccessToken(apiKey, apiSecret).SetIdentity("mallory").
		SetRoomGrant(&auth.RoomGrant{Join: true, Room: "r1", PublishSources: []string{"microphone"}}).ToJWT()
	mallory := testclient.Join(t, a.addr, "r1", "mallory", tok)

	carol := testclient.Watch(t, e.addr, "r1", viewerToken(t, "carol", "r1"))
	carol.WaitState("room state through the edge", func(s map[string]*moqitv1.ParticipantInfo) bool {
		return len(s["alice"].GetTracks()) == 1 && s["mallory"] != nil
	})
	sub := carol.Subscribe("alice", room.TrackMic)
	sendMic(t, alice, "from A")
	if got := testclient.NextObject(t, sub); got != "from A" {
		t.Errorf("viewer on the edge got %q", got)
	}
	// The edge can't know mallory's grant; her node refuses the track.
	_, err := carol.TrySubscribe("mallory", "camera-h")
	wantCodeErr(t, "a source the publisher may not publish", err, v22.ReqErrUnauthorized)

	alice.WaitRoom("edge viewers counted", func(r *moqitv1.Room) bool { return r.NumViewers == 1 })
	if ps := a.Manager.Room("r1").Participants(); len(ps) != 2 {
		t.Errorf("participants %v; want alice and mallory", ps)
	}

	// The room moves to B: once it closes on A, the viewer's room track
	// ends and is fetched again from B.
	b := startNode(t, mr, "B")
	bob := join(t, b, "bob")
	carol.WaitState("bob", testclient.Present("bob"))
	alice.Sess.Close()
	mallory.Sess.Close()
	waitFor(t, "room closed on A", func() bool { return a.Manager.Room("r1") == nil })
	md := "on B"
	bob.Send(&moqitv1.ClientSignal{Signal: &moqitv1.ClientSignal_UpdateParticipant{UpdateParticipant: &moqitv1.UpdateParticipant{Metadata: &md}}})
	carol.WaitStateFor("room state from B", 10*time.Second, func(s map[string]*moqitv1.ParticipantInfo) bool {
		return s["alice"] == nil && s["bob"].GetMetadata() == "on B"
	})

	// The publisher comes back on B; the viewer's next subscribe finds her
	// there.
	alice = join(t, b, "alice")
	alice.Send(testclient.MicTracks())
	sub = carol.Subscribe("alice", room.TrackMic)
	sendMic(t, alice, "from B")
	if got := testclient.NextObject(t, sub); got != "from B" {
		t.Errorf("viewer got %q after the publisher moved", got)
	}
}

// A viewer on a tier 2 edge is served through a tier 1 edge, and straight
// from the origin once that edge goes away.
func TestCascade(t *testing.T) {
	mr := miniredis.RunT(t)
	a := startNode(t, mr, "A")
	regional := startNodeWith(t, mr, "R", func(c *server.Config) { c.Edge, c.Tier, c.Region = true, 1, "eu" })
	leaf := startNodeWith(t, mr, "L", func(c *server.Config) { c.Edge, c.Tier, c.Region = true, 2, "eu" })

	alice := join(t, a, "alice")
	alice.Send(testclient.MicTracks())
	carol := testclient.Watch(t, leaf.addr, "r1", viewerToken(t, "carol", "r1"))
	carol.WaitState("room state through two edges", func(s map[string]*moqitv1.ParticipantInfo) bool {
		return len(s["alice"].GetTracks()) == 1
	})
	sub := carol.Subscribe("alice", room.TrackMic)
	mic := sendMic(t, alice, "two hops")
	if got := testclient.NextObject(t, sub); got != "two hops" {
		t.Errorf("viewer got %q", got)
	}
	if n := regional.Manager.Relay().TrackCount(); n != 2 {
		t.Errorf("the tier 1 edge relays %d tracks; want the room track and the mic", n)
	}

	regional.Close()
	md := "after the regional edge left"
	alice.Send(&moqitv1.ClientSignal{Signal: &moqitv1.ClientSignal_UpdateParticipant{UpdateParticipant: &moqitv1.UpdateParticipant{Metadata: &md}}})
	carol.WaitStateFor("room state straight from the origin", 15*time.Second, func(s map[string]*moqitv1.ParticipantInfo) bool {
		return s["alice"].GetMetadata() == md
	})
	// The origin may still hold its subscription to alice's mic (relays
	// linger), so the new path is served by the same writer.
	sub = carol.Subscribe("alice", room.TrackMic)
	select {
	case w := <-alice.Mic:
		writeGroup(w, 0, "one hop")
	case <-time.After(time.Second):
		writeGroup(mic, 1, "one hop")
	}
	if got := testclient.NextObject(t, sub); got != "one hop" {
		t.Errorf("viewer got %q after the regional edge left", got)
	}
}

func TestRouteEdges(t *testing.T) {
	mr := miniredis.RunT(t)
	a := startNode(t, mr, "A")
	eu := startNodeWith(t, mr, "EU", func(c *server.Config) { c.Edge, c.Region, c.MaxViewers = true, "eu", 10 })
	us := startNodeWith(t, mr, "US", func(c *server.Config) { c.Edge, c.Region, c.MaxViewers = true, "us", 10 })
	url := func(n *node) string { return "https://" + n.addr + "/moq" }
	route := func(via *node, tok, query string) (int, string) {
		rec := httptest.NewRecorder()
		via.RouteHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/route?token="+tok+query, nil))
		var out struct{ URL string }
		json.NewDecoder(rec.Body).Decode(&out)
		return rec.Code, out.URL
	}
	for _, via := range []*node{a, eu} {
		if _, got := route(via, token(t, "p", "r1"), ""); got != url(a) {
			t.Errorf("participant routed to %s; want the origin", got)
		}
	}
	seen := map[string]bool{}
	for range 40 {
		_, got := route(a, viewerToken(t, "v", "r1"), "")
		seen[got] = true
	}
	if !seen[url(eu)] || !seen[url(us)] || seen[url(a)] {
		t.Errorf("viewers routed to %v; want both edges only", seen)
	}
	for range 10 {
		if _, got := route(a, viewerToken(t, "v", "r1"), "&region=eu"); got != url(eu) {
			t.Fatalf("viewer in eu routed to %s", got)
		}
	}

	solo := miniredis.RunT(t)
	edge := startNodeWith(t, solo, "E", func(c *server.Config) { c.Edge = true })
	if code, _ := route(edge, token(t, "p", "r1"), ""); code != 503 {
		t.Errorf("participant with only an edge: %d; want 503", code)
	}
	if _, got := route(edge, viewerToken(t, "v", "r1"), ""); got != url(edge) {
		t.Errorf("viewer with only an edge routed to %s", got)
	}
}
