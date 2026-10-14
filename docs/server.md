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

In production, pass `-keys key:secret` and `-cert`/`-key`. Run `moqit-server -h` for all flags.

## Access tokens

Tokens are HS256 JWTs signed with an API secret: `iss` is the API key, `sub` the participant identity, and `grants.room` holds the permissions (`join`, `room`, `publish`, `subscribe`, `publishData`, `publishSources`, `updateOwnMetadata`, `hidden`, and `create` / `list` / `admin` for the server API). Mint them with `moqit token` or the Go SDK:

```go
import "github.com/MoQ-it/moqit/pkg/auth"

tok, err := auth.NewAccessToken(key, secret).SetIdentity("alice").SetName("Alice").
	SetRoomGrant(&auth.RoomGrant{Join: true, Room: "standup"}).ToJWT()
```

A second session with the same identity replaces the first.

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

## Webhooks

`moqit-server -webhook https://app.example.com/hook`

- **Events:** `room_started`, `room_finished`, `participant_joined`, `participant_left`, `track_published`, `track_unpublished`.
- **Format:** a JSON `WebhookEvent` POST, delivered in order with retries.
- **Signature:** the `Moqit-Signature` header is a JWT whose `sha256` claim is the body's hash; verify it with `moqit.ReceiveWebhook(r, keys)`.
- **Once per event:** `room_started` and `room_finished` are sent once cluster-wide.
