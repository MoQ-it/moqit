// Package dvr records media tracks to disk and serves them back by MoQ
// Group, so viewers can rewind a room.
//
// Each recording of one track is an epoch. Archive Group IDs are
// epoch<<32 | publisher Group, so they never collide across a
// participant's sessions and identify the epoch (and the node holding it).
package dvr

import (
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Meta describes one epoch; it is stored in each of its segments.
type Meta struct {
	Epoch    uint32 `json:"epoch"`
	Room     string `json:"room"` // room SID
	Name     string `json:"name"` // room name
	Identity string `json:"identity"`
	Track    string `json:"track"`
	Codec    string `json:"codec,omitempty"`
	Width    uint32 `json:"width,omitempty"`
	Height   uint32 `json:"height,omitempty"`
	Channels uint32 `json:"channels,omitempty"`
	// Window is how long the room keeps history, in seconds.
	Window uint32 `json:"window"`
}

func (m Meta) key() Key { return Key{Room: m.Room, Identity: m.Identity, Track: m.Track} }

// Key names a recorded track.
type Key struct{ Room, Identity, Track string }

// ArchiveGroup returns the archive Group ID of a publisher Group.
func ArchiveGroup(epoch uint32, group uint64) uint64 { return uint64(epoch)<<32 | group }

// SplitGroup splits an archive Group ID.
func SplitGroup(g uint64) (epoch uint32, group uint64) { return uint32(g >> 32), g & (1<<32 - 1) }

// Config configures a Store.
type Config struct {
	Dir string
	// MaxBytes caps the store; the oldest segments go first. 0 = no cap.
	MaxBytes int64
	// SegmentDuration is how much media goes in one file (default 60s).
	SegmentDuration time.Duration
	Logger          *slog.Logger
	Now             func() time.Time
}

// Store holds recorded tracks on disk with an in-memory index.
type Store struct {
	cfg Config
	log *slog.Logger

	mu        sync.Mutex
	epochs    map[Key][]*epoch // ascending by epoch
	byID      map[uint32]*epoch
	total     int64
	nextEpoch uint32
}

type epoch struct {
	meta Meta
	segs []*segment
	cps  []checkpoint // ascending by group
	live bool         // a Writer is appending
}

type segment struct {
	path       string
	dataOff    int64 // where records start
	size       int64 // committed bytes
	start, end int64 // server µs of the first and last record
	firstGroup uint64
	lastGroup  uint64
	lastObject uint64
	records    int
}

func (s *segment) add(r *record) {
	if s.records == 0 {
		s.start, s.firstGroup = r.at, r.group
	}
	s.records++
	s.end, s.lastGroup, s.lastObject = r.at, r.group, r.object
}

// checkpoint marks where a Group starts: one per second at most, so
// seeking costs a bounded index.
type checkpoint struct {
	group uint64
	at    int64
	loc   int64
	seg   *segment
	off   int64
}

const checkpointEvery = int64(time.Second / time.Microsecond)

// groupStart is the first record of a Group found while scanning.
type groupStart struct {
	r   *record
	off int64
}

func (e *epoch) checkpoint(seg *segment, r *record, off int64) {
	if r.object != 0 {
		return
	}
	if n := len(e.cps); n > 0 && (r.group <= e.cps[n-1].group || r.at-e.cps[n-1].at < checkpointEvery) {
		return
	}
	e.cps = append(e.cps, checkpoint{group: r.group, at: r.at, loc: r.loc(), seg: seg, off: off})
}

func (e *epoch) first() int64 {
	if len(e.segs) == 0 {
		return 0
	}
	return e.segs[0].start
}

func (e *epoch) last() int64 {
	if len(e.segs) == 0 {
		return 0
	}
	return e.segs[len(e.segs)-1].end
}

// Open loads the store in cfg.Dir, rebuilding its index from the segments.
func Open(cfg Config) (*Store, error) {
	if cfg.SegmentDuration == 0 {
		cfg.SegmentDuration = time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{cfg: cfg, log: cfg.Logger, epochs: map[Key][]*epoch{}, byID: map[uint32]*epoch{}, nextEpoch: 1}
	starts := map[*segment][]groupStart{}
	err := filepath.WalkDir(cfg.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".seg") {
			return err
		}
		m, seg, gs, err := scanSegment(path)
		if err != nil {
			s.log.Warn("dvr: skipping unreadable segment", "path", path, "err", err)
			return nil
		}
		ep := s.epochLocked(m)
		ep.segs = append(ep.segs, seg)
		starts[seg] = gs
		s.total += seg.size
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, ep := range s.byID {
		sort.Slice(ep.segs, func(i, j int) bool { return ep.segs[i].start < ep.segs[j].start })
		for _, seg := range ep.segs {
			for _, g := range starts[seg] {
				ep.checkpoint(seg, g.r, g.off)
			}
		}
	}
	return s, nil
}

func (s *Store) epochLocked(m Meta) *epoch {
	if ep := s.byID[m.Epoch]; ep != nil {
		return ep
	}
	ep := &epoch{meta: m}
	s.byID[m.Epoch] = ep
	k := m.key()
	s.epochs[k] = append(s.epochs[k], ep)
	slices.SortFunc(s.epochs[k], func(a, b *epoch) int { return int(int64(a.meta.Epoch) - int64(b.meta.Epoch)) })
	if m.Epoch >= s.nextEpoch {
		s.nextEpoch = m.Epoch + 1
	}
	return ep
}

// NextEpoch allocates an epoch number unique within this store.
func (s *Store) NextEpoch() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.nextEpoch
	s.nextEpoch++
	return e
}

// Has reports whether the store holds an epoch.
func (s *Store) Has(epoch uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byID[epoch] != nil
}

func (s *Store) dir(k Key) string {
	return filepath.Join(s.cfg.Dir, url.PathEscape(k.Room), url.PathEscape(k.Identity), url.PathEscape(k.Track))
}

// LatestRoom returns the SID of the most recently recorded room with a
// name, or "" if none is stored.
func (s *Store) LatestRoom(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sid string
	var last int64
	for _, ep := range s.byID {
		if ep.meta.Name == name && len(ep.segs) > 0 && (sid == "" || ep.last() > last) {
			sid, last = ep.meta.Room, ep.last()
		}
	}
	return sid
}

// EpochInfo describes a stored epoch.
type EpochInfo struct {
	Meta
	First, Last int64 // server µs of the first and last stored Object
}

// Epochs lists a participant's stored epochs, ascending.
func (s *Store) Epochs(room, identity string) []EpochInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []EpochInfo
	for k, eps := range s.epochs {
		if k.Room != room || k.Identity != identity {
			continue
		}
		for _, ep := range eps {
			if len(ep.segs) > 0 {
				out = append(out, EpochInfo{Meta: ep.meta, First: ep.first(), Last: ep.last()})
			}
		}
	}
	slices.SortFunc(out, func(a, b EpochInfo) int { return int(int64(a.Epoch) - int64(b.Epoch)) })
	return out
}

