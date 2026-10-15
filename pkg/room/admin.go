package room

import (
	"context"
	"errors"
	"slices"
	"time"

	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"google.golang.org/protobuf/proto"
)

// Admin operations behind the RoomService API. They work across the
// cluster: reads come from the shared registry, and changes to a
// participant are sent as commands to the node hosting it.

// ErrNotFound is returned for unknown rooms, participants and tracks.
var ErrNotFound = errors.New("not found")

// CreateRoom creates a room ahead of its first participant, or returns the
// existing one. A room nobody joins closes after EmptyTimeout.
func (m *Manager) CreateRoom(ctx context.Context, req *moqitv1.CreateRoomRequest) (*moqitv1.Room, error) {
	if err := m.waitStarted(ctx); err != nil {
		return nil, err
	}
	empty := m.cfg.EmptyTimeout
	if req.EmptyTimeout > 0 {
		empty = time.Duration(req.EmptyTimeout) * time.Second
	}
	settings := &moqitv1.Room{
		Sid: newID("RM_"), Name: req.Name, Metadata: req.Metadata, CreatedAt: time.Now().UnixMilli(),
		EmptyTimeout: uint32(empty / time.Second), DepartureTimeout: req.DepartureTimeout, MaxParticipants: req.MaxParticipants,
		DvrWindow: m.dvrWindow(req.DvrWindow),
	}
	if c := m.cfg.Cluster; c != nil {
		r, created, err := c.CreateRoom(ctx, settings, empty)
		if err != nil {
			return nil, err
		}
		if created {
			m.notify(webhookRoomStarted, r, nil, nil)
		}
		return r, nil
	}
	m.mu.Lock()
	r := m.rooms[req.Name]
	created := r == nil
	if created {
		r = newRoom(m, req.Name, settings)
		m.rooms[req.Name] = r
	}
	m.mu.Unlock()
	if err := r.waitReady(ctx); err != nil {
		return nil, err
	}
	if created {
		r.expireIfUnused(empty)
	}
	return r.roomInfo(), nil
}

// ListRooms returns rooms, all of them when names is empty.
func (m *Manager) ListRooms(ctx context.Context, names []string) ([]*moqitv1.Room, error) {
	var rooms []*moqitv1.Room
	if c := m.cfg.Cluster; c != nil {
		all, err := c.Rooms(ctx)
		if err != nil {
			return nil, err
		}
		rooms = all
	} else {
		m.mu.Lock()
		for _, r := range m.rooms {
			rooms = append(rooms, r.roomInfo())
		}
		m.mu.Unlock()
	}
	if len(names) > 0 {
		rooms = slices.DeleteFunc(rooms, func(r *moqitv1.Room) bool { return !slices.Contains(names, r.Name) })
	}
	slices.SortFunc(rooms, func(a, b *moqitv1.Room) int { return int(a.CreatedAt - b.CreatedAt) })
	return rooms, nil
}

func (m *Manager) roomExists(ctx context.Context, name string) (bool, error) {
	if c := m.cfg.Cluster; c != nil {
		r, err := c.Room(ctx, name)
		return r != nil, err
	}
	return m.Room(name) != nil, nil
}

// command runs an admin command on whichever nodes host the room.
func (m *Manager) command(ctx context.Context, room string, cmd *moqitv1.ClusterCommand) error {
	if c := m.cfg.Cluster; c != nil {
		return c.SendCommand(ctx, room, cmd)
	}
	if r := m.Room(room); r != nil {
		r.handleCommand(cmd)
	}
	return nil
}

// DeleteRoom disconnects everyone and removes the room.
func (m *Manager) DeleteRoom(ctx context.Context, name string) error {
	ok, err := m.roomExists(ctx, name)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if err := m.command(ctx, name, &moqitv1.ClusterCommand{Command: &moqitv1.ClusterCommand_DeleteRoom{DeleteRoom: true}}); err != nil {
		return err
	}
	if c := m.cfg.Cluster; c != nil {
		return c.DeleteRoom(ctx, name)
	}
	return nil
}

