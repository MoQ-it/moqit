package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
)

var keys = KeyProvider{"devkey": "devsecret-0123456789abcdef0123456789"}

func TestRoundTrip(t *testing.T) {
	f := false
	tok, err := NewAccessToken("devkey", keys["devkey"]).
		SetIdentity("alice").SetName("Alice").SetMetadata(`{"a":1}`).
		SetRoomGrant(&RoomGrant{Join: true, Room: "standup", PublishData: &f, PublishSources: []string{"camera", "microphone"}}).
		ToJWT()
	if err != nil {
		t.Fatal(err)
	}
	c, err := keys.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.Identity() != "alice" || c.Name != "Alice" || c.Issuer != "devkey" || c.Grants.Room.Room != "standup" {
		t.Fatalf("claims %+v", c)
	}
	p, err := c.Grants.Room.Permission()
	if err != nil {
		t.Fatal(err)
	}
	if !p.CanPublish || !p.CanSubscribe || p.CanPublishData || p.CanUpdateMetadata {
		t.Errorf("permission %+v", p)
	}
	if !CanPublishSource(p, moqitv1.TrackSource_TRACK_SOURCE_CAMERA) || CanPublishSource(p, moqitv1.TrackSource_TRACK_SOURCE_SCREEN) {
		t.Error("source restrictions not applied")
	}
}

func TestRejects(t *testing.T) {
	good := func() *AccessToken {
		return NewAccessToken("devkey", keys["devkey"]).SetIdentity("a").SetRoomGrant(&RoomGrant{Join: true, Room: "r"})
	}
	expired, _ := good().SetValidFor(-time.Hour).ToJWT()
	wrongSecret, _ := NewAccessToken("devkey", "another-secret").SetIdentity("a").SetRoomGrant(&RoomGrant{Join: true, Room: "r"}).ToJWT()
	unknownKey, _ := NewAccessToken("other", "x").SetIdentity("a").SetRoomGrant(&RoomGrant{Join: true, Room: "r"}).ToJWT()
	ok, _ := good().ToJWT()
	parts := strings.Split(ok, ".")
	noneAlg := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0." + parts[1] + "."
	for name, tok := range map[string]string{"expired": expired, "wrong secret": wrongSecret, "unknown key": unknownKey, "alg none": noneAlg, "garbage": "x.y.z"} {
		if _, err := keys.Verify(tok); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: %v; want ErrInvalidToken", name, err)
		}
	}
	if _, err := NewAccessToken("devkey", "s").SetRoomGrant(&RoomGrant{Join: true, Room: "r"}).ToJWT(); err == nil {
		t.Error("join grant without identity must fail")
	}
	if _, err := NewAccessToken("devkey", "s").SetIdentity("a").SetRoomGrant(&RoomGrant{Join: true, Room: "r", PublishSources: []string{"hologram"}}).ToJWT(); err == nil {
		t.Error("unknown source must fail")
	}
}
