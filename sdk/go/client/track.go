package client

import (
	"sync"
	"time"

	"github.com/MoQ-it/moqt"
	v22 "github.com/MoQ-it/moqt/wire/v22"
)

// LOC object properties, as the browser SDK writes them.
const (
	locTimestamp  = 0x10 // capture time, µs since the Unix epoch
	locAudioLevel = 0x0c // RFC 6464 level, audio only
)

const (
	videoQueue = 512 // objects; more than a GOP, which a joiner gets at once
	audioQueue = 25  // 500 ms
)

type object struct {
	Group, ID uint64
	TS        int64 // µs since the Unix epoch
	Level     int   // audio level; -1 for none
	Payload   []byte
}

func (o *object) wire() *v22.Object {
	props := v22.KeyValues{{Type: locTimestamp, Int: uint64(o.TS)}}
	if o.Level >= 0 {
		props = append(props, v22.KeyValue{Type: locAudioLevel, Int: uint64(o.Level)})
	}
	return &v22.Object{ID: o.ID, Properties: props, Payload: o.Payload}
}

// Track publishes one MoQ track to its subscribers (in practice the
// node's relay). Video has a Group per GOP, and the current Group is kept
// so a new subscriber starts from its keyframe: the publisher need not
// make keyframes on demand, as the browser does. Audio has a Group per
// frame.
type Track struct {
	name  string
	audio bool

	mu      sync.Mutex
	group   uint64
	nextID  uint64
	started bool
	gop     []*object
	subs    map[*subscriber]struct{}
	muted   bool
	closed  bool
	active  int            // subscribers receiving objects (FORWARD)
	running sync.WaitGroup // subscribers still sending

	onChange func(track, event string, attrs ...any)
}

func newTrack(name string, audio bool) *Track {
	return &Track{name: name, audio: audio, subs: map[*subscriber]struct{}{}}
}

// Name is the MoQ track name, a Rendition's TrackName.
func (t *Track) Name() string { return t.name }

// sending reports whether any subscriber receives objects.
func (t *Track) sending() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active > 0
}

// report records a subscriber's change of state; delta changes the
// number receiving objects.
func (t *Track) report(delta int, event string, attrs ...any) {
	t.mu.Lock()
	t.active += delta
	t.mu.Unlock()
	if t.onChange != nil {
		t.onChange(t.name, event, attrs...)
	}
}

func (t *Track) subscribe(req *moqt.SubscribeRequest) {
	var props v22.KeyValues
	if t.audio {
		props = v22.KeyValues{{Type: v22.PropDefaultPublisherPriority, Int: 16}}
	}
	t.mu.Lock()
	closed := t.closed
	var largest *v22.Location
	if t.started && !closed {
		largest = &v22.Location{Group: t.group, Object: t.nextID - 1}
	}
	t.mu.Unlock()
	if closed {
		req.Reject(v22.ReqErrDoesNotExist, "stream ended")
		return
	}
	w, err := req.Accept(moqt.AcceptOptions{Properties: props, LargestObject: largest})
	if err != nil {
		return
	}
	size := videoQueue
	if t.audio {
		size = audioQueue
	}
	s := &subscriber{t: t, w: w, q: make(chan *object, size)}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		w.Close(v22.DoneTrackEnded, "stream ended")
		return
	}
	for _, o := range t.gop {
		if !t.muted {
			s.offer(o)
		}
	}
	t.subs[s] = struct{}{}
	t.running.Add(1)
	t.mu.Unlock()
	go s.run()
}

// WriteVideo sends an access unit captured at ts; a keyframe starts a new
// Group. Access units before the first keyframe are dropped.
func (t *Track) WriteVideo(ts time.Time, key bool, data []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || (!key && !t.started) {
		return
	}
	if key {
		if t.started {
			t.group++
		}
		t.started, t.nextID, t.gop = true, 0, t.gop[:0]
	}
	o := &object{Group: t.group, ID: t.nextID, TS: ts.UnixMicro(), Level: -1, Payload: data}
	t.nextID++
	t.gop = append(t.gop, o)
	if !t.muted {
		for s := range t.subs {
			s.offer(o)
		}
	}
}

