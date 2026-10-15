package room

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/MoQ-it/moqit/pkg/auth"
	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"github.com/MoQ-it/moqt"
	v22 "github.com/MoQ-it/moqt/wire/v22"
	"google.golang.org/protobuf/proto"
)

// Participant is one participant on this node. Its info is guarded by the
// room's mutex.
type Participant struct {
	room *Room
	sess *moqt.Session
	info *moqitv1.ParticipantInfo

	trackSids  map[string]string // track name -> sid, stable across SetTracks
	inbox      inbox
	uplink     uplink
	signalOnce sync.Once
}

// Info returns a copy of the participant's info.
func (p *Participant) Info() *moqitv1.ParticipantInfo {
	p.room.mu.Lock()
	defer p.room.mu.Unlock()
	return proto.CloneOf(p.info)
}

func (p *Participant) permission() *moqitv1.ParticipantPermission {
	p.room.mu.Lock()
	defer p.room.mu.Unlock()
	return p.info.Permission
}

// readSignals subscribes to the participant's signal track once it has
// published its namespace, and handles each ClientSignal.
func (p *Participant) readSignals(srv *moqt.Session) {
	p.signalOnce.Do(func() {
		ctx, cancel := context.WithTimeout(p.sess.Context(), 5*time.Second)
		sub, err := srv.Subscribe(ctx, v22.FullTrackName{Namespace: ParticipantNamespace(p.room.name, p.info.Identity), Name: TrackSignal}, nil)
		cancel()
		if err != nil {
			p.room.m.log.Warn("participant signal track unavailable", "identity", p.info.Identity, "err", err)
			return
		}
		p.room.updateParticipant(p, func(info *moqitv1.ParticipantInfo) {
			info.State = moqitv1.ParticipantState_PARTICIPANT_STATE_ACTIVE
		})
		go func() {
			<-p.sess.Context().Done()
			sub.Unsubscribe()
		}()
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
				var cs moqitv1.ClientSignal
				if err := proto.Unmarshal(o.Payload, &cs); err != nil {
					p.respond(0, moqitv1.RequestResult_REQUEST_RESULT_INVALID, "malformed signal")
					continue
				}
				p.handleSignal(&cs)
			}
		}
	})
}

func (p *Participant) respond(id uint64, result moqitv1.RequestResult, msg string) {
	if id == 0 && result == moqitv1.RequestResult_REQUEST_RESULT_OK {
		return
	}
	p.inbox.send(&moqitv1.InboxMessage{Message: &moqitv1.InboxMessage_Response{Response: &moqitv1.RequestResponse{
		RequestId: id, Result: result, Message: msg,
	}}})
}

