package room_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/big"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/MoQ-it/moqit/internal/testclient"
	"github.com/MoQ-it/moqit/pkg/auth"
	"github.com/MoQ-it/moqit/pkg/room"
	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"github.com/MoQ-it/moqt"
	"github.com/MoQ-it/moqt/transport/rawquic"
	v22 "github.com/MoQ-it/moqt/wire/v22"
	"github.com/quic-go/quic-go"
	"google.golang.org/protobuf/proto"
)

const apiKey, apiSecret = "devkey", "devsecret-0123456789abcdef0123456789"

var keys = auth.KeyProvider{apiKey: apiSecret}

// node runs a relay + room manager on a QUIC listener.
type node struct {
	t    *testing.T
	m    *room.Manager
	addr string
}

func newNode(t *testing.T, cfg room.Config) *node {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	tlsConf := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, NextProtos: []string{v22.ALPN}}
	ln, err := quic.ListenAddr("127.0.0.1:0", tlsConf, qconf())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	cfg.Keys, cfg.ServerKey = keys, apiKey
	n := &node{t: t, m: room.New(cfg), addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			go n.m.Serve(context.Background(), rawquic.New(c), "")
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := n.m.Start(ctx, func(ctx context.Context) (moqt.Conn, error) {
		c, err := quic.DialAddr(ctx, n.addr, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{v22.ALPN}}, qconf())
		if err != nil {
			return nil, err
		}
		return rawquic.New(c), nil
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func qconf() *quic.Config {
	return &quic.Config{EnableDatagrams: true, MaxIncomingStreams: 1000, MaxIncomingUniStreams: 1000}
}

func token(t *testing.T, identity string, g auth.RoomGrant) string {
	g.Join = true
	if g.Room == "" {
		g.Room = "r1"
	}
	tok, err := auth.NewAccessToken(apiKey, apiSecret).SetIdentity(identity).SetName(identity + "-name").SetRoomGrant(&g).ToJWT()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// client is a minimal participant speaking raw MoQ, keeping a view of the
// room as an SDK would.
type client struct {
	moqt.RejectAll
	t        *testing.T
	identity string
	sess     *moqt.Session

	mu      sync.Mutex
	state   map[string]*moqitv1.ParticipantInfo
	changed chan struct{}
	inbox   chan *moqitv1.InboxMessage
	signal  chan *moqt.TrackWriter
	mic     chan *moqt.TrackWriter

	sigSG  *moqt.OutgoingSubgroup
	sigObj uint64
	reqID  uint64
}

func (c *client) HandleSubscribe(r *moqt.SubscribeRequest) {
	var ch chan *moqt.TrackWriter
	switch r.Track.Name {
	case room.TrackSignal:
		ch = c.signal
	case room.TrackMic:
		ch = c.mic
	default:
		r.Reject(v22.ReqErrDoesNotExist, "")
		return
	}
	w, err := r.Accept(moqt.AcceptOptions{})
	if err == nil {
		ch <- w
	}
}

func dialClient(t *testing.T, n *node, identity, tok string) (*client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := &client{t: t, identity: identity, state: map[string]*moqitv1.ParticipantInfo{}, changed: make(chan struct{}, 1),
		inbox: make(chan *moqitv1.InboxMessage, 16), signal: make(chan *moqt.TrackWriter, 1), mic: make(chan *moqt.TrackWriter, 1)}
	qc, err := quic.DialAddr(ctx, n.addr, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{v22.ALPN}}, qconf())
	if err != nil {
		t.Fatal(err)
	}
	opt := v22.Token{AliasType: v22.AliasUseValue, Value: []byte(tok)}
	sess, err := moqt.NewSession(ctx, rawquic.New(qc), moqt.Client, moqt.Config{Handler: c, SetupOptions: v22.KeyValues{{Type: v22.OptAuthorizationToken, Bytes: opt.Append(nil)}}})
	if err != nil {
		return nil, err
	}
	c.sess = sess
	t.Cleanup(func() { sess.Close() })
	return c, nil
}

// join publishes the participant's namespace and subscribes to the room and
// inbox tracks, like the SDK's connect().
func join(t *testing.T, n *node, identity string, g auth.RoomGrant) *client {
	t.Helper()
	c, err := dialClient(t, n, identity, token(t, identity, g))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rm := "r1"
	if g.Room != "" {
		rm = g.Room
	}
	if _, err := c.sess.PublishNamespace(ctx, room.ParticipantNamespace(rm, identity), nil); err != nil {
		t.Fatal(err)
	}
	roomSub, err := c.sess.Subscribe(ctx, v22.FullTrackName{Namespace: room.ServerNamespace(rm), Name: room.TrackRoom},
		&v22.Params{LocationFilter: &v22.LocationFilter{Type: v22.FilterRelativeStart, StartGroup: 1}})
	if err != nil {
		t.Fatal(err)
	}
	go c.readObjects(roomSub, func(b []byte) {
		var u moqitv1.RoomStateUpdate
		if proto.Unmarshal(b, &u) == nil {
			c.apply(&u)
		}
	})
	inboxSub, err := c.sess.Subscribe(ctx, v22.FullTrackName{Namespace: room.InboxNamespace(rm, identity), Name: room.TrackInbox}, nil)
	if err != nil {
		t.Fatal(err)
	}
	go c.readObjects(inboxSub, func(b []byte) {
		var m moqitv1.InboxMessage
		if proto.Unmarshal(b, &m) == nil {
			c.inbox <- &m
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

func (c *client) readObjects(sub *moqt.Subscription, f func([]byte)) {
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

func (c *client) apply(u *moqitv1.RoomStateUpdate) {
	c.mu.Lock()
	switch x := u.Update.(type) {
	case *moqitv1.RoomStateUpdate_Snapshot:
		c.state = map[string]*moqitv1.ParticipantInfo{}
		for _, p := range x.Snapshot.Participants {
			c.state[p.Identity] = p
		}
	case *moqitv1.RoomStateUpdate_ParticipantUpdated:
		c.state[x.ParticipantUpdated.Identity] = x.ParticipantUpdated
	case *moqitv1.RoomStateUpdate_ParticipantLeft:
		delete(c.state, x.ParticipantLeft.Identity)
	}
	c.mu.Unlock()
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

// waitState waits until the room view satisfies ok.
func (c *client) waitState(what string, ok func(map[string]*moqitv1.ParticipantInfo) bool) {
	c.t.Helper()
	deadline := time.After(5 * time.Second)
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
			c.mu.Lock()
			defer c.mu.Unlock()
			c.t.Fatalf("%s: %s never true; state %v", c.identity, what, c.state)
		}
	}
}

// signal sends a ClientSignal and waits for its response on the inbox.
func (c *client) send(cs *moqitv1.ClientSignal) *moqitv1.RequestResponse {
	c.t.Helper()
	c.reqID++
	cs.RequestId = c.reqID
	b, _ := proto.Marshal(cs)
	if err := c.sigSG.WriteObject(&v22.Object{ID: c.sigObj, Payload: b}); err != nil {
		c.t.Fatal(err)
	}
	c.sigObj++
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-c.inbox:
			if r := m.GetResponse(); r != nil && r.RequestId == cs.RequestId {
				return r
			}
		case <-deadline:
			c.t.Fatalf("%s: no response to request %d", c.identity, cs.RequestId)
		}
	}
}

func micTracks() *moqitv1.ClientSignal {
	return &moqitv1.ClientSignal{Signal: &moqitv1.ClientSignal_SetTracks{SetTracks: &moqitv1.SetTracks{Tracks: []*moqitv1.TrackInfo{{
		Name: "microphone", Kind: moqitv1.TrackKind_TRACK_KIND_AUDIO, Source: moqitv1.TrackSource_TRACK_SOURCE_MICROPHONE,
		Codec: "opus", Renditions: []*moqitv1.Rendition{{TrackName: room.TrackMic, Bitrate: 32000}},
	}}}}}
}

func present(ids ...string) func(map[string]*moqitv1.ParticipantInfo) bool {
	return func(s map[string]*moqitv1.ParticipantInfo) bool {
		for _, id := range ids {
			if p := s[id]; p == nil || p.State != moqitv1.ParticipantState_PARTICIPANT_STATE_ACTIVE {
				return false
			}
		}
		return true
	}
}

func wantUnauthorized(t *testing.T, what string, err error) {
	t.Helper()
	var re *moqt.RequestError
	if !errors.As(err, &re) || re.Code != v22.ReqErrUnauthorized {
		t.Errorf("%s: %v; want UNAUTHORIZED", what, err)
	}
}

func TestRoomFlow(t *testing.T) {
	n := newNode(t, room.Config{})
	no := false
	alice := join(t, n, "alice", auth.RoomGrant{UpdateOwnMetadata: true})
	alice.waitState("alice active", present("alice"))
	bob := join(t, n, "bob", auth.RoomGrant{Publish: &no})
	alice.waitState("bob visible to alice", present("alice", "bob"))
	bob.waitState("alice visible to bob", present("alice", "bob"))

	// Alice declares a microphone track; Bob sees it with a server sid.
	if r := alice.send(micTracks()); r.Result != moqitv1.RequestResult_REQUEST_RESULT_OK {
		t.Fatalf("SetTracks: %v", r)
	}
	bob.waitState("alice's mic visible", func(s map[string]*moqitv1.ParticipantInfo) bool {
		tr := s["alice"].GetTracks()
		return len(tr) == 1 && tr[0].Sid != "" && tr[0].Renditions[0].TrackName == room.TrackMic
	})

	// Bob subscribes to Alice's mic through the relay.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sub, err := bob.sess.Subscribe(ctx, v22.FullTrackName{Namespace: room.ParticipantNamespace("r1", "alice"), Name: room.TrackMic}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := <-alice.mic
	sg := w.OpenSubgroup(0, 0, moqt.SubgroupOptions{})
	sg.WriteObject(&v22.Object{ID: 0, Payload: []byte("opus frame")})
	sg.Close()
	g, err := sub.AcceptSubgroup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if o, err := g.Next(); err != nil || string(o.Payload) != "opus frame" {
		t.Fatalf("media object %v, %v", o, err)
	}

	// Authorization.
	_, err = bob.sess.PublishNamespace(ctx, room.ParticipantNamespace("r1", "alice"), nil)
	wantUnauthorized(t, "publishing someone else's namespace", err)
	_, err = bob.sess.Subscribe(ctx, v22.FullTrackName{Namespace: room.ParticipantNamespace("r1", "alice"), Name: room.TrackSignal}, nil)
	wantUnauthorized(t, "reading another participant's signal track", err)
	_, err = bob.sess.Subscribe(ctx, v22.FullTrackName{Namespace: room.InboxNamespace("r1", "alice"), Name: room.TrackInbox}, nil)
	wantUnauthorized(t, "reading another participant's inbox", err)
	_, err = bob.sess.Subscribe(ctx, v22.FullTrackName{Namespace: room.ServerNamespace("r2"), Name: room.TrackRoom}, nil)
	wantUnauthorized(t, "another room's state", err)
	if r := bob.send(micTracks()); r.Result != moqitv1.RequestResult_REQUEST_RESULT_NOT_ALLOWED {
		t.Errorf("bob without publish permission: SetTracks gave %v", r)
	}

	// Metadata: allowed for Alice, not for Bob.
	md := `{"hand":"raised"}`
	upd := &moqitv1.ClientSignal{Signal: &moqitv1.ClientSignal_UpdateParticipant{UpdateParticipant: &moqitv1.UpdateParticipant{Metadata: &md, Attributes: map[string]string{"role": "host"}}}}
	if r := alice.send(upd); r.Result != moqitv1.RequestResult_REQUEST_RESULT_OK {
		t.Fatalf("alice update: %v", r)
	}
	bob.waitState("alice's metadata", func(s map[string]*moqitv1.ParticipantInfo) bool {
		return s["alice"].GetMetadata() == md && s["alice"].GetAttributes()["role"] == "host"
	})
	if r := bob.send(upd); r.Result != moqitv1.RequestResult_REQUEST_RESULT_NOT_ALLOWED {
		t.Errorf("bob update without permission: %v", r)
	}

	// Targeted data reaches only Bob's inbox.
	data := &moqitv1.ClientSignal{Signal: &moqitv1.ClientSignal_SendData{SendData: &moqitv1.SendData{DestinationIdentities: []string{"bob"}, Topic: "chat", Payload: []byte("hi bob")}}}
	if r := alice.send(data); r.Result != moqitv1.RequestResult_REQUEST_RESULT_OK {
		t.Fatalf("send data: %v", r)
	}
	select {
	case m := <-bob.inbox:
		if d := m.GetData(); d == nil || d.FromIdentity != "alice" || string(d.Payload) != "hi bob" || d.Topic != "chat" {
			t.Errorf("bob inbox %v", m)
		}
	case <-ctx.Done():
		t.Fatal("bob did not get the data")
	}

	// Alice leaves.
	alice.sess.Close()
	bob.waitState("alice gone", func(s map[string]*moqitv1.ParticipantInfo) bool { return s["alice"] == nil && s["bob"] != nil })
}

func TestLateJoinerGetsSnapshot(t *testing.T) {
	n := newNode(t, room.Config{SnapshotEvery: 3})
	alice := join(t, n, "alice", auth.RoomGrant{UpdateOwnMetadata: true})
	alice.waitState("alice", present("alice"))
	// Enough updates to roll the room track over to new Groups.
	for i := range 7 {
		name := string(rune('a' + i))
		alice.send(&moqitv1.ClientSignal{Signal: &moqitv1.ClientSignal_UpdateParticipant{UpdateParticipant: &moqitv1.UpdateParticipant{Name: &name}}})
	}
	carol := join(t, n, "carol", auth.RoomGrant{})
	carol.waitState("full state for a late joiner", func(s map[string]*moqitv1.ParticipantInfo) bool {
		return s["alice"].GetName() == "g" && s["carol"] != nil
	})
}

func TestDuplicateIdentity(t *testing.T) {
	n := newNode(t, room.Config{})
	first := join(t, n, "alice", auth.RoomGrant{})
	first.waitState("first", present("alice"))
	firstSid := first.state["alice"].Sid
	second := join(t, n, "alice", auth.RoomGrant{})
	select {
	case <-first.sess.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("first session not closed")
	}
	second.waitState("second alice", func(s map[string]*moqitv1.ParticipantInfo) bool {
		return s["alice"] != nil && s["alice"].Sid != firstSid && s["alice"].State == moqitv1.ParticipantState_PARTICIPANT_STATE_ACTIVE
	})
}

func TestTokensAndSources(t *testing.T) {
	n := newNode(t, room.Config{MaxParticipants: 1})
	bad, _ := auth.NewAccessToken(apiKey, "wrong-secret").SetIdentity("eve").SetRoomGrant(&auth.RoomGrant{Join: true, Room: "r1"}).ToJWT()
	noJoin, _ := auth.NewAccessToken(apiKey, apiSecret).SetIdentity("eve").SetRoomGrant(&auth.RoomGrant{List: true}).ToJWT()
	for what, tok := range map[string]string{"wrong secret": bad, "no join grant": noJoin} {
		expectRejected(t, what, n, tok)
	}

	carol := join(t, n, "carol", auth.RoomGrant{PublishSources: []string{"microphone"}})
	carol.waitState("carol", present("carol"))
	cam := &moqitv1.ClientSignal{Signal: &moqitv1.ClientSignal_SetTracks{SetTracks: &moqitv1.SetTracks{Tracks: []*moqitv1.TrackInfo{{
		Name: "camera", Kind: moqitv1.TrackKind_TRACK_KIND_VIDEO, Source: moqitv1.TrackSource_TRACK_SOURCE_CAMERA,
		Renditions: []*moqitv1.Rendition{{TrackName: "camera-h"}},
	}}}}}
	if r := carol.send(cam); r.Result != moqitv1.RequestResult_REQUEST_RESULT_NOT_ALLOWED {
		t.Errorf("camera with microphone-only grant: %v", r)
	}
	if r := carol.send(micTracks()); r.Result != moqitv1.RequestResult_REQUEST_RESULT_OK {
		t.Errorf("microphone: %v", r)
	}
	mic := micTracks()
	mic.GetSetTracks().Tracks[0].Channels = 2
	if r := carol.send(mic); r.Result != moqitv1.RequestResult_REQUEST_RESULT_OK {
		t.Errorf("stereo microphone: %v", r)
	}
	mic.GetSetTracks().Tracks[0].Channels = 6
	if r := carol.send(mic); r.Result != moqitv1.RequestResult_REQUEST_RESULT_NOT_ALLOWED {
		t.Errorf("6-channel microphone: %v", r)
	}

	// The room holds one participant; a second is turned away.
	expectRejected(t, "room full", n, token(t, "dave", auth.RoomGrant{}))
}

// expectRejected checks that the server closes a session with UNAUTHORIZED.
// The client's handshake can complete first: the server sends its SETUP
// before checking the client's token.
func expectRejected(t *testing.T, what string, n *node, tok string) {
	t.Helper()
	c, err := dialClient(t, n, "eve", tok)
	if err != nil {
		return
	}
	select {
	case <-c.sess.Context().Done():
		var appErr *quic.ApplicationError
		if !errors.As(context.Cause(c.sess.Context()), &appErr) || appErr.ErrorCode != quic.ApplicationErrorCode(v22.ErrUnauthorized) {
			t.Errorf("%s: closed with %v; want UNAUTHORIZED", what, context.Cause(c.sess.Context()))
		}
	case <-time.After(3 * time.Second):
		t.Errorf("%s: session was not rejected", what)
	}
}

func TestActiveSpeakers(t *testing.T) {
	n := newNode(t, room.Config{})
	alice := testclient.Join(t, n.addr, "r1", "alice", token(t, "alice", auth.RoomGrant{}))
	bob := testclient.Join(t, n.addr, "r1", "bob", token(t, "bob", auth.RoomGrant{}))
	alice.Send(testclient.MicTracks())
	bob.WaitState("both", testclient.Present("alice", "bob"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sub, err := bob.Sess.Subscribe(ctx, v22.FullTrackName{Namespace: room.ParticipantNamespace("r1", "alice"), Name: room.TrackMic}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := <-alice.Mic
	send := func(dbov uint64, frames int) {
		for i := range frames {
			sg := w.OpenSubgroup(uint64(time.Now().UnixNano()+int64(i)), 0, moqt.SubgroupOptions{Properties: true, EndOfGroup: true})
			sg.WriteObject(&v22.Object{ID: 0, Payload: []byte{1}, Properties: v22.KeyValues{{Type: 0x0C, Int: dbov}}})
			sg.Close()
			time.Sleep(20 * time.Millisecond)
		}
	}
	go func() {
		for {
			g, err := sub.AcceptSubgroup(context.Background())
			if err != nil {
				return
			}
			g.Next()
		}
	}()
	send(10, 25) // loud: -10 dBov
	waitUntil(t, "alice announced as speaking", func() bool { return slices.Equal(bob.Speakers(), []string{"alice"}) })
	send(127, 60) // silence
	waitUntil(t, "alice no longer speaking", func() bool { return len(bob.Speakers()) == 0 })
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out: %s", what)
}
