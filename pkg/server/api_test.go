package server_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/MoQ-it/moqit/internal/testclient"
	"github.com/MoQ-it/moqit/pkg/auth"
	"github.com/MoQ-it/moqit/pkg/server"
	"github.com/MoQ-it/moqit/pkg/webhook"
	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"github.com/MoQ-it/moqit/proto/moqit/v1/moqitv1connect"
	moqit "github.com/MoQ-it/moqit/sdk/go"
	"github.com/alicebob/miniredis/v2"
)

// hooks collects verified webhook events.
type hooks struct {
	mu     sync.Mutex
	events []string
}

func (h *hooks) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ev, err := webhook.Receive(r, keys)
	if err != nil {
		http.Error(w, err.Error(), 401)
		return
	}
	s := ev.Event
	if ev.Participant != nil {
		s += ":" + ev.Participant.Identity
	}
	if ev.Track != nil {
		s += ":" + ev.Track.Name
	}
	h.mu.Lock()
	h.events = append(h.events, s)
	h.mu.Unlock()
}

func (h *hooks) wait(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		ok := slices.Contains(h.events, want)
		h.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	t.Fatalf("webhook %q never arrived; got %v", want, h.events)
}

func (h *hooks) count(want string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, e := range h.events {
		if e == want {
			n++
		}
	}
	return n
}

func startAPINode(t *testing.T, mr *miniredis.Miniredis, id string, hookURL string) (*node, moqitv1connect.RoomServiceClient) {
	t.Helper()
	n := startNodeWith(t, mr, id, func(c *server.Config) {
		c.Webhook = webhook.NewNotifier([]string{hookURL}, apiKey, apiSecret, nil)
	})
	api := httptest.NewServer(n.APIHandler())
	t.Cleanup(api.Close)
	return n, moqit.NewRoomServiceClient(api.URL, apiKey, apiSecret)
}

