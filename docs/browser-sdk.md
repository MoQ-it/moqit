# Browser SDK

`@moqit/client` (`web/packages/client`) joins rooms from the browser over WebTransport; `@moqit/react` (`web/packages/react`) adds hooks and components.

## Rooms, participants and tracks

```ts
import { Room, TrackSource } from "@moqit/client";

const room = new Room(); // options: autoSubscribe, videoQuality, adaptiveStream, disconnectOnPageLeave
room.on("trackSubscribed", (track, pub, participant) => {
  if (pub.source === TrackSource.CAMERA) document.body.append(track.attach());
});
room.on("dataReceived", (payload, from, topic) => console.log(from?.identity, topic, payload));

await room.connect("https://localhost:4443/moq", token, { serverCertificateHashes: [hash] }); // hash only for a -dev server
await room.localParticipant.setCameraEnabled(true);
await room.localParticipant.setMicrophoneEnabled(true);
await room.localParticipant.setScreenShareEnabled(true);
await room.localParticipant.publishData(new TextEncoder().encode("hi"), { topic: "chat" });
```

`connect` rejects if a step of joining stalls for longer than `timeoutMs` (default 10 s), with the step in the message, e.g. `timed out opening a WebTransport session to …` or `timed out announcing the participant`. In a browser without WebTransport it rejects with `this browser does not support WebTransport`.

Events follow LiveKit's: `participantConnected`, `trackPublished`, `trackSubscribed`, `trackMuted`, `activeSpeakersChanged`, `dataReceived`, `connectionStateChanged` and more (see `RoomEvents` in `room.ts`).

## Watching as a viewer

