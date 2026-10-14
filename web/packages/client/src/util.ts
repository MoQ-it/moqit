// Small shared helpers: events, naming, LOC properties, codecs.

import type { Namespace } from "@moq-it/moqt";

/** Minimal typed event emitter. */
// biome-ignore lint: handlers are stored type-erased.
type Handler = (...args: any[]) => void;

export class Emitter<E extends { [K in keyof E]: unknown[] }> {
  private handlers = new Map<keyof E, Set<Handler>>();

  on<K extends keyof E>(event: K, fn: (...args: E[K]) => void): this {
    let set = this.handlers.get(event);
    if (!set) this.handlers.set(event, (set = new Set()));
    set.add(fn as Handler);
    return this;
  }

  off<K extends keyof E>(event: K, fn: (...args: E[K]) => void): this {
    this.handlers.get(event)?.delete(fn as Handler);
    return this;
  }

  once<K extends keyof E>(event: K, fn: (...args: E[K]) => void): this {
    const wrap = (...args: E[K]) => {
      this.off(event, wrap);
      fn(...args);
    };
    return this.on(event, wrap);
  }

  /** Resolves with the arguments of the next `event`. */
  next<K extends keyof E>(event: K, timeoutMs = 10000): Promise<E[K]> {
    return new Promise((resolve, reject) => {
      const t = setTimeout(() => reject(new Error(`timed out waiting for ${String(event)}`)), timeoutMs);
      this.once(event, (...args) => {
        clearTimeout(t);
        resolve(args);
      });
    });
  }

  protected emit<K extends keyof E>(event: K, ...args: E[K]): void {
    for (const fn of [...(this.handlers.get(event) ?? [])]) {
      try {
        fn(...args);
      } catch (e) {
        console.error("moqit: event handler failed", e);
      }
    }
  }
}

// MoQ naming scheme; mirrors pkg/room/names.go on the server.
export const participantNamespace = (room: string, identity: string): Namespace => ["moqit", room, "p", identity];
export const serverNamespace = (room: string): Namespace => ["moqit", room, "srv"];
export const inboxNamespace = (room: string, identity: string): Namespace => ["moqit", room, "srv", "to", identity];

export const TrackName = {
  Room: "room",
  Inbox: "inbox",
  Signal: "signal",
  Data: "data",
  DataLossy: "data-lossy",
  Mic: "mic",
  ScreenAudio: "screen-audio",
} as const;

/** LOC (draft-ietf-moq-loc-04) Object properties used by moqit. */
export const LocProperty = {
  /** Capture time in microseconds on the publisher's clock. */
  Timestamp: 0x10,
  /** Audio level as -dBov, 0 (loudest) to 127 (silence), as in RFC 6464. */
  AudioLevel: 0x0c,
} as const;

/** Microseconds on a clock shared by a publisher's audio and video. */
export function captureClock(): number {
  return Math.round((performance.timeOrigin + performance.now()) * 1000);
}

export interface RenditionSpec {
  trackName: string;
  width: number;
  height: number;
  bitrate: number;
  framerate: number;
}

/** The largest camera picture a participant sends. */
export type VideoResolution = "1080p" | "720p" | "360p" | "180p";

const cameraL: RenditionSpec = { trackName: "camera-l", width: 320, height: 180, bitrate: 150_000, framerate: 15 };

/**
 * Capture size and renditions (simulcast layers, largest first) per send
 * resolution. Lower resolutions drop the top layers, so subscribers
 * choose among what is sent.
 */
export const CameraPresets: Record<VideoResolution, { width: number; height: number; frameRate: number; renditions: RenditionSpec[] }> = {
  "1080p": {
    width: 1920, height: 1080, frameRate: 30,
    renditions: [
      { trackName: "camera-h", width: 1920, height: 1080, bitrate: 3_000_000, framerate: 30 },
      { trackName: "camera-m", width: 960, height: 540, bitrate: 800_000, framerate: 30 },
      cameraL,
    ],
  },
  "720p": {
    width: 1280, height: 720, frameRate: 30,
    renditions: [
      { trackName: "camera-h", width: 1280, height: 720, bitrate: 1_700_000, framerate: 30 },
      { trackName: "camera-m", width: 640, height: 360, bitrate: 500_000, framerate: 30 },
      cameraL,
    ],
  },
  "360p": {
    width: 640, height: 360, frameRate: 30,
    renditions: [{ trackName: "camera-m", width: 640, height: 360, bitrate: 500_000, framerate: 30 }, cameraL],
  },
  "180p": { width: 320, height: 180, frameRate: 15, renditions: [cameraL] },
};

export const CameraRenditions: RenditionSpec[] = CameraPresets["720p"].renditions;

export const ScreenRenditions: RenditionSpec[] = [
  { trackName: "screen-h", width: 1920, height: 1080, bitrate: 2_500_000, framerate: 15 },
  { trackName: "screen-l", width: 960, height: 540, bitrate: 600_000, framerate: 5 },
];