func TestRoomServiceAcrossCluster(t *testing.T) {
	mr := miniredis.RunT(t)
	h := &hooks{}
	hookSrv := httptest.NewServer(h)
	defer hookSrv.Close()
	a, apiA := startAPINode(t, mr, "A", hookSrv.URL)
	b, apiB := startAPINode(t, mr, "B", hookSrv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	created, err := apiA.CreateRoom(ctx, connect.NewRequest(&moqitv1.CreateRoomRequest{Name: "r1", Metadata: "m0", MaxParticipants: 5}))
	if err != nil || created.Msg.Room.Sid == "" || created.Msg.Room.Metadata != "m0" {
		t.Fatalf("CreateRoom: %v, %v", created, err)
	}
	h.wait(t, "room_started")
	rooms, err := apiB.ListRooms(ctx, connect.NewRequest(&moqitv1.ListRoomsRequest{}))
	if err != nil || len(rooms.Msg.Rooms) != 1 || rooms.Msg.Rooms[0].Sid != created.Msg.Room.Sid {
		t.Fatalf("ListRooms on another node: %v, %v", rooms, err)
	}

	alice := join(t, a, "alice")
	bob := join(t, b, "bob")
	bob.WaitState("both", testclient.Present("alice", "bob"))
	h.wait(t, "participant_joined:alice")
	h.wait(t, "participant_joined:bob")
	if alice.RoomMetadata() != "m0" {
		t.Errorf("room metadata seen by alice: %q", alice.RoomMetadata())
	}

	// Viewers are counted but are not participants and send no webhooks.
	testclient.Watch(t, b.addr, "r1", viewerToken(t, "carol", "r1"))
	waitFor(t, "ListRooms counting the viewer", func() bool {
		rooms, err := apiB.ListRooms(ctx, connect.NewRequest(&moqitv1.ListRoomsRequest{}))
		return err == nil && len(rooms.Msg.Rooms) == 1 && rooms.Msg.Rooms[0].NumViewers == 1
	})
	alice.WaitRoom("viewer count in room state", func(r *moqitv1.Room) bool { return r.NumViewers == 1 })
	ps, err := apiB.ListParticipants(ctx, connect.NewRequest(&moqitv1.ListParticipantsRequest{Room: "r1"}))
	if err != nil || len(ps.Msg.Participants) != 2 {
		t.Fatalf("ListParticipants: %v, %v", ps, err)
	}
	if h.count("participant_joined:carol") != 0 {
		t.Error("participant_joined sent for a viewer")
	}

	alice.Send(testclient.MicTracks())
	h.wait(t, "track_published:alice:microphone")
	p, err := apiB.GetParticipant(ctx, connect.NewRequest(&moqitv1.GetParticipantRequest{Room: "r1", Identity: "alice"}))
	if err != nil || len(p.Msg.Participant.Tracks) != 1 {
		t.Fatalf("GetParticipant: %v, %v", p, err)
	}
	micSid := p.Msg.Participant.Tracks[0].Sid

	// Changes made through node B reach alice on node A.
	md := "updated by the API"
	upd, err := apiB.UpdateParticipant(ctx, connect.NewRequest(&moqitv1.UpdateParticipantRequest{Room: "r1", Identity: "alice", Metadata: &md, Attributes: map[string]string{"role": "host"}}))
	if err != nil || upd.Msg.Participant.Metadata != md {
		t.Fatalf("UpdateParticipant: %v, %v", upd, err)
	}
	bob.WaitState("alice's new metadata", func(s map[string]*moqitv1.ParticipantInfo) bool {
		return s["alice"].GetMetadata() == md && s["alice"].GetAttributes()["role"] == "host"
	})

	if _, err := apiB.MuteTrack(ctx, connect.NewRequest(&moqitv1.MuteTrackRequest{Room: "r1", Identity: "alice", TrackSid: micSid, Muted: true})); err != nil {
		t.Fatal(err)
	}
	waitInbox(t, alice, func(m *moqitv1.InboxMessage) bool {
		return m.GetMuteTrack().GetTrackSid() == micSid && m.GetMuteTrack().GetMuted()
	})
	bob.WaitState("alice's mic muted", func(s map[string]*moqitv1.ParticipantInfo) bool {
		tr := s["alice"].GetTracks()
		return len(tr) == 1 && tr[0].Muted
	})

	if _, err := apiA.SendData(ctx, connect.NewRequest(&moqitv1.SendDataRequest{Room: "r1", Topic: "announce", Payload: []byte("hello all")})); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*testclient.Client{alice, bob} {
		waitInbox(t, c, func(m *moqitv1.InboxMessage) bool {
			return string(m.GetData().GetPayload()) == "hello all" && m.GetData().GetFromIdentity() == ""
		})
	}

	if _, err := apiB.UpdateRoomMetadata(ctx, connect.NewRequest(&moqitv1.UpdateRoomMetadataRequest{Room: "r1", Metadata: "m1"})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "room metadata on both nodes", func() bool { return alice.RoomMetadata() == "m1" && bob.RoomMetadata() == "m1" })

	// Remove bob (on node B) through node A.
	if _, err := apiA.RemoveParticipant(ctx, connect.NewRequest(&moqitv1.RemoveParticipantRequest{Room: "r1", Identity: "bob"})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-bob.Sess.Context().Done():
	case <-ctx.Done():
		t.Fatal("bob not disconnected")
	}
	alice.WaitState("bob removed", func(s map[string]*moqitv1.ParticipantInfo) bool { return s["bob"] == nil })
	h.wait(t, "participant_left:bob")

	// Errors.
	_, err = apiA.GetParticipant(ctx, connect.NewRequest(&moqitv1.GetParticipantRequest{Room: "r1", Identity: "nobody"}))
	wantCode(t, "unknown participant", err, connect.CodeNotFound)

	if _, err := apiA.DeleteRoom(ctx, connect.NewRequest(&moqitv1.DeleteRoomRequest{Room: "r1"})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-alice.Sess.Context().Done():
	case <-ctx.Done():
		t.Fatal("alice not disconnected by DeleteRoom")
	}
	h.wait(t, "participant_left:alice")
	h.wait(t, "room_finished")
	time.Sleep(200 * time.Millisecond)
	if n := h.count("room_finished"); n != 1 {
		t.Errorf("room_finished sent %d times; want once", n)
	}
	rooms, _ = apiB.ListRooms(ctx, connect.NewRequest(&moqitv1.ListRoomsRequest{}))
	if len(rooms.Msg.Rooms) != 0 {
		t.Errorf("rooms after delete: %v", rooms.Msg.Rooms)
	}
}

func TestRoomServiceAuth(t *testing.T) {
	mr := miniredis.RunT(t)
	h := &hooks{}
	hookSrv := httptest.NewServer(h)
	defer hookSrv.Close()
	a, _ := startAPINode(t, mr, "A", hookSrv.URL)
	api := httptest.NewServer(a.APIHandler())
	defer api.Close()
	ctx := context.Background()
	call := func(g *auth.RoomGrant) error {
		c := moqitv1connect.NewRoomServiceClient(http.DefaultClient, api.URL, connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				if g != nil {
					tok, _ := auth.NewAccessToken(apiKey, apiSecret).SetIdentity("backend").SetRoomGrant(g).ToJWT()
					req.Header().Set("Authorization", "Bearer "+tok)
				}
				return next(ctx, req)
			}
		})))
		_, err := c.ListParticipants(ctx, connect.NewRequest(&moqitv1.ListParticipantsRequest{Room: "r1"}))
		return err
	}
	wantCode(t, "no token", call(nil), connect.CodeUnauthenticated)
	wantCode(t, "join grant only", call(&auth.RoomGrant{Join: true, Room: "r1"}), connect.CodePermissionDenied)
	wantCode(t, "admin of another room", call(&auth.RoomGrant{Admin: true, Room: "r2"}), connect.CodePermissionDenied)
	wantCode(t, "admin of r1, room missing", call(&auth.RoomGrant{Admin: true, Room: "r1"}), connect.CodeNotFound)
}

func wantCode(t *testing.T, what string, err error, code connect.Code) {
	t.Helper()
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != code {
		t.Errorf("%s: %v; want %v", what, err, code)
	}
}

func waitInbox(t *testing.T, c *testclient.Client, ok func(*moqitv1.InboxMessage) bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-c.Inbox:
			if ok(m) {
				return
			}
		case <-deadline:
			t.Fatalf("%s: expected inbox message never arrived", c.Identity)
		}
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal(fmt.Sprintf("timed out: %s", what))
}
