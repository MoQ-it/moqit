package room

import (
	"context"
	"sync"
	"time"

	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"github.com/MoQ-it/moqt"
	v22 "github.com/MoQ-it/moqt/wire/v22"
	"google.golang.org/protobuf/proto"
)

// Room is one room on this node. All state, including writes to the room
// track, is guarded by mu, so updates go out in version order.
//
// In a cluster the room spans nodes: participants connected here are
// local; those on other nodes are known from the cluster's event stream
// (remote) and appear in this node's room track like any other.
type Room struct {
	m    *Manager
	name string

	ready    chan struct{} // initialised: server namespace published, cluster state loaded
	readyErr error
	ns       *moqt.PublishedNamespace

	mu           sync.Mutex
	info         *moqitv1.Room
	participants map[string]*Participant             // local, by identity
	remote       map[string]*moqitv1.ParticipantInfo // on other nodes, by identity
	version      uint64
	state        stateTrack
	departure    *time.Timer
	closed       bool

	// Cluster writes go through one goroutine so Redis sees them in order
	// and is never called with mu held.
	clusterQ    chan func(context.Context)
	stopCluster context.CancelFunc

	speakers  speakers
	stopLoops chan struct{} // closed when the room closes
}

// newRoom creates a room on this node. settings, if set, come from
// CreateRoom (single node); in a cluster the shared record wins.
func newRoom(m *Manager, name string, settings *moqitv1.Room) *Room {
	info := &moqitv1.Room{
		Sid:              newID("RM_"),
		Name:             name,
		DepartureTimeout: uint32(m.cfg.DepartureTimeout / time.Second),
		MaxParticipants:  uint32(m.cfg.MaxParticipants),
		CreatedAt:        time.Now().UnixMilli(),
	}
	if settings != nil {
		info.Metadata, info.EmptyTimeout = settings.Metadata, settings.EmptyTimeout
		if settings.DepartureTimeout > 0 {
			info.DepartureTimeout = settings.DepartureTimeout
		}
		if settings.MaxParticipants > 0 {
			info.MaxParticipants = settings.MaxParticipants
		}
	}
	r := &Room{
		m:            m,
		name:         name,
		info:         info,
		ready:        make(chan struct{}),
		participants: map[string]*Participant{},
		remote:       map[string]*moqitv1.ParticipantInfo{},
		stopLoops:    make(chan struct{}),
	}
	go r.init()
	go r.publishSpeakers(r.stopLoops)
	go r.publishViewers(r.stopLoops)
	return r
}

func (r *Room) init() {
	defer close(r.ready)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if c := r.m.cfg.Cluster; c != nil {
		if err := r.joinCluster(ctx); err != nil {
			r.readyErr = err
			return
		}
	} else {
		r.m.notify(webhookRoomStarted, r.roomInfo(), nil, nil)
	}
	r.ns, r.readyErr = r.m.srv.PublishNamespace(ctx, ServerNamespace(r.name), nil)
}

func (r *Room) roomInfo() *moqitv1.Room {
	r.mu.Lock()
	defer r.mu.Unlock()
	return proto.CloneOf(r.info)
}

func (r *Room) departureTimeout() time.Duration {
	if r.info.DepartureTimeout > 0 {
		return time.Duration(r.info.DepartureTimeout) * time.Second
	}
	return r.m.cfg.DepartureTimeout
}

// joinCluster loads the room's shared record and participants and starts
// following its events and direct messages.
func (r *Room) joinCluster(ctx context.Context) error {
	c := r.m.cfg.Cluster
	info, created, err := c.EnsureRoom(ctx, r.info)
	if err != nil {
		return err
	}
	if created {
		r.m.notify(webhookRoomStarted, info, nil, nil)
	}
	from, ps, err := c.Participants(ctx, r.name)
	if err != nil {
		return err
	}
	cctx, stop := context.WithCancel(context.Background())
	if err := c.ReceiveDirect(cctx, r.name, r.deliverDirect); err != nil {
		stop()
		return err
	}
	if err := c.ReceiveCommands(cctx, r.name, r.handleCommand); err != nil {
		stop()
		return err
	}
	r.mu.Lock()
	r.info.Sid, r.info.CreatedAt, r.info.Metadata, r.info.EmptyTimeout = info.Sid, info.CreatedAt, info.Metadata, info.EmptyTimeout
	if info.DepartureTimeout > 0 {
		r.info.DepartureTimeout = info.DepartureTimeout
	}
	if info.MaxParticipants > 0 {
		r.info.MaxParticipants = info.MaxParticipants
	}
	for _, p := range ps {
		if p.NodeId != c.NodeID() {
			r.remote[p.Identity] = p
		}
	}
	r.info.NumParticipants = uint32(len(r.participants) + len(r.remote))
	r.mu.Unlock()
	r.clusterQ = make(chan func(context.Context), 256)
	r.stopCluster = stop
	go func() {
		for {
			select {
			case f := <-r.clusterQ:
				wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				f(wctx)
				cancel()
			case <-cctx.Done():
				// Drain what was queued before the room closed (e.g. the
				// last participant's removal).
				for {
					select {
					case f := <-r.clusterQ:
						wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						f(wctx)
						cancel()
					default:
						return
					}
				}
			}
		}
	}()
	go c.Watch(cctx, r.name, from, r.applyEvent)
	return nil
}

