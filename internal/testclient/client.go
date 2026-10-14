// Package testclient is a minimal moqit participant speaking raw MoQ over
// QUIC, for tests. It keeps a view of the room as an SDK would.
package testclient

import (
	"context"
	"crypto/tls"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/MoQ-it/moqit/pkg/room"
	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"github.com/MoQ-it/moqt"
	"github.com/MoQ-it/moqt/transport/rawquic"
	v22 "github.com/MoQ-it/moqt/wire/v22"
	"github.com/quic-go/quic-go"
	"google.golang.org/protobuf/proto"
)

// Client is one participant.
type Client struct {
	moqt.RejectAll
	T        testing.TB
	Identity string
	Room     string
	Sess     *moqt.Session
	Inbox    chan *moqitv1.InboxMessage
	// Mic receives a writer whenever the relay subscribes to this
	// participant's "mic" track.
	Mic chan *moqt.TrackWriter
	// Vanish cuts the client's network without closing anything, like a
	// crashed machine or lost connection.
	Vanish func()

	mu       sync.Mutex
	roomInfo *moqitv1.Room
	speakers []string
	state    map[string]*moqitv1.ParticipantInfo
	changed  chan struct{}
	signal   chan *moqt.TrackWriter
	sigSG    *moqt.OutgoingSubgroup
	sigObj   uint64
	reqID    uint64
}

func (c *Client) HandleSubscribe(r *moqt.SubscribeRequest) {
	var ch chan *moqt.TrackWriter
	switch r.Track.Name {
	case room.TrackSignal:
		ch = c.signal
	case room.TrackMic:
		ch = c.Mic
	default:
		r.Reject(v22.ReqErrDoesNotExist, "")
		return
	}
	w, err := r.Accept(moqt.AcceptOptions{})
	if err == nil {
		ch <- w
	}
}

// Dial connects to a node with a token, without joining yet.
func Dial(t testing.TB, addr, identity, token string) (*Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := &Client{T: t, Identity: identity, state: map[string]*moqitv1.ParticipantInfo{}, changed: make(chan struct{}, 1),
		Inbox: make(chan *moqitv1.InboxMessage, 16), signal: make(chan *moqt.TrackWriter, 1), Mic: make(chan *moqt.TrackWriter, 4)}
	// Our own socket, so Vanish can cut it.
	udp, err := net.ListenUDP("udp", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { udp.Close() })
	c.Vanish = func() { udp.Close() }
	raddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	qc, err := (&quic.Transport{Conn: udp}).Dial(ctx, raddr, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{v22.ALPN}},
		&quic.Config{EnableDatagrams: true, MaxIncomingStreams: 1000, MaxIncomingUniStreams: 1000})
	if err != nil {
		t.Fatal(err)
	}
	opt := v22.Token{AliasType: v22.AliasUseValue, Value: []byte(token)}
	sess, err := moqt.NewSession(ctx, rawquic.New(qc), moqt.Client, moqt.Config{Handler: c, SetupOptions: v22.KeyValues{{Type: v22.OptAuthorizationToken, Bytes: opt.Append(nil)}}})
	if err != nil {
		return nil, err
	}
	c.Sess = sess
	t.Cleanup(func() { sess.Close() })
	return c, nil
}

// Join connects and joins a room like the SDK: publish the participant
// namespace, follow the room and inbox tracks, answer the signal track.
func Join(t testing.TB, addr, roomName, identity, token string) *Client {
	t.Helper()
	c, err := Dial(t, addr, identity, token)
	if err != nil {
		t.Fatal(err)
	}
	c.Room = roomName
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Sess.PublishNamespace(ctx, room.ParticipantNamespace(roomName, identity), nil); err != nil {
		t.Fatal(err)
	}
	roomSub, err := c.Sess.Subscribe(ctx, v22.FullTrackName{Namespace: room.ServerNamespace(roomName), Name: room.TrackRoom},
		&v22.Params{LocationFilter: &v22.LocationFilter{Type: v22.FilterRelativeStart, StartGroup: 1}})
	if err != nil {
		t.Fatal(err)
	}
	go readObjects(roomSub, func(b []byte) {
		var u moqitv1.RoomStateUpdate
		if proto.Unmarshal(b, &u) == nil {
			c.apply(&u)
		}
	})
	inboxSub, err := c.Sess.Subscribe(ctx, v22.FullTrackName{Namespace: room.InboxNamespace(roomName, identity), Name: room.TrackInbox}, nil)
	if err != nil {
		t.Fatal(err)
	}
	go readObjects(inboxSub, func(b []byte) {
		var m moqitv1.InboxMessage
		if proto.Unmarshal(b, &m) == nil {
			c.Inbox <- &m
		}
	})
	select {
	case w := <-c.signal:
		c.sigSG = w.OpenSubgroup(0, 0, moqt.SubgroupOptions{})
	case <-ctx.Done():
		t.Fatal("server never subscribed to the signal track")
	}
	return c
}

