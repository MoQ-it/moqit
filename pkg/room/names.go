package room

import (
	"strings"

	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	v22 "github.com/MoQ-it/moqt/wire/v22"
)

// MoQ namespace layout of a room:
//
//	("moqit", room, "p", identity)          a participant's own tracks
//	("moqit", room, "srv")                  room-wide server tracks ("room")
//	("moqit", room, "srv", "to", identity)  per-participant server tracks ("inbox")
const (
	nsRoot   = "moqit"
	nsPeer   = "p"
	nsServer = "srv"
	nsTo     = "to"
)

// Track names.
const (
	TrackRoom      = "room"   // server: RoomStateUpdate objects
	TrackInbox     = "inbox"  // server: InboxMessage objects for one participant
	TrackSignal    = "signal" // participant: ClientSignal objects, read only by the server
	TrackCatalog   = "catalog"
	TrackData      = "data"
	TrackDataLossy = "data-lossy"
	TrackMic       = "mic"
	TrackScreenAud = "screen-audio"
	// Camera and screen video use one track per rendition: "camera-h",
	// "camera-m", "screen-h", ...
	prefixCamera = "camera"
	prefixScreen = "screen"
)

func RoomNamespace(room string) v22.Namespace { return v22.Namespace{nsRoot, room} }
func ParticipantNamespace(room, identity string) v22.Namespace {
	return v22.Namespace{nsRoot, room, nsPeer, identity}
}
func ServerNamespace(room string) v22.Namespace { return v22.Namespace{nsRoot, room, nsServer} }
func InboxNamespace(room, identity string) v22.Namespace {
	return v22.Namespace{nsRoot, room, nsServer, nsTo, identity}
}

// nsKind classifies a namespace.
type nsKind int

const (
	nsUnknown nsKind = iota
	nsParticipant
	nsRoomServer
	nsInbox
)

// parseNamespace returns the kind, room and (for participant and inbox
// namespaces) the identity.
func parseNamespace(ns v22.Namespace) (kind nsKind, room, identity string) {
	if len(ns) < 3 || ns[0] != nsRoot {
		return nsUnknown, "", ""
	}
	room = ns[1]
	switch {
	case len(ns) == 4 && ns[2] == nsPeer:
		return nsParticipant, room, ns[3]
	case len(ns) == 3 && ns[2] == nsServer:
		return nsRoomServer, room, ""
	case len(ns) == 5 && ns[2] == nsServer && ns[3] == nsTo:
		return nsInbox, room, ns[4]
	}
	return nsUnknown, room, ""
}

// trackClass is what a participant track name carries.
type trackClass int

const (
	classUnknown trackClass = iota
	classMedia
	classData
	classCatalog
	classSignal
)

// classifyTrack maps a participant track name to its class and, for media,
// its source.
func classifyTrack(name string) (trackClass, moqitv1.TrackSource) {
	switch {
	case name == TrackSignal:
		return classSignal, 0
	case name == TrackCatalog:
		return classCatalog, 0
	case name == TrackData || name == TrackDataLossy:
		return classData, 0
	case name == TrackMic:
		return classMedia, moqitv1.TrackSource_TRACK_SOURCE_MICROPHONE
	case name == TrackScreenAud:
		return classMedia, moqitv1.TrackSource_TRACK_SOURCE_SCREEN_AUDIO
	case name == prefixCamera || strings.HasPrefix(name, prefixCamera+"-"):
		return classMedia, moqitv1.TrackSource_TRACK_SOURCE_CAMERA
	case name == prefixScreen || strings.HasPrefix(name, prefixScreen+"-"):
		return classMedia, moqitv1.TrackSource_TRACK_SOURCE_SCREEN
	}
	return classUnknown, 0
}