// WriteAudio sends one frame captured at ts as its own Group, with its
// RFC 6464 level (0 is loudest, 127 silence) for active speakers.
func (t *Track) WriteAudio(ts time.Time, level uint8, data []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if t.started {
		t.group++
	}
	t.started, t.nextID = true, 1
	if t.muted {
		return
	}
	o := &object{Group: t.group, TS: ts.UnixMicro(), Level: int(level), Payload: data}
	for s := range t.subs {
		s.offer(o)
	}
}

func (t *Track) setMuted(muted bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.muted = muted
	if muted {
		for s := range t.subs {
			s.needKey = true
		}
	}
}

// close ends every subscription once its queue has drained.
func (t *Track) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	for s := range t.subs {
		close(s.q)
	}
	t.subs = nil
}

func (t *Track) remove(s *subscriber) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.subs[s]; ok {
		delete(t.subs, s)
		close(s.q)
	}
}

type subscriber struct {
	t *Track
	w *moqt.TrackWriter
	q chan *object
	// needKey (guarded by t.mu) skips video until the next Group after
	// objects were dropped.
	needKey bool

	objects, bytes int // sent; used only by run
}

func one(b bool) int {
	if b {
		return 1
	}
	return 0
}

// offer queues an object without blocking; t.mu is held. A subscriber
// that falls behind loses the rest of the Group.
func (s *subscriber) offer(o *object) {
	if !s.t.audio && s.needKey {
		if o.ID != 0 {
			return
		}
		s.needKey = false
	}
	select {
	case s.q <- o:
	default:
		s.needKey = true
	}
}

func (s *subscriber) run() {
	var sg *moqt.OutgoingSubgroup
	var sgGroup uint64
	skip := false // the rest of sgGroup is not sent
	start, fwd := time.Now(), s.w.Forwarding()
	s.t.report(one(fwd), "subscribed", "forward", fwd)
	defer func() {
		if sg != nil {
			sg.Close()
		}
		s.w.Close(v22.DoneTrackEnded, "stream ended")
		s.t.remove(s)
		s.t.report(-one(fwd), "unsubscribed", "objects", s.objects, "bytes", s.bytes, "for", time.Since(start).Round(time.Millisecond))
		s.t.running.Done()
	}()
	for {
		var o *object
		var ok bool
		select {
		case <-s.w.Context().Done():
			return
		case o, ok = <-s.q:
			if !ok {
				return
			}
		}
		if f := s.w.Forwarding(); f != fwd {
			fwd = f
			event := "paused"
			if f {
				event = "resumed"
			}
			s.t.report(2*one(f)-1, event)
		}
		if !fwd {
			if sg != nil {
				sg.Close() // resume at the next Group's keyframe
				sg = nil
			}
			continue
		}
		if s.t.audio {
			g := s.w.OpenSubgroup(o.Group, 0, moqt.SubgroupOptions{Properties: true, EndOfGroup: true, FirstObject: true})
			if g.WriteObject(o.wire()) == nil {
				g.Close()
				s.objects, s.bytes = s.objects+1, s.bytes+len(o.Payload)
			} else {
				g.Reset(v22.ResetCancelled)
			}
			continue
		}
		if sg == nil || o.Group != sgGroup {
			if sg != nil {
				sg.Close()
				sg = nil
			}
			sgGroup, skip = o.Group, o.ID != 0 // joined mid-Group: wait for the next
			if !skip {
				sg = s.w.OpenSubgroup(o.Group, 0, moqt.SubgroupOptions{Properties: true, FirstObject: true})
			}
		}
		if skip {
			continue
		}
		if err := sg.WriteObject(o.wire()); err != nil {
			sg.Reset(v22.ResetCancelled)
			sg, skip = nil, true
			if s.w.Context().Err() != nil {
				return
			}
		} else {
			s.objects, s.bytes = s.objects+1, s.bytes+len(o.Payload)
		}
	}
}
