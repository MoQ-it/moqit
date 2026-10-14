# Clustering

Nodes that share a Redis form a cluster, and a room spans nodes: participants join whichever node they are routed to.

```sh
moqit-server -redis redis://redis:6379/0 -node-id node1 -advertise 10.0.0.1:4443 \
  -public-url https://node1.example.com:4443/moq -keys key:secret -cert … -key …
```

- **Control plane (Redis, `pkg/cluster`):**
  - **Node registry:** a heartbeat with a TTL.
  - **Participants:** each room has a participant registry and an event stream that every node hosting the room follows. Remote participants appear in each node's `room` track.
  - **Direct messages** to a participant on another node go over pub/sub.
  - **Cleanup:** removals check the session id atomically in a Lua script, so a dead node's participants are reaped by the survivors exactly once.
- **Data plane (MoQ):** when Bob on node B subscribes to Alice's track on node A, B's relay subscribes to A once and fans the track out locally. Node-to-node sessions use raw QUIC with a server-grant token, pinned to each node's certificate hash from the registry.
- **Identities:** with the same identity on two nodes, the newer session wins.
- **Routing:** `GET /v1/route?token=…` on any node's HTTP port returns the node already hosting the token's room, otherwise the least loaded. Viewer tokens go to edges (below). `meet -route-url` uses it.
- **Development clusters:** with `-dev`, routing also returns the node's self-signed certificate hash.
- **Docker:** `deploy/docker-compose.yml` runs Redis, two nodes, two edges and Meet for development, with self-signed certificates. It has been checked with `moqit bench` from the host: viewers were routed to both edges, participants to a node, and media flowed. Meet in a browser has not been tried against it. For a single node on the internet, see [deploy.md](deploy.md).

## Edge nodes and viewers

For large audiences, put viewers on **edge nodes**: `moqit-server -edge` serves read-only viewers only, and fetches every track from the node hosting it.

```sh
moqit-server -edge -max-viewers 5000 -redis redis://redis:6379/0 -node-id edge1 -region eu \
  -advertise 10.0.1.1:4443 -public-url https://edge1.example.com:4443/moq -keys key:secret -cert … -key …
moqit token -viewer -room town-hall          # or RoomGrant{Viewer: true, Room: "town-hall"}
```

- **Viewers:** a viewer token (`viewer` grant) never makes a participant. A viewer is left out of the registry, room state, `MaxParticipants` and webhooks, and is only counted, in `Room.num_viewers`.
  - **May subscribe to:** the room track and the participants' media, `data` and `catalog` tracks.
  - **May not:** publish, read inboxes, use SUBSCRIBE_NAMESPACE, or receive server-to-participant inbox data.
  - **Where they connect:** any node accepts viewers. An edge accepts only viewers and other nodes.
- **Fan-out:** an edge subscribes to each track once, on the node of its publisher (looked up in Redis), and serves every viewer from its cache. The room track comes from one node hosting the room, picked the same way by every edge. When that node closes the room, or a publisher reconnects elsewhere, the subscription ends and the SDK subscribes again with jittered backoff.
- **Authorization:** an edge checks the viewer's room and track names. The publisher's node checks what the publisher may publish.
- **Routing:**
  - `GET /v1/route` sends viewer tokens to an edge with spare capacity, chosen at random weighted by `-max-viewers` minus its current viewers. With `?region=`, edges in that region are preferred.
  - With no edge available, viewers are routed like participants. Participants never go to edges.
  - An edge refuses viewers beyond `-max-viewers` itself, since routing works from heartbeats that are a few seconds old.
- **Upgrades:** upgrade every node before starting the first edge. Older nodes would route participants to it. Upgrade every edge before starting the first tier 2 edge.
- **Trust:** edges hold the API secret, since they verify tokens. Run them as part of your own infrastructure.

### Cascading edges

Edges can be tiered, so that one stream per track crosses each region:

```sh
moqit-server -edge -tier 1 -region eu …   # regional edge: fetches from the nodes hosting tracks
moqit-server -edge -tier 2 -region eu …   # leaf edge: fetches through a tier 1 edge in eu
```

- **Choosing a parent:** an edge of tier *n* fetches every track through an edge of tier *n − 1*, in its own region if there is one there, otherwise in any region. Rendezvous hashing on the room sends all of a room's tracks through one parent, which fetches each track once.
- **Loops:** fetching only from a lower tier rules them out.
- **Failover:** if the parent can't be reached within 2 s, the edge fetches from the node hosting the track instead. A parent that shuts down unregisters at once. One that dies drops out of the registry when its heartbeat expires (10 s), and viewers' subscriptions resume through another path, as above.
- **Routing:** viewers are routed to the deepest tier among the edges with room for them, in their `?region=` if it has any. Edges in upper tiers still accept viewers that connect to them directly.
- **Cost of a hop:** over loopback, viewers on the origin, a tier 1 edge and a tier 2 edge all saw p50 2 ms and p95 4 ms capture-to-arrival latency (200 viewers each, audio and 600 kbps video). Each hop's cost over a real network is its round trip and serialisation, which this does not measure.

### Capacity

`moqit bench` (see [development.md](development.md#load-testing)) measures this. One edge on a 24-core machine, with the bench client on the same machine over loopback:

| Viewers | Each receives | Edge CPU | Latency p50 / p95 | Resets |
|---|---|---|---|---|
| 500 | audio + 600 kbps video | 2.2 cores | 4 / 10 ms | 0 |
| 1000 | audio + 600 kbps video | 4.2 cores | 10 / 103 ms | 0 |
| 1000 | audio only (64 kbps) | 3.0 cores | 6 / 13 ms | 0 |
| 2000 | audio + 600 kbps video | 5.6 cores | 245 ms / 1.2 s | 11 260 |

- **CPU follows streams, not bytes.** Audio travels as one Group, so one QUIC stream, per 20 ms frame: 50 new streams a second per viewer. Audio alone accounts for about three quarters of the edge's CPU.
- **Above 1000 viewers the run degraded.** At 2000, latency rose and groups were reset, but the client and the edge shared one machine (load 15 on 24 cores), so the bottleneck was not isolated.
- **Measure on real hardware before sizing.** Run the client on separate machines and edges behind real network interfaces.
