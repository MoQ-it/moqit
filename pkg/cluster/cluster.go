// Package cluster is moqit's control plane across nodes, kept in Redis:
// the node registry, each room's participant registry and event stream,
// and direct messages to participants on other nodes. Media between nodes
// travels over MoQ, not Redis.
package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

// NodeInfo describes a node in the registry.
type NodeInfo struct {
	ID string `json:"id"`
	// QUICAddr is where other nodes reach this node over raw QUIC (MoQ).
	QUICAddr string `json:"quicAddr"`
	// PublicURL is the WebTransport URL clients connect to.
	PublicURL string `json:"publicUrl"`
	// CertSHA256 pins the node's certificate for node-to-node sessions.
	CertSHA256   string `json:"certSha256"`
	Region       string `json:"region,omitempty"`
	Participants int    `json:"participants"`
	UpdatedAt    int64  `json:"updatedAt"`
	// Role is RoleEdge for nodes that serve viewers only; empty otherwise.
	Role string `json:"role,omitempty"`
	// Tier is an edge's level in a cascade: tier 1 edges fetch from the
	// nodes hosting tracks, tier n from tier n-1 edges.
	Tier int `json:"tier,omitempty"`
	// Viewers is the number of viewer sessions; MaxViewers caps it (0 =
	// unlimited).
	Viewers    int `json:"viewers,omitempty"`
	MaxViewers int `json:"maxViewers,omitempty"`
}

// RoleEdge marks a node that serves read-only viewers and hosts no
// participants.
const RoleEdge = "edge"

// Cluster is one node's handle on the shared state.
type Cluster struct {
	rdb    redis.UniversalClient
	nodeID string
	ttl    time.Duration
	log    *slog.Logger
	now    func() time.Time

	dvrMu    sync.Mutex
	dvrCache map[string]dvrCached
}

// New returns a Cluster for nodeID. Node registrations expire after ttl
// without a heartbeat (default 10s).
func New(rdb redis.UniversalClient, nodeID string, ttl time.Duration, log *slog.Logger) *Cluster {
	if ttl == 0 {
		ttl = 10 * time.Second
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Cluster{rdb: rdb, nodeID: nodeID, ttl: ttl, log: log, now: time.Now}
}

func (c *Cluster) NodeID() string { return c.nodeID }

const keyNodes = "moqit:nodes"

func keyNode(id string) string        { return "moqit:node:" + id }
func keyRoom(room string) string      { return "moqit:room:" + room }
func keyMembers(room string) string   { return "moqit:room:" + room + ":p" }
func keyMemberSid(room string) string { return "moqit:room:" + room + ":sid" }
func keyEvents(room string) string    { return "moqit:room:" + room + ":events" }
func chanDirect(room string) string   { return "moqit:room:" + room + ":direct" }

// Heartbeat registers or refreshes this node.
func (c *Cluster) Heartbeat(ctx context.Context, info NodeInfo) error {
	info.ID = c.nodeID
	info.UpdatedAt = time.Now().UnixMilli()
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	_, err = c.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, keyNode(c.nodeID), b, c.ttl)
		p.SAdd(ctx, keyNodes, c.nodeID)
		return nil
	})
	return err
}

// RunHeartbeat refreshes the registration every ttl/3 until ctx ends, then
// unregisters.
func (c *Cluster) RunHeartbeat(ctx context.Context, info func() NodeInfo) {
	t := time.NewTicker(c.ttl / 3)
	defer t.Stop()
	for {
		if err := c.Heartbeat(ctx, info()); err != nil && ctx.Err() == nil {
			c.log.Warn("cluster heartbeat failed", "err", err)
		}
		select {
		case <-ctx.Done():
			c.Unregister(context.Background())
			return
		case <-t.C:
		}
	}
}

func (c *Cluster) Unregister(ctx context.Context) error {
	_, err := c.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Del(ctx, keyNode(c.nodeID))
		p.SRem(ctx, keyNodes, c.nodeID)
		return nil
	})
	return err
}

