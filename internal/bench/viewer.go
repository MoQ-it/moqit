package bench

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/MoQ-it/moqit/pkg/room"
	"github.com/MoQ-it/moqt"
	v22 "github.com/MoQ-it/moqt/wire/v22"
)

var fromCurrentGroup = &v22.Params{LocationFilter: &v22.LocationFilter{Type: v22.FilterRelativeStart, StartGroup: 1}}

// watch runs one viewer until ctx ends: it follows the room track, the
// publisher's microphone and a camera rendition, as the SDK would.
func (b *bench) watch(ctx context.Context, identity string) error {
	tok, err := b.token(identity, true)
	if err != nil {
		return err
	}
	sess, err := b.dial(ctx, tok, moqt.RejectAll{})
	if err != nil {
		return err
	}
	defer sess.Close()
	b.stats.connect(1)
	defer b.stats.connect(-1)

	ns := room.ParticipantNamespace(b.cfg.Room, b.cfg.Publisher)
	go b.follow(ctx, sess, v22.FullTrackName{Namespace: room.ServerNamespace(b.cfg.Room), Name: room.TrackRoom}, false)
	go b.follow(ctx, sess, v22.FullTrackName{Namespace: ns, Name: room.TrackMic}, true)
	if r, ok := renditions[b.cfg.Video]; ok {
		go b.follow(ctx, sess, v22.FullTrackName{Namespace: ns, Name: r}, true)
	}
	select {
	case <-ctx.Done():
		return nil
	case <-sess.Context().Done():
		return errors.New("session closed: " + context.Cause(sess.Context()).Error())
	}
}

// follow subscribes to a track and again whenever the subscription fails
// or ends, counting what arrives.
func (b *bench) follow(ctx context.Context, sess *moqt.Session, track v22.FullTrackName, media bool) {
	for ctx.Err() == nil && sess.Context().Err() == nil {
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		sub, err := sess.Subscribe(sctx, track, fromCurrentGroup)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				b.stats.subscribeFailed()
			}
			sleep(ctx, time.Second)
			continue
		}
		for {
			g, err := sub.AcceptSubgroup(ctx)
			if err != nil {
				break
			}
			go b.read(ctx, g, media)
		}
		if ctx.Err() != nil {
			return
		}
		b.stats.ended()
		sleep(ctx, 500*time.Millisecond)
	}
}

func (b *bench) read(ctx context.Context, g *moqt.IncomingSubgroup, media bool) {
	for {
		o, err := g.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			if ctx.Err() == nil {
				b.stats.reset()
			}
			return
		}
		var latency time.Duration = -1
		if kv, ok := o.Properties.Lookup(locTimestamp); ok && media {
			latency = time.Duration(time.Now().UnixMicro()-int64(kv.Int)) * time.Microsecond
		}
		b.stats.object(len(o.Payload), latency)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
