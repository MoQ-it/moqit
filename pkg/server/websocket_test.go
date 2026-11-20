package server_test

import (
	"net"
	"testing"
	"time"

	"github.com/MoQ-it/moqit/internal/testclient"
	"github.com/MoQ-it/moqit/pkg/room"
	"github.com/MoQ-it/moqit/pkg/server"
	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
)

// A participant on WebSocket and one on QUIC share a room and hear each other.
func TestWebSocketParticipant(t *testing.T) {
	a := startNodeWith(t, nil, "A", func(c *server.Config) { c.WebSocket = true })
	alice := testclient.Join(t, a.addr, "r1", "alice", token(t, "alice", "r1"))
	alice.Send(testclient.MicTracks())
	bob := testclient.Join(t, "wss://"+a.addr+"/moq", "r1", "bob", token(t, "bob", "r1"))
	bob.Send(testclient.MicTracks())

	bob.WaitState("alice's mic seen over WebSocket", func(s map[string]*moqitv1.ParticipantInfo) bool {
		return len(s["alice"].GetTracks()) == 1
	})
	alice.WaitState("bob's mic seen over QUIC", func(s map[string]*moqitv1.ParticipantInfo) bool {
		return len(s["bob"].GetTracks()) == 1
	})
	sub := bob.Subscribe("alice", room.TrackMic)
	sendMic(t, alice, "to bob")
	if got := testclient.NextObject(t, sub); got != "to bob" {
		t.Errorf("bob got %q", got)
	}
	sub = alice.Subscribe("bob", room.TrackMic)
	sendMic(t, bob, "to alice")
	if got := testclient.NextObject(t, sub); got != "to alice" {
		t.Errorf("alice got %q", got)
	}

	bob.Sess.Close()
	alice.WaitState("bob left", func(s map[string]*moqitv1.ParticipantInfo) bool { return s["bob"] == nil })
}

func TestWebSocketViewerAndAuth(t *testing.T) {
	a := startNodeWith(t, nil, "A", func(c *server.Config) { c.WebSocket = true })
	url := "wss://" + a.addr + "/moq"
	alice := testclient.Join(t, a.addr, "r1", "alice", token(t, "alice", "r1"))
	carol := testclient.Watch(t, url, "r1", viewerToken(t, "carol", "r1"))
	carol.WaitState("alice in the viewer's room state", func(s map[string]*moqitv1.ParticipantInfo) bool { return s["alice"] != nil })
	alice.WaitRoom("viewer counted", func(r *moqitv1.Room) bool { return r.NumViewers == 1 })
	if !rejected(t, &node{addr: url}, "not-a-token") {
		t.Error("a WebSocket session with a bad token was accepted")
	}
}

func TestWebSocketOff(t *testing.T) {
	a := startNodeWith(t, nil, "A", nil)
	if c, err := net.DialTimeout("tcp", a.addr, time.Second); err == nil {
		c.Close()
		t.Fatal("a node without WebSocket listens on TCP")
	}
}
