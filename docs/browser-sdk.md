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

**Muting someone for yourself:** `RemoteParticipant.setVolume` sets how loud a participant's microphone (or, with a source, screen audio) plays on this client; 0 silences it. Nobody else is affected, and the publisher is not told. The audio is still received, so speaking detection and lip sync go on. The setting is kept while the room is in use, also when the participant reconnects or republishes, and `participantVolumeChanged` reports changes.

```ts
const bob = room.remoteParticipants.get("bob")!;
bob.setVolume(0);                            // microphone
bob.setVolume(0, TrackSource.SCREEN_AUDIO);  // screen audio
bob.getVolume();                             // 0; 1 is as sent
```

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

### Rewinding

When the room keeps history (`room.info.dvrWindow > 0`; see [server.md](server.md#rewind-dvr)), a viewer can rewind it, like a live stream:
- `seek(seconds)` plays every participant from that far behind live, as far back as the history goes, and keeps playing at normal speed from there.
- `goLive()` returns to the live room.
- `pause()` freezes every participant where they are, video and audio, and `resume()` plays on from there, so `behind` grows by the time paused. Paused while live, the picture holds at once, and `resume()` plays the recording from that moment. A seek while paused shows the frame there and stays paused; `goLive()` ends the pause.
- `room.timeline` reports `available`, `live`, `paused`, `behind` (seconds) and `history` (seconds there are to rewind to); `timelineChanged` fires when it changes.
- `track.rewound` tells whether a remote track is playing history.

```ts
if (room.timeline.available) {
  await room.seek(30);          // everyone, 30 s ago, lip-synced
  room.timeline.behind;         // ≈ 30
  await room.pause();           // hold; room.timeline.paused is true
  await room.resume();          // on from the same moment
  await room.goLive();
}
```

- **What is played:** one rendition per source is recorded (by default the largest up to 720p). While rewound, the quality and adaptive-stream settings don't apply. Tracks subscribed while rewound start rewound too.
- **Who appears:** participants who have left are not listed, so their history isn't played.
- **How it works:** history arrives by MoQ FETCH, about 10 seconds at a time, and is paced by a playhead in server time. Participants stay in step, and each one's audio and video stay in sync.

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
room.getStats();                                           // { sendBitrate, receiveBitrate, sendLimited, sendTarget }, bits/s
localVideoTrack.sending();                                 // per layer sent: rendition, width, height, bitrate,
                                                           //   targetBitrate, framerate, limited, sendDelayMs, droppedFrames
localVideoTrack.suspended();                               // layers paused because the uplink can't carry them
room.on("sendLimitedChanged", (limited) => …);
remoteVideoTrack.bitrate;                                  // also width, height; publication.currentRendition
remoteAudioTrack.channels;                                 // 2 for stereo, e.g. from the RTMP ingress
```

`high`, `medium` and `low` pick the largest, middle and smallest of however many layers a track has: three for a browser's camera, any number from the [RTMP ingress](https://github.com/MoQ-it/moqit-ingress).

### Fitting the uplink

With `adaptiveSend` (on by default), the SDK fits the video it sends to what its connection carries:

- **The signal:** the node a participant is connected to measures how its media arrives. Each Object's arrival time minus its LOC capture time, compared with the lowest seen in the last two minutes, gives the queuing delay on the way in. The node reports it, with the bitrate it receives, in a `SendFeedback` inbox message every half second.
- **Why the server:** browsers expose neither the RTT (`WebTransport.getStats()` is missing in Chrome 155 and Firefox 157) nor their send buffers. In tests with a slow uplink, latency grew in the network while the SDK's own send queue stayed empty.
- **Decreasing:** when the delay goes over 100 ms (or the local send queue over 150 ms), the video target drops to 85% of what the node received, and holds for 2 s.
- **Increasing:** while the delay stays under 30 ms, the target grows by 5% (at least 50 kbps) a second. 30 s after the last decrease it is back to the full bitrates.
- **Sharing the target:** the target is shared among the layers being encoded, smallest layer of each track first and screens before cameras.
  - Each layer's bitrate is lowered down to 40% of its own.
  - Below that it runs at half frame rate.
  - Below 25% it is paused: re-announced without that layer, so its viewers move to a smaller one. The smallest layer of each track is never paused.
- **Resuming:** a paused layer resumes after 10 s of calm, if the target leaves a quarter to spare. If it is paused again within 30 s, it waits twice as long, up to 2 minutes.
- **Latency guard:** a frame that has waited 500 ms in the send queue drops the queue. The Group in progress is reset and sending resumes at a keyframe.

In the browser tests, Chrome and Firefox behind a 600 kbps uplink went from 720p to the 540p layer within 4.5–6 s. Viewer latency then stayed under 30 ms, against 0.3–0.8 s without adaptation. Once the limit was lifted, 720p came back within 8–17 s.

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
- **Decoder failures:** a video decoder that fails (hardware decoders can, mid-stream) is replaced, and the video resumes at a fresh keyframe. A second failure within 30 s switches that track to software decoding; after five, it stops retrying.
- **Audio playout:** a jitter-buffer AudioWorklet (60 ms target, 200 ms cap) mixed into one `<audio>` element. Stereo tracks (`TrackInfo.channels` 2) play in stereo, mono on both channels.

**Quality:**
- **Adaptive stream** (`adaptiveStream`, on by default):
  - each remote video subscribes to the smallest rendition at least as wide as its element in device pixels;
  - while the element is hidden, forwarding is paused (`FORWARD=0`) and resumes with a fresh keyframe;
  - two relay-dropped groups within 5 s step quality down, and 20 s without one steps it back up;
  - `setVideoQuality` pins a rendition; `undefined` returns to automatic.
- **Delivery timeouts (relay):** a subscriber more than 1 s behind the live edge has its stale group reset, so it skips ahead rather than queueing old video.
- **Send-side adaptation:** a publisher lowers bitrates, frame rates, and finally pauses its largest layers when its uplink can't carry them. See [Fitting the uplink](#fitting-the-uplink).
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
- **`FocusLayout`:** takes `presenter`, an optional `source` (default `TrackSource.SCREEN`), and optional `onPin` / `onUnpin` callbacks; the stage has a full-screen button. `ParticipantTile` takes `onPin` and `pinned` to show the pin button. A remote participant's camera tile also has a "mute for me" button, which sets both of their audio volumes to 0 or back to 1.

```tsx
<FocusLayout presenter={p} source={TrackSource.CAMERA} onUnpin={() => setPinned(undefined)} />
```

For a viewer, `useParticipants` lists only the remote participants and `useLocalParticipant` returns `undefined`; `useViewerCount` gives the number of viewers.

For a viewer of a room that keeps history, `<Scrubber />` is a rewind bar: a pause / play button, a slider over the history, a −10s button and LIVE. It renders nothing otherwise. `useTimeline()` returns `room.timeline` and re-renders every second.

```tsx
<RoomProvider room={room}>
  <ConferenceLayout />
  <Scrubber />
</RoomProvider>
```

Hooks include `useParticipants`, `useLocalParticipant`, `useViewerCount`, `useTimeline`, `useVideoTrack`, `useIsSpeaking`, `useActiveSpeakers`, `useChat`, `useScreenShares`, `useMediaDevices` and `useActiveDevice`; components include `ParticipantTile`, `GridLayout`, `FocusLayout`, `DeviceSelect`, `Chat`, `MeetingStats` and `Scrubber`.
