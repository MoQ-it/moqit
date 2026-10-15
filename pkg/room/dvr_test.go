package room

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/MoQ-it/moqit/pkg/dvr"
	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"github.com/MoQ-it/moqt"
	v22 "github.com/MoQ-it/moqt/wire/v22"
)

func TestDvrTracks(t *testing.T) {
	video := func(src moqitv1.TrackSource, prefix string, heights ...uint32) *moqitv1.TrackInfo {
		ti := &moqitv1.TrackInfo{Kind: moqitv1.TrackKind_TRACK_KIND_VIDEO, Source: src}
		for i, h := range heights {
			ti.Renditions = append(ti.Renditions, &moqitv1.Rendition{TrackName: prefix + string(rune('a'+i)), Height: h})
		}
		return ti
	}
	mic := &moqitv1.TrackInfo{Kind: moqitv1.TrackKind_TRACK_KIND_AUDIO, Renditions: []*moqitv1.Rendition{{TrackName: TrackMic}}}
	tracks := []*moqitv1.TrackInfo{
		mic,
		video(moqitv1.TrackSource_TRACK_SOURCE_CAMERA, "camera-", 1080, 720, 360),
		video(moqitv1.TrackSource_TRACK_SOURCE_SCREEN, "screen-", 1440, 1080),
	}
	got := dvrTracks(tracks, 720)
	if len(got) != 3 || got[TrackMic].track != mic || got["camera-b"].rd.Height != 720 || got["screen-b"].rd.Height != 1080 {
		t.Errorf("max 720: %v", got)
	}
	if got := dvrTracks(tracks, 0); got["camera-a"].rd == nil || got["screen-a"].rd == nil {
		t.Errorf("no cap: %v", got)
	}
}

func TestFetchRange(t *testing.T) {
	f := func(typ v22.LocationFilterType, start, delta uint64) *v22.Params {
		return &v22.Params{LocationFilter: &v22.LocationFilter{Type: typ, StartGroup: start, EndGroupDelta: delta}}
	}
	for _, c := range []struct {
		p          *v22.Params
		start, end uint64
		ok         bool
	}{
		{f(v22.FilterAbsoluteRange, 5, 3), 5, 8, true},
		{f(v22.FilterAbsoluteStartGroupEnd, 5, 0), 5, 5, true},
		{f(v22.FilterAbsoluteStart, 9, 0), 9, math.MaxUint64, true},
		{f(v22.FilterRelativeStart, 1, 0), 0, 0, false},
		{f(v22.FilterAbsoluteRange, math.MaxUint64, 1), 0, 0, false},
		{nil, 0, 0, false},
	} {
		start, end, err := fetchRange(c.p)
		var re *moqt.RequestError
		if c.ok != (err == nil) || (err != nil && (!errors.As(err, &re) || re.Code != v22.ReqErrInvalidRange)) || start != c.start || end != c.end {
			t.Errorf("%+v: %d %d %v", c.p, start, end, err)
		}
	}
}

func TestDvrWindow(t *testing.T) {
	m := &Manager{}
	if w := m.dvrWindow(600); w != 0 {
		t.Errorf("without a store: %d", w)
	}
	m.cfg.DVR = &dvr.Store{}
	m.cfg.DVRWindow, m.cfg.DVRMaxWindow = time.Hour, 2*time.Hour
	for req, want := range map[uint32]uint32{0: 3600, 60: 60, 99999: 7200} {
		if w := m.dvrWindow(req); w != want {
			t.Errorf("requested %d: %d; want %d", req, w, want)
		}
	}
}
