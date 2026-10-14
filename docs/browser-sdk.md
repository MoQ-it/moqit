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

## Devices

```ts
const cams = await Room.getLocalDevices("videoinput");
await room.switchActiveDevice("videoinput", cams[1].deviceId); // a live track moves; before connecting, sets the device to use
await room.switchActiveDevice("audiooutput", speakerId);       // Chromium and Firefox
```

## Quality and stats

```ts
await room.localParticipant.setCameraResolution("1080p"); // largest layer sent; applies live
await room.setVideoQuality("low");                         // every remote video at its lowest layer; undefined = by size
room.getStats();                                           // { sendBitrate, receiveBitrate } in bits/s
localVideoTrack.sending();                                 // [{ rendition, width, height, bitrate }] per layer sent
remoteVideoTrack.bitrate;                                  // also width, height; publication.currentRendition
```

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
- **Audio playout:** a jitter-buffer AudioWorklet (60 ms target, 200 ms cap) mixed into one `<audio>` element.

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
  <ConferenceLayout />   {/* grid, or a shared screen on stage */}
  <ControlBar onLeave={leave} />
  <StartAudio />
</RoomProvider>
```

Hooks include `useParticipants`, `useLocalParticipant`, `useVideoTrack`, `useIsSpeaking`, `useActiveSpeakers`, `useChat`, `useScreenShares`, `useMediaDevices` and `useActiveDevice`; components include `ParticipantTile`, `GridLayout`, `FocusLayout`, `DeviceSelect`, `Chat` and `MeetingStats`.
