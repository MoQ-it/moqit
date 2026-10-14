package webhook

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoQ-it/moqit/pkg/auth"
	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
)

var keys = auth.KeyProvider{"k": "webhook-secret-0123456789abcdef0123"}

func TestDeliverVerifyRetry(t *testing.T) {
	got := make(chan *moqitv1.WebhookEvent, 4)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(500) // first attempt fails; the notifier retries
			return
		}
		ev, err := Receive(r, keys)
		if err != nil {
			t.Error(err)
			return
		}
		got <- ev
	}))
	defer srv.Close()

	n := NewNotifier([]string{srv.URL}, "k", keys["k"], nil)
	n.Notify(ParticipantJoined, &moqitv1.Room{Name: "r1"}, &moqitv1.ParticipantInfo{Identity: "alice"}, nil)
	select {
	case ev := <-got:
		if ev.Event != ParticipantJoined || ev.Room.Name != "r1" || ev.Participant.Identity != "alice" || !strings.HasPrefix(ev.Id, "EV_") {
			t.Errorf("event %v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event not delivered")
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	body := []byte(`{"event":"room_started"}`)
	sig, _ := auth.NewAccessToken("k", keys["k"]).SetSha256("bm90IHRoZSBoYXNo").ToJWT()
	if _, err := Verify(body, sig, keys); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("wrong body hash: %v", err)
	}
	if _, err := Verify(body, "", keys); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("no signature: %v", err)
	}
}
