package cluster

import (
	"context"
	"testing"
	"time"

	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func pair(t *testing.T) (*miniredis.Miniredis, *Cluster, *Cluster) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return mr, New(rdb, "A", 3*time.Second, nil), New(rdb, "B", 3*time.Second, nil)
}

func TestNodes(t *testing.T) {
	mr, a, b := pair(t)
	ctx := context.Background()
	if err := a.Heartbeat(ctx, NodeInfo{QUICAddr: "10.0.0.1:4443"}); err != nil {
		t.Fatal(err)
	}
	b.Heartbeat(ctx, NodeInfo{QUICAddr: "10.0.0.2:4443"})
	nodes, err := a.Nodes(ctx)
	if err != nil || len(nodes) != 2 {
		t.Fatalf("nodes %v, %v", nodes, err)
	}
	n, _ := a.Node(ctx, "B")
	if n == nil || n.QUICAddr != "10.0.0.2:4443" {
		t.Fatalf("node B %+v", n)
	}
	mr.FastForward(4 * time.Second) // A and B both miss their heartbeats
	a.Heartbeat(ctx, NodeInfo{})
	if n, _ := a.Node(ctx, "B"); n != nil {
		t.Error("expired node still listed")
	}
	if nodes, _ := a.Nodes(ctx); len(nodes) != 1 || nodes[0].ID != "A" {
		t.Errorf("nodes after expiry %v", nodes)
	}
}

func TestRoomEvents(t *testing.T) {
	_, a, b := pair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r1, created1, _ := a.EnsureRoom(ctx, &moqitv1.Room{Name: "r", Sid: "RM_a"})
	r2, created2, _ := b.EnsureRoom(ctx, &moqitv1.Room{Name: "r", Sid: "RM_b"})
	if r1.Sid != "RM_a" || r2.Sid != "RM_a" || !created1 || created2 {
		t.Fatalf("nodes disagree on the room: %s %s (created %v %v)", r1.Sid, r2.Sid, created1, created2)
	}
	if rooms, _ := a.Rooms(ctx); len(rooms) != 1 || rooms[0].Name != "r" {
		t.Errorf("rooms %v", rooms)
	}
	if !a.Once(ctx, "x", time.Minute) || b.Once(ctx, "x", time.Minute) {
		t.Error("Once must succeed exactly once")
	}

	alice := &moqitv1.ParticipantInfo{Identity: "alice", Sid: "PA_1", NodeId: "A", JoinedAt: 1}
	if err := a.PutParticipant(ctx, "r", alice); err != nil {
		t.Fatal(err)
	}
	from, ps, err := b.Participants(ctx, "r")
	if err != nil || len(ps) != 1 || ps[0].Identity != "alice" {
		t.Fatalf("participants %v, %v", ps, err)
	}

	events := make(chan *moqitv1.ClusterRoomEvent, 8)
	go b.Watch(ctx, "r", from, func(e *moqitv1.ClusterRoomEvent) { events <- e })
	a.PutParticipant(ctx, "r", &moqitv1.ParticipantInfo{Identity: "alice", Sid: "PA_1", NodeId: "A", Metadata: "x"})

	// A stale removal (wrong sid) does nothing; the right one wins once.
	if ok, _ := b.RemoveParticipant(ctx, "r", "alice", "PA_old", 0, time.Second); ok {
		t.Error("removal with a stale sid succeeded")
	}
	if ok, _ := a.RemoveParticipant(ctx, "r", "alice", "PA_1", moqitv1.DisconnectReason_DISCONNECT_REASON_CLIENT_INITIATED, time.Minute); !ok {
		t.Error("removal failed")
	}
	if ok, _ := b.RemoveParticipant(ctx, "r", "alice", "PA_1", 0, time.Minute); ok {
		t.Error("second removal also succeeded")
	}

	want := []string{"put:x", "left"}
	for _, w := range want {
		select {
		case e := <-events:
			got := "left"
			if p := e.GetParticipantPut(); p != nil {
				got = "put:" + p.Metadata
			}
			if got != w || e.NodeId != "A" {
				t.Errorf("event %s from %s; want %s from A", got, e.NodeId, w)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("missing event %s", w)
		}
	}
}

func TestDirectAndReap(t *testing.T) {
	_, a, b := pair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan string, 1)
	if err := b.ReceiveDirect(ctx, "r", func(id string, m *moqitv1.InboxMessage) { got <- id + ":" + string(m.GetData().GetPayload()) }); err != nil {
		t.Fatal(err)
	}
	a.SendDirect(ctx, "r", "bob", &moqitv1.InboxMessage{Message: &moqitv1.InboxMessage_Data{Data: &moqitv1.DataReceived{Payload: []byte("hi")}}})
	select {
	case s := <-got:
		if s != "bob:hi" {
			t.Errorf("direct %q", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("direct message not delivered")
	}

	a.PutParticipant(ctx, "r", &moqitv1.ParticipantInfo{Identity: "alice", Sid: "PA_1", NodeId: "A"})
	b.PutParticipant(ctx, "r", &moqitv1.ParticipantInfo{Identity: "bob", Sid: "PA_2", NodeId: "B"})
	reaped, err := b.ReapDead(ctx, "r", func(id string) bool { return id == "B" }, time.Minute)
	if err != nil || len(reaped) != 1 || reaped[0].Identity != "alice" {
		t.Fatalf("reaped %v, %v", reaped, err)
	}
	if _, ps, _ := b.Participants(ctx, "r"); len(ps) != 1 || ps[0].Identity != "bob" {
		t.Errorf("after reap %v", ps)
	}
}
