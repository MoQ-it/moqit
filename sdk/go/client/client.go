// Package client joins moqit rooms as a participant from Go, over raw
// QUIC, and publishes media into them: for services such as an ingress
// that bring media from outside the browser.
//
//	p, err := client.Join(ctx, client.Config{URL: "https://moqit.example.com:4443/moq"}, token,
//		[]*moqitv1.TrackInfo{{Name: "microphone", Kind: moqitv1.TrackKind_TRACK_KIND_AUDIO,
//			Source: moqitv1.TrackSource_TRACK_SOURCE_MICROPHONE, Codec: "opus",
//			Renditions: []*moqitv1.Rendition{{TrackName: "mic"}}}})
//	mic := p.Track("mic")
//	mic.WriteAudio(time.Now(), level, opusFrame) // for each 20 ms frame
//	p.Close("done")
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/MoQ-it/moqit/pkg/auth"
	"github.com/MoQ-it/moqit/pkg/room"
	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"github.com/MoQ-it/moqt"
	v22 "github.com/MoQ-it/moqt/wire/v22"
	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/protobuf/proto"
)

// drainTimeout bounds how long Close waits for queued objects to be sent.
const drainTimeout = 500 * time.Millisecond

// Participant is a session in a room, joined like the browser SDK joins.
// It follows RoomService.MuteTrack by stopping the track's renditions and
// declaring the track muted.
type Participant struct {
	moqt.RejectAll
	room, identity string
	log            *slog.Logger
	tracks         map[string]*Track // by MoQ track name
	signal         chan *moqt.TrackWriter

	sess *moqt.Session

	mu      sync.Mutex
	infos   []*moqitv1.TrackInfo
	sigSG   *moqt.OutgoingSubgroup
	sigObj  uint64
	reqID   uint64
	pending map[uint64]chan *moqitv1.RequestResponse
	sids    map[string]string // track sid → TrackInfo name, from the room state
}

