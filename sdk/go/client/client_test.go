package client_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/MoQ-it/moqit/internal/testclient"
	"github.com/MoQ-it/moqit/pkg/auth"
	"github.com/MoQ-it/moqit/pkg/room"
	"github.com/MoQ-it/moqit/pkg/server"
	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	moqit "github.com/MoQ-it/moqit/sdk/go"
	"github.com/MoQ-it/moqit/sdk/go/client"
	"github.com/MoQ-it/moqt"
	v22 "github.com/MoQ-it/moqt/wire/v22"
)

const apiKey, apiSecret = "clientkey", "client-secret-0123456789abcdef01234"

func token(t *testing.T, identity string, g auth.RoomGrant) string {
	t.Helper()
	g.Room = "r1"
	tok, err := auth.NewAccessToken(apiKey, apiSecret).SetIdentity(identity).SetRoomGrant(&g).ToJWT()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func tracks() []*moqitv1.TrackInfo {
	return []*moqitv1.TrackInfo{
		{Name: "camera", Kind: moqitv1.TrackKind_TRACK_KIND_VIDEO, Source: moqitv1.TrackSource_TRACK_SOURCE_CAMERA, Codec: "avc1.42e01f",
			Renditions: []*moqitv1.Rendition{{TrackName: "camera-720", Width: 1280, Height: 720}, {TrackName: "camera-360", Width: 640, Height: 360}}},
		{Name: "microphone", Kind: moqitv1.TrackKind_TRACK_KIND_AUDIO, Source: moqitv1.TrackSource_TRACK_SOURCE_MICROPHONE, Codec: "opus", Channels: 2,
			Renditions: []*moqitv1.Rendition{{TrackName: "mic", Bitrate: 96000}}},
	}
}

// logs collects JSON log records.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(b)
}

type record struct {
	Msg     string
	Track   string
	Sending []string
	Objects int
}

// find returns the first record with msg for track.
func (l *logs) find(msg, track string) *record {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range bytes.Split(l.buf.Bytes(), []byte("\n")) {
		var r record
		if json.Unmarshal(line, &r) == nil && r.Msg == msg && r.Track == track {
			return &r
		}
	}
	return nil
}

// wait returns the first record with msg for track, waiting up to 5s.
func (l *logs) wait(t *testing.T, msg, track string) *record {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if r := l.find(msg, track); r != nil {
			return r
		}
	}
	t.Fatalf("no %q log for %s", msg, track)
	return nil
}

// fromGroupStart subscribes to one of enc's tracks from the current
// Group, as the browser does.
func fromGroupStart(t *testing.T, c *testclient.Client, name string) *moqt.Subscription {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sub, err := c.Sess.Subscribe(ctx, v22.FullTrackName{Namespace: room.ParticipantNamespace("r1", "enc"), Name: name},
		&v22.Params{LocationFilter: &v22.LocationFilter{Type: v22.FilterRelativeStart, StartGroup: 1}})
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

// next returns the next object on sub with its Group.
func next(t *testing.T, sub *moqt.Subscription, g **moqt.IncomingSubgroup) (uint64, *v22.Object) {
	t.Helper()
	for {
		if *g == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			var err error
			*g, err = sub.AcceptSubgroup(ctx)
			cancel()
			if err != nil {
				t.Fatalf("%s: no subgroup: %v", sub.Track.Name, err)
			}
		}
		if o, err := (*g).Next(); err == nil {
			return (*g).Header.Group, o
		}
		*g = nil
	}
}