// Checkpoint is a Group start: where a seek can begin decoding.
type Checkpoint struct {
	Track string
	Epoch uint32
	Group uint64 // archive Group ID
	At    int64  // server receive time, unix µs
	Loc   int64  // capture time from the LOC timestamp, unix µs; 0 if absent
}

// Checkpoints lists a participant's Group starts with From <= At < To,
// ordered by At.
func (s *Store) Checkpoints(room, identity string, from, to int64) []Checkpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Checkpoint
	for k, eps := range s.epochs {
		if k.Room != room || k.Identity != identity {
			continue
		}
		for _, ep := range eps {
			i := sort.Search(len(ep.cps), func(i int) bool { return ep.cps[i].at >= from })
			for ; i < len(ep.cps) && ep.cps[i].at < to; i++ {
				c := ep.cps[i]
				out = append(out, Checkpoint{Track: k.Track, Epoch: ep.meta.Epoch, Group: ArchiveGroup(ep.meta.Epoch, c.group), At: c.at, Loc: c.loc})
			}
		}
	}
	slices.SortFunc(out, func(a, b Checkpoint) int {
		if a.At != b.At {
			return int(a.At - b.At)
		}
		return strings.Compare(a.Track, b.Track)
	})
	return out
}

// Sweep deletes segments older than their room's window, then the oldest
// segments while the store is over MaxBytes. The segment being written is
// kept.
func (s *Store) Sweep() {
	now := s.cfg.Now().UnixMicro()
	s.mu.Lock()
	var doomed []string
	drop := func(ep *epoch, seg *segment) {
		ep.segs = slices.DeleteFunc(ep.segs, func(x *segment) bool { return x == seg })
		ep.cps = slices.DeleteFunc(ep.cps, func(c checkpoint) bool { return c.seg == seg })
		s.total -= seg.size
		doomed = append(doomed, seg.path)
	}
	open := func(ep *epoch, seg *segment) bool { return ep.live && seg == ep.segs[len(ep.segs)-1] }
	for _, ep := range s.byID {
		limit := now - int64(ep.meta.Window)*1e6
		for _, seg := range slices.Clone(ep.segs) {
			if seg.end < limit && !open(ep, seg) {
				drop(ep, seg)
			}
		}
	}
	for s.cfg.MaxBytes > 0 && s.total > s.cfg.MaxBytes {
		var oldest *segment
		var owner *epoch
		for _, ep := range s.byID {
			if len(ep.segs) > 0 && !open(ep, ep.segs[0]) && (oldest == nil || ep.segs[0].end < oldest.end) {
				oldest, owner = ep.segs[0], ep
			}
		}
		if oldest == nil {
			break
		}
		drop(owner, oldest)
	}
	for id, ep := range s.byID {
		if len(ep.segs) == 0 && !ep.live {
			delete(s.byID, id)
			k := ep.meta.key()
			s.epochs[k] = slices.DeleteFunc(s.epochs[k], func(x *epoch) bool { return x == ep })
			if len(s.epochs[k]) == 0 {
				delete(s.epochs, k)
			}
		}
	}
	s.mu.Unlock()
	for _, p := range doomed {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			s.log.Warn("dvr: removing segment", "path", p, "err", err)
		}
		// Remove the track, identity and room directories once empty.
		for d := filepath.Dir(p); d != s.cfg.Dir && strings.HasPrefix(d, s.cfg.Dir); d = filepath.Dir(d) {
			if os.Remove(d) != nil {
				break
			}
		}
	}
}

// Size reports the bytes the store holds.
func (s *Store) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

func segmentName(epoch uint32, start int64) string {
	return fmt.Sprintf("%010d-%016d.seg", epoch, start)
}
