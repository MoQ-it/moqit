package room

import (
	"context"
	"io"
	"math"
	"time"

	"github.com/MoQ-it/moqit/pkg/dvr"
	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"github.com/MoQ-it/moqt"
	"github.com/MoQ-it/moqt/relay"
	v22 "github.com/MoQ-it/moqt/wire/v22"
	"google.golang.org/protobuf/proto"
)

// Rewind: the node hosting a participant records one rendition per source
// to the dvr store while the room has a dvr_window, and viewers FETCH it
// back. Each participant namespace has a fetch-only "dvr" index track
// whose Group IDs are unix minutes.

// dvrWindow is the history window for a new room: the requested one, else
// the server default, capped at the server maximum; 0 without a store.
func (m *Manager) dvrWindow(requested uint32) uint32 {
	if m.cfg.DVR == nil {
		return 0
	}
	w := requested
	if w == 0 {
		w = uint32(m.cfg.DVRWindow / time.Second)
	}
	if limit := uint32(m.cfg.DVRMaxWindow / time.Second); limit > 0 && w > limit {
		w = limit
	}
	return w
}

type recKey struct{ room, identity, track string } // room name

type recording struct {
	w     *dvr.Writer
	stop  func()
	start time.Time
}

// dvrRecorder feeds a Writer and reports when the upstream ends.
type dvrRecorder struct {
	*dvr.Writer
	ended func()
}

func (r dvrRecorder) RecordEnd() {
	r.Writer.Close()
	r.ended()
}

// dvrChoice is a rendition to record for one source.
type dvrChoice struct {
	track *moqitv1.TrackInfo
	rd    *moqitv1.Rendition
}

// dvrTracks picks what to record from a participant's tracks: every audio
// track, and per video source the largest rendition at most maxHeight high
// (the smallest if all are higher).
func dvrTracks(tracks []*moqitv1.TrackInfo, maxHeight uint32) map[string]dvrChoice {
	out := map[string]dvrChoice{}
	for _, t := range tracks {
		if len(t.Renditions) == 0 {
			continue
		}
		if t.Kind == moqitv1.TrackKind_TRACK_KIND_AUDIO {
			out[t.Renditions[0].TrackName] = dvrChoice{t, t.Renditions[0]}
			continue
		}
		var best, smallest *moqitv1.Rendition
		for _, rd := range t.Renditions {
			if smallest == nil || rd.Height < smallest.Height {
				smallest = rd
			}
			if (maxHeight == 0 || rd.Height <= maxHeight) && (best == nil || rd.Height > best.Height) {
				best = rd
			}
		}
		if best == nil {
			best = smallest
		}
		out[best.TrackName] = dvrChoice{t, best}
	}
	return out
}

// syncRecordings starts and stops the recordings of a participant on this
// node to match its published tracks.
func (m *Manager) syncRecordings(r *Room, identity string) {
	if m.cfg.DVR == nil {
		return
	}
	r.mu.Lock()
	var want map[string]dvrChoice
	info := proto.CloneOf(r.info)
	if p := r.participants[identity]; p != nil && info.DvrWindow > 0 {
		want = dvrTracks(p.info.Tracks, uint32(m.cfg.DVRMaxHeight))
	}
	r.mu.Unlock()

	m.recMu.Lock()
	var stale []*recording
	for k, rec := range m.recs {
		if k.room == r.name && k.identity == identity {
			if _, ok := want[k.track]; !ok {
				stale = append(stale, rec)
				delete(m.recs, k)
			}
		}
	}
	var start []recKey
	for name := range want {
		k := recKey{r.name, identity, name}
		if m.recs[k] == nil {
			m.recs[k] = &recording{}
			start = append(start, k)
		}
	}
	m.recMu.Unlock()
	for _, rec := range stale {
		rec.close()
	}
	for _, k := range start {
		go m.startRecording(r, info, k, want[k.track])
	}
}

func (rec *recording) close() {
	if rec.stop != nil {
		rec.stop()
	}
	if rec.w != nil {
		rec.w.Close()
	}
}

