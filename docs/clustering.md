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
- **Routing:** `GET /v1/route?token=…` on any node's HTTP port returns the node already hosting the token's room, otherwise the least loaded. `meet -route-url` uses it.
- **Development clusters:** with `-dev`, routing also returns the node's self-signed certificate hash.
- **Docker:** `deploy/docker-compose.yml` runs Redis, two nodes and Meet. It is experimental and not yet verified.
