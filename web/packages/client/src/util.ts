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
  /** Fetch-only rewind index of a participant. */
  Dvr: "dvr",
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

/**
 * The rendition index (largest first) a quality selects among n: high is
 * the largest, low the smallest and medium the middle one, rounding down
 * in size.
 */
export function qualityIndex(q: VideoQuality, n: number): number {
  if (n <= 1) return 0;
  return q === "high" ? 0 : q === "low" ? n - 1 : Math.ceil((n - 1) / 2);
}

export { codecForRenditions, pickVideoCodec, videoEncoderConfig } from "./codecs.ts";

/** Decodes the payload of a JWT without verifying it. */
export function jwtClaims(token: string): { sub?: string; grants?: { room?: { room?: string; viewer?: boolean } } } {
  const part = token.split(".")[1] ?? "";
  const b64 = part.replace(/-/g, "+").replace(/_/g, "/").padEnd(Math.ceil(part.length / 4) * 4, "=");
  return JSON.parse(new TextDecoder().decode(Uint8Array.from(atob(b64), (c) => c.charCodeAt(0))));
}

/**
 * Delay before retry number attempt (from 0): doubling from 0.5s to 10s,
 * jittered down by up to half so that many clients don't retry together.
 */
export function retryDelay(attempt: number, random: () => number = Math.random): number {
  const base = Math.min(10000, 500 * 2 ** attempt);
  return base / 2 + (random() * base) / 2;
}

export const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

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
