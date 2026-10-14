package server_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MoQ-it/moqit/internal/testclient"
	"github.com/MoQ-it/moqit/pkg/auth"
	"github.com/MoQ-it/moqit/pkg/cluster"
	"github.com/MoQ-it/moqit/pkg/room"
	"github.com/MoQ-it/moqit/pkg/server"
	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"github.com/MoQ-it/moqt"
	v22 "github.com/MoQ-it/moqt/wire/v22"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

const apiKey, apiSecret = "clusterkey", "cluster-secret-0123456789abcdef01234"

var keys = auth.KeyProvider{apiKey: apiSecret}

type node struct {
	*server.Server
	addr string
	id   string
}

func freeUDPAddr(t *testing.T) string {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	return pc.LocalAddr().String()
}

func startNode(t *testing.T, mr *miniredis.Miniredis, id string) *node {
	t.Helper()
	return startNodeWith(t, mr, id, nil)
}

// startNodeWith starts a node, letting configure adjust its config; with a
// nil mr the node is standalone.
func startNodeWith(t *testing.T, mr *miniredis.Miniredis, id string, configure func(*server.Config)) *node {
	t.Helper()
	cert, err := server.SelfSignedCert("localhost,127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	addr := freeUDPAddr(t)
	cfg := server.Config{
		UDPAddr: addr, Keys: keys, Certificate: cert, DepartureTimeout: time.Second,
		AdvertiseAddr: addr, PublicURL: "https://" + addr + "/moq", ViewerInterval: 200 * time.Millisecond,
	}
	if mr != nil {
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { rdb.Close() })
		cfg.Cluster = cluster.New(rdb, id, 3*time.Second, nil)
	}
	if configure != nil {
		configure(&cfg)
	}
	srv, err := server.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return &node{Server: srv, addr: addr, id: id}
}