func (p *Participant) handleSignal(cs *moqitv1.ClientSignal) {
	switch s := cs.Signal.(type) {
	case *moqitv1.ClientSignal_UpdateParticipant:
		if !p.permission().CanUpdateMetadata {
			p.respond(cs.RequestId, moqitv1.RequestResult_REQUEST_RESULT_NOT_ALLOWED, "no permission to update metadata")
			return
		}
		u := s.UpdateParticipant
		p.room.updateParticipant(p, func(info *moqitv1.ParticipantInfo) {
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
		})
		p.respond(cs.RequestId, moqitv1.RequestResult_REQUEST_RESULT_OK, "")

	case *moqitv1.ClientSignal_SetTracks:
		if err := p.setTracks(s.SetTracks.Tracks); err != nil {
			p.respond(cs.RequestId, moqitv1.RequestResult_REQUEST_RESULT_NOT_ALLOWED, err.Error())
			return
		}
		p.respond(cs.RequestId, moqitv1.RequestResult_REQUEST_RESULT_OK, "")

	case *moqitv1.ClientSignal_SendData:
		if !p.permission().CanPublishData {
			p.respond(cs.RequestId, moqitv1.RequestResult_REQUEST_RESULT_NOT_ALLOWED, "no permission to send data")
			return
		}
		msg := &moqitv1.InboxMessage{Message: &moqitv1.InboxMessage_Data{Data: &moqitv1.DataReceived{
			FromIdentity: p.info.Identity, Topic: s.SendData.Topic, Payload: s.SendData.Payload,
		}}}
		var missing []string
		for _, id := range s.SendData.DestinationIdentities {
			if !p.room.sendTo(id, msg) {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			p.respond(cs.RequestId, moqitv1.RequestResult_REQUEST_RESULT_NOT_FOUND, fmt.Sprintf("not in room: %v", missing))
			return
		}
		p.respond(cs.RequestId, moqitv1.RequestResult_REQUEST_RESULT_OK, "")

	default:
		p.respond(cs.RequestId, moqitv1.RequestResult_REQUEST_RESULT_INVALID, "unknown signal")
	}
}

// setTracks validates and records the participant's published tracks.
func (p *Participant) setTracks(tracks []*moqitv1.TrackInfo) error {
	perm := p.permission()
	seen := map[string]bool{}
	for _, t := range tracks {
		if t.Name == "" || seen[t.Name] {
			return fmt.Errorf("track names must be unique and non-empty")
		}
		seen[t.Name] = true
		if !auth.CanPublishSource(perm, t.Source) {
			return fmt.Errorf("may not publish %v", t.Source)
		}
		if len(t.Renditions) == 0 {
			return fmt.Errorf("track %q has no renditions", t.Name)
		}
		if t.Channels > 2 {
			return fmt.Errorf("track %q has %d channels; at most 2 are supported", t.Name, t.Channels)
		}
		for _, rd := range t.Renditions {
			class, src := classifyTrack(rd.TrackName)
			if class != classMedia || src != t.Source {
				return fmt.Errorf("rendition track %q does not match source %v", rd.TrackName, t.Source)
			}
		}
	}
	var published, unpublished []*moqitv1.TrackInfo
	var roomInfo *moqitv1.Room
	var me *moqitv1.ParticipantInfo
	p.room.updateParticipant(p, func(info *moqitv1.ParticipantInfo) {
		before := map[string]*moqitv1.TrackInfo{}
		for _, t := range info.Tracks {
			before[t.Sid] = t
		}
		info.Tracks = nil
		for _, t := range tracks {
			t := proto.CloneOf(t)
			if p.trackSids[t.Name] == "" {
				p.trackSids[t.Name] = newID("TR_")
			}
			t.Sid = p.trackSids[t.Name]
			info.Tracks = append(info.Tracks, t)
			if before[t.Sid] == nil {
				published = append(published, proto.CloneOf(t))
			}
			delete(before, t.Sid)
		}
		for _, t := range before {
			unpublished = append(unpublished, t)
		}
		roomInfo, me = proto.CloneOf(p.room.info), proto.CloneOf(info)
	})
	for _, t := range published {
		p.room.m.notify(webhookTrackPublished, roomInfo, me, t)
	}
	for _, t := range unpublished {
		p.room.m.notify(webhookTrackUnpublished, roomInfo, me, t)
	}
	p.room.m.syncRecordings(p.room, me.Identity)
	return nil
}

// inbox writes the participant's inbox track: one Group per message.
type inbox struct {
	mu     sync.Mutex
	w      *moqt.TrackWriter
	group  uint64
	closed bool
}

func (in *inbox) attach(req *moqt.SubscribeRequest) {
	w, err := req.Accept(moqt.AcceptOptions{Properties: v22.KeyValues{{Type: v22.PropDefaultPublisherPriority, Int: 8}}})
	if err != nil {
		return
	}
	in.mu.Lock()
	if in.closed {
		in.mu.Unlock()
		w.Close(v22.DoneTrackEnded, "participant left")
		return
	}
	in.w = w
	in.mu.Unlock()
	go func() {
		<-w.Context().Done()
		in.mu.Lock()
		if in.w == w {
			in.w = nil
		}
		in.mu.Unlock()
	}()
}

// close ends the inbox track when the participant leaves. The relay keeps
// an idle track briefly for quick resubscription; ending it makes the
// relay drop it, so the same identity rejoining (a page reload) gets its
// new participant's inbox rather than this one's.
func (in *inbox) close() {
	in.mu.Lock()
	w := in.w
	in.w, in.closed = nil, true
	in.mu.Unlock()
	if w != nil {
		w.Close(v22.DoneTrackEnded, "participant left")
	}
}

func (in *inbox) send(m *moqitv1.InboxMessage) {
	b, err := proto.Marshal(m)
	if err != nil {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.w == nil {
		return // not subscribed: the message is dropped
	}
	sg := in.w.OpenSubgroup(in.group, 0, moqt.SubgroupOptions{EndOfGroup: true})
	in.group++
	if err := sg.WriteObject(&v22.Object{ID: 0, Payload: b}); err == nil {
		sg.Close()
	}
}