// Watch connects as a read-only viewer and follows the room track,
// subscribing again whenever it ends, as the SDK does.
func Watch(t testing.TB, addr, roomName, token string) *Client {
	t.Helper()
	c, err := Dial(t, addr, "", token)
	if err != nil {
		t.Fatal(err)
	}
	c.Room = roomName
	go func() {
		for c.Sess.Context().Err() == nil {
			ctx, cancel := context.WithTimeout(c.Sess.Context(), 5*time.Second)
			sub, err := c.Sess.Subscribe(ctx, v22.FullTrackName{Namespace: room.ServerNamespace(roomName), Name: room.TrackRoom},
				&v22.Params{LocationFilter: &v22.LocationFilter{Type: v22.FilterRelativeStart, StartGroup: 1}})
			cancel()
			if err == nil {
				readObjects(sub, func(b []byte) {
					var u moqitv1.RoomStateUpdate
					if proto.Unmarshal(b, &u) == nil {
						c.apply(&u)
					}
				})
			}
			select {
			case <-c.Sess.Context().Done():
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	return c
}

// Subscribe subscribes to a participant's track, retrying for up to 5s
// while the node can't serve it yet.
func (c *Client) Subscribe(identity, name string) *moqt.Subscription {
	c.T.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sub, err := c.TrySubscribe(identity, name)
		if err == nil {
			return sub
		}
		if time.Now().After(deadline) {
			c.T.Fatalf("%s: subscribing to %s/%s: %v", c.Identity, identity, name, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TrySubscribe subscribes to a participant's track once.
func (c *Client) TrySubscribe(identity, name string) (*moqt.Subscription, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Sess.Subscribe(ctx, v22.FullTrackName{Namespace: room.ParticipantNamespace(c.Room, identity), Name: name}, nil)
}

// NextObject returns the payload of the next object on sub.
func NextObject(t testing.TB, sub *moqt.Subscription) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g, err := sub.AcceptSubgroup(ctx)
	if err != nil {
		t.Fatalf("no subgroup: %v", err)
	}
	o, err := g.Next()
	if err != nil {
		t.Fatalf("no object: %v", err)
	}
	return string(o.Payload)
}

func readObjects(sub *moqt.Subscription, f func([]byte)) {
	for {
		g, err := sub.AcceptSubgroup(context.Background())
		if err != nil {
			return
		}
		for {
			o, err := g.Next()
			if err != nil {
				break
			}
			f(o.Payload)
		}
	}
}

func (c *Client) apply(u *moqitv1.RoomStateUpdate) {
	c.mu.Lock()
	switch x := u.Update.(type) {
	case *moqitv1.RoomStateUpdate_RoomUpdated:
		c.roomInfo = x.RoomUpdated
	case *moqitv1.RoomStateUpdate_SpeakersChanged:
		c.speakers = c.speakers[:0]
		for _, sp := range x.SpeakersChanged.Speakers {
			c.speakers = append(c.speakers, sp.Identity)
		}
	case *moqitv1.RoomStateUpdate_Snapshot:
		c.roomInfo = x.Snapshot.Room
		c.state = map[string]*moqitv1.ParticipantInfo{}
		for _, p := range x.Snapshot.Participants {
			c.state[p.Identity] = p
		}
	case *moqitv1.RoomStateUpdate_ParticipantUpdated:
		c.state[x.ParticipantUpdated.Identity] = x.ParticipantUpdated
	case *moqitv1.RoomStateUpdate_ParticipantLeft:
		if p := c.state[x.ParticipantLeft.Identity]; p != nil && p.Sid == x.ParticipantLeft.Sid {
			delete(c.state, x.ParticipantLeft.Identity)
		}
	}
	c.mu.Unlock()
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

// Speakers returns the active speakers as last announced, loudest first.
func (c *Client) Speakers() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.speakers...)
}

// NumViewers returns the room's viewer count as last seen.
func (c *Client) NumViewers() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.roomInfo.GetNumViewers()
}

// WaitRoom waits until the room info satisfies ok.
func (c *Client) WaitRoom(what string, ok func(*moqitv1.Room) bool) {
	c.T.Helper()
	deadline := time.After(10 * time.Second)
	for {
		c.mu.Lock()
		done := c.roomInfo != nil && ok(c.roomInfo)
		c.mu.Unlock()
		if done {
			return
		}
		select {
		case <-c.changed:
		case <-deadline:
			c.T.Fatalf("%s: %s never true", c.Identity, what)
		}
	}
}

// RoomMetadata returns the room's metadata as last seen.
func (c *Client) RoomMetadata() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.roomInfo.GetMetadata()
}

// State returns a copy of the room view.
func (c *Client) State() map[string]*moqitv1.ParticipantInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]*moqitv1.ParticipantInfo, len(c.state))
	for k, v := range c.state {
		out[k] = v
	}
	return out
}

