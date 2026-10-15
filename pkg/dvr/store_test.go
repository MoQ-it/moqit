package dvr

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v22 "github.com/MoQ-it/moqt/wire/v22"
)

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newStore(t *testing.T, dir string, c *clock, mod func(*Config)) *Store {
	t.Helper()
	cfg := Config{Dir: dir, Now: c.now}
	if mod != nil {
		mod(&cfg)
	}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func obj(id uint64, payload string, loc int64) *v22.Object {
	o := &v22.Object{ID: id, Payload: []byte(payload)}
	if loc != 0 {
		o.Properties = v22.KeyValues{{Type: locProperty, Int: uint64(loc)}}
	}
	return o
}

var key = Key{Room: "RM_1", Identity: "alice/1", Track: "camera-m"}

func meta(epoch uint32) Meta {
	return Meta{Epoch: epoch, Room: key.Room, Identity: key.Identity, Track: key.Track, Codec: "vp8", Window: 600}
}

func fetchAll(t *testing.T, s *Store, start, end uint64) []string {
	t.Helper()
	r, ok := s.Fetch(key, start, end)
	if !ok {
		return nil
	}
	defer r.Close()
	var got []string
	for {
		o, err := r.Next(context.Background())
		if err == io.EOF {
			return got
		}
		if err != nil {
			t.Fatal(err)
		}
		e, g := SplitGroup(o.Group)
		got = append(got, fmt.Sprintf("%d:%d/%d=%s", e, g, o.Object, o.Payload))
	}
}

func TestWriterOrdersGroups(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	s := newStore(t, t.TempDir(), c, nil)
	w, err := s.Begin(meta(7))
	if err != nil {
		t.Fatal(err)
	}
	// Joined mid-group: group 4 is not recorded.
	w.RecordObject(4, 0, 1, obj(3, "x", 0))
	// Groups 5 and 6 interleave, as on parallel streams.
	w.RecordObject(6, 0, 1, obj(0, "f0", 0))
	w.RecordObject(5, 0, 1, obj(0, "e0", 0))
	w.RecordObject(5, 0, 1, obj(2, "e2", 0))
	w.RecordObject(6, 0, 1, obj(1, "f1", 0))
	w.RecordObject(5, 0, 1, obj(1, "e1", 0))
	w.RecordObject(5, 0, 1, &v22.Object{ID: 3, Status: v22.StatusEndOfGroup})
	w.Close()
	want := "[7:5/0=e0 7:5/1=e1 7:5/2=e2 7:6/0=f0 7:6/1=f1]"
	if got := fmt.Sprint(fetchAll(t, s, ArchiveGroup(7, 0), ArchiveGroup(7, 100))); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	if got := fmt.Sprint(fetchAll(t, s, ArchiveGroup(7, 6), ArchiveGroup(7, 6))); got != "[7:6/0=f0 7:6/1=f1]" {
		t.Errorf("group 6: %s", got)
	}
	if got := fetchAll(t, s, ArchiveGroup(8, 0), ArchiveGroup(8, 9)); got != nil {
		t.Errorf("another epoch: %v", got)
	}
	if eps := s.Epochs(key.Room, key.Identity); len(eps) != 1 || eps[0].Epoch != 7 || eps[0].Codec != "vp8" {
		t.Errorf("epochs %+v", eps)
	}
}

func TestLateObjectsDropped(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	s := newStore(t, t.TempDir(), c, nil)
	w, _ := s.Begin(meta(1))
	w.RecordObject(1, 0, 1, obj(0, "a", 0))
	w.RecordObject(2, 0, 1, obj(0, "b", 0))
	c.add(time.Second)
	waitFor(t, func() bool { return len(fetchAll(t, s, 0, ArchiveGroup(1, 9))) == 1 })
	w.RecordObject(1, 0, 1, obj(1, "late", 0))
	w.Close()
	if got := fmt.Sprint(fetchAll(t, s, 0, ArchiveGroup(1, 9))); got != "[1:1/0=a 1:2/0=b]" {
		t.Errorf("got %s", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// recordGroups writes groups [from, to) of n objects, 500ms apart.
func recordGroups(c *clock, w *Writer, from, to uint64, n int) {
	for g := from; g < to; g++ {
		for o := range uint64(n) {
			w.RecordObject(g, 0, 1, obj(o, fmt.Sprintf("g%d", g), c.now().UnixMicro()-1000))
		}
		c.add(500 * time.Millisecond)
	}
}

func TestSegmentsAndCheckpoints(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	s := newStore(t, t.TempDir(), c, func(cfg *Config) { cfg.SegmentDuration = 2 * time.Second })
	w, _ := s.Begin(meta(3))
	recordGroups(c, w, 0, 20, 2)
	w.Close()

	segs, _ := filepath.Glob(filepath.Join(s.dir(key), "*.seg"))
	if len(segs) != 5 {
		t.Errorf("%d segments; want 5", len(segs))
	}
	cps := s.Checkpoints(key.Room, key.Identity, 0, 1<<62)
	if len(cps) != 10 {
		t.Fatalf("%d checkpoints; want one per second (10): %+v", len(cps), cps)
	}
	for i, cp := range cps {
		e, g := SplitGroup(cp.Group)
		if e != 3 || g != uint64(2*i) || cp.At != time.Unix(1000, 0).Add(time.Duration(i)*time.Second).UnixMicro() || cp.Loc != cp.At-1000 {
			t.Errorf("checkpoint %d: %+v", i, cp)
		}
	}
	if got := s.Checkpoints(key.Room, key.Identity, time.Unix(1003, 0).UnixMicro(), time.Unix(1005, 0).UnixMicro()); len(got) != 2 {
		t.Errorf("window: %+v", got)
	}
	// A range in the middle crosses segments.
	got := fetchAll(t, s, ArchiveGroup(3, 7), ArchiveGroup(3, 12))
	if len(got) != 12 || got[0] != "3:7/0=g7" || got[11] != "3:12/1=g12" {
		t.Errorf("got %v", got)
	}
	r, _ := s.Fetch(key, ArchiveGroup(3, 7), ArchiveGroup(3, 12))
	if end := r.EndLocation(); end.Group != ArchiveGroup(3, 12) {
		t.Errorf("end %+v", end)
	}
}

func TestReopenTruncatesTornTail(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Unix(1000, 0)}
	s := newStore(t, dir, c, nil)
	w, _ := s.Begin(meta(s.NextEpoch()))
	recordGroups(c, w, 0, 4, 3)
	w.Close()
	want := fmt.Sprint(fetchAll(t, s, 0, ArchiveGroup(1, 100)))

	segs, _ := filepath.Glob(filepath.Join(s.dir(key), "*.seg"))
	f, _ := os.OpenFile(segs[len(segs)-1], os.O_APPEND|os.O_WRONLY, 0)
	before, _ := f.Stat()
	f.Write([]byte{0x40, 0x50, 1, 2, 3}) // a record cut short
	f.Close()

	s2 := newStore(t, dir, c, nil)
	if got := fmt.Sprint(fetchAll(t, s2, 0, ArchiveGroup(1, 100))); got != want {
		t.Errorf("after reopen %s\nwant %s", got, want)
	}
	after, _ := os.Stat(segs[len(segs)-1])
	if after.Size() != before.Size() {
		t.Errorf("size %d; want truncated to %d", after.Size(), before.Size())
	}
	if len(s2.Checkpoints(key.Room, key.Identity, 0, 1<<62)) != 2 {
		t.Error("checkpoints not rebuilt")
	}
	if e := s2.NextEpoch(); e != 2 {
		t.Errorf("next epoch %d; want 2", e)
	}
}

func TestSweep(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Unix(1000, 0)}
	s := newStore(t, dir, c, func(cfg *Config) { cfg.SegmentDuration = 2 * time.Second })
	m := meta(1)
	m.Window = 5
	w, _ := s.Begin(m)
	recordGroups(c, w, 0, 20, 1) // 10s of media in 5 segments
	w.Close()
	s.Sweep()
	// The last 5s (from 1005) are kept: segments ending at or after it.
	got := fetchAll(t, s, 0, ArchiveGroup(1, 100))
	if len(got) == 0 || got[0] != "1:8/0=g8" {
		t.Errorf("after window sweep: %v", got)
	}

	// MaxBytes drops the oldest segments, across epochs.
	s.cfg.MaxBytes = s.Size() / 2
	s.Sweep()
	if s.Size() > s.cfg.MaxBytes {
		t.Errorf("size %d over cap %d", s.Size(), s.cfg.MaxBytes)
	}
	if got := fetchAll(t, s, 0, ArchiveGroup(1, 100)); len(got) == 0 || got[len(got)-1] != "1:19/0=g19" {
		t.Errorf("after size sweep: %v", got)
	}

	// Everything expires; the directories go too.
	c.add(time.Hour)
	s.Sweep()
	if s.Size() != 0 || len(s.Epochs(key.Room, key.Identity)) != 0 {
		t.Errorf("size %d, epochs %v", s.Size(), s.Epochs(key.Room, key.Identity))
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("left %v", entries)
	}
}

func TestSweepKeepsOpenSegment(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	s := newStore(t, t.TempDir(), c, nil)
	m := meta(1)
	m.Window = 1
	w, _ := s.Begin(m)
	defer w.Close()
	recordGroups(c, w, 0, 3, 1)
	waitFor(t, func() bool { return len(fetchAll(t, s, 0, ArchiveGroup(1, 9))) == 2 })
	c.add(time.Hour)
	s.Sweep()
	if got := fetchAll(t, s, 0, ArchiveGroup(1, 9)); len(got) != 2 {
		t.Errorf("open segment swept: %v", got)
	}
	if !strings.Contains(s.dir(key), "alice%2F1") {
		t.Errorf("identity not escaped: %s", s.dir(key))
	}
}