// clusterWrite queues a write to the cluster; mu must be held so writes
// are queued in the order the changes were made.
func (r *Room) clusterWriteLocked(f func(context.Context)) {
	if r.clusterQ == nil {
		return
	}
	select {
	case r.clusterQ <- f:
	default:
		r.m.log.Warn("cluster write queue full; dropping update", "room", r.name)
	}
}

func (r *Room) putLocked(info *moqitv1.ParticipantInfo) {
	c := r.m.cfg.Cluster
	if c == nil {
		return
	}
	info = proto.CloneOf(info)
	r.clusterWriteLocked(func(ctx context.Context) {
		if err := c.PutParticipant(ctx, r.name, info); err != nil {
			r.m.log.Warn("cluster: recording participant", "room", r.name, "identity", info.Identity, "err", err)
		}
	})
}

// applyEvent applies another node's change to this node's view.
func (r *Room) applyEvent(ev *moqitv1.ClusterRoomEvent) {
	if ev.NodeId == r.m.cfg.Cluster.NodeID() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	switch e := ev.Event.(type) {
	case *moqitv1.ClusterRoomEvent_ParticipantPut:
		r.applyRemotePutLocked(e.ParticipantPut)
	case *moqitv1.ClusterRoomEvent_ParticipantLeft:
		r.applyRemoteLeftLocked(e.ParticipantLeft)
	case *moqitv1.ClusterRoomEvent_RoomUpdated:
		r.setMetadataLocked(e.RoomUpdated.Metadata)
	}
}

func (r *Room) setMetadataLocked(md string) {
	if r.info.Metadata == md {
		return
	}
	r.info.Metadata = md
	r.publishLocked(&moqitv1.RoomStateUpdate{Update: &moqitv1.RoomStateUpdate_RoomUpdated{RoomUpdated: proto.CloneOf(r.info)}})
}

// handleCommand runs an admin command against this node's participants.
func (r *Room) handleCommand(cmd *moqitv1.ClusterCommand) {
	switch c := cmd.Command.(type) {
	case *moqitv1.ClusterCommand_RemoveParticipant:
		if p := r.participant(c.RemoveParticipant); p != nil {
			r.kick(p, moqitv1.DisconnectReason_DISCONNECT_REASON_PARTICIPANT_REMOVED)
		}
	case *moqitv1.ClusterCommand_UpdateParticipant:
		u := c.UpdateParticipant
		if p := r.participant(u.Identity); p != nil {
			r.updateParticipant(p, func(info *moqitv1.ParticipantInfo) { applyUpdate(info, u) })
		}
	case *moqitv1.ClusterCommand_MuteTrack:
		mt := c.MuteTrack
		if p := r.participant(mt.Identity); p != nil {
			r.updateParticipant(p, func(info *moqitv1.ParticipantInfo) {
				for _, t := range info.Tracks {
					if t.Sid == mt.TrackSid {
						t.Muted = mt.Muted
					}
				}
			})
			// The publisher has to stop sending; it is told through its inbox.
			p.inbox.send(&moqitv1.InboxMessage{Message: &moqitv1.InboxMessage_MuteTrack{MuteTrack: &moqitv1.MuteTrackCommand{TrackSid: mt.TrackSid, Muted: mt.Muted}}})
		}
	case *moqitv1.ClusterCommand_DeleteRoom:
		r.mu.Lock()
		local := make([]*Participant, 0, len(r.participants))
		for _, p := range r.participants {
			local = append(local, p)
		}
		r.mu.Unlock()
		for _, p := range local {
			r.kick(p, moqitv1.DisconnectReason_DISCONNECT_REASON_ROOM_DELETED)
		}
		r.closeIfEmpty()
	}
}

