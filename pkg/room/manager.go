// Package room implements moqit rooms on top of a MoQ relay: it admits
// participants by access token, authorizes every MoQ request, keeps room
// state, and talks to participants over MoQ tracks (see names.go and
// proto/moqit/v1/signal.proto).
package room

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/MoQ-it/moqit/pkg/auth"
	"github.com/MoQ-it/moqit/pkg/cluster"
	"github.com/MoQ-it/moqit/pkg/dvr"
	"github.com/MoQ-it/moqit/pkg/webhook"
	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"github.com/MoQ-it/moqt"
	"github.com/MoQ-it/moqt/relay"
	v22 "github.com/MoQ-it/moqt/wire/v22"
)

// Config configures a Manager.
type Config struct {
	Keys auth.KeyProvider
	// ServerKey signs the token of the manager's own loopback session; it
	// must be in Keys.
	ServerKey string
	NodeID    string
	// DepartureTimeout keeps an empty room alive this long after its last
	// participant leaves (default 20s).
	DepartureTimeout time.Duration
	// MaxParticipants per room; 0 = unlimited.
	MaxParticipants int
	// SnapshotEvery starts a new room-state Group (with a fresh snapshot)
	// after this many deltas, bounding what joiners replay (default 50).
	SnapshotEvery int
	Logger        *slog.Logger

	// Cluster, if set, shares rooms with other nodes; its node ID becomes
	// this node's. Node describes this node in the registry.
	Cluster *cluster.Cluster
	Node    cluster.NodeInfo
	// DialPeer opens a raw QUIC MoQ connection to another node, verifying
	// it presents the certificate with the given SHA-256 (hex).
	DialPeer func(ctx context.Context, addr, certSHA256 string) (moqt.Conn, error)
	// Webhook, if set, receives room and participant events.
	Webhook Notifier
	// EmptyTimeout closes a room created through the API that nobody
	// joins (default 5m).
	EmptyTimeout time.Duration

	// Edge makes this node serve viewers only: it admits viewer tokens,
	// never participants, and fetches every track from the nodes hosting
	// it. It needs Cluster.
	Edge bool
	// Tier is an edge's level in a cascade: tier 1 fetches tracks from the
	// nodes hosting them, higher tiers from an edge one tier up (default 1).
	Tier int
	// MaxViewers caps viewer sessions on this node; 0 = unlimited.
	MaxViewers int
	// ViewerInterval is how often viewer counts are shared and published
	// (default 5s).
	ViewerInterval time.Duration

	// DVR, if set, records rooms that keep history so viewers can rewind.
	DVR *dvr.Store
	// DVRWindow is the history window of rooms created without one; 0
	// leaves history off unless CreateRoom asks for it.
	DVRWindow time.Duration
	// DVRMaxWindow caps a room's history window; 0 = no cap.
	DVRMaxWindow time.Duration
	// DVRMaxHeight picks the video rendition recorded per source: the
	// largest at most this high (0 = the largest).
	DVRMaxHeight int
	// DVRFetchLimit caps a viewer's concurrent FETCHes; 0 = no cap.
	DVRFetchLimit int
}

// Notifier receives webhook events; *webhook.Notifier implements it.
type Notifier interface {
	Notify(event string, room *moqitv1.Room, p *moqitv1.ParticipantInfo, track *moqitv1.TrackInfo)
}

const (
	webhookRoomStarted       = webhook.RoomStarted
	webhookRoomFinished      = webhook.RoomFinished
	webhookParticipantJoined = webhook.ParticipantJoined
	webhookParticipantLeft   = webhook.ParticipantLeft
	webhookTrackPublished    = webhook.TrackPublished
	webhookTrackUnpublished  = webhook.TrackUnpublished
)

func (m *Manager) notify(event string, room *moqitv1.Room, p *moqitv1.ParticipantInfo, t *moqitv1.TrackInfo) {
	if m.cfg.Webhook != nil {
		m.cfg.Webhook.Notify(event, room, p, t)
	}
}