// WaitState waits until the room view satisfies ok.
func (c *Client) WaitState(what string, ok func(map[string]*moqitv1.ParticipantInfo) bool) {
	c.WaitStateFor(what, 5*time.Second, ok)
}

func (c *Client) WaitStateFor(what string, d time.Duration, ok func(map[string]*moqitv1.ParticipantInfo) bool) {
	c.T.Helper()
	deadline := time.After(d)
	for {
		c.mu.Lock()
		done := ok(c.state)
		c.mu.Unlock()
		if done {
			return
		}
		select {
		case <-c.changed:
		case <-deadline:
			c.T.Fatalf("%s: %s never true; state %v", c.Identity, what, c.State())
		}
	}
}

// Send sends a ClientSignal and waits for its response on the inbox.
func (c *Client) Send(cs *moqitv1.ClientSignal) *moqitv1.RequestResponse {
	c.T.Helper()
	c.reqID++
	cs.RequestId = c.reqID
	b, _ := proto.Marshal(cs)
	if err := c.sigSG.WriteObject(&v22.Object{ID: c.sigObj, Payload: b}); err != nil {
		c.T.Fatal(err)
	}
	c.sigObj++
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-c.Inbox:
			if r := m.GetResponse(); r != nil && r.RequestId == cs.RequestId {
				return r
			}
		case <-deadline:
			c.T.Fatalf("%s: no response to request %d", c.Identity, cs.RequestId)
		}
	}
}

// Present is a WaitState predicate: all identities present and active.
func Present(ids ...string) func(map[string]*moqitv1.ParticipantInfo) bool {
	return func(s map[string]*moqitv1.ParticipantInfo) bool {
		for _, id := range ids {
			if p := s[id]; p == nil || p.State != moqitv1.ParticipantState_PARTICIPANT_STATE_ACTIVE {
				return false
			}
		}
		return true
	}
}

// MicTracks is a SetTracks signal declaring a microphone.
func MicTracks() *moqitv1.ClientSignal {
	return &moqitv1.ClientSignal{Signal: &moqitv1.ClientSignal_SetTracks{SetTracks: &moqitv1.SetTracks{Tracks: []*moqitv1.TrackInfo{{
		Name: "microphone", Kind: moqitv1.TrackKind_TRACK_KIND_AUDIO, Source: moqitv1.TrackSource_TRACK_SOURCE_MICROPHONE,
		Codec: "opus", Renditions: []*moqitv1.Rendition{{TrackName: room.TrackMic, Bitrate: 32000}},
	}}}}}
}
