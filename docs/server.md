# Server, API and webhooks

## Running a node

```sh
go run ./cmd/moqit-server -dev            # UDP :4443 (WebTransport /moq + raw QUIC), HTTP :7880
curl 'http://localhost:7880/v1/dev/token?room=demo&identity=alice'
go run ./cmd/moqit token -room demo -identity bob -sources microphone
```

`-dev` does three things:
- adds the API key `devkey` with a fixed development secret;
- creates a self-signed 13-day certificate and logs its SHA-256, for WebTransport `serverCertificateHashes`;
- enables `/v1/dev/token`.

In production, pass `-keys key:secret` and `-cert`/`-key`. `-max-viewers` caps viewer sessions on a node, `-edge` makes it an edge node and `-tier` places an edge in a cascade (see [clustering.md](clustering.md#edge-nodes-and-viewers)). The certificate files are re-read when they change, so a renewed certificate (e.g. from an ACME client) needs no restart; until they exist the server waits for them. Run `moqit-server -h` for all flags.

### Rewind (DVR)

With `-dvr-dir`, a node records the rooms that keep history, and their viewers can rewind them (see [protocol.md](protocol.md#rewind)):

| Flag | Default | |
|---|---|---|
| `-dvr-dir` | (off) | where recordings are stored (env `MOQIT_DVR_DIR`); the store survives restarts |
| `-dvr-window` | `0` | history kept by rooms created without a `dvr_window`, e.g. by their first participant; 0 keeps none |
| `-dvr-max-window` | `6h` | the most a room may keep |
| `-dvr-max-bytes` | `0` | disk cap; the oldest segments go first; 0 is no cap |
| `-dvr-max-height` | `720` | the video rendition recorded: the largest at most this high |
| `-dvr-fetch-limit` | `4` | concurrent FETCHes per viewer; applies on edges too, with or without `-dvr-dir` |

```sh
go run ./cmd/moqit-server -dev -dvr-dir /var/lib/moqit/dvr -dvr-window 1h
```

Disk use follows the recorded bitrates: with the browser's 720p camera layer (1.7 Mbps) and microphone, about 0.8 GB per participant-hour (an estimate from the bitrates, not a measurement). Old history is deleted every 30 seconds.

### Troubleshooting connections

`-qlog-dir <dir>` writes a [qlog](https://datatracker.ietf.org/doc/draft-ietf-quic-qlog-main-schema/) file of every QUIC frame for each connection the node accepts, named `<connection id>_server.sqlog`. Each file grows with its connection's traffic (about 7 MB per minute of a 720p viewer), so turn it on only while reproducing a problem. The files open in [qvis](https://qvis.quictools.info/). With the Docker deployment, whose filesystem is read-only, add a writable mount to the `moqit` service for the duration:

```yaml
    command:
      - -qlog-dir=/qlog
      # … the existing flags
    volumes:
      - ./qlog:/qlog
```

## Access tokens

Tokens are HS256 JWTs signed with an API secret: `iss` is the API key, `sub` the participant identity, and `grants.room` holds the permissions (`join`, `room`, `publish`, `subscribe`, `publishData`, `publishSources`, `updateOwnMetadata`, `hidden`, `viewer`, and `create` / `list` / `admin` for the server API).

A `viewer` grant watches `room` read-only, without joining: see [clustering.md](clustering.md#edge-nodes-and-viewers). The identity is optional for viewers. Mint them with `moqit token` or the Go SDK:

```go
import "github.com/MoQ-it/moqit/pkg/auth"

tok, err := auth.NewAccessToken(key, secret).SetIdentity("alice").SetName("Alice").
	SetRoomGrant(&auth.RoomGrant{Join: true, Room: "standup"}).ToJWT()
```

A second session with the same identity replaces the first.

```sh
go run ./cmd/moqit token -viewer -room standup     # a read-only viewer
```

A participant token is also a stream key for the [RTMP ingress](https://github.com/MoQ-it/moqit-ingress), which publishes an OBS stream into the room as that participant:

```sh
go run ./cmd/moqit token -room standup -identity obs -sources camera,microphone -valid 720h
```

## RoomService

`moqit.v1.RoomService` (`proto/moqit/v1/room_service.proto`) is served on each node's HTTP port over [Connect](https://connectrpc.com): JSON or protobuf over HTTP POST, and gRPC. Authenticate with an access token carrying the right grant:

| Methods | Grant needed |
|---|---|
| CreateRoom, DeleteRoom | `create` |
| ListRooms | `list` |
| UpdateRoomMetadata, List/Get/Remove/UpdateParticipant, MuteTrack, SendData | `admin` for the room (or every room, when the grant names none) |

```sh
TOK=$(go run ./cmd/moqit token -create -list -admin)
curl -XPOST localhost:7880/moqit.v1.RoomService/CreateRoom -H "Authorization: Bearer $TOK" \
  -H 'Content-Type: application/json' -d '{"name":"standup","maxParticipants":10}'
```

```go
import moqit "github.com/MoQ-it/moqit/sdk/go"

rooms := moqit.NewRoomServiceClient("http://localhost:7880", key, secret) // signs each call
res, err := rooms.ListParticipants(ctx, connect.NewRequest(&moqitv1.ListParticipantsRequest{Room: "standup"}))
```

- **Across a cluster:** reads use the shared registry, and changes go to the node hosting the participant.
- `RemoveParticipant` disconnects the participant.
- `MuteTrack` flags the track and tells the publisher's SDK to stop sending (`localTrackMutedByServer` in the browser).
- `SendData` delivers to participants' inboxes, from the server.
- `DeleteRoom` disconnects everyone.
- `Room.num_viewers` counts the room's viewers on every node. It is refreshed every few seconds; viewers are not participants.
- `CreateRoom` takes `dvr_window`: seconds of history the room keeps for viewers to rewind. 0 means the server's `-dvr-window`, and the value is capped at `-dvr-max-window`. `Room.dvr_window` reports it. It is fixed when the room is created, and is 0 on a server without `-dvr-dir`.

```sh
curl -XPOST localhost:7880/moqit.v1.RoomService/CreateRoom -H "Authorization: Bearer $TOK" \
  -H 'Content-Type: application/json' -d '{"name":"town-hall","dvrWindow":7200}'
```

## Webhooks

`moqit-server -webhook https://app.example.com/hook`

- **Events:** `room_started`, `room_finished`, `participant_joined`, `participant_left`, `track_published`, `track_unpublished`. Viewers send none.
- **Format:** a JSON `WebhookEvent` POST, delivered in order with retries.
- **Signature:** the `Moqit-Signature` header is a JWT whose `sha256` claim is the body's hash; verify it with `moqit.ReceiveWebhook(r, keys)`.
- **Once per event:** `room_started` and `room_finished` are sent once cluster-wide.

## Joining from Go

`sdk/go/client` joins a room as a participant over raw QUIC and publishes media, as the browser SDK does: for services that bring media from outside the browser, such as the [RTMP ingress](https://github.com/MoQ-it/moqit-ingress). `Join` takes a participant token and the tracks to declare, and gives a `Track` for each rendition:

```go
import "github.com/MoQ-it/moqit/sdk/go/client"

p, err := client.Join(ctx, client.Config{URL: "https://moqit.example.com:4443/moq"}, token, []*moqitv1.TrackInfo{{
	Name: "microphone", Kind: moqitv1.TrackKind_TRACK_KIND_AUDIO, Source: moqitv1.TrackSource_TRACK_SOURCE_MICROPHONE,
	Codec: "opus", Renditions: []*moqitv1.Rendition{{TrackName: "mic"}},
}})
mic := p.Track("mic")
mic.WriteAudio(time.Now(), level, opusFrame) // each 20 ms Opus frame, with its RFC 6464 level
<-p.Context().Done() // removed, replaced by another session, or disconnected
p.Close("done")
```

- **Where it connects:** `Config.URL` is a node's WebTransport URL; the session uses raw QUIC on the same host and port, verified, pinned (`CertHash`) or unverified (`Insecure`). With `RouteURL`, a node's `/v1/route`, it joins the node hosting the room.
- **Media:** a video Track takes Annex B access units with `WriteVideo(captured, keyframe, au)`, and a keyframe starts a Group. Objects carry the LOC capture timestamp, and audio its level. A track keeps its current Group, and a new subscriber starts from its keyframe, so the publisher needs no keyframes on demand.
- **Server commands:** `RoomService.MuteTrack` stops the track's renditions and declares it muted.
- **Logs:** with `Config.Log`, each track subscribed, paused, resumed or unsubscribed is logged with the tracks being sent, and an unsubscribe with the objects and bytes sent.
