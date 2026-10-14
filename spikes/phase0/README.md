# Phase 0: browser and transport de-risking

A WebTransport echo server (webtransport-go v0.13.0 / quic-go v0.62.0, ALPN `moqt-22`) and a single test page. They check the risks the moqit plan depends on.

## Run

```sh
go run ./spikes/phase0                          # page on http://localhost:8080, WebTransport on :4443
```

The server creates a 13-day ECDSA P-256 certificate and the page passes its hash in `serverCertificateHashes`, so no CA setup is needed on localhost.

### Safari / iOS / another machine

1. Start the server with the machine's LAN address in the certificate:

   ```sh
   go run ./spikes/phase0 -hosts 192.168.1.20
   ```

2. Open `https://192.168.1.20:8443/` and accept the certificate warning. The page needs a secure context, which plain HTTP only gives on localhost.
3. Make sure UDP 4443 is reachable.

To avoid the warning, use a real or mkcert certificate: `-cert cert.pem -key key.pem`.

### What to run in each browser

1. **Echo tests**: bidirectional, unidirectional and datagram round trips; server-side stream resets (MoQT delivery timeouts); `sendOrder` support.
2. **Stream-rate stress**: Opus-sized objects, one per stream, as LOC does for audio. Try 50/s × 1 and 50/s × 10 tracks.
3. **Long session**: 30 minutes of 1 Hz pings over uni streams, a bidi stream and datagrams. This is the Safari stall risk.
4. **Video loopback**: per codec, both "through WebTransport" and local. Watch the capture→decoded latency.
5. **AEC test**: needs loudspeakers. It compares how much playout leaks into the microphone for:
   - Web Audio → `destination`
   - Web Audio → `MediaStreamAudioDestinationNode` → `<audio>`
   - a local RTCPeerConnection loopback
6. **Send report to server log** posts all results to the server's stderr.

Headless or unattended: `/?auto=echo,stress,video,video-local,long&secs=10&tracks=10&codecs=vp8,avc1.42e01f&close=1`.

## Results so far (2026-10-14, Linux, localhost, headless, fake devices)

| Check | Chrome 155 | Firefox 157 |
|---|---|---|
| Connect with `serverCertificateHashes`, `protocols: ['moqt-22']` | ✅ negotiated `moqt-22` | ✅ negotiated `moqt-22` |
| Bidi / uni / datagram echo | ✅ | ✅ |
| Server resets a uni stream (before or after data) | ✅ session survives | ✅ session survives |
| Server opens a uni stream and **closes it with no payload** | ✅ | ❌ **whole session reset** (H3_GENERAL_PROTOCOL_ERROR) |
| 500 streams/s (10 tracks × 50/s) for 6 s | ✅ 0 lost, p95 2 ms | ✅ 0 lost, p95 3 ms |
| `congestionControl`, `getStats` | ❌ / ❌ | ✅ / ✅ |
| `sendOrder` on streams | ❌ | ✅ |
| Capture path | `MediaStreamTrackProcessor` (window) | rVFC + `new VideoFrame(video)` |
| Render path | `MediaStreamTrackGenerator` | canvas |
| H.264 realtime encode latency | ~1 ms | ❌ **~1.37 s** (encoder holds ~41 frames) |
| VP8 / VP9 / AV1 encode latency | 1–2 ms | 2 / 2 / 6 ms |
| 30 min long session (1 Hz uni/bidi/datagram pings, localhost) | ✅ 1801 pings, 0 lost, 0 stalls, max 34 ms | ✅ 1800 pings, 0 lost, 0 stalls, max 26 ms |

**Consequences for the implementation:**

1. **Never send an empty server-initiated stream.**
   - The `moqt` relay must write the SUBGROUP_HEADER before it can close a stream.
   - A subgroup that ends up with no objects must be reset, not closed with FIN.
   - This should be reported upstream to Firefox.
2. **Don't assume H.264 for every publisher.**
   - Pick the publisher codec per browser, from capability checks plus a short encode-latency probe.
   - Prefer VP8 or VP9 on Firefox until the H.264 delay is confirmed or ruled out on macOS and Windows; the Linux build may use a software path.
   - Every browser can decode VP8, VP9 and H.264, so subscribers are not affected. Safari still needs checking.
3. **Chrome gives no transport stats**, so uplink adaptation must be built on write back-pressure, as the plan already says.

## Still needed (cannot be done headless here)

- **Safari 26.4+ on macOS and iOS**: everything above, especially the 30-minute session (known WebKit stall reports) and the video paths. Safari should take MSTP-worker capture and VideoTrackGenerator render.
- **Real network**: the long session over Wi-Fi or a LAN rather than localhost.
- **AEC test with loudspeakers** in Chrome, Firefox and Safari. Fake devices cannot exercise echo cancellation.
- **Firefox on macOS or Windows**: does H.264 encode still lag?