// applyUpdate applies an admin update to participant info.
func applyUpdate(info *moqitv1.ParticipantInfo, u *moqitv1.UpdateParticipantRequest) {
	if u.Name != nil {
		info.Name = *u.Name
	}
	if u.Metadata != nil {
		info.Metadata = *u.Metadata
	}
	for k, v := range u.Attributes {
		if v == "" {
			delete(info.Attributes, k)
		} else {
			info.Attributes[k] = v
		}
	}
	if u.Permission != nil {
		info.Permission = proto.CloneOf(u.Permission)
	}
}

// kick removes a local participant and closes its session.
func (r *Room) kick(p *Participant, reason moqitv1.DisconnectReason) {
	r.remove(p, reason)
	go p.sess.CloseWithError(v22.ErrNoError, reason.String())
}

func (r *Room) applyRemotePutLocked(info *moqitv1.ParticipantInfo) {
	if local := r.participants[info.Identity]; local != nil && local.info.Sid != info.Sid {
		if info.JoinedAt < local.info.JoinedAt {
			return // our session is newer; the other node will drop its own
		}
		// The same identity joined on another node more recently: it wins.
		delete(r.participants, info.Identity)
		go local.sess.CloseWithError(v22.ErrNoError, "duplicate identity")
		r.m.notify(webhookParticipantLeft, proto.CloneOf(r.info), proto.CloneOf(local.info), nil)
	}
	r.remote[info.Identity] = info
	r.info.NumParticipants = uint32(len(r.participants) + len(r.remote))
	if !info.Permission.GetHidden() {
		r.publishLocked(&moqitv1.RoomStateUpdate{Update: &moqitv1.RoomStateUpdate_ParticipantUpdated{ParticipantUpdated: proto.CloneOf(info)}})
	}
}

func (r *Room) applyRemoteLeftLocked(left *moqitv1.ParticipantLeft) {
	rem := r.remote[left.Identity]
	if rem == nil || rem.Sid != left.Sid {
		return
	}
	delete(r.remote, left.Identity)
	r.info.NumParticipants = uint32(len(r.participants) + len(r.remote))
	if !rem.Permission.GetHidden() {
		r.publishLocked(&moqitv1.RoomStateUpdate{Update: &moqitv1.RoomStateUpdate_ParticipantLeft{ParticipantLeft: proto.CloneOf(left)}})
	}
}

// deliverDirect hands a cluster direct message to a local participant.
func (r *Room) deliverDirect(identity string, msg *moqitv1.InboxMessage) {
	if p := r.participant(identity); p != nil {
		p.inbox.send(msg)
	}
}

// sendTo delivers an inbox message to a participant on any node. It
// reports false if the participant is not in the room.
func (r *Room) sendTo(identity string, msg *moqitv1.InboxMessage) bool {
	r.mu.Lock()
	local, remote := r.participants[identity], r.remote[identity] != nil
	r.mu.Unlock()
	switch {
	case local != nil:
		local.inbox.send(msg)
	case remote:
		c := r.m.cfg.Cluster
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := c.SendDirect(ctx, r.name, identity, msg); err != nil {
				r.m.log.Warn("cluster: direct message", "room", r.name, "err", err)
			}
		}()
	default:
		return false
	}
	return true
}

func (r *Room) waitReady(ctx context.Context) error {
	select {
	case <-r.ready:
		return r.readyErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Name returns the room name.
func (r *Room) Name() string { return r.name }

// participant returns a participant connected to this node.
func (r *Room) participant(identity string) *Participant {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.participants[identity]
}

// permissionOf returns the permission of a participant on any node.
func (r *Room) permissionOf(identity string) (*moqitv1.ParticipantPermission, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.participants[identity]; p != nil {
		return p.info.Permission, true
	}
	if p := r.remote[identity]; p != nil {
		return p.Permission, true
	}
	return nil, false
}

// nodeOf returns the node a remote participant is connected to.
func (r *Room) nodeOf(identity string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.remote[identity]; p != nil {
		return p.NodeId
	}
	return ""
}

// Participants returns every participant's info, on all nodes.
func (r *Room) Participants() []*moqitv1.ParticipantInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*moqitv1.ParticipantInfo, 0, len(r.participants)+len(r.remote))
	for _, p := range r.participants {
		out = append(out, proto.CloneOf(p.info))
	}
	for _, p := range r.remote {
		out = append(out, proto.CloneOf(p))
	}
	return out
}

