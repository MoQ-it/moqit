package room

import (
	"slices"
	"sync"
	"time"

	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	v22 "github.com/MoQ-it/moqt/wire/v22"
)

// Active speaker detection. The relay passes every audio Object it
// forwards through onObject, which reads the LOC Audio Level property
// (draft-ietf-moq-loc: -dBov, 0 loudest .. 127 silent) without touching the
// payload, so it keeps working with end-to-end encrypted media. Each room
// publishes the loudest participants on its room track when they change.

const (
	locAudioLevel     = 0x0C
	speakerThreshold  = 0.3 // level (0..1) above which a participant counts as speaking
	speakerHold       = 800 * time.Millisecond
	speakersInterval  = 250 * time.Millisecond
	maxActiveSpeakers = 5
)

// levelOf maps -dBov to 0..1 on a -60..0 dBFS scale, as the SDKs do.
func levelOf(dbov uint64) float32 {
	return max(0, min(1, float32(60-int(min(dbov, 127)))/60))
}

type speakerLevel struct {
	level float32 // smoothed
	at    time.Time
}

// speakers tracks a room's audio levels; its own lock, since the relay
// calls in on every audio Object.
type speakers struct {
	mu     sync.Mutex
	levels map[string]*speakerLevel
	last   []string // identities in the last published set
}

func (s *speakers) observe(identity string, level float32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.levels == nil {
		s.levels = map[string]*speakerLevel{}
	}
	l := s.levels[identity]
	if l == nil {
		l = &speakerLevel{}
		s.levels[identity] = l
	}
	// Fast attack, slow release, per 20 ms frame.
	l.level = max(level, l.level*0.9)
	l.at = time.Now()
}

// current returns the speakers above the threshold, loudest first, and
// whether the set differs from the last one returned.
func (s *speakers) current() ([]*moqitv1.SpeakerInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*moqitv1.SpeakerInfo
	for id, l := range s.levels {
		if time.Since(l.at) > speakerHold {
			delete(s.levels, id)
			continue
		}
		if l.level >= speakerThreshold {
			out = append(out, &moqitv1.SpeakerInfo{Identity: id, Level: l.level})
		}
	}
	slices.SortFunc(out, func(a, b *moqitv1.SpeakerInfo) int {
		if a.Level > b.Level {
			return -1
		}
		if a.Level < b.Level {
			return 1
		}
		return 0
	})
	if len(out) > maxActiveSpeakers {
		out = out[:maxActiveSpeakers]
	}
	ids := make([]string, len(out))
	for i, sp := range out {
		ids[i] = sp.Identity
	}
	changed := !slices.Equal(ids, s.last)
	s.last = ids
	return out, changed
}

// onObject is the relay's ObjectHook.
func (m *Manager) onObject(track v22.FullTrackName, _ uint64, o *v22.Object) {
	if track.Name != TrackMic {
		return
	}
	kind, roomName, identity := parseNamespace(track.Namespace)
	if kind != nsParticipant {
		return
	}
	kv, ok := o.Properties.Lookup(locAudioLevel)
	if !ok {
		return
	}
	if r := m.Room(roomName); r != nil {
		r.speakers.observe(identity, levelOf(kv.Int))
	}
}

// publishSpeakers runs while the room is open, sending SpeakersChanged when
// the set of speakers changes.
func (r *Room) publishSpeakers(stop <-chan struct{}) {
	t := time.NewTicker(speakersInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		sp, changed := r.speakers.current()
		if !changed {
			continue
		}
		r.mu.Lock()
		if !r.closed {
			r.publishLocked(&moqitv1.RoomStateUpdate{Update: &moqitv1.RoomStateUpdate_SpeakersChanged{SpeakersChanged: &moqitv1.SpeakersChanged{Speakers: sp}}})
		}
		r.mu.Unlock()
	}
}
