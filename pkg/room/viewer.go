package room

import (
	"context"
	"hash/fnv"
	"maps"
	"time"

	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"github.com/MoQ-it/moqt"
	"github.com/MoQ-it/moqt/relay"
	v22 "github.com/MoQ-it/moqt/wire/v22"
	"google.golang.org/protobuf/proto"
)

// viewerInfo describes a read-only viewer session: it may subscribe to its
// room's tracks but is never a participant.
type viewerInfo struct {
	room, identity string
}

// reserveViewer claims a viewer slot, unless the node has MaxViewers.
func (m *Manager) reserveViewer() bool {
	m.vmu.Lock()
	defer m.vmu.Unlock()
	if m.cfg.MaxViewers > 0 && m.viewerTotal >= m.cfg.MaxViewers {
		return false
	}
	m.viewerTotal++
	return true
}

func (m *Manager) releaseViewer() {
	m.vmu.Lock()
	m.viewerTotal--
	m.vmu.Unlock()
}

func (m *Manager) addViewer(room string, delta int) {
	m.vmu.Lock()
	defer m.vmu.Unlock()
	if n := m.viewerRooms[room] + delta; n > 0 {
		m.viewerRooms[room] = n
	} else {
		delete(m.viewerRooms, room)
	}
}

// ViewerCount returns the number of viewers of a room on this node.
func (m *Manager) ViewerCount(room string) int {
	m.vmu.Lock()
	defer m.vmu.Unlock()
	return m.viewerRooms[room]
}

// Viewers returns the number of viewer sessions on this node.
func (m *Manager) Viewers() int {
	m.vmu.Lock()
	defer m.vmu.Unlock()
	return m.viewerTotal
}

func (m *Manager) serveViewer(ctx context.Context, v *viewerInfo, peer *relay.Peer, sess *moqt.Session) error {
	if err := m.waitStarted(ctx); err != nil {
		sess.CloseWithError(v22.ErrInternal, "server starting")
		return err
	}
	m.addViewer(v.room, 1)
	defer m.addViewer(v.room, -1)
	m.log.Debug("viewer connected", "room", v.room, "identity", v.identity)
	peer.Attach(sess)
	<-sess.Context().Done()
	return nil
}

// syncViewers writes this node's viewer counts to the cluster until ctx
// ends, refreshing them so that readers can tell live counts from stale.
func (m *Manager) syncViewers(ctx context.Context) {
	c := m.cfg.Cluster
	t := time.NewTicker(m.cfg.ViewerInterval)
	defer t.Stop()
	written := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			for room := range written {
				c.SetViewers(cctx, room, 0)
			}
			cancel()
			return
		case <-t.C:
		}
		m.vmu.Lock()
		counts := maps.Clone(m.viewerRooms)
		m.vmu.Unlock()
		for room := range written {
			if counts[room] == 0 && c.SetViewers(ctx, room, 0) == nil {
				delete(written, room)
			}
		}
		for room, n := range counts {
			if c.SetViewers(ctx, room, n) == nil {
				written[room] = true
			}
		}
	}
}

// publishViewers runs while the room is open, sending RoomUpdated when the
// number of viewers (on every node, in a cluster) changes.
func (r *Room) publishViewers(stop <-chan struct{}) {
	t := time.NewTicker(r.m.cfg.ViewerInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		n := r.m.ViewerCount(r.name)
		if c := r.m.cfg.Cluster; c != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			v, err := c.Viewers(ctx, r.name)
			cancel()
			if err != nil {
				continue
			}
			n = v
		}
		r.mu.Lock()
		if !r.closed && r.info.NumViewers != uint32(n) {
			r.info.NumViewers = uint32(n)
			r.publishLocked(&moqitv1.RoomStateUpdate{Update: &moqitv1.RoomStateUpdate_RoomUpdated{RoomUpdated: proto.CloneOf(r.info)}})
		}
		r.mu.Unlock()
	}
}

// missTTL is how long a failed upstream lookup is remembered, so viewers
// retrying before a broadcast starts don't each query the cluster.
const missTTL = time.Second

func (m *Manager) missedRecently(key string) bool {
	m.missMu.Lock()
	defer m.missMu.Unlock()
	at, ok := m.misses[key]
	if ok && time.Since(at) >= missTTL {
		delete(m.misses, key)
		return false
	}
	return ok
}

func (m *Manager) miss(key string) {
	m.missMu.Lock()
	defer m.missMu.Unlock()
	if len(m.misses) > 10000 {
		clear(m.misses)
	}
	m.misses[key] = time.Now()
}

// participantNode returns the node a participant is connected to, from this
// node's view of the room or else from the cluster registry.
func (m *Manager) participantNode(ctx context.Context, room, identity string) (string, error) {
	if r := m.Room(room); r != nil {
		if n := r.nodeOf(identity); n != "" {
			return n, nil
		}
	}
	p, err := m.cfg.Cluster.ParticipantNode(ctx, room, identity)
	if err != nil || p == nil {
		return "", err
	}
	return p.NodeId, nil
}

// roomHost picks another node hosting the room to fetch its room track
// from. Rendezvous hashing makes every node pick the same one.
func (m *Manager) roomHost(ctx context.Context, room string) (string, error) {
	_, ps, err := m.cfg.Cluster.Participants(ctx, room)
	if err != nil {
		return "", err
	}
	var best string
	var bestScore uint64
	for _, p := range ps {
		if p.NodeId == m.cfg.NodeID || p.NodeId == best {
			continue
		}
		h := fnv.New64a()
		h.Write([]byte(room))
		h.Write([]byte{0})
		h.Write([]byte(p.NodeId))
		if s := h.Sum64(); best == "" || s > bestScore {
			best, bestScore = p.NodeId, s
		}
	}
	return best, nil
}
