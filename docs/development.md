# Development

Requirements: Go 1.26+, Node 22+, and Google Chrome and/or Firefox for the browser tests.

[moqt](https://github.com/MoQ-it/moqt) is not published yet: `go.mod` has `replace github.com/MoQ-it/moqt => ../moqt`, and the web workspace links its TypeScript package, so clone it next to this repository:

```sh
git clone https://github.com/MoQ-it/moqt
git clone https://github.com/MoQ-it/moqit
cd moqit && (cd web && npm install)
```

## Tests

```sh
go test -race ./...                                  # unit and integration (real QUIC, miniredis)
(cd web && npm test && npm run typecheck)            # TypeScript unit tests and types
go test -tags browser ./test/e2e                     # SDK in headless Chrome and Firefox
(cd web && npm run build -w meet && npm run e2e -w meet)  # Meet UI, Playwright + system Chrome
```

- `pkg/server` runs nodes against miniredis: cross-node presence, media, metadata and direct data, duplicate identities, rejoining, idle timeouts, node shutdown and routing.
- `test/e2e` runs two participants in real browsers (fake devices) against a real node, on a single node and across two clustered nodes: join, camera and mic, lip sync, quality switching, pausing hidden video, data, server-side mute, metadata and leave. On localhost, capture-to-arrival latency is about 1–7 ms.
- The Meet UI test is described in [meet.md](meet.md#developing).

## Protocol messages

The protos in `proto/moqit/v1` generate the Go code, the Connect service and the TypeScript types. Run `npm install` in `web/` first (buf uses its `protoc-gen-es`), then:

```sh
go run github.com/bufbuild/buf/cmd/buf@v1.73.0 generate
```