// Node returns a live node, or nil if it is unknown or its registration
// has expired.
func (c *Cluster) Node(ctx context.Context, id string) (*NodeInfo, error) {
	b, err := c.rdb.Get(ctx, keyNode(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var n NodeInfo
	return &n, json.Unmarshal(b, &n)
}

// Nodes returns every live node, pruning expired ones from the index.
func (c *Cluster) Nodes(ctx context.Context) ([]NodeInfo, error) {
	ids, err := c.rdb.SMembers(ctx, keyNodes).Result()
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = keyNode(id)
	}
	vals, err := c.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	var nodes []NodeInfo
	for i, v := range vals {
		s, ok := v.(string)
		if !ok {
			c.rdb.SRem(ctx, keyNodes, ids[i])
			continue
		}
		var n NodeInfo
		if json.Unmarshal([]byte(s), &n) == nil {
			nodes = append(nodes, n)
		}
	}
	return nodes, nil
}

// EnsureRoom stores info as the room's shared record unless one exists,
// and returns the stored one, so every node agrees on sid, creation time
// and settings. created reports whether this call made the record.
func (c *Cluster) EnsureRoom(ctx context.Context, info *moqitv1.Room) (room *moqitv1.Room, created bool, err error) {
	return c.createRoom(ctx, info, 0)
}

// CreateRoom records a room before anyone joins. If nobody joins within
// emptyTimeout the record expires.
func (c *Cluster) CreateRoom(ctx context.Context, info *moqitv1.Room, emptyTimeout time.Duration) (*moqitv1.Room, bool, error) {
	return c.createRoom(ctx, info, emptyTimeout)
}

func (c *Cluster) createRoom(ctx context.Context, info *moqitv1.Room, ttl time.Duration) (*moqitv1.Room, bool, error) {
	b, err := proto.Marshal(info)
	if err != nil {
		return nil, false, err
	}
	created, err := c.rdb.SetNX(ctx, keyRoom(info.Name), b, ttl).Result()
	if err != nil {
		return nil, false, err
	}
	if ttl == 0 {
		c.rdb.Persist(ctx, keyRoom(info.Name))
	}
	c.rdb.SAdd(ctx, keyRooms, info.Name)
	r, err := c.Room(ctx, info.Name)
	if err == nil && r == nil {
		err = errors.New("cluster: room record vanished")
	}
	return r, created, err
}

const keyRooms = "moqit:rooms"

// Room returns a room's shared record, or nil if there is none.
func (c *Cluster) Room(ctx context.Context, name string) (*moqitv1.Room, error) {
	b, err := c.rdb.Get(ctx, keyRoom(name)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r moqitv1.Room
	if err := proto.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	n, _ := c.rdb.HLen(ctx, keyMembers(name)).Result()
	r.NumParticipants = uint32(n)
	if v, err := c.Viewers(ctx, name); err == nil {
		r.NumViewers = uint32(v)
	}
	return &r, nil
}

// Rooms returns every room's record, pruning expired ones from the index.
func (c *Cluster) Rooms(ctx context.Context) ([]*moqitv1.Room, error) {
	names, err := c.rdb.SMembers(ctx, keyRooms).Result()
	if err != nil {
		return nil, err
	}
	var out []*moqitv1.Room
	for _, n := range names {
		r, err := c.Room(ctx, n)
		if err != nil {
			return nil, err
		}
		if r == nil {
			c.rdb.SRem(ctx, keyRooms, n)
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// UpdateRoom replaces a room's record and tells its nodes.
func (c *Cluster) UpdateRoom(ctx context.Context, info *moqitv1.Room) error {
	b, err := proto.Marshal(info)
	if err != nil {
		return err
	}
	_, err = c.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, keyRoom(info.Name), b, redis.KeepTTL)
		return c.addEvent(ctx, p, info.Name, &moqitv1.ClusterRoomEvent{Event: &moqitv1.ClusterRoomEvent_RoomUpdated{RoomUpdated: info}})
	})
	return err
}

// DeleteRoom removes a room's shared state.
func (c *Cluster) DeleteRoom(ctx context.Context, name string) error {
	_, err := c.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Del(ctx, keyRoom(name), keyMembers(name), keyMemberSid(name), keyEvents(name))
		p.SRem(ctx, keyRooms, name)
		return nil
	})
	return err
}

// Once reports true to exactly one caller per key within ttl, e.g. to send
// a cluster-wide webhook once.
func (c *Cluster) Once(ctx context.Context, key string, ttl time.Duration) bool {
	ok, err := c.rdb.SetNX(ctx, "moqit:once:"+key, c.nodeID, ttl).Result()
	return err == nil && ok
}