export type VideoQuality = "high" | "medium" | "low";

export function videoEncoderConfig(codec: string, width: number, height: number, bitrate: number, framerate: number): VideoEncoderConfig {
  const c: VideoEncoderConfig = { codec, width, height, bitrate, framerate, latencyMode: "realtime" };
  // Annex B keeps SPS/PPS in-band on keyframes, so decoders need no description.
  if (codec.startsWith("avc1")) c.avc = { format: "annexb" };
  return c;
}

/**
 * Picks the publisher's video codec. Firefox's H.264 encoder adds over a
 * second of latency even in realtime mode (moqit Phase 0), so it prefers
 * VP8 there; elsewhere H.264 is hardware-accelerated.
 */
// H.264 levels: [level_idc, max frame size in macroblocks, max macroblocks/s].
const AVC_LEVELS: [number, number, number][] = [
  [0x1f, 3600, 108000], // 3.1: 1280x720@30
  [0x20, 5120, 216000], // 3.2
  [0x28, 8192, 245760], // 4.0: 1920x1080@30
  [0x2a, 8704, 522240], // 4.2
  [0x32, 22080, 589824], // 5.0
  [0x33, 36864, 983040], // 5.1: 4096x2160@30
  [0x34, 36864, 2073600], // 5.2
];

/**
 * The codec string to publish for a set of renditions. H.264 encoders
 * refuse sizes above the level in the codec string (avc1.PPCCLL), so the
 * level is raised to fit the largest rendition; other codecs pass through.
 */
export function codecForRenditions(codec: string, specs: { width: number; height: number; framerate: number }[]): string {
  const m = /^(avc1\.[0-9a-f]{4})[0-9a-f]{2}$/i.exec(codec);
  if (!m || specs.length === 0) return codec;
  let fs = 0, mbps = 0;
  for (const s of specs) {
    const mbs = Math.ceil(s.width / 16) * Math.ceil(s.height / 16);
    fs = Math.max(fs, mbs);
    mbps = Math.max(mbps, mbs * s.framerate);
  }
  const level = AVC_LEVELS.find(([, maxFs, maxMbps]) => fs <= maxFs && mbps <= maxMbps) ?? AVC_LEVELS[AVC_LEVELS.length - 1];
  return m[1] + Math.max(level[0], parseInt(codec.slice(-2), 16)).toString(16).padStart(2, "0");
}

export async function pickVideoCodec(): Promise<string> {
  const firefox = /Firefox\//.test(navigator.userAgent);
  const prefs = firefox ? ["vp8", "vp09.00.10.08", "avc1.42e01f"] : ["avc1.42e01f", "vp8", "vp09.00.10.08"];
  for (const codec of prefs) {
    try {
      if ((await VideoEncoder.isConfigSupported(videoEncoderConfig(codec, 1280, 720, 1_000_000, 30))).supported) return codec;
    } catch {
      // try the next one
    }
  }
  throw new Error("no supported video encoder");
}

/** Decodes the payload of a JWT without verifying it. */
export function jwtClaims(token: string): { sub?: string; grants?: { room?: { room?: string } } } {
  const part = token.split(".")[1] ?? "";
  const b64 = part.replace(/-/g, "+").replace(/_/g, "/").padEnd(Math.ceil(part.length / 4) * 4, "=");
  return JSON.parse(new TextDecoder().decode(Uint8Array.from(atob(b64), (c) => c.charCodeAt(0))));
}

/** Runs async tasks one at a time, in order. */
export class Serial {
  private tail: Promise<unknown> = Promise.resolve();
  run<T>(f: () => Promise<T>): Promise<T> {
    const p = this.tail.then(f, f);
    this.tail = p.catch(() => {});
    return p;
  }
}

/**
 * Calls f for each Object of a subgroup until f returns false. A subgroup
 * the publisher resets (delivery timeout, track stopped) just ends early;
 * other Groups of the track are unaffected.
 */
export async function eachObject<O>(group: AsyncIterable<O> & { cancel(): void }, f: (o: O) => boolean | void, onReset?: () => void): Promise<void> {
  try {
    for await (const o of group) {
      if (f(o) === false) {
        group.cancel();
        return;
      }
    }
  } catch {
    // reset by the publisher or a relay (e.g. a delivery timeout)
    onReset?.();
  }
}

/** Bits per second over a sliding window (2 s by default). */
export class RateMeter {
  private samples: { at: number; bytes: number }[] = [];
  private total = 0;

  constructor(private windowMs = 2000, private now: () => number = () => performance.now()) {}

  add(bytes: number) {
    this.samples.push({ at: this.now(), bytes });
    this.total += bytes;
    this.trim();
  }

  get bitrate(): number {
    this.trim();
    return (this.total * 8000) / this.windowMs;
  }

  private trim() {
    const cut = this.now() - this.windowMs;
    while (this.samples.length && this.samples[0].at < cut) this.total -= this.samples.shift()!.bytes;
  }
}
