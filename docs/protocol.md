# How a room works over MoQ

moqit speaks [MoQ Transport draft-22](https://datatracker.ietf.org/doc/draft-ietf-moq-transport/) (ALPN `moqt-22`), implemented in [moqt](https://github.com/MoQ-it/moqt). Media uses the [LOC](https://datatracker.ietf.org/doc/draft-ietf-moq-loc/) container and an [MSF](https://datatracker.ietf.org/doc/draft-ietf-moq-msf/) catalog. Everything is a MoQ track, so signalling needs no separate WebSocket:

| Namespace / track | Direction | Content |
|---|---|---|
| `moqit/<room>/p/<identity>` / `mic`, `camera-h\|m\|l`, `screen-h\|l`, `screen-audio` | participant → anyone | media, one track per rendition |
| `moqit/<room>/p/<identity>` / `data`, `data-lossy`, `catalog` | participant → anyone | broadcast data, MSF catalog |
| `moqit/<room>/p/<identity>` / `signal` | participant → server only | `ClientSignal`: metadata, track list, targeted data |
| `moqit/<room>/p/<identity>` / `dvr` | server → viewers, FETCH only | `DvrIndex`: the participant's recorded history, one Group per minute (see [Rewind](#rewind)) |
| `moqit/<room>/srv` / `room` | server → room | `RoomStateUpdate`. Each group is a snapshot followed by deltas; subscribe with relative start 1. |
| `moqit/<room>/srv/to/<identity>` / `inbox` | server → one participant | `InboxMessage`: request responses, targeted data, `SendFeedback` on how its media arrives |

Messages are defined in `proto/moqit/v1`.

A participant declares its tracks in `SetTracks`. Each `TrackInfo` lists its renditions, largest first, and their MoQ track names. A camera may have any number of renditions named `camera-<anything>`: browsers use `camera-h|m|l`, the [RTMP ingress](https://github.com/MoQ-it/moqit-ingress) `camera-<height>`. For audio, `TrackInfo.channels` is 2 for stereo; 0 or 1 is mono.

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

## Rewind

A room created with a `dvr_window` keeps that many seconds of history, and viewers can rewind it. It needs a node with `-dvr-dir`.

**Recording.** The node hosting a participant records, for each source, one rendition: every audio track, and for video the largest rendition at most `-dvr-max-height` high. It holds an upstream subscription to each recorded track, so the publisher keeps encoding that rendition even when nobody watches it. Objects go to disk in Group order with their server receive time; datagrams are not recorded.

**Epochs and archive Group IDs.** Each continuous recording of a track is an epoch, numbered uniquely across the cluster. In the archive, a Group's ID is `epoch << 32 | publisher Group`. These IDs never collide across a participant's sessions, and stay fixed as long as the Group is stored. Archive IDs are used only with FETCH; live subscriptions keep the publisher's IDs.

**The index.** Each participant namespace has a fetch-only `dvr` track. Group *m* holds one Object, a `DvrIndex` for unix minute *m*:
- `checkpoints`: Group starts, at most one per second per track. Each has the archive Group ID, the server time `at` and the capture time `loc` (from the LOC timestamp), both in µs.
- `epochs`: the codec, size and channels of the epochs those checkpoints belong to.
- `earliest`, `latest`: the server times of the oldest and newest stored media of the participant.

Subscribing to `dvr` is refused.

**Playing back.** A viewer FETCHes index minutes, picks the checkpoint at or before the time it wants, and FETCHes the media track by an absolute range of archive Group IDs:
- FETCH needs an absolute `LOCATION_FILTER`: start, or start and end.
- Server time lines participants up. Within one participant, the LOC timestamps (shifted by the checkpoint's `at − loc`) pace playback.
- The browser SDK fetches about 10 seconds per FETCH and stays at most 20 seconds ahead.

**Authorization.**
- Only viewers may FETCH, and only media and `dvr` tracks of their own room's participants, while the room keeps history.
- Participants are refused.
- Other nodes may FETCH, to forward their viewers' requests.
- `-dvr-fetch-limit` caps a viewer's concurrent FETCHes (EXCESSIVE_LOAD beyond it).

**Clusters.** A FETCH the node can't serve is forwarded to the node storing it. A redis roster maps each epoch to its node and start time:
- Media goes by the epoch in its Group ID.
- An index range goes to the node of the participant's latest epoch that started by the range's end.
- Edges below tier 1 forward through their parent edge.

History outlives the participant: it stays fetchable after they leave or move nodes, until the room's window passes. After a move, the index minute in which the move happened lists only the new node's checkpoints. The media of both nodes stays fetchable.