func TestParticipant(t *testing.T) {
	ctx := context.Background()
	keys := auth.KeyProvider{apiKey: apiSecret}
	cert, err := server.SelfSignedCert("localhost,127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	n, err := server.Start(ctx, server.Config{UDPAddr: "127.0.0.1:0", Keys: keys, Certificate: cert})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	addr := "127.0.0.1:" + n.Port()
	cfg := client.Config{URL: "https://" + addr + "/moq", Insecure: true}
	api := httptest.NewServer(n.APIHandler())
	defer api.Close()
	rooms := moqit.NewRoomServiceClient(api.URL, apiKey, apiSecret)

	if _, err := client.Join(ctx, cfg, token(t, "v", auth.RoomGrant{Viewer: true}), nil); err == nil {
		t.Error("joined with a viewer token")
	}
	bad, _ := auth.NewAccessToken(apiKey, "wrong-secret-0123456789abcdef0123456").SetIdentity("x").
		SetRoomGrant(&auth.RoomGrant{Join: true, Room: "r1"}).ToJWT()
	if _, err := client.Join(ctx, cfg, bad, nil); err == nil {
		t.Error("joined with a token the node cannot verify")
	}
	dup := tracks()
	dup[1].Renditions[0].TrackName = "camera-720"
	if _, err := client.Join(ctx, cfg, token(t, "x", auth.RoomGrant{Join: true}), dup); err == nil {
		t.Error("joined with a repeated track name")
	}

	var log logs
	logged := cfg
	logged.Log = slog.New(slog.NewJSONHandler(&log, nil))
	p, err := client.Join(ctx, logged, token(t, "enc", auth.RoomGrant{Join: true}), tracks())
	if err != nil {
		t.Fatal(err)
	}
	if p.Room() != "r1" || p.Identity() != "enc" || p.Track("camera-360") == nil || p.Track("nope") != nil {
		t.Fatalf("participant %s/%s", p.Room(), p.Identity())
	}
	bob := testclient.Join(t, addr, "r1", "bob", token(t, "bob", auth.RoomGrant{Join: true}))
	var mic *moqitv1.TrackInfo
	bob.WaitState("enc's tracks", func(s map[string]*moqitv1.ParticipantInfo) bool {
		if e := s["enc"]; e != nil && len(e.Tracks) == 2 {
			mic = e.Tracks[1]
			return len(e.Tracks[0].Renditions) == 2 && mic.Channels == 2
		}
		return false
	})

	// A subscriber joining mid-GOP starts from the cached keyframe.
	cam := p.Track("camera-720")
	t0 := time.Now()
	cam.WriteVideo(t0, false, []byte("before the first keyframe"))
	cam.WriteVideo(t0, true, []byte("k0"))
	cam.WriteVideo(t0.Add(33*time.Millisecond), false, []byte("d0"))
	sub := fromGroupStart(t, bob, "camera-720")
	var sg *moqt.IncomingSubgroup
	for i, want := range []string{"k0", "d0"} {
		g, o := next(t, sub, &sg)
		ts, _ := o.Properties.Lookup(0x10)
		if g != 0 || o.ID != uint64(i) || string(o.Payload) != want || ts.Int != uint64(t0.Add(time.Duration(i)*33*time.Millisecond).UnixMicro()) {
			t.Fatalf("replayed object %d: group %d id %d %q ts %d", i, g, o.ID, o.Payload, ts.Int)
		}
	}
	cam.WriteVideo(t0.Add(66*time.Millisecond), true, []byte("k1"))
	if g, o := next(t, sub, &sg); g != 1 || o.ID != 0 || string(o.Payload) != "k1" {
		t.Fatalf("live keyframe: group %d id %d %q", g, o.ID, o.Payload)
	}
	if r := log.wait(t, "client: track subscribed", "camera-720"); !slices.Contains(r.Sending, "camera-720") || slices.Contains(r.Sending, "camera-360") {
		t.Errorf("sending after subscribing: %v", r.Sending)
	}

	audio := bob.Subscribe("enc", "mic")
	var ag *moqt.IncomingSubgroup
	stop := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				p.Track("mic").WriteAudio(time.Now(), 30, []byte{byte(i)})
			}
		}
	}()
	defer close(stop)
	if _, o := next(t, audio, &ag); len(o.Payload) != 1 {
		t.Fatalf("audio payload %v", o.Payload)
	} else if lvl, ok := o.Properties.Lookup(0x0c); !ok || lvl.Int != 30 {
		t.Fatalf("audio level %v %v", lvl, ok)
	}

	// RoomService.MuteTrack stops the track and declares it muted.
	if _, err := rooms.MuteTrack(ctx, connect.NewRequest(&moqitv1.MuteTrackRequest{Room: "r1", Identity: "enc", TrackSid: mic.Sid, Muted: true})); err != nil {
		t.Fatal(err)
	}
	bob.WaitState("mic muted", func(s map[string]*moqitv1.ParticipantInfo) bool {
		return s["enc"] != nil && len(s["enc"].Tracks) == 2 && s["enc"].Tracks[1].Muted
	})
	// Frames already on their way still arrive; after them, nothing.
	drainUntil := time.Now().Add(time.Second)
	for {
		dctx, cancel := context.WithDeadline(ctx, drainUntil)
		_, err := audio.AcceptSubgroup(dctx)
		cancel()
		if err != nil {
			break
		}
	}
	mctx, cancel := context.WithTimeout(ctx, time.Second)
	if g, err := audio.AcceptSubgroup(mctx); err == nil {
		t.Errorf("muted microphone still sending: group %d", g.Header.Group)
	}
	cancel()

	if _, err := rooms.RemoveParticipant(ctx, connect.NewRequest(&moqitv1.RemoveParticipantRequest{Room: "r1", Identity: "enc"})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("still joined after RemoveParticipant")
	}
	if r := log.wait(t, "client: track unsubscribed", "camera-720"); r.Objects != 3 || slices.Contains(r.Sending, "camera-720") {
		t.Errorf("unsubscribed after %d objects, sending %v", r.Objects, r.Sending)
	}

	p2, err := client.Join(ctx, cfg, token(t, "enc2", auth.RoomGrant{Join: true}), tracks())
	if err != nil {
		t.Fatal(err)
	}
	bob.WaitState("enc2 joined", testclient.Present("enc2"))
	p2.Close("done")
	bob.WaitState("enc2 left", func(s map[string]*moqitv1.ParticipantInfo) bool { return s["enc2"] == nil })
}

