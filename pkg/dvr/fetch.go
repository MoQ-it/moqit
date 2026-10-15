package dvr

import (
	"bufio"
	"context"
	"io"
	"os"
	"sort"

	v22 "github.com/MoQ-it/moqt/wire/v22"
)

// span is the part of one epoch a fetch reads.
type span struct {
	epoch    uint32
	from, to uint64 // publisher Groups, inclusive
	segs     []segRead
}

type segRead struct {
	path      string
	off, size int64
}

// Fetch reads the stored Objects of a track with archive Group IDs in
// [start, end], in ascending order. ok is false when nothing in the range
// is stored here.
func (s *Store) Fetch(k Key, start, end uint64) (r *Reader, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r = &Reader{}
	for _, ep := range s.epochs[k] {
		id := ep.meta.Epoch
		if uint64(id) < start>>32 || uint64(id) > end>>32 || len(ep.segs) == 0 {
			continue
		}
		sp := span{epoch: id, from: 0, to: 1<<32 - 1}
		if uint64(id) == start>>32 {
			sp.from = start & (1<<32 - 1)
		}
		if uint64(id) == end>>32 {
			sp.to = end & (1<<32 - 1)
		}
		// Start at the last checkpoint at or before the first Group.
		first, off := ep.segs[0], ep.segs[0].dataOff
		if i := sort.Search(len(ep.cps), func(i int) bool { return ep.cps[i].group > sp.from }); i > 0 {
			first, off = ep.cps[i-1].seg, ep.cps[i-1].off
		}
		reading := false
		for _, seg := range ep.segs {
			if seg == first {
				reading = true
			}
			if !reading || seg.records == 0 || seg.firstGroup > sp.to {
				continue
			}
			if seg.lastGroup < sp.from {
				continue
			}
			o := seg.dataOff
			if seg == first {
				o = off
			}
			sp.segs = append(sp.segs, segRead{path: seg.path, off: o, size: seg.size})
			last := seg.lastGroup
			if last > sp.to {
				last = sp.to
			}
			r.end = v22.Location{Group: ArchiveGroup(id, last), Object: seg.lastObject}
		}
		if len(sp.segs) > 0 {
			r.spans = append(r.spans, sp)
		}
	}
	return r, len(r.spans) > 0
}

// Reader yields stored Objects as fetch entries.
type Reader struct {
	spans []span
	end   v22.Location

	f    *os.File
	br   *bufio.Reader
	left int64
}

// EndLocation is the largest Location the fetch may return.
func (r *Reader) EndLocation() v22.Location { return r.end }

// Next returns the next Object, or io.EOF.
func (r *Reader) Next(ctx context.Context) (*v22.FetchObject, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if r.f == nil {
			if len(r.spans) == 0 {
				return nil, io.EOF
			}
			sp := &r.spans[0]
			if len(sp.segs) == 0 {
				r.spans = r.spans[1:]
				continue
			}
			sr := sp.segs[0]
			sp.segs = sp.segs[1:]
			f, err := os.Open(sr.path)
			if err != nil {
				continue // swept meanwhile
			}
			if _, err := f.Seek(sr.off, io.SeekStart); err != nil {
				f.Close()
				continue
			}
			r.f, r.br, r.left = f, bufio.NewReaderSize(f, 64<<10), sr.size-sr.off
		}
		if r.left <= 0 {
			r.closeFile()
			continue
		}
		rec, n, err := readRecord(r.br)
		if err != nil {
			r.closeFile()
			continue
		}
		r.left -= n
		sp := &r.spans[0]
		if rec.group < sp.from {
			continue
		}
		if rec.group > sp.to {
			r.closeFile()
			sp.segs = nil
			continue
		}
		return &v22.FetchObject{
			Group:      ArchiveGroup(sp.epoch, rec.group),
			Object:     rec.object,
			SubgroupID: rec.subgroup,
			Priority:   rec.prio,
			Properties: rec.props,
			Payload:    rec.payload,
		}, nil
	}
}

func (r *Reader) closeFile() {
	if r.f != nil {
		r.f.Close()
		r.f, r.br = nil, nil
	}
}

// Close releases the open segment.
func (r *Reader) Close() { r.closeFile() }