func (r *Room) add(p *Participant) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errRoomClosed
	}
	old := r.participants[p.info.Identity]
	if old == nil && r.info.MaxParticipants > 0 && len(r.participants)+len(r.remote) >= int(r.info.MaxParticipants) {
		return ErrRoomFull
	}
	if r.departure != nil {
		r.departure.Stop()
		r.departure = nil
	}
	if old != nil {
		// The same identity joined again: the newer session wins.
		delete(r.participants, old.info.Identity)
		r.publishLocked(&moqitv1.RoomStateUpdate{Update: &moqitv1.RoomStateUpdate_ParticipantLeft{ParticipantLeft: &moqitv1.ParticipantLeft{
			Identity: old.info.Identity, Sid: old.info.Sid, Reason: moqitv1.DisconnectReason_DISCONNECT_REASON_DUPLICATE_IDENTITY,
		}}})
		go old.sess.CloseWithError(v22.ErrNoError, "duplicate identity")
		r.m.notify(webhookParticipantLeft, proto.CloneOf(r.info), proto.CloneOf(old.info), nil)
	}
	// A session of this identity on another node is superseded too; that
	// node drops it when it sees our record.
	delete(r.remote, p.info.Identity)
	r.participants[p.info.Identity] = p
	r.info.NumParticipants = uint32(len(r.participants) + len(r.remote))
	r.publishParticipantLocked(p)
	r.putLocked(p.info)
	return nil
}

// remove takes p out of the room if it is still the participant with its
// identity. It reports whether anything changed.
func (r *Room) remove(p *Participant, reason moqitv1.DisconnectReason) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.participants[p.info.Identity] != p {
		return false
	}
	delete(r.participants, p.info.Identity)
	go p.inbox.close()
	r.info.NumParticipants = uint32(len(r.participants) + len(r.remote))
	if !p.info.Permission.GetHidden() {
		r.publishLocked(&moqitv1.RoomStateUpdate{Update: &moqitv1.RoomStateUpdate_ParticipantLeft{ParticipantLeft: &moqitv1.ParticipantLeft{
			Identity: p.info.Identity, Sid: p.info.Sid, Reason: reason,
		}}})
	}
	r.m.notify(webhookParticipantLeft, proto.CloneOf(r.info), proto.CloneOf(p.info), nil)
	if c := r.m.cfg.Cluster; c != nil {
		identity, sid, departure := p.info.Identity, p.info.Sid, r.departureTimeout()
		r.clusterWriteLocked(func(ctx context.Context) {
			if _, err := c.RemoveParticipant(ctx, r.name, identity, sid, reason, departure); err != nil {
				r.m.log.Warn("cluster: removing participant", "room", r.name, "identity", identity, "err", err)
			}
		})
	}
	// Only local participants keep the room open on this node; remote ones
	// are served by their own nodes.
	if len(r.participants) == 0 && r.departure == nil {
		r.departure = time.AfterFunc(r.departureTimeout(), r.closeIfEmpty)
	}
	return true
}

// expireIfUnused closes a room created through the API that nobody
// joined within d.
func (r *Room) expireIfUnused(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.participants) == 0 && r.departure == nil {
		r.departure = time.AfterFunc(d, r.closeIfEmpty)
	}
}

var errRoomClosed = &moqt.RequestError{Code: v22.ReqErrDoesNotExist, Reason: "room closed"}

func (r *Room) closeIfEmpty() {
	r.mu.Lock()
	if len(r.participants) > 0 || r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.state.close()
	close(r.stopLoops)
	stop := r.stopCluster
	info := proto.CloneOf(r.info)
	r.mu.Unlock()
	if stop != nil {
		stop()
	}
	if r.ns != nil {
		r.ns.Withdraw()
	}
	r.m.deleteRoom(r)
	r.m.roomFinished(info)
}

// updateParticipant applies f to p's info under the room lock and
// publishes the result.
func (r *Room) updateParticipant(p *Participant, f func(info *moqitv1.ParticipantInfo)) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.participants[p.info.Identity] != p {
		return false
	}
	f(p.info)
	r.publishParticipantLocked(p)
	r.putLocked(p.info)
	return true
}