// UpdateRoomMetadata sets the room's metadata for every participant.
func (m *Manager) UpdateRoomMetadata(ctx context.Context, name, metadata string) (*moqitv1.Room, error) {
	if c := m.cfg.Cluster; c != nil {
		info, err := c.Room(ctx, name)
		if err != nil {
			return nil, err
		}
		if info == nil {
			return nil, ErrNotFound
		}
		info.Metadata = metadata
		if err := c.UpdateRoom(ctx, info); err != nil {
			return nil, err
		}
		// Nodes ignore their own events, so apply it here directly.
		if r := m.Room(name); r != nil {
			r.mu.Lock()
			r.setMetadataLocked(metadata)
			r.mu.Unlock()
		}
		return info, nil
	}
	r := m.Room(name)
	if r == nil {
		return nil, ErrNotFound
	}
	r.mu.Lock()
	r.setMetadataLocked(metadata)
	r.mu.Unlock()
	return r.roomInfo(), nil
}

// ListParticipants returns every participant of a room, on all nodes.
func (m *Manager) ListParticipants(ctx context.Context, room string) ([]*moqitv1.ParticipantInfo, error) {
	if c := m.cfg.Cluster; c != nil {
		ok, err := m.roomExists(ctx, room)
		if err != nil || !ok {
			return nil, errors.Join(err, ErrNotFound)
		}
		_, ps, err := c.Participants(ctx, room)
		return ps, err
	}
	r := m.Room(room)
	if r == nil {
		return nil, ErrNotFound
	}
	ps := r.Participants()
	slices.SortFunc(ps, func(a, b *moqitv1.ParticipantInfo) int { return int(a.JoinedAt - b.JoinedAt) })
	return ps, nil
}

// GetParticipant returns one participant.
func (m *Manager) GetParticipant(ctx context.Context, room, identity string) (*moqitv1.ParticipantInfo, error) {
	ps, err := m.ListParticipants(ctx, room)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		if p.Identity == identity {
			return p, nil
		}
	}
	return nil, ErrNotFound
}

// RemoveParticipant disconnects a participant.
func (m *Manager) RemoveParticipant(ctx context.Context, room, identity string) error {
	if _, err := m.GetParticipant(ctx, room, identity); err != nil {
		return err
	}
	return m.command(ctx, room, &moqitv1.ClusterCommand{Command: &moqitv1.ClusterCommand_RemoveParticipant{RemoveParticipant: identity}})
}

// UpdateParticipant changes a participant's name, metadata, attributes or
// permission, and returns the result.
func (m *Manager) UpdateParticipant(ctx context.Context, req *moqitv1.UpdateParticipantRequest) (*moqitv1.ParticipantInfo, error) {
	p, err := m.GetParticipant(ctx, req.Room, req.Identity)
	if err != nil {
		return nil, err
	}
	if err := m.command(ctx, req.Room, &moqitv1.ClusterCommand{Command: &moqitv1.ClusterCommand_UpdateParticipant{UpdateParticipant: req}}); err != nil {
		return nil, err
	}
	p = proto.CloneOf(p)
	if p.Attributes == nil {
		p.Attributes = map[string]string{}
	}
	applyUpdate(p, req)
	return p, nil
}

// MuteTrack (un)mutes a participant's track.
func (m *Manager) MuteTrack(ctx context.Context, req *moqitv1.MuteTrackRequest) error {
	p, err := m.GetParticipant(ctx, req.Room, req.Identity)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(p.Tracks, func(t *moqitv1.TrackInfo) bool { return t.Sid == req.TrackSid }) {
		return ErrNotFound
	}
	return m.command(ctx, req.Room, &moqitv1.ClusterCommand{Command: &moqitv1.ClusterCommand_MuteTrack{MuteTrack: req}})
}

// SendData delivers data from the server to participants' inboxes; with no
// destinations, to everyone in the room.
func (m *Manager) SendData(ctx context.Context, req *moqitv1.SendDataRequest) error {
	ps, err := m.ListParticipants(ctx, req.Room)
	if err != nil {
		return err
	}
	msg := &moqitv1.InboxMessage{Message: &moqitv1.InboxMessage_Data{Data: &moqitv1.DataReceived{Topic: req.Topic, Payload: req.Payload}}}
	for _, p := range ps {
		if len(req.DestinationIdentities) > 0 && !slices.Contains(req.DestinationIdentities, p.Identity) {
			continue
		}
		if r := m.Room(req.Room); r != nil {
			if local := r.participant(p.Identity); local != nil {
				local.inbox.send(msg)
				continue
			}
		}
		if c := m.cfg.Cluster; c != nil {
			if err := c.SendDirect(ctx, req.Room, p.Identity, msg); err != nil {
				return err
			}
		}
	}
	return nil
}
