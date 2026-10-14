# moqit

**Real-time video conferencing over Media over QUIC.**

moqit is a self-hostable platform for rooms with live audio, video, screen sharing and data, in the style of [LiveKit](https://livekit.io) — but built on the IETF's [Media over QUIC](https://datatracker.ietf.org/wg/moq/about/) (MoQ) instead of WebRTC. Browsers connect over WebTransport; a Go server relays tracks between participants, and nodes relay to each other the same way to form a cluster.

It comes with a browser SDK, React components, a server API with webhooks, a Go server SDK, and **Meet**, a ready-to-run conferencing web app.

> **Status: experimental.** moqit implements MoQ Transport draft-22, which is still changing. It works in Chromium and Firefox and is tested end to end in both, but is not yet verified on Safari and has not been tested over real-world lossy networks or at scale. Expect breaking changes.

## Why MoQ?

- **One protocol for everything.** Media, signalling and node-to-node relaying are all MoQ tracks over QUIC — no separate signalling socket, no RTP stack.
- **Built to fan out.** MoQ relays forward named tracks like a CDN: one upstream subscription per track, however many viewers, across any number of nodes.
- **Latency over completeness.** Each group of frames travels on its own QUIC stream, so a lost packet delays only that group, and a viewer who falls behind skips ahead instead of queueing old video.
- **Simple to run.** One Go binary, one UDP port for WebTransport and node links, and Redis only when you cluster.

## Features

- **Rooms, participants and tracks** with JWT access tokens and fine-grained grants
- **Camera, microphone and screen sharing** (with tab or system audio), encoded with WebCodecs
- **Simulcast** — up to three layers per camera, each encoded only while someone watches it
- **Adaptive stream** — each viewer receives the layer that fits its video's size, and pauses hidden video
- **Send and receive quality control** — 180p to 1080p, switched live
- **Lip-synced playout** with a jitter buffer; active speaker detection on the server
- **Reliable and lossy data** to everyone or to specific participants, and room chat
- **Device switching** of microphone, camera and speaker without republishing
- **Track processors** for effects such as background blur and noise removal
- **Server API** (Connect: JSON, protobuf or gRPC) and signed **webhooks**
- **Clustering** — a room can span nodes; Redis for state, MoQ between nodes
- **Meet** — a complete conferencing app with chat, presenting, effects, devices and stats

## Quick start

Requirements: Go 1.26+, Node 22+, and Chrome or Firefox. moqit uses the [moqt](https://github.com/MoQ-it/moqt) library: Go fetches the version pinned in `go.mod`, and the web app links its TypeScript package from a sibling checkout until that is on npm:

```sh
git clone https://github.com/MoQ-it/moqt
git clone https://github.com/MoQ-it/moqit
cd moqit
(cd web && npm install && npm run build -w meet)
go run ./cmd/meet -dev
```

Open http://localhost:8080, start a meeting, and open the copied link in a second window. `-dev` runs a moqit node in the same process with a self-signed certificate and a development API key.

## How it works

```
 Browser (@moqit/client)                         Your backend
   │  1. get a token  ────────────────────────►  mints a JWT (Go SDK or `moqit token`)
   │  2. WebTransport, MoQ draft-22
   ▼
 moqit-server node A  ◄═══ MoQ over QUIC ═══►  node B  ◄──  other browsers
   relay · rooms · auth · API · webhooks           │
   └───────────────── Redis (cluster state) ───────┘
```

Every participant publishes its media as MoQ tracks under its own namespace (`moqit/<room>/p/<identity>`), one track per simulcast layer. The server publishes room state on a `room` track and private replies on each participant's `inbox`; participants send requests on a `signal` track only the server may read. Subscribing to someone's camera is just a MoQ SUBSCRIBE — the relay fetches it once, from whichever node the publisher is on, and fans it out. See [docs/protocol.md](docs/protocol.md).

## Using moqit

**Mint a token** on your backend:

```go
tok, err := auth.NewAccessToken(apiKey, apiSecret).SetIdentity("alice").SetName("Alice").
	SetRoomGrant(&auth.RoomGrant{Join: true, Room: "standup"}).ToJWT()
```

**Join from the browser:**

```ts
import { Room, TrackSource } from "@moqit/client";

const room = new Room();
room.on("trackSubscribed", (track, pub) => {
  if (pub.source === TrackSource.CAMERA) document.body.append(track.attach());
});
await room.connect("https://moqit.example.com:4443/moq", token);
await room.localParticipant.setCameraEnabled(true);
await room.localParticipant.setMicrophoneEnabled(true);
```

**Or use React:**

```tsx
<RoomProvider room={room}>
  <ConferenceLayout />
  <ControlBar onLeave={leave} />
</RoomProvider>
```

**Manage rooms** from your server:

```go
rooms := moqit.NewRoomServiceClient("http://localhost:7880", apiKey, apiSecret)
_, err := rooms.RemoveParticipant(ctx, connect.NewRequest(&moqitv1.RemoveParticipantRequest{Room: "standup", Identity: "alice"}))
```

## Running a server

```sh
go run ./cmd/moqit-server -dev                       # try it locally
moqit-server -keys key:secret -cert cert.pem -key key.pem -webhook https://app.example.com/hook
```

A node listens on one UDP port (WebTransport at `/moq`, plus raw QUIC for other nodes) and an HTTP port for the API. Add `-redis` to join a cluster — see [docs/clustering.md](docs/clustering.md).

**Put Meet on the internet** with Docker Compose: Caddy obtains a Let's Encrypt certificate, and the node, which re-reads it on renewal, serves media on UDP. See [docs/deploy.md](docs/deploy.md).

## Browser support

| Browser | Status |
|---|---|
| Chrome, Edge and other Chromium browsers | Supported, tested end to end |
| Firefox | Supported, tested end to end (VP8 video) |
| Safari 26.4+ | Not yet verified: a fix for joining awaits testing on macOS |

WebTransport is required; there is no WebSocket fallback.

## Documentation

- [Meet](docs/meet.md) — the conferencing app and its features
- [Browser SDK](docs/browser-sdk.md) — `@moqit/client` and `@moqit/react`
- [Server, API and webhooks](docs/server.md) — running a node, tokens, `RoomService`, webhooks
- [Deploying Meet](docs/deploy.md) — Docker Compose with HTTPS, on your own server
- [Clustering](docs/clustering.md) — multi-node rooms, routing, Redis
- [How a room works over MoQ](docs/protocol.md) — namespaces, tracks and authorization
- [Development](docs/development.md) — building, tests and code generation

## Repository layout

| Path | Contents |
|---|---|
| `cmd/moqit-server` | the server |
| `cmd/meet` | the Meet app's backend: serves the web app and mints tokens |
| `cmd/moqit` | CLI (`moqit token`) |
| `pkg/` | rooms, auth, cluster, server and webhook packages |
| `sdk/go` | Go server SDK |
| `proto/moqit/v1` | protocol and API definitions |
| `web/packages/client`, `web/packages/react` | browser SDK and React components |
| `web/apps/meet` | the Meet web app |
| `test/e2e` | browser end-to-end tests |

## Contributing

Issues and pull requests are welcome. Before sending a change, run the tests described in [docs/development.md](docs/development.md): `go test -race ./...`, the TypeScript tests, and the browser tests for anything that touches media or the UI.

## Acknowledgements

moqit's API is modelled on [LiveKit](https://livekit.io)'s. It builds on the work of the IETF [MoQ working group](https://datatracker.ietf.org/wg/moq/about/), [quic-go](https://github.com/quic-go/quic-go) and [webtransport-go](https://github.com/quic-go/webtransport-go). Meet's effects use [Longpipe](https://github.com/sb2702/longpipe).
