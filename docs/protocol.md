# How a room works over MoQ

moqit speaks [MoQ Transport draft-22](https://datatracker.ietf.org/doc/draft-ietf-moq-transport/) (ALPN `moqt-22`), implemented in [moqt](https://github.com/MoQ-it/moqt). Media uses the [LOC](https://datatracker.ietf.org/doc/draft-ietf-moq-loc/) container and an [MSF](https://datatracker.ietf.org/doc/draft-ietf-moq-msf/) catalog. Everything is a MoQ track, so signalling needs no separate WebSocket:

| Namespace / track | Direction | Content |
|---|---|---|
| `moqit/<room>/p/<identity>` / `mic`, `camera-h\|m\|l`, `screen-h\|l`, `screen-audio` | participant → anyone | media, one track per rendition |
| `moqit/<room>/p/<identity>` / `data`, `data-lossy`, `catalog` | participant → anyone | broadcast data, MSF catalog |
| `moqit/<room>/p/<identity>` / `signal` | participant → server only | `ClientSignal`: metadata, track list, targeted data |
| `moqit/<room>/srv` / `room` | server → room | `RoomStateUpdate`. Each group is a snapshot followed by deltas; subscribe with relative start 1. |
| `moqit/<room>/srv/to/<identity>` / `inbox` | server → one participant | `InboxMessage`: request responses, targeted data |

Messages are defined in `proto/moqit/v1`.

## Joining

1. Connect with the token, either in the SETUP AUTHORIZATION TOKEN option or as `?token=` in the WebTransport URL.
2. Publish your own namespace.
3. Subscribe to `room` and to your `inbox`.
4. Answer the server's SUBSCRIBE for your `signal` track.

The server's own components (`pkg/room`) connect to the node's relay as an ordinary MoQ session over loopback, with a pinned certificate. The relay routes the `srv` namespaces to them like any other publisher.

## Authorization

Every MoQ request is checked by `pkg/room`, which implements the relay's `Authorizer`:
- A participant may publish only its own namespace.
- `signal` tracks are readable only by the server; an `inbox` only by its owner.
- Nothing outside the participant's room is reachable.
- A media track is served only if its publisher's grant allows that source (camera, microphone, screen, screen-audio). The publisher's node checks this for other nodes too, since an edge does not know the grant.
- The same checks apply to `SetTracks` declarations.
- A second session with the same identity replaces the first.

## Viewers

A session with a `viewer` token skips joining: it publishes nothing and has no inbox or signal track. It subscribes to `room`, with relative start 1, and to the participants' tracks it wants. Each `room` subscription starts with a snapshot. A resubscription may be served by another node, whose versions differ, so versions are compared only within one subscription.
