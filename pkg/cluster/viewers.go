package cluster

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

func keyViewers(room string) string { return "moqit:room:" + room + ":viewers" }

// ParticipantNode returns a participant's record, or nil if the room has no
// participant with that identity.
func (c *Cluster) ParticipantNode(ctx context.Context, room, identity string) (*moqitv1.ParticipantInfo, error) {
	b, err := c.rdb.HGet(ctx, keyMembers(room), identity).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p moqitv1.ParticipantInfo
	return &p, proto.Unmarshal(b, &p)
}

// SetViewers records how many viewers of room this node serves. Entries
// carry their write time, and readers ignore stale ones, so a dead node's
// count fades without cleanup.
func (c *Cluster) SetViewers(ctx context.Context, room string, n int) error {
	if n <= 0 {
		return c.rdb.HDel(ctx, keyViewers(room), c.nodeID).Err()
	}
	v := strconv.Itoa(n) + ":" + strconv.FormatInt(c.now().UnixMilli(), 10)
	_, err := c.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, keyViewers(room), c.nodeID, v)
		p.PExpire(ctx, keyViewers(room), c.viewersStale())
		return nil
	})
	return err
}

// viewersStale is how old a node's viewer count may be before it is
// ignored: a few heartbeats.
func (c *Cluster) viewersStale() time.Duration { return 3 * c.ttl }

// Viewers returns the number of viewers of room on every node.
func (c *Cluster) Viewers(ctx context.Context, room string) (int, error) {
	all, err := c.rdb.HGetAll(ctx, keyViewers(room)).Result()
	if err != nil {
		return 0, err
	}
	oldest := c.now().Add(-c.viewersStale()).UnixMilli()
	total := 0
	for _, v := range all {
		n, at, ok := strings.Cut(v, ":")
		if !ok {
			continue
		}
		count, err1 := strconv.Atoi(n)
		ms, err2 := strconv.ParseInt(at, 10, 64)
		if err1 == nil && err2 == nil && ms >= oldest {
			total += count
		}
	}
	return total, nil
}