func chanCommands(room string) string { return "moqit:room:" + room + ":cmd" }

// SendCommand delivers an admin command to every node hosting the room.
func (c *Cluster) SendCommand(ctx context.Context, room string, cmd *moqitv1.ClusterCommand) error {
	b, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}
	return c.rdb.Publish(ctx, chanCommands(room), b).Err()
}

// ReceiveCommands calls handle for each admin command for the room until
// ctx ends. It returns once the subscription is active.
func (c *Cluster) ReceiveCommands(ctx context.Context, room string, handle func(*moqitv1.ClusterCommand)) error {
	sub := c.rdb.Subscribe(ctx, chanCommands(room))
	if _, err := sub.Receive(ctx); err != nil {
		sub.Close()
		return fmt.Errorf("cluster: subscribing to commands: %w", err)
	}
	go func() {
		defer sub.Close()
		ch := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case m, ok := <-ch:
				if !ok {
					return
				}
				var cmd moqitv1.ClusterCommand
				if proto.Unmarshal([]byte(m.Payload), &cmd) == nil {
					handle(&cmd)
				}
			}
		}
	}()
	return nil
}

// eventsMaxLen bounds each room's event stream; nodes resynchronise from the
// participant registry if they ever fall further behind.
const eventsMaxLen = 10000

func (c *Cluster) addEvent(ctx context.Context, p redis.Pipeliner, room string, ev *moqitv1.ClusterRoomEvent) error {
	ev.NodeId = c.nodeID
	b, err := proto.Marshal(ev)
	if err != nil {
		return err
	}
	p.XAdd(ctx, &redis.XAddArgs{Stream: keyEvents(room), MaxLen: eventsMaxLen, Approx: true, Values: map[string]any{"e": b}})
	return nil
}

// PutParticipant records a participant joining or changing and tells the
// room's other nodes.
func (c *Cluster) PutParticipant(ctx context.Context, room string, info *moqitv1.ParticipantInfo) error {
	b, err := proto.Marshal(info)
	if err != nil {
		return err
	}
	_, err = c.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, keyMembers(room), info.Identity, b)
		p.HSet(ctx, keyMemberSid(room), info.Identity, info.Sid)
		for _, k := range []string{keyRoom(room), keyMembers(room), keyMemberSid(room), keyEvents(room)} {
			p.Persist(ctx, k)
		}
		return c.addEvent(ctx, p, room, &moqitv1.ClusterRoomEvent{Event: &moqitv1.ClusterRoomEvent_ParticipantPut{ParticipantPut: info}})
	})
	return err
}

// removeScript deletes a participant only if its sid still matches, so a
// stale removal never deletes a newer session of the same identity, and
// only one caller wins when several nodes reap the same participant. When
// the room becomes empty its keys expire after the departure timeout.
var removeScript = redis.NewScript(`
if redis.call('HGET', KEYS[2], ARGV[1]) ~= ARGV[2] then return 0 end
redis.call('HDEL', KEYS[1], ARGV[1])
redis.call('HDEL', KEYS[2], ARGV[1])
redis.call('XADD', KEYS[3], 'MAXLEN', '~', ARGV[5], '*', 'e', ARGV[3])
if redis.call('HLEN', KEYS[1]) == 0 then
  for i = 1, 4 do redis.call('PEXPIRE', KEYS[i], ARGV[4]) end
end
return 1
`)

// RemoveParticipant removes a participant session. It reports whether this
// call removed it.
func (c *Cluster) RemoveParticipant(ctx context.Context, room, identity, sid string, reason moqitv1.DisconnectReason, departure time.Duration) (bool, error) {
	ev, err := proto.Marshal(&moqitv1.ClusterRoomEvent{NodeId: c.nodeID, Event: &moqitv1.ClusterRoomEvent_ParticipantLeft{
		ParticipantLeft: &moqitv1.ParticipantLeft{Identity: identity, Sid: sid, Reason: reason},
	}})
	if err != nil {
		return false, err
	}
	if departure < time.Millisecond {
		departure = time.Millisecond
	}
	n, err := removeScript.Run(ctx, c.rdb, []string{keyMembers(room), keyMemberSid(room), keyEvents(room), keyRoom(room)},
		identity, sid, ev, departure.Milliseconds(), eventsMaxLen).Int()
	return n == 1, err
}