func (m *Manager) startRecording(r *Room, info *moqitv1.Room, k recKey, c dvrChoice) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m.recMu.Lock()
	rec := m.recs[k]
	m.recMu.Unlock()
	fail := func(err error) {
		m.log.Warn("dvr: not recording", "room", k.room, "identity", k.identity, "track", k.track, "err", err)
		m.recMu.Lock()
		if m.recs[k] == rec {
			delete(m.recs, k)
		}
		m.recMu.Unlock()
	}
	epoch, err := m.nextEpoch(ctx)
	if err != nil {
		fail(err)
		return
	}
	w, err := m.cfg.DVR.Begin(dvr.Meta{
		Epoch: epoch, Room: info.Sid, Name: k.room, Identity: k.identity, Track: k.track,
		Codec: c.track.Codec, Width: c.rd.Width, Height: c.rd.Height, Channels: c.track.Channels, Window: info.DvrWindow,
	})
	if err != nil {
		fail(err)
		return
	}
	_, stop, err := m.relay.Record(v22.FullTrackName{Namespace: ParticipantNamespace(k.room, k.identity), Name: k.track},
		dvrRecorder{Writer: w, ended: func() { m.recordingEnded(r, k, rec) }})
	if err != nil {
		w.Close()
		fail(err)
		return
	}
	m.recMu.Lock()
	current := m.recs[k] == rec
	if current {
		rec.w, rec.stop, rec.start = w, stop, time.Now()
	}
	m.recMu.Unlock()
	if !current { // stopped while starting
		stop()
		w.Close()
		return
	}
	if c := m.cfg.Cluster; c != nil {
		if err := c.PutDvrEpoch(ctx, k.room, k.identity, epoch, rec.start, m.rosterTTL(info.DvrWindow)); err != nil {
			m.log.Warn("dvr: registering epoch", "err", err)
		}
	}
	m.log.Debug("dvr: recording", "room", k.room, "identity", k.identity, "track", k.track, "epoch", epoch)
}

// recordingEnded handles the publisher ending a recorded track: a later
// sync starts a new epoch if the track is still published.
func (m *Manager) recordingEnded(r *Room, k recKey, rec *recording) {
	m.recMu.Lock()
	ours := m.recs[k] == rec
	if ours {
		delete(m.recs, k)
	}
	m.recMu.Unlock()
	if rec.stop != nil {
		rec.stop()
	}
	if ours {
		time.AfterFunc(2*time.Second, func() { m.syncRecordings(r, k.identity) })
	}
}

func (m *Manager) nextEpoch(ctx context.Context) (uint32, error) {
	c := m.cfg.Cluster
	if c == nil {
		return m.cfg.DVR.NextEpoch(), nil
	}
	for {
		e, err := c.NextDvrEpoch(ctx)
		if err != nil || !m.cfg.DVR.Has(e) {
			return e, err
		}
	}
}

func (m *Manager) rosterTTL(window uint32) time.Duration {
	return time.Duration(window)*time.Second + 10*time.Minute
}

// dvrLoop sweeps expired history and keeps the cluster roster of ongoing
// recordings alive.
func (m *Manager) dvrLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.recMu.Lock()
			recs := m.recs
			m.recs = map[recKey]*recording{}
			m.recMu.Unlock()
			for _, rec := range recs {
				rec.close()
			}
			return
		case <-t.C:
		}
		m.cfg.DVR.Sweep()
		c := m.cfg.Cluster
		if c == nil {
			continue
		}
		m.recMu.Lock()
		type entry struct {
			k     recKey
			epoch uint32
			start time.Time
		}
		var live []entry
		for k, rec := range m.recs {
			if rec.w != nil {
				live = append(live, entry{k, rec.w.Epoch(), rec.start})
			}
		}
		m.recMu.Unlock()
		for _, e := range live {
			if r := m.Room(e.k.room); r != nil {
				c.PutDvrEpoch(ctx, e.k.room, e.k.identity, e.epoch, e.start, m.rosterTTL(r.roomInfo().DvrWindow))
			}
		}
	}
}

// dvrRoomSID maps a room name to the SID its history is stored under.
func (m *Manager) dvrRoomSID(name string) string {
	if r := m.Room(name); r != nil {
		return r.roomInfo().Sid
	}
	return m.cfg.DVR.LatestRoom(name)
}

