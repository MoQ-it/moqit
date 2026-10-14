// Package webhook sends moqit events to application endpoints and verifies
// them on receipt.
//
// Each event is POSTed as JSON (a moqit.v1.WebhookEvent) with the header
//
//	Moqit-Signature: <JWT signed with the API secret, sha256 claim = base64 SHA-256 of the body>
package webhook

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/MoQ-it/moqit/pkg/auth"
	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// Event names.
const (
	RoomStarted       = "room_started"
	RoomFinished      = "room_finished"
	ParticipantJoined = "participant_joined"
	ParticipantLeft   = "participant_left"
	TrackPublished    = "track_published"
	TrackUnpublished  = "track_unpublished"
)

// SignatureHeader carries the event's signature.
const SignatureHeader = "Moqit-Signature"

// Notifier delivers events to URLs. It is safe for concurrent use; Notify
// never blocks the caller.
type Notifier struct {
	urls           []string
	apiKey, secret string
	client         *http.Client
	log            *slog.Logger
	queue          chan *moqitv1.WebhookEvent
}

// NewNotifier starts a notifier sending to urls, signing with an API key.
func NewNotifier(urls []string, apiKey, secret string, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	n := &Notifier{urls: urls, apiKey: apiKey, secret: secret, client: &http.Client{Timeout: 5 * time.Second}, log: log, queue: make(chan *moqitv1.WebhookEvent, 1024)}
	go n.run()
	return n
}

// Notify queues an event; room, participant and track may be nil.
func (n *Notifier) Notify(event string, room *moqitv1.Room, p *moqitv1.ParticipantInfo, track *moqitv1.TrackInfo) {
	if n == nil {
		return
	}
	id := make([]byte, 8)
	rand.Read(id)
	ev := &moqitv1.WebhookEvent{Id: "EV_" + hex.EncodeToString(id), Event: event, CreatedAt: time.Now().Unix(), Room: room, Participant: p, Track: track}
	select {
	case n.queue <- ev:
	default:
		n.log.Warn("webhook queue full; dropping event", "event", event)
	}
}

// run delivers events in order, so a receiver sees participant_joined
// before participant_left.
func (n *Notifier) run() {
	for ev := range n.queue {
		body, err := protojson.Marshal(ev)
		if err != nil {
			continue
		}
		for _, url := range n.urls {
			n.deliver(url, ev.Event, body)
		}
	}
}

func (n *Notifier) deliver(url, event string, body []byte) {
	sum := sha256.Sum256(body)
	sig, err := auth.NewAccessToken(n.apiKey, n.secret).SetSha256(base64.StdEncoding.EncodeToString(sum[:])).SetValidFor(5 * time.Minute).ToJWT()
	if err != nil {
		n.log.Error("webhook signing", "err", err)
		return
	}
	backoff := 500 * time.Millisecond
	for attempt := 1; attempt <= 3; attempt++ {
		req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/webhook+json")
		req.Header.Set(SignatureHeader, sig)
		req.Header.Set("User-Agent", "moqit-webhook")
		res, err := n.client.Do(req)
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode < 300 {
				return
			}
			err = fmt.Errorf("status %d", res.StatusCode)
		}
		n.log.Warn("webhook delivery failed", "url", url, "event", event, "attempt", attempt, "err", err)
		time.Sleep(backoff)
		backoff *= 2
	}
}

// ErrInvalidSignature is returned by Receive for unsigned or tampered events.
var ErrInvalidSignature = errors.New("webhook: invalid signature")

// Receive reads and verifies a webhook request.
func Receive(r *http.Request, keys auth.KeyProvider) (*moqitv1.WebhookEvent, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return Verify(body, r.Header.Get(SignatureHeader), keys)
}

// Verify checks a webhook body against its signature header.
func Verify(body []byte, signature string, keys auth.KeyProvider) (*moqitv1.WebhookEvent, error) {
	claims, err := keys.Verify(signature)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSignature, err)
	}
	sum := sha256.Sum256(body)
	want := base64.StdEncoding.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(want), []byte(claims.Sha256)) != 1 {
		return nil, fmt.Errorf("%w: body hash mismatch", ErrInvalidSignature)
	}
	var ev moqitv1.WebhookEvent
	if err := protojson.Unmarshal(body, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// Close stops accepting events once queued ones are sent; for tests.
func (n *Notifier) Close(ctx context.Context) {
	for len(n.queue) > 0 && ctx.Err() == nil {
		time.Sleep(10 * time.Millisecond)
	}
}