With a viewer token (see [clustering.md](clustering.md#edge-nodes-and-viewers)), `connect` watches the room read-only:
- It announces nothing, so participants never see the viewer.
- It waits up to `timeoutMs` for the room to start.
- Remote participants and their tracks work as usual.

```ts
const room = new Room();
room.on("trackSubscribed", (track, pub) => pub.source === TrackSource.CAMERA && document.body.append(track.attach()));
room.on("viewerCountChanged", (n) => console.log(`${n} watching`));
await room.connect(url, viewerToken);        // the URL from /v1/route: usually an edge
room.isViewer;                               // true
await room.localParticipant.setMicrophoneEnabled(true); // rejects: "viewers cannot publish"
```

Viewers receive participants' broadcast data but not targeted data.

**Resubscribing:** for viewers and participants alike, the room track and subscribed remote tracks are subscribed again, with jittered backoff (0.5 s doubling to 10 s), when they end or can't be had yet. This covers a node closing the room, a publisher reconnecting to another node, and a track a node can't serve yet. `room.numViewers` and `viewerCountChanged` report the room's viewers for everyone.

## Devices

```ts
const cams = await Room.getLocalDevices("videoinput");
await room.switchActiveDevice("videoinput", cams[1].deviceId); // a live track moves; before connecting, sets the device to use
await room.switchActiveDevice("audiooutput", speakerId);       // Chromium and Firefox
```

## Quality and stats

```ts
await room.localParticipant.setCameraResolution("1080p"); // largest layer sent; applies live
await room.setVideoQuality("low");                         // every remote video at its smallest layer; undefined = by size
room.getStats();                                           // { sendBitrate, receiveBitrate } in bits/s
localVideoTrack.sending();                                 // [{ rendition, width, height, bitrate }] per layer sent
remoteVideoTrack.bitrate;                                  // also width, height; publication.currentRendition
remoteAudioTrack.channels;                                 // 2 for stereo, e.g. from the RTMP ingress
```

`high`, `medium` and `low` pick the largest, middle and smallest of however many layers a track has: three for a browser's camera, any number from the [RTMP ingress](https://github.com/MoQ-it/moqit-ingress).

## Processors (effects)

A processor turns the captured track into the track to send, e.g. background blur or noise removal. The camera is published, or switched to the processed track, only once `start()` resolves, so unprocessed frames never go out.

```ts
const blur: TrackProcessor = { start: async (track) => processedTrack, stop: () => {} };
await room.localParticipant.setProcessor(TrackSource.CAMERA, blur); // kept when the camera is turned off and on again
```

## How media flows

**Publishing:**
- **Capture:** `MediaStreamTrackProcessor` in Chromium; `requestVideoFrameCallback` plus `new VideoFrame(video)` in Firefox and Safari.
- **Video codec:** H.264 by default, with the level raised to fit the largest rendition; VP8 in Firefox, whose H.264 encoder adds over a second of delay.
- **Simulcast on demand:** each rendition (`camera-h`, `-m`, `-l`; `screen-h`, `-l`) is encoded only while someone subscribes to it.
- **Groups and keyframes:** each GOP is a MoQ Group; NEW_GROUP_REQUEST forces a keyframe.
- **Audio:** an AudioWorklet slices it into 20 ms Opus frames, one Group per frame, with a LOC audio-level property.

**Subscribing:**
- **Joining a track:** subscribers start at the current group and ask for a fresh keyframe. Switching rendition drops the old one once the new one delivers.
- **Video rendering:** `MediaStreamTrackGenerator` into a `<video>` element where available, canvas elsewhere.
- **Audio playout:** a jitter-buffer AudioWorklet (60 ms target, 200 ms cap) mixed into one `<audio>` element. Stereo tracks (`TrackInfo.channels` 2) play in stereo, mono on both channels.

**Quality:**
- **Adaptive stream** (`adaptiveStream`, on by default):
  - each remote video subscribes to the smallest rendition at least as wide as its element in device pixels;
  - while the element is hidden, forwarding is paused (`FORWARD=0`) and resumes with a fresh keyframe;
  - two relay-dropped groups within 5 s step quality down, and 20 s without one steps it back up;
  - `setVideoQuality` pins a rendition; `undefined` returns to automatic.
- **Delivery timeouts (relay):** a subscriber more than 1 s behind the live edge has its stale group reset, so it skips ahead rather than queueing old video.
- **Active speaker (server):** the relay reads the LOC audio-level property of every relayed `mic` object (never the payload) and publishes the loudest participants on the room track (`room.activeSpeakers`).
- **Lip sync:** the playout worklet reports the capture time of the audio being heard, and video frames ahead of it are held, up to 500 ms. In the browser tests the mean A/V offset is within ±4 ms.

## React

```tsx
import { ConferenceLayout, ControlBar, RoomProvider, StartAudio } from "@moqit/react";

<RoomProvider room={room}>
  <ConferenceLayout />   {/* grid, or a pinned tile or shared screen on stage */}
  <ControlBar onLeave={leave} />
  <StartAudio />
</RoomProvider>
```

- **`ConferenceLayout`:** shows `GridLayout`, or `FocusLayout` with one tile on the stage: the one the user pinned, else a shared screen. Every tile has a pin button; a pin ends when its participant leaves or stops sharing.
- **`GridLayout`:** measures its element and uses `gridFit(width, height, n)` to choose the column count that makes the tiles largest while all fit. It sets the tile width as the `--moqit-tile-width` CSS variable, for your stylesheet to apply (Meet: `.moqit-grid > .moqit-tile { flex: 0 0 var(--moqit-tile-width) }` in a wrapping, centred flexbox).
- **`FocusLayout`:** takes `presenter`, an optional `source` (default `TrackSource.SCREEN`), and optional `onPin` / `onUnpin` callbacks; the stage has a full-screen button. `ParticipantTile` takes `onPin` and `pinned` to show the pin button.

```tsx
<FocusLayout presenter={p} source={TrackSource.CAMERA} onUnpin={() => setPinned(undefined)} />
```

For a viewer, `useParticipants` lists only the remote participants and `useLocalParticipant` returns `undefined`; `useViewerCount` gives the number of viewers.

Hooks include `useParticipants`, `useLocalParticipant`, `useViewerCount`, `useVideoTrack`, `useIsSpeaking`, `useActiveSpeakers`, `useChat`, `useScreenShares`, `useMediaDevices` and `useActiveDevice`; components include `ParticipantTile`, `GridLayout`, `FocusLayout`, `DeviceSelect`, `Chat` and `MeetingStats`.
