package room

import (
	"fmt"
	"slices"
	"strings"

	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
)

// VideoCodecs are the names a room's video codec policy may list.
var VideoCodecs = []string{"h264", "h265", "vp8", "vp9", "av1"}

// codecTags are the track name suffixes of video codecs: "camera-h.av1".
var codecTags = map[string]string{"h264": "avc", "h265": "hevc", "vp8": "vp8", "vp9": "vp9", "av1": "av1"}

// EncodingTrackName is the MoQ track of a rendition in a codec (a
// WebCodecs codec string or a codec name such as "av1"), e.g.
// "camera-h.av1". The suffix fixes the codec family for the track's life.
func EncodingTrackName(rendition, codec string) string {
	f := VideoCodecOf(codec)
	if f == "" && slices.Contains(VideoCodecs, codec) {
		f = codec
	}
	if f == "" {
		return rendition
	}
	return rendition + "." + codecTags[f]
}

// VideoCodecOf maps a WebCodecs codec string to its family ("h264",
// "h265", "vp8", "vp9", "av1"); "" if it is not a known video codec.
func VideoCodecOf(codec string) string {
	c := strings.ToLower(codec)
	switch {
	case strings.HasPrefix(c, "avc1"), strings.HasPrefix(c, "avc3"):
		return "h264"
	case strings.HasPrefix(c, "hev1"), strings.HasPrefix(c, "hvc1"):
		return "h265"
	case c == "vp8":
		return "vp8"
	case c == "vp9", strings.HasPrefix(c, "vp09"):
		return "vp9"
	case strings.HasPrefix(c, "av01"):
		return "av1"
	}
	return ""
}

// CheckVideoCodecs validates a room's video codec policy.
func CheckVideoCodecs(codecs []string) error {
	for _, c := range codecs {
		if !slices.Contains(VideoCodecs, c) {
			return fmt.Errorf("unknown video codec %q; known: %s", c, strings.Join(VideoCodecs, ", "))
		}
	}
	return nil
}

// Encodings returns a rendition's encodings; without any, the rendition
// itself in the track's codec.
func Encodings(t *moqitv1.TrackInfo, rd *moqitv1.Rendition) []*moqitv1.VideoEncoding {
	if len(rd.Encodings) > 0 {
		return rd.Encodings
	}
	return []*moqitv1.VideoEncoding{{TrackName: rd.TrackName, Codec: t.Codec}}
}

// checkEncodings validates a video track's encodings against the room's
// policy. seen collects MoQ track names across all of a participant's tracks.
func checkEncodings(t *moqitv1.TrackInfo, allowed []string, seen map[string]bool) error {
	for _, rd := range t.Renditions {
		if len(rd.Encodings) > 0 && (rd.Encodings[0].TrackName != rd.TrackName || rd.Encodings[0].Codec != t.Codec) {
			return fmt.Errorf("rendition %q: the first encoding must be the rendition's track in the track's codec", rd.TrackName)
		}
		for _, e := range Encodings(t, rd) {
			if seen[e.TrackName] {
				return fmt.Errorf("track name %q is used twice", e.TrackName)
			}
			seen[e.TrackName] = true
			if class, src := classifyTrack(e.TrackName); class != classMedia || src != t.Source {
				return fmt.Errorf("rendition track %q does not match source %v", e.TrackName, t.Source)
			}
			if t.Kind != moqitv1.TrackKind_TRACK_KIND_VIDEO || len(allowed) == 0 {
				continue
			}
			if f := VideoCodecOf(e.Codec); !slices.Contains(allowed, f) {
				return fmt.Errorf("video codec %q is not allowed in this room (allowed: %s)", e.Codec, strings.Join(allowed, ", "))
			}
		}
	}
	return nil
}
