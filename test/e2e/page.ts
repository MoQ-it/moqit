// Browser side of e2e_test.go: two participants and a viewer in one page,
// each with its own Room (and WebTransport session), exercising the SDK end
// to end.
import { type RemoteAudioTrack, type RemoteVideoTrack, Room, TrackSource } from "../../web/packages/client/src/index.ts";

// Failure injection: while failDecodes > 0, a decode() fails the way a
// hardware decoder can, closing the decoder and reporting an error.
let failDecodes = 0;
const RealDecoder = globalThis.VideoDecoder;
globalThis.VideoDecoder = class extends RealDecoder {
  private onError: (e: DOMException) => void;
  constructor(init: VideoDecoderInit) {
    super(init);
    this.onError = init.error;
  }
  decode(chunk: EncodedVideoChunk) {
    if (failDecodes > 0 && this.state === "configured") {
      failDecodes--;
      this.close();
      this.onError(new DOMException("injected decoder failure", "EncodingError"));
      return;
    }
    super.decode(chunk);
  }
} as typeof VideoDecoder;

const enc = new TextEncoder();
const dec = new TextDecoder();
const steps: string[] = [];
const step = (s: string) => {
  steps.push(`${Math.round(performance.now())}ms ${s}`);
};

async function until(what: string, ok: () => boolean, timeoutMs = 15000) {
  const end = performance.now() + timeoutMs;
  while (!ok()) {
    if (performance.now() > end) throw new Error(`timed out: ${what}`);
    await new Promise((r) => setTimeout(r, 50));
  }
  step(what);
}