func (r *Room) publishParticipantLocked(p *Participant) {
	if p.info.Permission.GetHidden() {
		return
	}
	r.publishLocked(&moqitv1.RoomStateUpdate{Update: &moqitv1.RoomStateUpdate_ParticipantUpdated{ParticipantUpdated: proto.CloneOf(p.info)}})
}

func (r *Room) publishLocked(u *moqitv1.RoomStateUpdate) {
	r.version++
	u.Version = r.version
	r.state.publish(u, r)
}

func (r *Room) snapshotLocked() *moqitv1.RoomStateUpdate {
	snap := &moqitv1.RoomSnapshot{Room: proto.CloneOf(r.info)}
	for _, p := range r.participants {
		if !p.info.Permission.GetHidden() {
			snap.Participants = append(snap.Participants, proto.CloneOf(p.info))
		}
	}
	for _, p := range r.remote {
		if !p.Permission.GetHidden() {
			snap.Participants = append(snap.Participants, proto.CloneOf(p))
		}
	}
	r.speakers.mu.Lock()
	for _, id := range r.speakers.last {
		if l := r.speakers.levels[id]; l != nil {
			snap.Speakers = append(snap.Speakers, &moqitv1.SpeakerInfo{Identity: id, Level: l.level})
		}
	}
	r.speakers.mu.Unlock()
	return &moqitv1.RoomStateUpdate{Version: r.version, Update: &moqitv1.RoomStateUpdate_Snapshot{Snapshot: snap}}
}

// attachState answers the relay's SUBSCRIBE for the room track.
func (r *Room) attachState(req *moqt.SubscribeRequest) {
	w, err := req.Accept(moqt.AcceptOptions{Properties: v22.KeyValues{
		{Type: v22.PropDynamicGroups, Int: 1},
		{Type: v22.PropDefaultPublisherPriority, Int: 8}, // signalling ahead of media
	}})
	if err != nil {
		return
	}
	r.mu.Lock()
	r.state.attach(w, r)
	r.mu.Unlock()
	go func() {
		for {
			select {
			case p := <-w.Updates():
				if p.NewGroupRequest != nil {
					r.mu.Lock()
					if r.state.w == w {
						r.state.newGroup(r)
					}
					r.mu.Unlock()
				}
			case <-w.Context().Done():
				r.mu.Lock()
				if r.state.w == w {
					r.state.w, r.state.sg = nil, nil
				}
				r.mu.Unlock()
				return
			}
		}
	}()
}

// stateTrack writes the room track: each Group is a snapshot followed by
// deltas, on one subgroup stream. Callers hold the room's mutex.
type stateTrack struct {
	w      *moqt.TrackWriter
	sg     *moqt.OutgoingSubgroup
	group  uint64
	object uint64
	deltas int
}

func (s *stateTrack) attach(w *moqt.TrackWriter, r *Room) {
	if s.sg != nil {
		s.sg.Close()
	}
	s.w, s.sg = w, nil
	s.newGroup(r)
}

func (s *stateTrack) newGroup(r *Room) {
	if s.w == nil {
		return
	}
	if s.sg != nil {
		s.sg.Close()
		s.group++
	}
	s.sg = s.w.OpenSubgroup(s.group, 0, moqt.SubgroupOptions{FirstObject: true})
	s.object, s.deltas = 0, 0
	s.write(r.snapshotLocked())
}

func (s *stateTrack) publish(u *moqitv1.RoomStateUpdate, r *Room) {
	if s.w == nil {
		return
	}
	if s.deltas >= r.m.cfg.SnapshotEvery {
		s.newGroup(r) // the snapshot already includes u
		return
	}
	s.deltas++
	s.write(u)
}

func (s *stateTrack) write(u *moqitv1.RoomStateUpdate) {
	b, err := proto.Marshal(u)
	if err != nil {
		return
	}
	if err := s.sg.WriteObject(&v22.Object{ID: s.object, Payload: b}); err != nil {
		s.w, s.sg = nil, nil
		return
	}
	s.object++
}

func (s *stateTrack) close() {
	if s.sg != nil {
		s.sg.Close()
	}
	if s.w != nil {
		s.w.Close(v22.DoneTrackEnded, "room closed")
	}
	s.w, s.sg = nil, nil
}
