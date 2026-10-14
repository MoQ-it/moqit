// Camera and microphone effects with Longpipe (https://github.com/sb2702/longpipe),
// plugged into moqit as TrackProcessors. Longpipe is loaded on first use,
// and its model weights come from cdn.longpipe.dev.

import type { TrackProcessor } from "@moqit/client";
import type { BackgroundInput, EffectsPipeline, PipelineOptions } from "longpipe";

export type Background = "none" | "blur-light" | "blur" | "sunset" | "studio";

export interface VideoEffects {
  background: Background;
  touchup: boolean;
  reframe: boolean;
}

export const NO_EFFECTS: VideoEffects = { background: "none", touchup: false, reframe: false };

export const backgrounds: { id: Background; label: string }[] = [
  { id: "none", label: "None" },
  { id: "blur-light", label: "Light blur" },
  { id: "blur", label: "Blur" },
  { id: "sunset", label: "Sunset" },
  { id: "studio", label: "Studio" },
];

export function hasVideoEffects(e: VideoEffects): boolean {
  return e.background !== "none" || e.touchup || e.reframe;
}

const READY_TIMEOUT_MS = 20000;
const load = () => import("longpipe");

let sunset: Promise<ImageBitmap> | undefined;

// A generated backdrop, so the demo ships no images.
function sunsetImage(): Promise<ImageBitmap> {
  sunset ??= (() => {
    const c = new OffscreenCanvas(1280, 720);
    const g = c.getContext("2d")!;
    const sky = g.createLinearGradient(0, 0, 0, 720);
    sky.addColorStop(0, "#1d2b64");
    sky.addColorStop(0.55, "#f8786b");
    sky.addColorStop(1, "#fbc57d");
    g.fillStyle = sky;
    g.fillRect(0, 0, 1280, 720);
    g.fillStyle = "rgba(255, 236, 179, 0.9)";
    g.beginPath();
    g.arc(900, 470, 90, 0, Math.PI * 2);
    g.fill();
    g.fillStyle = "#2a1b3d";
    g.beginPath();
    g.moveTo(0, 720);
    g.lineTo(0, 560);
    g.bezierCurveTo(300, 470, 520, 640, 800, 560);
    g.bezierCurveTo(1000, 500, 1150, 560, 1280, 530);
    g.lineTo(1280, 720);
    g.fill();
    return createImageBitmap(c);
  })();
  return sunset;
}

async function backgroundInput(b: Background): Promise<BackgroundInput> {
  switch (b) {
    case "none":
      return "none";
    case "blur-light":
      return { blur: { strength: "low" } };
    case "blur":
      return { blur: { strength: "high" } };
    case "sunset":
      return { image: await sunsetImage() };
    case "studio":
      return { color: "#3a4150" };
  }
}

async function videoOptions(e: VideoEffects): Promise<PipelineOptions> {
  return { background: await backgroundInput(e.background), touchup: e.touchup ? {} : undefined, reframe: e.reframe };
}

// Resolves when the pipeline shows processed frames; rejects on a fatal
// error or if that takes too long, e.g. without WebGPU or WebGL2.
function whenReady(p: EffectsPipeline, failed: Promise<never>): Promise<void> {
  return Promise.race([
    p.ready,
    failed,
    new Promise<never>((_, reject) => setTimeout(() => reject(new Error("effects did not start in time")), READY_TIMEOUT_MS)),
  ]);
}

/**
 * Background, touch-up and auto-frame for a camera track. Change effects
 * with update(); the pipeline keeps running.
 */
export class CameraEffects implements TrackProcessor {
  private pipeline?: EffectsPipeline;

  constructor(private effects: VideoEffects) {}

  get current(): VideoEffects {
    return this.effects;
  }

  async start(track: MediaStreamTrack): Promise<MediaStreamTrack> {
    const { EffectsPipeline } = await load();
    let fail: (e: Error) => void = () => {};
    const failed = new Promise<never>((_, reject) => (fail = reject));
    const p = new EffectsPipeline(new MediaStream([track]), {
      ...(await videoOptions(this.effects)),
      onError: (e) => {
        console.warn("longpipe:", e.source, e.message);
        if (!e.recoverable) fail(new Error(`video effects failed: ${e.message}`));
      },
    });
    try {
      await whenReady(p, failed);
    } catch (e) {
      p.destroy();
      throw e;
    }
    // A new device: the old pipeline goes once the new one is live.
    this.pipeline?.destroy();
    this.pipeline = p;
    return p.stream.getVideoTracks()[0];
  }

  async update(effects: VideoEffects): Promise<void> {
    const old = this.effects;
    this.effects = effects;
    const p = this.pipeline;
    if (!p) return;
    if (effects.background !== old.background) await p.setBackground(await backgroundInput(effects.background));
    if (effects.touchup !== old.touchup) await p.setTouchup(effects.touchup ? {} : null);
    if (effects.reframe !== old.reframe) await p.setReframe(effects.reframe);
  }

  stop() {
    this.pipeline?.destroy();
    this.pipeline = undefined;
  }
}

/**
 * Microphone noise removal. Longpipe's pipeline needs a video track, so it
 * gets a tiny idle one with video effects off; only the audio is used.
 */
export class NoiseRemoval implements TrackProcessor {
  private pipeline?: EffectsPipeline;
  private dummy?: MediaStreamTrack;

  async start(track: MediaStreamTrack): Promise<MediaStreamTrack> {
    const { EffectsPipeline } = await load();
    const canvas = Object.assign(document.createElement("canvas"), { width: 16, height: 16 });
    const dummy = canvas.captureStream(1).getVideoTracks()[0];
    let fail: (e: Error) => void = () => {};
    const failed = new Promise<never>((_, reject) => (fail = reject));
    const p = new EffectsPipeline(new MediaStream([track, dummy]), {
      audio: "denoise",
      enabled: false,
      adaptive: false,
      onError: (e) => {
        console.warn("longpipe:", e.source, e.message);
        if (e.source === "audio" || !e.recoverable) fail(new Error(`noise removal failed: ${e.message}`));
      },
    });
    try {
      await whenReady(p, failed);
    } catch (e) {
      p.destroy();
      dummy.stop();
      throw e;
    }
    this.stop();
    this.pipeline = p;
    this.dummy = dummy;
    return p.stream.getAudioTracks()[0];
  }

  stop() {
    this.pipeline?.destroy();
    this.dummy?.stop();
    this.pipeline = undefined;
    this.dummy = undefined;
  }
}
