# moqit Meet

Meet is the conferencing app in `web/apps/meet`, served with its token endpoint by `cmd/meet`. It is built on [`@moqit/react`](browser-sdk.md#react).

```sh
(cd web && npm install && npm run build -w meet)
go run ./cmd/meet -dev          # web app on http://localhost:8080, moqit node on UDP :4443
```

With `-dev`, `meet` runs a moqit node in the same process. Without it, `meet` points at an existing node (`-node-url`, `-key`, `-secret`) or a cluster's router (`-route-url`) and only serves the app and tokens.

## Features

- **Joining:** "New meeting" creates a room code; anyone with the link joins. The pre-join page shows a camera preview.
- **Meeting:** a participant grid with speaking rings, mute / camera / screen-share controls, chat with an unread count, and a copyable link. The grid picks the number of columns that makes the tiles largest while all of them fit the window, and centres the last row; on a small screen the tiles stop shrinking and the grid scrolls.
- **Pinning:** the ⤢ button on a tile (or a double click) pins that camera or screen to the stage, with everyone else in a strip beside it; ✕ (or another double click) unpins it. The stage's ⛶ button shows it full screen. A pin ends when the participant leaves or stops sharing, and only changes your own view.
- **Watching:** `/watch/<code>` (the "Copy watch link" button) shows a meeting read-only with a viewer token. It needs no name, camera or microphone, and goes through an edge node when the cluster has one. The page waits for the meeting to start and reconnects when the connection drops. Participants see "N watching" in the header.
- **Devices:** microphone (with a level meter), camera and speaker pickers before joining. The ⚙ Devices menu switches them live without republishing, so others see no interruption. Speaker choice needs `setSinkId`, which Chromium and Firefox have and Safari lacks.
- **Quality:** choose the camera resolution you send (1080p, 720p, 360p or 180p) before joining or live from the 🎚 Quality menu. The same menu sets the quality you receive of others' video: Auto fits each tile, or fixed High, Medium or Low.
- **Stats:** each tile shows its resolution, simulcast layer and bitrate; the header shows the meeting's total sent ↑ and received ↓. They can be turned off in the Quality menu.
- **Screen sharing:** a shared screen takes the stage with cameras in a side strip and "<name> is presenting" in the header. The presenter sees a "You are presenting" card instead of a mirror of their own screen. Tab or system audio is shared when the browser allows it, and the browser's own "Stop sharing" bar works too.
- **Effects:** [Longpipe](https://github.com/sb2702/longpipe) adds background blur or replacement, touch-up, auto-frame and microphone noise removal, chosen before joining (with a live preview) or from the ✨ Effects menu. The camera is published only once the effect is running, and switching cameras keeps it on, so unprocessed video never reaches the room. Longpipe is loaded only when an effect is turned on (about 120 kB gzipped) and fetches its model weights from `cdn.longpipe.dev`.
- **Refreshing:** reloading the page during a meeting rejoins as the same participant, so others never see a frozen copy of you. A participant whose connection is lost without closing (crash, network gone) is removed after about 20 seconds.
- **Remembered choices:** name, devices, effects and quality are kept in the browser.

## Developing

- **Hot reload:** `npm run dev -w meet` runs Vite; `/api` is proxied to `meet` on :8080.
- **UI test:** `npm run e2e -w meet` drives the real UI with Playwright in the system Chrome (fake camera and microphone). Two users join, a third watches through the watch link and a third participant joins later; the test covers watching (the viewer sees video, is counted and is not a participant), device picking and live switching, effects (seen on the other side, and held while the camera switches), send and receive quality, bitrates (sent matches received), refreshing mid-meeting, video and audio both ways, screen sharing (including 1080p and windows of any shape), the grid fitting three participants into windows of different shapes, pinning (the pinned tile gets the top layer), full screen, chat, mute, camera-off and leaving. Set `SCREENSHOTS=dir` to keep the screenshots, or `MEET_URL` to test a running deployment.