// Join connects with a participant token, joins its room and declares
// tracks; each rendition gets a Track to write to. The node verifies the
// token; here it is only read for the room and identity.
func Join(ctx context.Context, cfg Config, token string, tracks []*moqitv1.TrackInfo) (*Participant, error) {
	var c auth.Claims
	if _, _, err := jwt.NewParser().ParseUnverified(token, &c); err != nil {
		return nil, fmt.Errorf("client: reading token: %w", err)
	}
	g := c.Grants.Room
	if g == nil || !g.Join || g.Room == "" || c.Identity() == "" {
		return nil, errors.New("client: not a participant token with a join grant")
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	p := &Participant{room: g.Room, identity: c.Identity(), log: log, tracks: map[string]*Track{},
		signal: make(chan *moqt.TrackWriter, 1), pending: map[uint64]chan *moqitv1.RequestResponse{}, sids: map[string]string{}}
	for _, t := range tracks {
		p.infos = append(p.infos, proto.CloneOf(t))
		for _, r := range t.Renditions {
			if r.TrackName == "" || p.tracks[r.TrackName] != nil {
				return nil, fmt.Errorf("client: track %q: missing or repeated rendition track name %q", t.Name, r.TrackName)
			}
			tr := newTrack(r.TrackName, t.Kind == moqitv1.TrackKind_TRACK_KIND_AUDIO)
			tr.onChange = p.trackChanged
			p.tracks[r.TrackName] = tr
		}
	}
	if err := p.join(ctx, cfg, token); err != nil {
		return nil, err
	}
	return p, nil
}

// Room is the room joined.
func (p *Participant) Room() string { return p.room }

// Identity is the participant's identity.
func (p *Participant) Identity() string { return p.identity }

// Track returns the Track for a rendition's track name, or nil.
func (p *Participant) Track(name string) *Track { return p.tracks[name] }

// Context ends when the participant leaves the room: on Close, when the
// server removes it or another session takes its identity, or when the
// connection is lost.
func (p *Participant) Context() context.Context { return p.sess.Context() }

// Close ends every track, waits briefly for queued objects to be sent and
// leaves the room.
func (p *Participant) Close(reason string) {
	for _, t := range p.tracks {
		t.close()
	}
	done := make(chan struct{})
	go func() {
		for _, t := range p.tracks {
			t.running.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(drainTimeout):
	case <-p.sess.Context().Done():
	}
	p.sess.CloseWithError(v22.ErrNoError, reason)
}

// trackChanged logs a subscriber's change on a track, with every track
// being sent.
func (p *Participant) trackChanged(track, event string, attrs ...any) {
	var sending []string
	p.mu.Lock()
	for _, info := range p.infos {
		for _, r := range info.Renditions {
			if p.tracks[r.TrackName].sending() {
				sending = append(sending, r.TrackName)
			}
		}
	}
	p.mu.Unlock()
	p.log.Info("client: track "+event, append([]any{"track", track, "sending", sending}, attrs...)...)
}

func (p *Participant) HandleSubscribe(r *moqt.SubscribeRequest) {
	if r.Track.Name == room.TrackSignal {
		if w, err := r.Accept(moqt.AcceptOptions{}); err == nil {
			select {
			case p.signal <- w:
			default:
			}
		}
		return
	}
	if t := p.tracks[r.Track.Name]; t != nil {
		t.subscribe(r)
		return
	}
	r.Reject(v22.ReqErrDoesNotExist, "not published")
}

func (p *Participant) join(ctx context.Context, cfg Config, token string) error {
	sess, err := Dial(ctx, nil, cfg, token, p)
	if err != nil {
		return err
	}
	p.sess = sess
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	fail := func(err error) error {
		sess.CloseWithError(v22.ErrNoError, "join failed")
		return err
	}
	if _, err := sess.PublishNamespace(ctx, room.ParticipantNamespace(p.room, p.identity), nil); err != nil {
		return fail(err)
	}
	inbox, err := sess.Subscribe(ctx, v22.FullTrackName{Namespace: room.InboxNamespace(p.room, p.identity), Name: room.TrackInbox}, nil)
	if err != nil {
		return fail(err)
	}
	go readObjects(inbox, p.onInbox)
	state, err := sess.Subscribe(ctx, v22.FullTrackName{Namespace: room.ServerNamespace(p.room), Name: room.TrackRoom},
		&v22.Params{LocationFilter: &v22.LocationFilter{Type: v22.FilterRelativeStart, StartGroup: 1}})
	if err != nil {
		return fail(err)
	}
	go readObjects(state, p.onRoom)
	select {
	case w := <-p.signal:
		p.mu.Lock()
		p.sigSG = w.OpenSubgroup(0, 0, moqt.SubgroupOptions{})
		p.mu.Unlock()
	case <-ctx.Done():
		return fail(errors.New("client: the server never subscribed to the signal track"))
	}
	if err := p.setTracks(ctx); err != nil {
		return fail(err)
	}
	return nil
}

func (p *Participant) setTracks(ctx context.Context) error {
	p.mu.Lock()
	infos := make([]*moqitv1.TrackInfo, len(p.infos))
	for i, t := range p.infos {
		infos[i] = proto.CloneOf(t)
	}
	p.mu.Unlock()
	return p.request(ctx, &moqitv1.ClientSignal{Signal: &moqitv1.ClientSignal_SetTracks{SetTracks: &moqitv1.SetTracks{Tracks: infos}}})
}

// request sends a signal and waits for its response on the inbox.
func (p *Participant) request(ctx context.Context, cs *moqitv1.ClientSignal) error {
	ch := make(chan *moqitv1.RequestResponse, 1)
	p.mu.Lock()
	p.reqID++
	cs.RequestId = p.reqID
	p.pending[cs.RequestId] = ch
	b, err := proto.Marshal(cs)
	if err == nil {
		err = p.sigSG.WriteObject(&v22.Object{ID: p.sigObj, Payload: b})
		p.sigObj++
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, cs.RequestId)
		p.mu.Unlock()
	}()
	if err != nil {
		return err
	}
	select {
	case r := <-ch:
		if r.Result != moqitv1.RequestResult_REQUEST_RESULT_OK {
			return fmt.Errorf("%v: %s", r.Result, r.Message)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Participant) onInbox(b []byte) {
	var m moqitv1.InboxMessage
	if proto.Unmarshal(b, &m) != nil {
		return
	}
	switch x := m.Message.(type) {
	case *moqitv1.InboxMessage_Response:
		p.mu.Lock()
		ch := p.pending[x.Response.RequestId]
		p.mu.Unlock()
		if ch != nil {
			ch <- x.Response
		}
	case *moqitv1.InboxMessage_MuteTrack:
		go p.mute(x.MuteTrack.TrackSid, x.MuteTrack.Muted)
	}
}

func (p *Participant) mute(sid string, muted bool) {
	p.mu.Lock()
	name := p.sids[sid]
	var info *moqitv1.TrackInfo
	for _, t := range p.infos {
		if t.Name == name {
			info = t
		}
	}
	if info == nil || info.Muted == muted {
		p.mu.Unlock()
		return
	}
	info.Muted = muted
	for _, r := range info.Renditions {
		if t := p.tracks[r.TrackName]; t != nil {
			t.setMuted(muted)
		}
	}
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(p.sess.Context(), 10*time.Second)
	defer cancel()
	if err := p.setTracks(ctx); err != nil {
		p.log.Warn("declaring mute", "track", name, "err", err)
	}
}

func (p *Participant) onRoom(b []byte) {
	var u moqitv1.RoomStateUpdate
	if proto.Unmarshal(b, &u) != nil {
		return
	}
	var me *moqitv1.ParticipantInfo
	switch x := u.Update.(type) {
	case *moqitv1.RoomStateUpdate_Snapshot:
		for _, pi := range x.Snapshot.Participants {
			if pi.Identity == p.identity {
				me = pi
			}
		}
	case *moqitv1.RoomStateUpdate_ParticipantUpdated:
		if x.ParticipantUpdated.Identity == p.identity {
			me = x.ParticipantUpdated
		}
	}
	if me == nil {
		return
	}
	p.mu.Lock()
	for _, t := range me.Tracks {
		p.sids[t.Sid] = t.Name
	}
	p.mu.Unlock()
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