func TestEncodings(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"camera-h", "av01.0.05M.08"}: "camera-h.av1", {"camera-h", "avc1.42e01f"}: "camera-h.avc", {"screen-l", "hev1.1.6.L93.B0"}: "screen-l.hevc",
		{"camera-m", "vp9"}: "camera-m.vp9", {"camera-m", "vp8"}: "camera-m.vp8", {"mic", "opus"}: "mic",
	} {
		if got := client.EncodingTrackName(in[0], in[1]); got != want {
			t.Errorf("EncodingTrackName(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}

	ctx := context.Background()
	cert, err := server.SelfSignedCert("localhost,127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	n, err := server.Start(ctx, server.Config{UDPAddr: "127.0.0.1:0", Keys: auth.KeyProvider{apiKey: apiSecret}, Certificate: cert})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	addr := "127.0.0.1:" + n.Port()
	cfg := client.Config{URL: "https://" + addr + "/moq", Insecure: true}
	camera := func(codecs ...string) []*moqitv1.TrackInfo {
		r := &moqitv1.Rendition{TrackName: client.EncodingTrackName("camera-m", codecs[0]), Width: 640, Height: 360}
		for _, c := range codecs {
			r.Encodings = append(r.Encodings, &moqitv1.VideoEncoding{TrackName: client.EncodingTrackName("camera-m", c), Codec: c})
		}
		return []*moqitv1.TrackInfo{{Name: "camera", Kind: moqitv1.TrackKind_TRACK_KIND_VIDEO, Source: moqitv1.TrackSource_TRACK_SOURCE_CAMERA,
			Codec: codecs[0], Renditions: []*moqitv1.Rendition{r}}}
	}
	encodings := func(s map[string]*moqitv1.ParticipantInfo) []string {
		var out []string
		if e := s["enc"]; e != nil && len(e.Tracks) == 1 {
			for _, enc := range e.Tracks[0].Renditions[0].Encodings {
				out = append(out, enc.TrackName)
			}
		}
		return out
	}

	p, err := client.Join(ctx, cfg, token(t, "enc", auth.RoomGrant{Join: true}), camera("avc1.42e01f", "vp8"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close("done")
	bob := testclient.Join(t, addr, "r1", "bob", token(t, "bob", auth.RoomGrant{Join: true}))
	bob.WaitState("enc's encodings", func(s map[string]*moqitv1.ParticipantInfo) bool {
		return slices.Equal(encodings(s), []string{"camera-m.avc", "camera-m.vp8"})
	})

	// Only the encoding subscribed to needs encoding.
	vp8 := p.Track("camera-m.vp8")
	sub := fromGroupStart(t, bob, "camera-m.vp8")
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s never true", what)
			}
		}
	}
	waitFor("vp8 subscribed", vp8.Subscribed)
	if p.Track("camera-m.avc").Subscribed() {
		t.Error("avc subscribed without a subscriber")
	}
	vp8.WriteVideo(time.Now(), true, []byte("vp8 k0"))
	var sg *moqt.IncomingSubgroup
	if _, o := next(t, sub, &sg); string(o.Payload) != "vp8 k0" {
		t.Fatalf("got %q", o.Payload)
	}

	// Switching to AV1 drops the H.264 track and keeps VP8's subscribers.
	if err := p.SetTracks(ctx, camera("av01.0.05M.08", "vp8")); err != nil {
		t.Fatal(err)
	}
	bob.WaitState("AV1 first", func(s map[string]*moqitv1.ParticipantInfo) bool {
		return slices.Equal(encodings(s), []string{"camera-m.av1", "camera-m.vp8"}) && s["enc"].Tracks[0].Codec == "av01.0.05M.08"
	})
	if p.Track("camera-m.avc") != nil || p.Track("camera-m.av1") == nil || p.Track("camera-m.vp8") != vp8 {
		t.Fatal("tracks after the switch")
	}
	vp8.WriteVideo(time.Now(), true, []byte("vp8 k1"))
	sg = nil
	if _, o := next(t, sub, &sg); string(o.Payload) != "vp8 k1" {
		t.Fatalf("got %q after the switch", o.Payload)
	}
	if err := p.SetTracks(ctx, append(camera("vp8"), camera("vp8")...)); err == nil {
		t.Error("SetTracks with a repeated track name")
	}
}
