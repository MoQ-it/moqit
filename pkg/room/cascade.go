package room

import (
	"context"
	"hash/fnv"
	"time"

	"github.com/MoQ-it/moqit/pkg/cluster"
	"github.com/MoQ-it/moqt"
)

// parentTimeout bounds reaching a parent edge before fetching from the
// node hosting the track instead; a dead parent stays registered until its
// heartbeat expires.
const parentTimeout = 2 * time.Second

// fromParent fetches through an edge one tier up, for edges of tier 2 and
// above. It returns nil when there is no live parent.
func (m *Manager) fromParent(ctx context.Context, room string) *moqt.Session {
	if !m.cfg.Edge || m.cfg.Tier < 2 {
		return nil
	}
	nodes, err := m.cfg.Cluster.Nodes(ctx)
	if err != nil {
		return nil
	}
	parent := pickParent(nodes, m.cfg.Tier-1, m.cfg.Node.Region, room)
	if parent == "" {
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, parentTimeout)
	defer cancel()
	sess, err := m.peer(pctx, parent)
	if err != nil {
		m.log.Warn("parent edge unavailable; fetching from the origin", "parent", parent, "err", err)
		return nil
	}
	return sess
}

// pickParent picks an edge of the given tier for a room, in the region if
// there is one there. Rendezvous hashing sends every child's requests for a
// room to the same parent, which then fetches each track once.
func pickParent(nodes []cluster.NodeInfo, tier int, region, room string) string {
	var best string
	var bestScore uint64
	bestLocal := false
	for _, n := range nodes {
		if n.Role != cluster.RoleEdge || max(n.Tier, 1) != tier {
			continue
		}
		local := region != "" && n.Region == region
		h := fnv.New64a()
		h.Write([]byte(room))
		h.Write([]byte{0})
		h.Write([]byte(n.ID))
		s := h.Sum64()
		if best == "" || (local && !bestLocal) || (local == bestLocal && s > bestScore) {
			best, bestScore, bestLocal = n.ID, s, local
		}
	}
	return best
}
