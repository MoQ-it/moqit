package cluster

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const keyDvrEpoch = "moqit:dvr:epoch"

func keyDvrRoster(room, identity string) string { return "moqit:dvr:" + room + ":" + identity }

// NextDvrEpoch allocates a recording epoch unique across the cluster.
func (c *Cluster) NextDvrEpoch(ctx context.Context) (uint32, error) {
	n, err := c.rdb.Incr(ctx, keyDvrEpoch).Result()
	if err != nil {
		return 0, err
	}
	if n <= 0 || n >= 1<<32 {
		return 0, fmt.Errorf("cluster: dvr epoch %d out of range", n)
	}
	return uint32(n), nil
}

// DvrEpoch is where one recording of a participant's track is stored.
type DvrEpoch struct {
	Epoch uint32
	Node  string
	Start time.Time
}

// PutDvrEpoch records that this node stores an epoch of a participant in
// a room, for ttl (the room's history window, plus slack).
func (c *Cluster) PutDvrEpoch(ctx context.Context, room, identity string, epoch uint32, start time.Time, ttl time.Duration) error {
	key := keyDvrRoster(room, identity)
	p := c.rdb.TxPipeline()
	p.HSet(ctx, key, strconv.FormatUint(uint64(epoch), 10), c.nodeID+"|"+strconv.FormatInt(start.Unix(), 10))
	p.Expire(ctx, key, ttl)
	_, err := p.Exec(ctx)
	return err
}

// DvrEpochs lists the stored epochs of a participant in a room.
func (c *Cluster) DvrEpochs(ctx context.Context, room, identity string) ([]DvrEpoch, error) {
	m, err := c.rdb.HGetAll(ctx, keyDvrRoster(room, identity)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]DvrEpoch, 0, len(m))
	for k, v := range m {
		e, err1 := strconv.ParseUint(k, 10, 32)
		node, start, ok := strings.Cut(v, "|")
		sec, err2 := strconv.ParseInt(start, 10, 64)
		if err1 != nil || err2 != nil || !ok {
			continue
		}
		out = append(out, DvrEpoch{Epoch: uint32(e), Node: node, Start: time.Unix(sec, 0)})
	}
	return out, nil
}

type dvrCached struct {
	at  time.Time
	eps []DvrEpoch
}

// dvrCacheFor is how long DvrEpochsCached reuses a roster read.
const dvrCacheFor = 2 * time.Second

// DvrEpochsCached is DvrEpochs reusing a recent read, for routing every
// FETCH; it returns nil on errors.
func (c *Cluster) DvrEpochsCached(ctx context.Context, room, identity string) []DvrEpoch {
	key := keyDvrRoster(room, identity)
	c.dvrMu.Lock()
	if e, ok := c.dvrCache[key]; ok && c.now().Sub(e.at) < dvrCacheFor {
		c.dvrMu.Unlock()
		return e.eps
	}
	c.dvrMu.Unlock()
	eps, err := c.DvrEpochs(ctx, room, identity)
	if err != nil {
		c.log.Warn("cluster: reading dvr roster", "room", room, "identity", identity, "err", err)
		return nil
	}
	c.dvrMu.Lock()
	defer c.dvrMu.Unlock()
	if c.dvrCache == nil || len(c.dvrCache) > 10000 {
		c.dvrCache = map[string]dvrCached{}
	}
	c.dvrCache[key] = dvrCached{at: c.now(), eps: eps}
	return eps
}
