# Development

Requirements: Go 1.26+, Node 22+, and Google Chrome and/or Firefox for the browser tests.

[moqt](https://github.com/MoQ-it/moqt) comes from GitHub at the commit pinned in `go.mod`. Its TypeScript package is not on npm yet, so the web workspace links it from a checkout next to this repository:

```sh
git clone https://github.com/MoQ-it/moqt
git clone https://github.com/MoQ-it/moqit
cd moqit && (cd web && npm install)
```

To work on both repositories at once, use the sibling checkout for Go too, with a `go.work` (ignored by git and by the Docker build):

```sh
go work init . ../moqt
```

Once moqt changes are pushed, pin them, and check that moqit builds and passes its tests without the workspace:

```sh
GOWORK=off go get github.com/MoQ-it/moqt@<commit> && GOWORK=off go mod tidy
GOWORK=off go test -race ./...
```

The Docker image (`deploy/Dockerfile`, built from this directory alone) takes both the Go module and its TypeScript package from the pinned version.

## Tests

```sh
go test -race ./...                                  # unit and integration (real QUIC, miniredis)
(cd web && npm test && npm run typecheck)            # TypeScript unit tests and types
go test -tags browser ./test/e2e                     # SDK in headless Chrome and Firefox
(cd web && npm run build -w meet && npm run e2e -w meet)  # Meet UI, Playwright + system Chrome
```

- `pkg/server` runs nodes against miniredis:
  - cross-node presence, media, metadata and direct data;
  - duplicate identities, rejoining, idle timeouts and node shutdown;
  - routing, including viewers to edges;
  - viewers (permissions, the viewer cap, viewer counts);
  - edge nodes, including the room and a publisher moving to another node;
  - cascaded edges, including failover when a tier 1 edge goes away.
- `sdk/go/client` joins a real node as a participant: declaring tracks, a subscriber starting from the cached keyframe, audio levels, `MuteTrack`, `RemoveParticipant` and leaving.
- `test/e2e` runs two participants and a viewer in real browsers (fake devices) against a real node, on a single node and across two clustered nodes with the viewer on an edge. It covers join, camera and mic, lip sync, quality switching, pausing hidden video, data, server-side mute, metadata and leave, plus the viewer receiving video and audio without being a participant. On localhost, capture-to-arrival latency is about 1–7 ms.
- `internal/bench` runs the load generator against an origin and an edge.

## Load testing

`moqit bench` connects headless viewers over raw QUIC. Each viewer follows the room track, a participant's microphone and a camera rendition, and the tool reports throughput, capture-to-arrival latency, group resets and failures every few seconds. With `-publish`, a synthetic publisher joins with 64 kbps audio and three video renditions (1.5 Mbps, 600 kbps, 200 kbps), so no browser is needed.

```sh
moqit bench -route http://node1:7880/v1/route -room town-hall -n 2000 -rate 200 -duration 2m -publish -video medium
moqit bench -url https://edge1.example.com:4443/moq -room town-hall -publisher alice -n 500   # watch a real participant
```

- **Routing:** with `-route`, every session is routed as a browser's would be, so viewers spread over the edges.
- **Latency:** it uses the LOC capture timestamp, so it is only meaningful when the clocks of publisher and viewers agree.
- **Placement:** run the client on machines other than the edges, so that its own CPU use does not distort the result. [clustering.md](clustering.md#capacity) has results from one machine.
- The Meet UI test is described in [meet.md](meet.md#developing).

## Protocol messages

The protos in `proto/moqit/v1` generate the Go code, the Connect service and the TypeScript types. Run `npm install` in `web/` first (buf uses its `protoc-gen-es`), then:

```sh
go run github.com/bufbuild/buf/cmd/buf@v1.73.0 generate
```
