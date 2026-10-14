// Browser side of e2e_test.go: two participants in one page, each with its
// own Room (and WebTransport session), exercising the SDK end to end.
import { type RemoteAudioTrack, type RemoteVideoTrack, Room, TrackSource } from "../../web/packages/client/src/index.ts";

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
    step(`A/V sync ${result.avSync}`);
    const mic = alice.localParticipant.tracks.get("microphone") as { encodeDelayMs?: number; sendDelayMs?: number } | undefined;
    result.latency = `video ${Math.round(video!.latencyMs)}ms, audio ${Math.round(audio!.latencyMs)}ms (capture to arrival, same machine); audio encode ${Math.round(mic?.encodeDelayMs ?? -1)}ms, send ${Math.round(mic?.sendDelayMs ?? -1)}ms`;

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
    result.error = String((e as Error)?.stack ?? e);
  }
  await fetch("/result", { method: "POST", body: JSON.stringify(result) });
  window.close();
}

void main();