async function main() {
  const result: Record<string, unknown> = { ua: navigator.userAgent, steps };
  try {
    const cfg = await (await fetch("/config")).json();
    const hashOf = (hex: string) => ({ serverCertificateHashes: [Uint8Array.from(hex.match(/../g)!.map((h) => parseInt(h, 16)))] });

    // In the cluster variant Alice and Bob are on different nodes.
    const alice = new Room();
    const bob = new Room({ videoQuality: "high" });
    await alice.connect(cfg.aliceUrl, cfg.aliceToken, hashOf(cfg.aliceCertHash));
    step(`alice connected to ${cfg.aliceUrl}`);
    await bob.connect(cfg.bobUrl, cfg.bobToken, hashOf(cfg.bobCertHash));
    step(`bob connected to ${cfg.bobUrl}`);
    await until("each sees the other", () => alice.remoteParticipants.has("bob") && bob.remoteParticipants.has("alice"));

    let video: RemoteVideoTrack | undefined;
    let audio: RemoteAudioTrack | undefined;
    bob.on("trackSubscribed", (t, pub) => {
      if (pub.source === TrackSource.CAMERA) {
        video = t as RemoteVideoTrack;
        document.body.append(video.attach());
      }
      if (pub.source === TrackSource.MICROPHONE) audio = t as RemoteAudioTrack;
    });
    await alice.localParticipant.setCameraEnabled(true);
    await alice.localParticipant.setMicrophoneEnabled(true);
    step(`alice publishing (codec ${alice.localParticipant.tracks.get("camera")?.codec})`);
    await until("bob decodes 30 video frames", () => (video?.framesDecoded ?? 0) >= 30);
    await until("bob decodes 50 audio frames", () => (audio?.framesDecoded ?? 0) >= 50);
    result.highSize = `${video!.width}x${video!.height}`;

    // Lip sync: video is shown in step with the audio being played.
    const offsets: number[] = [];
    for (let i = 0; i < 20; i++) {
      await new Promise((r) => setTimeout(r, 50));
      if (!Number.isNaN(video!.avOffsetMs)) offsets.push(video!.avOffsetMs);
    }
    if (offsets.length < 10) throw new Error(`no A/V sync measurement (audio clock unavailable?): ${offsets.length} samples`);
    const mean = offsets.reduce((a, b) => a + b, 0) / offsets.length;
    result.avSync = `mean ${mean.toFixed(1)}ms, max |offset| ${Math.max(...offsets.map(Math.abs)).toFixed(1)}ms over ${offsets.length} samples`;
    if (Math.abs(mean) > 60) throw new Error(`video out of sync with audio: ${result.avSync}`);
    if (alice.getStats().sendLimited) throw new Error("send-side adaptation limited video on an unshaped connection");
    step(`A/V sync ${result.avSync}`);
    // A failed decoder is replaced, and after a second failure video goes
    // on with a software decoder.
    for (const failures of [1, 1]) {
      failDecodes = failures;
      const from = video!.framesDecoded;
      await until(`video recovers from a decoder failure`, () => failDecodes === 0 && video!.framesDecoded > from + 30, 10000);
    }
    const mic = alice.localParticipant.tracks.get("microphone") as { encodeDelayMs?: number; sendDelayMs?: number } | undefined;
    result.latency = `video ${Math.round(video!.latencyMs)}ms, audio ${Math.round(audio!.latencyMs)}ms (capture to arrival, same machine); audio encode ${Math.round(mic?.encodeDelayMs ?? -1)}ms, send ${Math.round(mic?.sendDelayMs ?? -1)}ms`;

    // Carol watches read-only: she gets the media but is no participant.
    const carol = new Room();
    let carolVideo: RemoteVideoTrack | undefined;
    let carolAudio: RemoteAudioTrack | undefined;
    carol.on("trackSubscribed", (t, pub) => {
      if (pub.source === TrackSource.CAMERA) carolVideo = t as RemoteVideoTrack;
      if (pub.source === TrackSource.MICROPHONE) carolAudio = t as RemoteAudioTrack;
    });
    await carol.connect(cfg.carolUrl, cfg.carolToken, hashOf(cfg.carolCertHash));
    step(`carol watching through ${cfg.carolUrl}`);
    await until("carol decodes 30 video frames", () => (carolVideo?.framesDecoded ?? 0) >= 30);
    await until("carol decodes 50 audio frames", () => (carolAudio?.framesDecoded ?? 0) >= 50);
    await until("alice and bob count one viewer", () => alice.numViewers === 1 && bob.numViewers === 1);
    if (alice.remoteParticipants.has("carol") || bob.remoteParticipants.has("carol")) throw new Error("the viewer appears as a participant");
    if (!carol.isViewer || carol.remoteParticipants.size !== 2) throw new Error(`carol sees ${[...carol.remoteParticipants.keys()]}`);
    const refused = await carol.localParticipant.setMicrophoneEnabled(true).then(() => false, () => true);
    if (!refused) throw new Error("a viewer published its microphone");

    // Rewind: Carol plays the meeting from 5 seconds ago, then goes live.
    await until("carol has 8s of history", () => carol.timeline.available && carol.timeline.history >= 8, 30000);
    await carol.seek(5);
    let vmark = carolVideo!.framesDecoded;
    let amark = carolAudio!.framesDecoded;
    await until("rewound video and audio decode about 5s behind", () =>
      carolVideo!.rewound && carolVideo!.framesDecoded > vmark + 30 && carolVideo!.latencyMs > 3000 &&
      carolAudio!.framesDecoded > amark + 50 && carolAudio!.latencyMs > 3000, 20000);
    const rewoundOffsets: number[] = [];
    for (let i = 0; i < 20; i++) {
      await new Promise((r) => setTimeout(r, 50));
      if (!Number.isNaN(carolVideo!.avOffsetMs)) rewoundOffsets.push(carolVideo!.avOffsetMs);
    }
    const rewoundMean = rewoundOffsets.reduce((a, b) => a + b, 0) / Math.max(1, rewoundOffsets.length);
    result.rewind = `5s back: video ${Math.round(carolVideo!.latencyMs)}ms, audio ${Math.round(carolAudio!.latencyMs)}ms behind capture; ` +
      `timeline ${carol.timeline.behind.toFixed(1)}s behind of ${carol.timeline.history.toFixed(0)}s; A/V mean ${rewoundMean.toFixed(1)}ms over ${rewoundOffsets.length} samples`;
    step(result.rewind as string);
    if (Math.abs(carolVideo!.latencyMs - 5000) > 2500) throw new Error(`rewound video is not ~5s behind: ${result.rewind}`);
    if (rewoundOffsets.length < 10 || Math.abs(rewoundMean) > 80) throw new Error(`rewound video out of sync with audio: ${result.rewind}`);

    // Pause holds video and audio where they are; resume plays on from there.
    const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
    const holds = async (what: string) => {
      await sleep(300); // frames decoding when paused come out
      const v = carolVideo!.framesDecoded, a = carolAudio!.framesDecoded, behind = carol.timeline.behind;
      await sleep(1500);
      if (carolVideo!.framesDecoded !== v || carolAudio!.framesDecoded > a + 1) {
        throw new Error(`${what}: media went on while paused (video +${carolVideo!.framesDecoded - v}, audio +${carolAudio!.framesDecoded - a})`);
      }
      if (carol.timeline.behind < behind + 1) throw new Error(`${what}: paused but not falling behind (${behind.toFixed(1)}s, then ${carol.timeline.behind.toFixed(1)}s)`);
      step(`${what}: held at ${carol.timeline.behind.toFixed(1)}s behind`);
      return behind;
    };
    await carol.pause();
    if (!carol.timeline.paused) throw new Error("timeline not paused");
    const pausedAt = await holds("paused while rewound");
    await carol.resume();
    vmark = carolVideo!.framesDecoded;
    amark = carolAudio!.framesDecoded;
    await until("playing on after pause", () => !carol.timeline.paused && carolVideo!.framesDecoded > vmark + 30 && carolAudio!.framesDecoded > amark + 50, 10000);
    if (carolVideo!.latencyMs < (pausedAt + 1.5) * 1000) throw new Error(`resume jumped ahead: ${Math.round(carolVideo!.latencyMs)}ms behind capture, paused at ${pausedAt.toFixed(1)}s`);
    await carol.goLive();
    vmark = carolVideo!.framesDecoded;
    amark = carolAudio!.framesDecoded;
    await until("back live", () => !carolVideo!.rewound && carolVideo!.framesDecoded > vmark + 30 && carolVideo!.latencyMs < 1500 &&
      carolAudio!.framesDecoded > amark + 50 && carolAudio!.latencyMs < 1500, 20000);
    if (!carol.timeline.live) throw new Error("timeline not live after goLive");
    // Pausing while live freezes what is shown; resume plays the recording from there.
    await carol.pause();
    if (carol.timeline.live || !carol.timeline.paused) throw new Error("pausing while live did not pause");
    await holds("paused while live");
    await carol.resume();
    vmark = carolVideo!.framesDecoded;
    await until("resumed from the recording, behind live", () => carolVideo!.rewound && carolVideo!.framesDecoded > vmark + 10 && carolVideo!.latencyMs > 2000, 10000);
    await sleep(500); // past the catch-up from the keyframe before the pause
    vmark = carolVideo!.framesDecoded;
    amark = carolAudio!.framesDecoded;
    await until("playing on after pausing live", () => carolVideo!.framesDecoded > vmark + 20 && carolAudio!.framesDecoded > amark + 40 && carolVideo!.latencyMs > 2000, 10000);
    await carol.goLive();
    vmark = carolVideo!.framesDecoded;
    await until("live again after the pause", () => !carolVideo!.rewound && carolVideo!.framesDecoded > vmark + 30 && carolVideo!.latencyMs < 1500, 20000);
    await carol.disconnect();
    await until("viewer count drops", () => alice.numViewers === 0);

    // Simulcast: switch to the low rendition.
    const pub = bob.remoteParticipants.get("alice")!.getTrackPublication(TrackSource.CAMERA)!;
    await pub.setVideoQuality("low");
    const before = video!.framesDecoded;
    await until("frames keep coming at low quality", () => video!.framesDecoded > before + 15 && video!.width <= 320);
    result.lowSize = `${video!.width}x${video!.height}`;

    // Adaptive stream: back to automatic, the rendition follows the
    // element's size, and a hidden element stops the video entirely.
    await pub.setVideoQuality(undefined);
    const el = video!.attach() as HTMLElement;
    el.style.width = "300px";
    let mark = video!.framesDecoded;
    await until("300px element gets the 320px rendition", () => video!.width <= 320 && video!.framesDecoded > mark + 10);
    el.style.width = "900px";
    mark = video!.framesDecoded;
    await until("900px element gets the 1280px rendition", () => video!.width === 1280 && video!.framesDecoded > mark + 10);
    el.style.display = "none";
    await new Promise((r) => setTimeout(r, 700));
    mark = video!.framesDecoded;
    await new Promise((r) => setTimeout(r, 1000));
    const whileHidden = video!.framesDecoded - mark;
    if (whileHidden > 3) throw new Error(`hidden video kept decoding (${whileHidden} frames in 1s)`);
    step(`hidden element paused (${whileHidden} frames in 1s)`);
    el.style.display = "";
    mark = video!.framesDecoded;
    await until("video resumes when shown again", () => video!.framesDecoded > mark + 10);

    // Broadcast data, targeted data, metadata.
    const got: string[] = [];
    bob.on("dataReceived", (payload, from, topic) => got.push(`${from?.identity}:${topic}:${dec.decode(payload)}`));
    alice.on("dataReceived", (payload, from, topic) => got.push(`${from?.identity}:${topic}:${dec.decode(payload)}`));
    await until("bob subscribed to alice's data", () => true);
    for (let i = 0; i < 20 && !got.some((g) => g.startsWith("alice:chat")); i++) {
      await alice.localParticipant.publishData(enc.encode("hello room"), { topic: "chat" });
      await new Promise((r) => setTimeout(r, 100));
    }
    await until("bob gets alice's broadcast", () => got.includes("alice:chat:hello room"));
    await bob.localParticipant.publishData(enc.encode("psst"), { topic: "dm", destinationIdentities: ["alice"] });
    await until("alice gets bob's direct message", () => got.includes("bob:dm:psst"));

    // The server mutes Alice's microphone through the RoomService API.
    let mutedForBob = false;
    bob.on("trackMuted", (pub) => (mutedForBob ||= pub.source === TrackSource.MICROPHONE));
    let mutedByServer = false;
    alice.on("localTrackMutedByServer", (_t, muted) => (mutedByServer = muted));
    const micSid = alice.localParticipant.info.tracks.find((t) => t.name === "microphone")?.sid;
    await fetch(`/admin/mute?identity=alice&track=${micSid}`);
    await until("server mute reaches alice and bob", () => mutedByServer && mutedForBob);

    let md = "";
    bob.on("participantMetadataChanged", (_old, p) => (md = p.metadata));
    await alice.localParticipant.setMetadata('{"hand":"up"}');
    await until("bob sees alice's metadata", () => md === '{"hand":"up"}');

    let left = false;
    bob.on("participantDisconnected", (p) => (left = p.identity === "alice"));
    await alice.disconnect();
    await until("bob sees alice leave", () => left);
    await bob.disconnect();
    result.ok = true;
  } catch (e) {
    result.error = e instanceof Error ? `${e.message}\n${e.stack}` : String(e); // Firefox stacks omit the message
  }
  await fetch("/result", { method: "POST", body: JSON.stringify(result) });
  window.close();
}

void main();