// fetchRange reads a FETCH's absolute Group range (inclusive).
func fetchRange(p *v22.Params) (start, end uint64, err error) {
	var f *v22.LocationFilter
	if p != nil {
		f = p.LocationFilter
	}
	if f == nil {
		return 0, 0, &moqt.RequestError{Code: v22.ReqErrInvalidRange, Reason: "FETCH needs an absolute LOCATION_FILTER"}
	}
	switch f.Type {
	case v22.FilterAbsoluteStart:
		return f.StartGroup, math.MaxUint64, nil
	case v22.FilterAbsoluteRange, v22.FilterAbsoluteStartGroupEnd:
		if f.EndGroupDelta > math.MaxUint64-f.StartGroup {
			return 0, 0, &moqt.RequestError{Code: v22.ReqErrInvalidRange, Reason: "range overflows"}
		}
		return f.StartGroup, f.StartGroup + f.EndGroupDelta, nil
	}
	return 0, 0, &moqt.RequestError{Code: v22.ReqErrInvalidRange, Reason: "FETCH needs an absolute LOCATION_FILTER"}
}

// Fetch implements relay.Archive.
func (m *Manager) Fetch(ctx context.Context, _ *relay.Peer, track v22.FullTrackName, params *v22.Params) (relay.FetchSource, bool, error) {
	kind, room, identity := parseNamespace(track.Namespace)
	if kind != nsParticipant {
		return nil, false, nil
	}
	start, end, err := fetchRange(params)
	if err != nil {
		return nil, false, err
	}
	sid := m.dvrRoomSID(room)
	if sid == "" {
		return nil, false, nil
	}
	if track.Name == TrackDvr {
		if len(m.cfg.DVR.Epochs(sid, identity)) == 0 {
			return nil, false, nil
		}
		if node, _ := m.dvrIndexNode(ctx, room, identity, end); node != "" && node != m.cfg.NodeID {
			return nil, false, nil
		}
		return m.indexSource(ctx, room, sid, identity, start, end), true, nil
	}
	r, ok := m.cfg.DVR.Fetch(dvr.Key{Room: sid, Identity: identity, Track: track.Name}, start, end)
	if !ok {
		return nil, false, nil
	}
	return archiveSource{r}, true, nil
}

type archiveSource struct{ r *dvr.Reader }

func (a archiveSource) Options() moqt.FetchAcceptOptions {
	return moqt.FetchAcceptOptions{EndLocation: a.r.EndLocation()}
}
func (a archiveSource) Next(ctx context.Context) (*v22.FetchObject, error) { return a.r.Next(ctx) }
func (a archiveSource) Close()                                             { a.r.Close() }

func nowMinute() uint64 { return uint64(time.Now().Unix() / 60) }

// indexSource yields a DvrIndex per minute in [start, end] that has
// checkpoints.
func (m *Manager) indexSource(ctx context.Context, room, sid, identity string, start, end uint64) *indexSource {
	if now := nowMinute(); end > now {
		end = now
	}
	s := &indexSource{m: m, sid: sid, identity: identity, next: start, end: end}
	eps := m.cfg.DVR.Epochs(sid, identity)
	s.earliest, s.latest = eps[0].First, eps[0].Last
	for _, e := range eps {
		s.earliest, s.latest = min(s.earliest, e.First), max(s.latest, e.Last)
	}
	if c := m.cfg.Cluster; c != nil {
		// Older epochs may be on other nodes.
		if r, err := c.Room(ctx, room); err == nil && r != nil {
			oldest := time.Now().Add(-time.Duration(r.DvrWindow) * time.Second).UnixMicro()
			for _, ep := range c.DvrEpochsCached(ctx, room, identity) {
				s.earliest = min(s.earliest, max(ep.Start.UnixMicro(), oldest))
			}
		}
	}
	return s
}

type indexSource struct {
	m                *Manager
	sid, identity    string
	next, end        uint64
	earliest, latest int64
}

func (s *indexSource) Options() moqt.FetchAcceptOptions {
	return moqt.FetchAcceptOptions{EndLocation: v22.Location{Group: s.end}}
}