// Participants returns the room's participants on every node, and the event
// stream position they reflect; Watch from that position.
func (c *Cluster) Participants(ctx context.Context, room string) (string, []*moqitv1.ParticipantInfo, error) {
	last := "0-0"
	msgs, err := c.rdb.XRevRangeN(ctx, keyEvents(room), "+", "-", 1).Result()
	if err != nil {
		return "", nil, err
	}
	if len(msgs) > 0 {
		last = msgs[0].ID
	}
	all, err := c.rdb.HGetAll(ctx, keyMembers(room)).Result()
	if err != nil {
		return "", nil, err
	}
	var out []*moqitv1.ParticipantInfo
	for _, v := range all {
		var p moqitv1.ParticipantInfo
		if proto.Unmarshal([]byte(v), &p) == nil {
			out = append(out, &p)
		}
	}
	slices.SortFunc(out, func(a, b *moqitv1.ParticipantInfo) int { return int(a.JoinedAt - b.JoinedAt) })
	return last, out, nil
}

// Watch delivers the room's events after position from, including this
// node's own (callers skip those by NodeId), until ctx ends.
func (c *Cluster) Watch(ctx context.Context, room, from string, apply func(*moqitv1.ClusterRoomEvent)) {
	for ctx.Err() == nil {
		streams, err := c.rdb.XRead(ctx, &redis.XReadArgs{Streams: []string{keyEvents(room), from}, Block: 2 * time.Second, Count: 100}).Result()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			if ctx.Err() == nil {
				c.log.Warn("cluster watch", "room", room, "err", err)
				time.Sleep(500 * time.Millisecond)
			}
			continue
		}
		for _, s := range streams {
			for _, m := range s.Messages {
				from = m.ID
				raw, _ := m.Values["e"].(string)
				var ev moqitv1.ClusterRoomEvent
				if err := proto.Unmarshal([]byte(raw), &ev); err != nil {
					c.log.Warn("cluster watch: bad event", "room", room, "id", m.ID)
					continue
				}
				apply(&ev)
			}
		}
	}
}

// SendDirect delivers an inbox message to a participant on whichever node
// hosts it.
func (c *Cluster) SendDirect(ctx context.Context, room, identity string, msg *moqitv1.InboxMessage) error {
	b, err := proto.Marshal(&moqitv1.ClusterDirect{Identity: identity, Message: msg})
	if err != nil {
		return err
	}
	return c.rdb.Publish(ctx, chanDirect(room), b).Err()
}

// ReceiveDirect calls deliver for each direct message in the room until ctx
// ends. It returns once the subscription is active.
func (c *Cluster) ReceiveDirect(ctx context.Context, room string, deliver func(identity string, msg *moqitv1.InboxMessage)) error {
	sub := c.rdb.Subscribe(ctx, chanDirect(room))
	if _, err := sub.Receive(ctx); err != nil {
		sub.Close()
		return fmt.Errorf("cluster: subscribing to direct messages: %w", err)
	}
	go func() {
		defer sub.Close()
		ch := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case m, ok := <-ch:
				if !ok {
					return
				}
				var d moqitv1.ClusterDirect
				if proto.Unmarshal([]byte(m.Payload), &d) == nil {
					deliver(d.Identity, d.Message)
				}
			}
		}
	}()
	return nil
}

// ReapDead removes the room's participants whose node is no longer alive,
// reporting each removal this call won.
func (c *Cluster) ReapDead(ctx context.Context, room string, alive func(nodeID string) bool, departure time.Duration) ([]*moqitv1.ParticipantInfo, error) {
	_, ps, err := c.Participants(ctx, room)
	if err != nil {
		return nil, err
	}
	var reaped []*moqitv1.ParticipantInfo
	for _, p := range ps {
		if alive(p.NodeId) {
			continue
		}
		ok, err := c.RemoveParticipant(ctx, room, p.Identity, p.Sid, moqitv1.DisconnectReason_DISCONNECT_REASON_CONNECTION_LOST, departure)
		if err != nil {
			return reaped, err
		}
		if ok {
			reaped = append(reaped, p)
		}
	}
	return reaped, nil
}
