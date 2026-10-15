// Browser side of congestion_test.go: Alice publishes through a shaped
// uplink and Bob watches her; the page samples how sending keeps up.
import { type RemoteAudioTrack, type RemoteVideoTrack, Room, TrackSource } from "../../web/packages/client/src/index.ts";

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

async function main() {
  const result: Record<string, unknown> = { ua: navigator.userAgent };
  const samples: Record<string, unknown>[] = [];
  result.samples = samples;
  try {
    const cfg = await (await fetch("/config")).json();
    const hashOf = (hex: string) => ({ serverCertificateHashes: [Uint8Array.from(hex.match(/../g)!.map((h) => parseInt(h, 16)))] });
    const alice = new Room();
    const bob = new Room({ videoQuality: "high", adaptiveStream: false });
    await alice.connect(cfg.aliceUrl, cfg.aliceToken, hashOf(cfg.certHash));
    await bob.connect(cfg.bobUrl, cfg.bobToken, hashOf(cfg.certHash));
    let video: RemoteVideoTrack | undefined;
    let audio: RemoteAudioTrack | undefined;
    bob.on("trackSubscribed", (t, pub) => {
      if (pub.source === TrackSource.CAMERA) video = t as RemoteVideoTrack;
      if (pub.source === TrackSource.MICROPHONE) audio = t as RemoteAudioTrack;
    });
    await alice.localParticipant.setCameraEnabled(true);
    await alice.localParticipant.setMicrophoneEnabled(true);
    const camera = alice.localParticipant.tracks.get("camera") as unknown as { sending(): Record<string, unknown>[]; suspended(): string[] };
    const sample = async (phase: string) => {
      const wt = alice.session?.wt as unknown as { getStats?: () => Promise<Record<string, unknown>> };
      const stats = await wt.getStats?.().catch((e: unknown) => ({ error: String(e) }));
      samples.push({
        t: Math.round(performance.now()), phase,
        sending: camera.sending(),
        alice: { ...alice.getStats(), suspended: camera.suspended() },
        transport: stats,
        bob: { width: video?.width, frames: video?.framesDecoded, latencyMs: Math.round(video?.latencyMs ?? -1), audioFrames: audio?.framesDecoded, audioLatencyMs: Math.round(audio?.latencyMs ?? -1) },
      });
    };
    const last = () => samples[samples.length - 1] as { bob: { width: number; frames: number; latencyMs: number; audioLatencyMs: number }; alice: { sendLimited: boolean; suspended: string[] } };
    const phase = async (name: string, bps: number, secs: number, done?: () => boolean) => {
      await fetch(`/rate?bps=${bps}`);
      for (let i = 0; i < secs * 2; i++) {
        await sleep(500);
        await sample(name);
        if (done?.()) return true;
      }
      return false;
    };
    // Unlimited: nothing is held back.
    await phase("open", 0, 5);
    if (last().alice.sendLimited || (last().bob.width ?? 0) !== 1280) throw new Error(`limited without congestion: ${JSON.stringify(last())}`);
    // A 600 kbps uplink can't carry 720p: it moves to the 540p layer, and
    // the delay that built up drains.
    const settled = await phase("600k", 600_000, 25, () => {
      const s = last();
      return s.alice.suspended.includes("camera-h") && s.bob.width < 1280 && s.bob.latencyMs < 300 && s.bob.audioLatencyMs < 300;
    });
    if (!settled) throw new Error("did not adapt to a 600 kbps uplink");
    const adapted = samples.length;
    await phase("600k", 600_000, 5);
    const after = samples.slice(adapted).map((s) => (s as { bob: { latencyMs: number; frames: number } }).bob);
    result.limited = `adapted in ${(adapted * 0.5 - 5).toFixed(1)}s; then video latency max ${Math.max(...after.map((b) => b.latencyMs))}ms, ${after[after.length - 1].frames - after[0].frames} frames in 5s`;
    if (after[after.length - 1].frames - after[0].frames < 60) throw new Error(`video stalled while limited: ${result.limited}`);
    // Unlimited again: 720p comes back.
    const back = await phase("open again", 0, 60, () => {
      const s = last();
      return !s.alice.sendLimited && s.alice.suspended.length === 0 && s.bob.width === 1280 && s.bob.latencyMs < 300;
    });
    if (!back) throw new Error("720p did not come back with the uplink");
    result.recovered = `720p back ${((samples.length - adapted - 10) * 0.5).toFixed(1)}s after the limit was lifted`;
    await alice.disconnect();
    await bob.disconnect();
    result.ok = true;
  } catch (e) {
    result.error = String((e as Error)?.stack ?? e);
  }
  await fetch("/result", { method: "POST", body: JSON.stringify(result) });
  window.close();
}

void main();