func (s *indexSource) Next(ctx context.Context) (*v22.FetchObject, error) {
	for ; s.next <= s.end; s.next++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		from := int64(s.next) * 60e6
		cps := s.m.cfg.DVR.Checkpoints(s.sid, s.identity, from, from+60e6)
		if len(cps) == 0 {
			continue
		}
		idx := &moqitv1.DvrIndex{Earliest: s.earliest, Latest: s.latest}
		used := map[uint32]bool{}
		for _, c := range cps {
			idx.Checkpoints = append(idx.Checkpoints, &moqitv1.DvrCheckpoint{Track: c.Track, Group: c.Group, At: c.At, Loc: c.Loc})
			used[c.Epoch] = true
		}
		for _, e := range s.m.cfg.DVR.Epochs(s.sid, s.identity) {
			if used[e.Epoch] {
				idx.Epochs = append(idx.Epochs, &moqitv1.DvrEpoch{Epoch: e.Epoch, Track: e.Track, Codec: e.Codec, Width: e.Width, Height: e.Height, Channels: e.Channels})
			}
		}
		b, err := proto.Marshal(idx)
		if err != nil {
			return nil, err
		}
		g := s.next
		s.next++
		return &v22.FetchObject{Group: g, Payload: b}, nil
	}
	return nil, io.EOF
}

func (s *indexSource) Close() {}

// dvrIndexNode picks the node serving a participant's index up to a
// minute: the one recording its latest epoch that started by then.
func (m *Manager) dvrIndexNode(ctx context.Context, room, identity string, minute uint64) (string, error) {
	c := m.cfg.Cluster
	if c == nil {
		return "", nil
	}
	if now := nowMinute(); minute > now {
		minute = now
	}
	limit := time.Unix(int64(minute+1)*60, 0)
	var node string
	var latest time.Time
	for _, ep := range c.DvrEpochsCached(ctx, room, identity) {
		if !ep.Start.After(limit) && (node == "" || ep.Start.After(latest)) {
			node, latest = ep.Node, ep.Start
		}
	}
	return node, nil
}

// ResolveFetch implements relay.FetchResolver: a FETCH this node can't
// serve goes to the node storing the epoch (or the index minute), through
// the parent edge for edges below tier 1.
func (m *Manager) ResolveFetch(ctx context.Context, track v22.FullTrackName, params *v22.Params) (*moqt.Session, error) {
	kind, room, identity := parseNamespace(track.Namespace)
	if kind != nsParticipant {
		return nil, nil
	}
	if sess := m.fromParent(ctx, room); sess != nil {
		return sess, nil
	}
	start, end, err := fetchRange(params)
	if err != nil {
		return nil, err
	}
	var node string
	if track.Name == TrackDvr {
		node, err = m.dvrIndexNode(ctx, room, identity, end)
	} else {
		epoch, _ := dvr.SplitGroup(start)
		for _, ep := range m.cfg.Cluster.DvrEpochsCached(ctx, room, identity) {
			if ep.Epoch == epoch {
				node = ep.Node
			}
		}
	}
	if err != nil || node == "" || node == m.cfg.NodeID {
		return nil, err
	}
	return m.peer(ctx, node)
}

// AuthorizeFetch implements relay.FetchAuthorizer: viewers may rewind
// media and the index of rooms keeping history; other nodes forward their
// viewers' requests.
func (m *Manager) AuthorizeFetch(peer *relay.Peer, track v22.FullTrackName, _ *v22.Params) error {
	pi := infoOf(peer)
	if pi.server {
		return nil
	}
	v := pi.viewer
	if v == nil {
		return denied("rewinding is for viewers")
	}
	kind, room, _ := parseNamespace(track.Namespace)
	if room != v.room || kind != nsParticipant {
		return denied("outside your room")
	}
	if class, _ := classifyTrack(track.Name); class != classMedia && class != classDvr {
		return &moqt.RequestError{Code: v22.ReqErrDoesNotExist, Reason: "no such track"}
	}
	if !m.dvrEnabled(room) {
		return denied("this room keeps no history")
	}
	return nil
}

// dvrEnabled reports whether a room keeps history. A room that has ended
// may still have some, until its window passes.
func (m *Manager) dvrEnabled(room string) bool {
	if r := m.Room(room); r != nil {
		return r.roomInfo().DvrWindow > 0
	}
	if c := m.cfg.Cluster; c != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if info, err := c.Room(ctx, room); err == nil && info != nil {
			return info.DvrWindow > 0
		}
	}
	return true
}

func (m *Manager) fetchLimit(p *relay.Peer) int {
	if infoOf(p).viewer != nil {
		return m.cfg.DVRFetchLimit
	}
	return 0
}
