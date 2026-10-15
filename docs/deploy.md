# Deploying Meet on the internet

`deploy/internet/docker-compose.yml` runs Meet on a server of your own:

| Service | Role | Ports |
|---|---|---|
| `caddy` | HTTPS for the web app; obtains and renews a Let's Encrypt certificate | TCP 80, 443 |
| `moqit` | the moqit node: WebTransport media and signalling, with Caddy's certificate | UDP 4443 |
| `meet` | serves the web app and mints access tokens | internal |

Browsers load the app from `https://<domain>` and connect to the node at `https://<domain>:4443/moq` over WebTransport. WebTransport cannot pass through an HTTP proxy, so the node is reached directly on UDP with the same certificate; it re-reads the certificate when Caddy renews it.

## Requirements

- A server with Docker (Compose v2) and a public IP address.
- A DNS name pointing at it (an `A` and/or `AAAA` record), e.g. `meet.example.com`.
- Open in the firewall: **TCP 80** and **TCP 443** (web app and certificate issuance) and **UDP 4443** (media).

## Start

```sh
git clone https://github.com/MoQ-it/moqit
cd moqit/deploy/internet
cp .env.example .env
# edit .env: MEET_DOMAIN, and MOQIT_API_SECRET (openssl rand -hex 32)
docker compose up -d --build
```

On the first start Caddy obtains the certificate, which takes a few seconds once DNS points at the server; the node logs `waiting for certificate` until it appears. Then open `https://<domain>`.

For better throughput, raise the host's UDP buffer limits (QUIC logs a warning otherwise):

```sh
sudo sysctl -w net.core.rmem_max=7500000 net.core.wmem_max=7500000
```

## Configuration (`.env`)

| Variable | Default | Meaning |
|---|---|---|
| `MEET_DOMAIN` | — (required) | the public DNS name |
| `MOQIT_API_SECRET` | — (required) | secret shared by Meet (signing tokens) and the node (verifying them) |
| `MOQIT_API_KEY` | `meet` | the API key name |
| `HTTP_PORT`, `HTTPS_PORT` | `80`, `443` | host ports for Caddy; Let's Encrypt needs 80 and 443 |
| `MOQ_PORT` | `4443` | host UDP port for media; it is also the port in the URL browsers use |
| `CERT_ISSUER` | `acme-v02.api.letsencrypt.org-directory` | where Caddy stores the certificate; `local` for a `localhost` test |

## Updating

```sh
git pull
docker compose up -d --build
```

Restarting the node ends meetings in progress; participants rejoin by reloading the page.

## Large audiences

This setup runs one node. Viewers of `https://<domain>/watch/<code>` count against the same node's capacity. For large audiences, run a cluster with edge nodes for viewers: see [clustering.md](clustering.md#edge-nodes-and-viewers).

## Rewind

Rewinding is off in this setup. To turn it on, give the node a persistent volume and start it with `-dvr-dir` on it (or `MOQIT_DVR_DIR`) and `-dvr-window`, e.g. `1h`. Size the volume from [server.md](server.md#rewind-dvr), or cap it with `-dvr-max-bytes`. This has not been tested with Docker.

## Security notes

- **Anyone who can open the site can create and join meetings**: Meet's token endpoint has no login. Put your own authentication in front of it, or mint tokens from your own backend instead (see [server.md](server.md)).
- The node's HTTP API port (7880) is not published. To use the server API from outside, publish it behind your own TLS and keep the API secret private.
- Caddy's private keys are readable only by root, so the `moqit` container runs as root, with all capabilities dropped, privilege escalation disabled and a read-only filesystem.

## Troubleshooting

- **"Could not join" / WebTransport handshake failed:** UDP 4443 is blocked (cloud security groups often allow TCP only), or `MEET_DOMAIN` does not match the certificate. Check `docker compose logs moqit`.
- **No certificate:** DNS does not point at the server yet, or TCP 80/443 are blocked. Check `docker compose logs caddy`.
- **A browser stays on "connecting…" or reports a timeout:** the error names the step that stalled. A failed WebTransport handshake is logged by the node as `webtransport upgrade failed`, with the browser's user agent.
- **Some participants cannot connect:** networks that block UDP block QUIC; moqit has no TCP fallback yet.

## Testing it locally

With `MEET_DOMAIN=localhost` and `CERT_ISSUER=local`, Caddy uses its own local certificate authority instead of Let's Encrypt (use other host ports if 80 or 443 are taken). Browsers do not trust that authority. Unlike the web page, WebTransport also refuses locally trusted roots unless Chrome runs with `--webtransport-developer-mode`. The Meet UI test can drive such a deployment:

```sh
# in deploy/internet: a throwaway HOME whose NSS database trusts Caddy's root
mkdir -p /tmp/meet-home
docker compose exec -T caddy cat /data/caddy/pki/authorities/local/root.crt > /tmp/meet-home/caddy-root.crt
docker run --rm -v /tmp/meet-home:/h debian:bookworm-slim sh -c "apt-get update -qq && apt-get install -y -qq libnss3-tools &&
  mkdir -p /h/.pki/nssdb && certutil -N -d sql:/h/.pki/nssdb --empty-password &&
  certutil -A -d sql:/h/.pki/nssdb -n caddy -t C,, -i /h/caddy-root.crt && chown -R $(id -u):$(id -g) /h"
cd ../../web/apps/meet && CHROME_HOME=/tmp/meet-home CHROME_ARGS=--webtransport-developer-mode \
  MEET_URL=https://localhost:8443 MEET_INSECURE=1 node e2e/meet.e2e.mjs
```