func token(t *testing.T, identity, rm string) string {
	tok, err := auth.NewAccessToken(apiKey, apiSecret).SetIdentity(identity).
		SetRoomGrant(&auth.RoomGrant{Join: true, Room: rm, UpdateOwnMetadata: true}).ToJWT()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func join(t *testing.T, n *node, identity string) *testclient.Client {
	t.Helper()
	return testclient.Join(t, n.addr, "r1", identity, token(t, identity, "r1"))
}

func TestCrossNodeRoom(t *testing.T) {
	mr := miniredis.RunT(t)
	a, b := startNode(t, mr, "A"), startNode(t, mr, "B")

	alice := join(t, a, "alice")
	bob := join(t, b, "bob")
	alice.WaitState("bob (on B) visible to alice (on A)", testclient.Present("alice", "bob"))
	bob.WaitState("alice (on A) visible to bob (on B)", testclient.Present("alice", "bob"))
	if s := bob.State(); s["alice"].NodeId != "A" || s["bob"].NodeId != "B" {
		t.Errorf("node ids: alice %q bob %q", s["alice"].NodeId, s["bob"].NodeId)
	}

	if r := alice.Send(testclient.MicTracks()); r.Result != moqitv1.RequestResult_REQUEST_RESULT_OK {
		t.Fatalf("SetTracks: %v", r)
	}
	bob.WaitState("alice's mic", func(s map[string]*moqitv1.ParticipantInfo) bool { return len(s["alice"].GetTracks()) == 1 })

	// Bob's node fetches Alice's mic from node A.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sub, err := bob.Sess.Subscribe(ctx, v22.FullTrackName{Namespace: room.ParticipantNamespace("r1", "alice"), Name: room.TrackMic}, nil)
	if err != nil {
		t.Fatalf("subscribing across nodes: %v", err)
	}
	w := <-alice.Mic
	sg := w.OpenSubgroup(0, 0, moqt.SubgroupOptions{})
	sg.WriteObject(&v22.Object{ID: 0, Payload: []byte("opus from A")})
	sg.Close()
	g, err := sub.AcceptSubgroup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if o, err := g.Next(); err != nil || string(o.Payload) != "opus from A" {
		t.Fatalf("media across nodes: %v, %v", o, err)
	}

	// Metadata and direct data cross nodes too.
	md := "hello"
	alice.Send(&moqitv1.ClientSignal{Signal: &moqitv1.ClientSignal_UpdateParticipant{UpdateParticipant: &moqitv1.UpdateParticipant{Metadata: &md}}})
	bob.WaitState("alice's metadata", func(s map[string]*moqitv1.ParticipantInfo) bool { return s["alice"].GetMetadata() == "hello" })
	r := alice.Send(&moqitv1.ClientSignal{Signal: &moqitv1.ClientSignal_SendData{SendData: &moqitv1.SendData{DestinationIdentities: []string{"bob"}, Topic: "dm", Payload: []byte("psst")}}})
	if r.Result != moqitv1.RequestResult_REQUEST_RESULT_OK {
		t.Fatalf("send data across nodes: %v", r)
	}
	select {
	case m := <-bob.Inbox:
		if d := m.GetData(); d == nil || d.FromIdentity != "alice" || string(d.Payload) != "psst" {
			t.Errorf("bob inbox %v", m)
		}
	case <-ctx.Done():
		t.Fatal("direct message did not cross nodes")
	}

	bob.Sess.Close()
	alice.WaitState("bob gone", func(s map[string]*moqitv1.ParticipantInfo) bool { return s["bob"] == nil && s["alice"] != nil })
}

func TestDuplicateIdentityAcrossNodes(t *testing.T) {
	mr := miniredis.RunT(t)
	a, b := startNode(t, mr, "A"), startNode(t, mr, "B")
	watcher := join(t, a, "watcher")
	first := join(t, a, "alice")
	watcher.WaitState("first alice", testclient.Present("alice"))
	firstSid := watcher.State()["alice"].Sid

	join(t, b, "alice")
	select {
	case <-first.Sess.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the older session on node A was not closed")
	}
	watcher.WaitState("only the newer alice, on B", func(s map[string]*moqitv1.ParticipantInfo) bool {
		p := s["alice"]
		return p != nil && p.Sid != firstSid && p.NodeId == "B"
	})
}

func TestNodeShutdown(t *testing.T) {
	mr := miniredis.RunT(t)
	a, b := startNode(t, mr, "A"), startNode(t, mr, "B")
	join(t, a, "alice")
	bob := join(t, b, "bob")
	bob.WaitState("alice", testclient.Present("alice", "bob"))

	a.Close()
	bob.WaitStateFor("alice removed after her node went away", 15*time.Second, func(s map[string]*moqitv1.ParticipantInfo) bool {
		return s["alice"] == nil && s["bob"] != nil
	})
}

func TestRoute(t *testing.T) {
	mr := miniredis.RunT(t)
	a, b := startNode(t, mr, "A"), startNode(t, mr, "B")
	join(t, b, "bob") // r1 lives on B
	route := func(rm string) string {
		rec := httptest.NewRecorder()
		a.RouteHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/route?token="+token(t, "x", rm), nil))
		var out struct{ URL string }
		json.NewDecoder(rec.Body).Decode(&out)
		return out.URL
	}
	if got := route("r1"); got != "https://"+b.addr+"/moq" {
		t.Errorf("route for r1 = %s; want node B", got)
	}
	if got := route("elsewhere"); got != "https://"+a.addr+"/moq" {
		t.Errorf("route for a new room = %s; want the least loaded node, A", got)
	}
	rec := httptest.NewRecorder()
	a.RouteHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/route?token=bad", nil))
	if rec.Code != 401 {
		t.Errorf("bad token: %d", rec.Code)
	}
}

// A page reload: the participant leaves cleanly and rejoins at once with
// the same identity. Its requests (signal and inbox tracks) and media must
// work on the new session, not hang on state left by the old one.
func TestRejoinSameIdentity(t *testing.T) {
	for _, clustered := range []bool{false, true} {
		t.Run(map[bool]string{false: "single node", true: "across nodes"}[clustered], func(t *testing.T) {
			mr := miniredis.RunT(t)
			a := startNode(t, mr, "A")
			b := a
			if clustered {
				b = startNode(t, mr, "B")
			}
			alice := join(t, a, "alice")
			bob := join(t, b, "bob")
			if r := bob.Send(testclient.MicTracks()); r.Result != moqitv1.RequestResult_REQUEST_RESULT_OK {
				t.Fatalf("SetTracks: %v", r)
			}
			alice.WaitState("bob's mic", func(s map[string]*moqitv1.ParticipantInfo) bool { return len(s["bob"].GetTracks()) == 1 })
			subscribeMic := func(c *testclient.Client) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				sub, err := alice.Sess.Subscribe(ctx, v22.FullTrackName{Namespace: room.ParticipantNamespace("r1", "bob"), Name: room.TrackMic}, nil)
				if err != nil {
					t.Fatalf("subscribing to bob's mic: %v", err)
				}
				defer sub.Unsubscribe()
				select {
				case w := <-c.Mic:
					sg := w.OpenSubgroup(0, 0, moqt.SubgroupOptions{})
					if err := sg.WriteObject(&v22.Object{Payload: []byte("hi")}); err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("bob's session never asked for its mic")
				}
				g, err := sub.AcceptSubgroup(ctx)
				if err != nil {
					t.Fatalf("no media from bob: %v", err)
				}
				if o, err := g.Next(); err != nil || string(o.Payload) != "hi" {
					t.Fatalf("object: %v %v", o, err)
				}
			}
			subscribeMic(bob)

			bob.Sess.Close()
			alice.WaitState("bob gone", func(s map[string]*moqitv1.ParticipantInfo) bool { return s["bob"] == nil })
			bob = join(t, b, "bob")
			if r := bob.Send(testclient.MicTracks()); r.Result != moqitv1.RequestResult_REQUEST_RESULT_OK {
				t.Fatalf("SetTracks after rejoining: %v", r)
			}
			alice.WaitState("bob's mic after rejoining", func(s map[string]*moqitv1.ParticipantInfo) bool { return len(s["bob"].GetTracks()) == 1 })
			subscribeMic(bob)
		})
	}
}

// A client that disappears without closing (crash, lost network) is
// dropped once its connection idles out, well before quic-go's 30 s default.
func TestVanishedClientTimesOut(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the idle timeout")
	}
	mr := miniredis.RunT(t)
	a := startNode(t, mr, "A")
	alice := join(t, a, "alice")
	bob := join(t, a, "bob")
	alice.WaitState("bob", testclient.Present("bob"))
	start := time.Now()
	bob.Vanish()
	deadline := time.After(25 * time.Second)
	for alice.State()["bob"] != nil {
		select {
		case <-deadline:
			t.Fatal("vanished participant still listed after 25s")
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Logf("vanished participant removed after %v", time.Since(start).Round(100*time.Millisecond))
}
