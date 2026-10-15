package dvr

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	v22 "github.com/MoQ-it/moqt/wire/v22"
)

// settle is how long a Group must go without new Objects, once a newer
// Group has started, before it is written. Groups arrive on parallel
// streams; buffering them keeps each epoch on disk in Group order.
const settle = 300 * time.Millisecond

// Writer appends one epoch of a track. It implements relay.Recorder's
// RecordObject.
type Writer struct {
	s  *Store
	ep *epoch

	mu        sync.Mutex
	pending   map[uint64]*pendingGroup
	maxGroup  uint64
	flushed   bool
	watermark uint64 // the last Group written
	closed    bool

	// Only the flush goroutine touches the file.
	f    *os.File
	bw   *bufio.Writer
	seg  *segment
	stop chan struct{}
	done chan struct{}
}

type pendingGroup struct {
	recs []*record
	last time.Time
	skip bool // joined after the Group's first Object
}

// Begin starts recording an epoch. Meta.Epoch must be unique (see
// NextEpoch).
func (s *Store) Begin(m Meta) (*Writer, error) {
	if err := os.MkdirAll(s.dir(m.key()), 0o755); err != nil {
		return nil, err
	}
	s.mu.Lock()
	ep := s.epochLocked(m)
	ep.live = true
	s.mu.Unlock()
	w := &Writer{s: s, ep: ep, pending: map[uint64]*pendingGroup{}, stop: make(chan struct{}), done: make(chan struct{})}
	go w.run()
	return w, nil
}

// Epoch returns the epoch being recorded.
func (w *Writer) Epoch() uint32 { return w.ep.meta.Epoch }

// RecordObject buffers one Object. Groups beyond 32 bits, and Objects
// arriving after their Group was written, are dropped.
func (w *Writer) RecordObject(group, subgroupID uint64, priority uint8, o *v22.Object) {
	if group >= 1<<32 || o.Status != v22.StatusNormal {
		return
	}
	now := w.s.cfg.Now()
	r := &record{group: group, object: o.ID, subgroup: subgroupID, prio: priority, at: now.UnixMicro(), props: o.Properties, payload: o.Payload}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || (w.flushed && group <= w.watermark) {
		return
	}
	pg := w.pending[group]
	if pg == nil {
		pg = &pendingGroup{skip: o.ID != 0}
		w.pending[group] = pg
	}
	pg.recs = append(pg.recs, r)
	pg.last = now
	if group > w.maxGroup {
		w.maxGroup = group
	}
}

// RecordEnd implements relay.Recorder; it closes the Writer.
func (w *Writer) RecordEnd() { w.Close() }

// Close writes what is buffered and ends the epoch.
func (w *Writer) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		<-w.done
		return
	}
	w.closed = true
	w.mu.Unlock()
	close(w.stop)
	<-w.done
}

func (w *Writer) run() {
	defer close(w.done)
	t := time.NewTicker(settle / 3)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			w.flush(false)
		case <-w.stop:
			w.flush(true)
			if w.f != nil {
				w.f.Close()
			}
			w.s.mu.Lock()
			w.ep.live = false
			w.s.mu.Unlock()
			return
		}
	}
}

// flush writes the settled Groups, oldest first; all of them when final.
func (w *Writer) flush(final bool) {
	now := w.s.cfg.Now()
	w.mu.Lock()
	ids := make([]uint64, 0, len(w.pending))
	for g := range w.pending {
		ids = append(ids, g)
	}
	slices.Sort(ids)
	var ready []*pendingGroup
	for _, g := range ids {
		pg := w.pending[g]
		if !final && (g == w.maxGroup || now.Sub(pg.last) < settle) {
			break
		}
		delete(w.pending, g)
		w.flushed, w.watermark = true, g
		if !pg.skip {
			ready = append(ready, pg)
		}
	}
	w.mu.Unlock()
	for _, pg := range ready {
		slices.SortStableFunc(pg.recs, func(a, b *record) int { return int(int64(a.object) - int64(b.object)) })
		if err := w.writeGroup(pg.recs); err != nil {
			w.s.log.Warn("dvr: writing", "track", w.ep.meta.Track, "err", err)
		}
	}
}

func (w *Writer) writeGroup(recs []*record) error {
	if w.seg == nil || recs[0].at-w.seg.start >= int64(w.s.cfg.SegmentDuration/time.Microsecond) {
		if err := w.rotate(recs[0].at); err != nil {
			return err
		}
	}
	type pos struct {
		r   *record
		off int64
	}
	var written []pos
	var buf []byte
	off := w.seg.size
	for _, r := range recs {
		buf = r.append(buf[:0])
		if _, err := w.bw.Write(buf); err != nil {
			return err
		}
		written = append(written, pos{r, off})
		off += int64(len(buf))
	}
	if err := w.bw.Flush(); err != nil {
		return err
	}
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	for _, p := range written {
		w.seg.add(p.r)
		w.ep.checkpoint(w.seg, p.r, p.off)
	}
	w.s.total += off - w.seg.size
	w.seg.size = off
	return nil
}

func (w *Writer) rotate(start int64) error {
	if w.f != nil {
		w.f.Close()
	}
	w.f, w.bw, w.seg = nil, nil, nil // retried on the next Group if this fails
	m := w.ep.meta
	dir := w.s.dir(m.key())
	if err := os.MkdirAll(dir, 0o755); err != nil { // Sweep removes empty directories
		return err
	}
	path := filepath.Join(dir, segmentName(m.Epoch, start))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	n, err := writeHeader(f, m)
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.bw = f, bufio.NewWriterSize(f, 64<<10)
	w.seg = &segment{path: path, dataOff: n, size: n, start: start}
	w.s.mu.Lock()
	w.ep.segs = append(w.ep.segs, w.seg)
	w.s.total += n
	w.s.mu.Unlock()
	return nil
}