// roomFinished sends room_finished when a room closes on this node: on a
// single node always, in a cluster only when the room is empty everywhere,
// and then by exactly one node.
func (m *Manager) roomFinished(info *moqitv1.Room) {
	c := m.cfg.Cluster
	if c == nil {
		m.notify(webhookRoomFinished, info, nil, nil)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, ps, err := c.Participants(ctx, info.Name); err != nil || len(ps) > 0 {
		return
	}
	if c.Once(ctx, "finished:"+info.Sid, 24*time.Hour) {
		m.notify(webhookRoomFinished, info, nil, nil)
	}
}

// Manager runs the rooms of one node.
type Manager struct {
	cfg   Config
	log   *slog.Logger
	relay *relay.Relay

	srv     *moqt.Session // loopback session to our own relay; set before started closes
	started chan struct{}
	token   string // server-grant token for our own and node-to-node sessions

	peerMu sync.Mutex
	peers  map[string]*peerSession // by node ID

	mu         sync.Mutex
	certSHA256 string // renewed certificate; overrides cfg.Node's
	rooms      map[string]*Room

	vmu         sync.Mutex
	viewerTotal int            // viewer sessions, including those still being set up
	viewerRooms map[string]int // attached viewers by room

	missMu sync.Mutex
	misses map[string]time.Time // failed upstream lookups by namespace

	recMu sync.Mutex
	recs  map[recKey]*recording // DVR recordings of local participants
}

// New creates a Manager and the relay it authorizes. Serve every incoming
// connection with Manager.Serve, then call Start.
func New(cfg Config) *Manager {
	if cfg.DepartureTimeout == 0 {
		cfg.DepartureTimeout = 20 * time.Second
	}
	if cfg.SnapshotEvery == 0 {
		cfg.SnapshotEvery = 50
	}
	if cfg.EmptyTimeout == 0 {
		cfg.EmptyTimeout = 5 * time.Minute
	}
	if cfg.ViewerInterval == 0 {
		cfg.ViewerInterval = 5 * time.Second
	}
	if cfg.Edge && cfg.Tier < 1 {
		cfg.Tier = 1
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Cluster != nil {
		cfg.NodeID = cfg.Cluster.NodeID()
	}
	if cfg.NodeID == "" {
		cfg.NodeID = newID("ND_")
	}
	m := &Manager{cfg: cfg, log: cfg.Logger, rooms: map[string]*Room{}, started: make(chan struct{}), peers: map[string]*peerSession{},
		viewerRooms: map[string]int{}, misses: map[string]time.Time{}, recs: map[recKey]*recording{}}
	rc := relay.Config{Authorizer: m, NamespaceHook: m.onNamespace, ObjectHook: m.onObject, Logger: cfg.Logger}
	if cfg.Cluster != nil {
		rc.Upstream = m
	}
	if cfg.DVR != nil {
		rc.Archive = m
	}
	rc.FetchLimit = m.fetchLimit
	m.relay = relay.New(rc)
	return m
}

// NodeID returns this node's ID.
func (m *Manager) NodeID() string { return m.cfg.NodeID }

// Relay returns the relay the manager authorizes.
func (m *Manager) Relay() *relay.Relay { return m.relay }

// Start opens the manager's own session to the relay, using dial to
// connect to this node's listener. The session serves the room and inbox
// tracks and reads participants' signal tracks.
func (m *Manager) Start(ctx context.Context, dial func(context.Context) (moqt.Conn, error)) error {
	token, err := auth.NewAccessToken(m.cfg.ServerKey, m.cfg.Keys[m.cfg.ServerKey]).
		SetIdentity("server:" + m.cfg.NodeID).SetServerGrant().SetValidFor(10 * 365 * 24 * time.Hour).ToJWT()
	if err != nil {
		return err
	}
	m.token = token
	conn, err := dial(ctx)
	if err != nil {
		return fmt.Errorf("room: dialing own relay: %w", err)
	}
	sess, err := moqt.NewSession(ctx, conn, moqt.Client, moqt.Config{
		Handler:      &serverHandler{m: m},
		SetupOptions: m.tokenOption(),
		Logger:       m.log.With("session", "server"),
	})
	if err != nil {
		return fmt.Errorf("room: server session: %w", err)
	}
	m.srv = sess
	if c := m.cfg.Cluster; c != nil {
		if err := c.Heartbeat(ctx, m.nodeInfo()); err != nil {
			return fmt.Errorf("room: registering node: %w", err)
		}
		go c.RunHeartbeat(ctx, m.nodeInfo)
		go m.reapLoop(ctx)
		go m.syncViewers(ctx)
	}
	if m.cfg.DVR != nil {
		go m.dvrLoop(ctx)
	}
	close(m.started)
	return nil
}

func (m *Manager) tokenOption() v22.KeyValues {
	tok := v22.Token{AliasType: v22.AliasUseValue, Value: []byte(m.token)}
	return v22.KeyValues{{Type: v22.OptAuthorizationToken, Bytes: tok.Append(nil)}}
}

// SetCertSHA256 updates the certificate hash this node advertises to the
// cluster, after its certificate is renewed.
func (m *Manager) SetCertSHA256(hash string) {
	m.mu.Lock()
	m.certSHA256 = hash
	m.mu.Unlock()
}

func (m *Manager) nodeInfo() cluster.NodeInfo {
	info := m.cfg.Node
	m.mu.Lock()
	if m.certSHA256 != "" {
		info.CertSHA256 = m.certSHA256
	}
	for _, r := range m.rooms {
		r.mu.Lock()
		info.Participants += len(r.participants)
		r.mu.Unlock()
	}
	m.mu.Unlock()
	if m.cfg.Edge {
		info.Role, info.Tier = cluster.RoleEdge, m.cfg.Tier
	}
	info.Viewers, info.MaxViewers = m.Viewers(), m.cfg.MaxViewers
	return info
}

// reapLoop removes participants of dead nodes from the rooms this node
// hosts. Removal is atomic in Redis, so when several nodes reap the same
// participant only one wins; the winner applies the change locally, since
// nodes ignore their own events, and the others learn it from the stream.
func (m *Manager) reapLoop(ctx context.Context) {
	c := m.cfg.Cluster
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		nodes, err := c.Nodes(ctx)
		if err != nil {
			continue
		}
		alive := map[string]bool{}
		for _, n := range nodes {
			alive[n.ID] = true
		}
		m.mu.Lock()
		rooms := make([]*Room, 0, len(m.rooms))
		for _, r := range m.rooms {
			rooms = append(rooms, r)
		}
		m.mu.Unlock()
		for _, r := range rooms {
			reaped, err := c.ReapDead(ctx, r.name, func(id string) bool { return alive[id] }, m.cfg.DepartureTimeout)
			if err != nil {
				continue
			}
			for _, p := range reaped {
				m.log.Info("removed participant of a dead node", "room", r.name, "identity", p.Identity, "node", p.NodeId)
				m.notify(webhookParticipantLeft, r.roomInfo(), p, nil)
				r.mu.Lock()
				r.applyRemoteLeftLocked(&moqitv1.ParticipantLeft{Identity: p.Identity, Sid: p.Sid, Reason: moqitv1.DisconnectReason_DISCONNECT_REASON_CONNECTION_LOST})
				r.mu.Unlock()
			}
		}
	}
}

// ResolveUpstream implements relay.UpstreamResolver: a track of a
// participant connected to another node is fetched from that node, and the
// room track of a room this node doesn't host from a node that does. Edges
// below tier 1 go through their parent edge.
func (m *Manager) ResolveUpstream(ctx context.Context, track v22.FullTrackName) (*moqt.Session, error) {
	kind, room, identity := parseNamespace(track.Namespace)
	if kind != nsParticipant && (kind != nsRoomServer || track.Name != TrackRoom) {
		return nil, nil
	}
	if sess := m.fromParent(ctx, room); sess != nil {
		return sess, nil
	}
	key := track.Namespace.Key()
	if m.missedRecently(key) {
		return nil, nil
	}
	var node string
	var err error
	if kind == nsParticipant {
		node, err = m.participantNode(ctx, room, identity)
	} else {
		node, err = m.roomHost(ctx, room)
	}
	if err != nil {
		return nil, err
	}
	if node == "" || node == m.cfg.NodeID {
		m.miss(key)
		return nil, nil
	}
	return m.peer(ctx, node)
}

// peerSession is a MoQ session to another node, shared by every track this
// node fetches from it.
type peerSession struct {
	ready chan struct{}
	sess  *moqt.Session
	err   error
}

func (m *Manager) peer(ctx context.Context, nodeID string) (*moqt.Session, error) {
	m.peerMu.Lock()
	ps := m.peers[nodeID]
	if ps != nil {
		select {
		case <-ps.ready:
			if ps.err != nil || ps.sess.Context().Err() != nil {
				ps = nil // failed or closed: dial again
			}
		default:
		}
	}
	if ps == nil {
		ps = &peerSession{ready: make(chan struct{})}
		m.peers[nodeID] = ps
		go m.dialPeer(nodeID, ps)
	}
	m.peerMu.Unlock()
	select {
	case <-ps.ready:
		return ps.sess, ps.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *Manager) dialPeer(nodeID string, ps *peerSession) {
	defer close(ps.ready)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := m.cfg.Cluster.Node(ctx, nodeID)
	if err != nil || info == nil {
		ps.err = fmt.Errorf("room: node %s unavailable: %v", nodeID, err)
		return
	}
	if m.cfg.DialPeer == nil {
		ps.err = errors.New("room: no DialPeer configured")
		return
	}
	conn, err := m.cfg.DialPeer(ctx, info.QUICAddr, info.CertSHA256)
	if err != nil {
		ps.err = fmt.Errorf("room: dialing node %s at %s: %w", nodeID, info.QUICAddr, err)
		return
	}
	// The peer only answers our requests; it makes its own session to us.
	ps.sess, ps.err = moqt.NewSession(context.Background(), conn, moqt.Client, moqt.Config{
		Handler:      moqt.RejectAll{},
		SetupOptions: m.tokenOption(),
		Logger:       m.log.With("peer", nodeID),
	})
	if ps.err == nil {
		m.log.Info("connected to peer node", "node", nodeID, "addr", info.QUICAddr)
	}
}

// waitStarted blocks until Start has completed; participants are not
// admitted before the manager's own session exists.
func (m *Manager) waitStarted(ctx context.Context) error {
	select {
	case <-m.started:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// peerInfo is attached to each relay peer.
type peerInfo struct {
	server      bool
	participant *Participant
	viewer      *viewerInfo
}

func infoOf(p *relay.Peer) *peerInfo {
	if pi, ok := p.Info.(*peerInfo); ok {
		return pi
	}
	return &peerInfo{}
}

// Serve runs one client connection until it ends. queryToken is a token
// from the WebTransport URL, used when SETUP carries none.
func (m *Manager) Serve(ctx context.Context, conn moqt.Conn, queryToken string) error {
	peer := m.relay.NewPeer(nil)
	var claims *auth.Claims
	reserved := false
	sess, err := moqt.NewSession(ctx, conn, moqt.Server, moqt.Config{
		Handler: peer,
		Logger:  m.log,
		CheckSetup: func(s *v22.Setup) error {
			token := queryToken
			if kv, ok := s.Options.Get(v22.OptAuthorizationToken); ok {
				t, err := v22.ParseToken(kv.Bytes)
				if err != nil {
					return err
				}
				token = string(t.Value)
			}
			if token == "" {
				return errors.New("no access token")
			}
			c, err := m.cfg.Keys.Verify(token)
			if err != nil {
				return err
			}
			switch {
			case c.Grants.Server:
			case c.IsViewer():
				if !m.reserveViewer() {
					return errors.New("node has no room for more viewers")
				}
				reserved = true
			case m.cfg.Edge:
				return errors.New("this node serves viewers only")
			case c.Grants.Room == nil || !c.Grants.Room.Join || c.Grants.Room.Room == "":
				return errors.New("token has no room join grant")
			}
			claims = c
			return nil
		},
	})
	if reserved {
		defer m.releaseViewer()
	}
	if err != nil {
		return err
	}
	if claims.Grants.Server {
		peer.Info = &peerInfo{server: true}
		peer.Attach(sess)
		<-sess.Context().Done()
		return nil
	}
	if claims.IsViewer() {
		v := &viewerInfo{room: claims.Grants.Room.Room, identity: claims.Identity()}
		if v.identity == "" {
			v.identity = newID("viewer-")
		}
		peer.Info = &peerInfo{viewer: v}
		return m.serveViewer(ctx, v, peer, sess)
	}
	p, err := m.join(ctx, claims, sess)
	if err != nil {
		sess.CloseWithError(v22.ErrUnauthorized, err.Error())
		return err
	}
	peer.Info = &peerInfo{participant: p}
	peer.Attach(sess)
	<-sess.Context().Done()
	m.leave(p, moqitv1.DisconnectReason_DISCONNECT_REASON_CLIENT_INITIATED)
	return nil
}

// ErrRoomFull is returned when a room has MaxParticipants.
var ErrRoomFull = errors.New("room is full")

func (m *Manager) join(ctx context.Context, c *auth.Claims, sess *moqt.Session) (*Participant, error) {
	perm, err := c.Grants.Room.Permission()
	if err != nil {
		return nil, err
	}
	if err := m.waitStarted(ctx); err != nil {
		return nil, err
	}
	r, err := m.room(ctx, c.Grants.Room.Room)
	if err != nil {
		return nil, err
	}
	p := &Participant{
		room: r,
		sess: sess,
		info: &moqitv1.ParticipantInfo{
			Sid:        newID("PA_"),
			Identity:   c.Identity(),
			Name:       c.Name,
			State:      moqitv1.ParticipantState_PARTICIPANT_STATE_JOINED,
			Metadata:   c.Metadata,
			Attributes: c.Attributes,
			Permission: perm,
			JoinedAt:   time.Now().UnixMilli(),
			NodeId:     m.cfg.NodeID,
		},
		trackSids: map[string]string{},
	}
	if p.info.Attributes == nil {
		p.info.Attributes = map[string]string{}
	}
	if err := r.add(p); err != nil {
		return nil, err
	}
	m.log.Info("participant joined", "room", r.name, "identity", p.info.Identity)
	m.notify(webhookParticipantJoined, r.roomInfo(), p.Info(), nil)
	return p, nil
}

func (m *Manager) leave(p *Participant, reason moqitv1.DisconnectReason) {
	if p.room.remove(p, reason) {
		m.log.Info("participant left", "room", p.room.name, "identity", p.info.Identity, "reason", reason)
	}
}

// room returns a room, creating it (and publishing its server namespace)
// on first use.
func (m *Manager) room(ctx context.Context, name string) (*Room, error) {
	m.mu.Lock()
	r := m.rooms[name]
	if r == nil {
		r = newRoom(m, name, nil)
		m.rooms[name] = r
	}
	m.mu.Unlock()
	return r, r.waitReady(ctx)
}

// Room returns an existing room.
func (m *Manager) Room(name string) *Room {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rooms[name]
}

func (m *Manager) deleteRoom(r *Room) {
	m.mu.Lock()
	if m.rooms[r.name] == r {
		delete(m.rooms, r.name)
	}
	m.mu.Unlock()
	m.log.Info("room closed", "room", r.name)
}

// onNamespace starts reading a participant's signal track once its
// namespace is published, so the relay can route the SUBSCRIBE to it.
func (m *Manager) onNamespace(peer *relay.Peer, prefix v22.Namespace, active bool) {
	p := infoOf(peer).participant
	if active && p != nil && prefix.Key() == ParticipantNamespace(p.room.name, p.info.Identity).Key() {
		go p.readSignals(m.srv)
	}
}

// serverHandler answers the relay's SUBSCRIBEs for server-published tracks.
type serverHandler struct {
	moqt.RejectAll
	m *Manager
}

func (h *serverHandler) HandleSubscribe(req *moqt.SubscribeRequest) {
	kind, room, identity := parseNamespace(req.Track.Namespace)
	r := h.m.Room(room)
	switch {
	case r == nil:
	case kind == nsRoomServer && req.Track.Name == TrackRoom:
		r.attachState(req)
		return
	case kind == nsInbox && req.Track.Name == TrackInbox:
		if p := r.participant(identity); p != nil {
			p.inbox.attach(req)
			return
		}
	}
	req.Reject(v22.ReqErrDoesNotExist, "no such track")
}

// Authorization (relay.Authorizer).

func denied(reason string) error {
	return &moqt.RequestError{Code: v22.ReqErrUnauthorized, Reason: reason}
}

func (m *Manager) AuthorizePublishNamespace(peer *relay.Peer, prefix v22.Namespace, _ *v22.Params) error {
	pi := infoOf(peer)
	if pi.server {
		return nil
	}
	if p := pi.participant; p != nil && prefix.Key() == ParticipantNamespace(p.room.name, p.info.Identity).Key() {
		return nil
	}
	return denied("participants publish only their own namespace")
}

func (m *Manager) AuthorizeSubscribeNamespace(peer *relay.Peer, prefix v22.Namespace, _ *v22.Params) error {
	pi := infoOf(peer)
	if pi.server {
		return nil
	}
	if p := pi.participant; p != nil && prefix.HasPrefix(RoomNamespace(p.room.name)) {
		return nil
	}
	return denied("outside your room")
}

func (m *Manager) AuthorizeSubscribe(peer *relay.Peer, track v22.FullTrackName, _ *v22.Params) error {
	pi := infoOf(peer)
	if pi.server {
		return m.authorizeServerSubscribe(track)
	}
	p, v := pi.participant, pi.viewer
	var own string
	switch {
	case p != nil:
		own = p.room.name
	case v != nil:
		own = v.room
	default:
		return denied("not a participant")
	}
	kind, room, identity := parseNamespace(track.Namespace)
	if room != own {
		return denied("outside your room")
	}
	switch kind {
	case nsRoomServer:
		if track.Name == TrackRoom {
			return nil
		}
	case nsInbox:
		if p != nil && track.Name == TrackInbox && identity == p.info.Identity {
			return nil
		}
		return denied("not your inbox")
	case nsParticipant:
		class, source := classifyTrack(track.Name)
		switch class {
		case classSignal:
			return denied("signal tracks are read by the server only")
		case classUnknown:
			return &moqt.RequestError{Code: v22.ReqErrDoesNotExist, Reason: "unknown track name"}
		case classDvr:
			return &moqt.RequestError{Code: v22.ReqErrDoesNotExist, Reason: "the dvr track is fetch-only"}
		}
		if p != nil && !p.permission().CanSubscribe {
			return denied("no subscribe permission")
		}
		if class == classCatalog {
			return nil
		}
		r := m.Room(room)
		if r == nil {
			// Only a viewer gets here, on a node not hosting the room: the
			// node serving the track checks the publisher.
			return nil
		}
		perm, ok := r.permissionOf(identity)
		if !ok {
			return &moqt.RequestError{Code: v22.ReqErrDoesNotExist, Reason: "no such participant"}
		}
		return publisherAllows(perm, class, source)
	}
	return &moqt.RequestError{Code: v22.ReqErrDoesNotExist, Reason: "no such track"}
}

// authorizeServerSubscribe lets other nodes and our own session read any
// track, except data and media a local publisher may not publish: an edge
// can't check those itself.
func (m *Manager) authorizeServerSubscribe(track v22.FullTrackName) error {
	kind, room, identity := parseNamespace(track.Namespace)
	if kind != nsParticipant {
		return nil
	}
	class, source := classifyTrack(track.Name)
	if class != classData && class != classMedia {
		return nil
	}
	r := m.Room(room)
	if r == nil {
		return nil
	}
	p := r.participant(identity)
	if p == nil {
		return nil
	}
	return publisherAllows(p.permission(), class, source)
}

func publisherAllows(perm *moqitv1.ParticipantPermission, class trackClass, source moqitv1.TrackSource) error {
	if class == classData && !perm.CanPublishData {
		return denied("publisher may not publish data")
	}
	if class == classMedia && !auth.CanPublishSource(perm, source) {
		return denied("publisher may not publish " + strings.ToLower(strings.TrimPrefix(source.String(), "TRACK_SOURCE_")))
	}
	return nil
}

func newID(prefix string) string {
	b := make([]byte, 10)
	rand.Read(b)
	return prefix + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
}
