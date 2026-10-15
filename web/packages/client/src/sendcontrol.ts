// Send-side adaptation: bounded send queues, and a controller that fits
// the video sent to what the uplink carries.

/** One encoded frame waiting to be sent. */
export interface QueuedChunk {
  key: boolean;
  bytes: number;
}

/**
 * A rendition's send queue. Frames wait here while the transport is busy;
 * once the oldest has waited maxAgeMs the queue is dropped and sending
 * resumes at the next keyframe, so a slow uplink adds bounded latency.
 */
export class SendQueue<T extends QueuedChunk> {
  /** Frames dropped so far, and times the queue was dropped. */
  droppedFrames = 0;
  drops = 0;
  private items: { c: T; at: number }[] = [];
  private inFlight?: number; // when the chunk being sent was queued
  private running = false;
  private idleWaiters: (() => void)[] = [];
  private waitKey = false;
  private closed = false;

  constructor(
    private send: (c: T) => Promise<void>,
    /** Called on a drop: abandon the Group in progress and encode a keyframe. */
    private onDrop: () => void,
    private now: () => number = () => performance.now(),
    private maxAgeMs = 500,
  ) {}

  push(c: T) {
    if (this.closed) return;
    if (this.waitKey && !c.key) {
      this.droppedFrames++;
      return;
    }
    this.waitKey = false;
    this.items.push({ c, at: this.now() });
    if (this.delay() > this.maxAgeMs) this.drop(c.key);
    void this.pump();
  }

  /** How long the oldest unsent frame has waited, in ms. */
  delay(): number {
    const oldest = this.inFlight ?? this.items[0]?.at;
    return oldest === undefined ? 0 : this.now() - oldest;
  }

  /** Bytes waiting, not counting the frame being sent. */
  get queuedBytes(): number {
    return this.items.reduce((n, i) => n + i.c.bytes, 0);
  }

  close() {
    this.closed = true;
    this.items = [];
  }

  /** Resolves once no frame is being sent. */
  idle(): Promise<void> {
    if (!this.running) return Promise.resolve();
    return new Promise((r) => this.idleWaiters.push(r));
  }

  // Drops everything waiting; a keyframe just pushed survives and starts afresh.
  private drop(keepLastKey: boolean) {
    const keep = keepLastKey ? this.items.pop() : undefined;
    this.droppedFrames += this.items.length;
    this.items = keep ? [keep] : [];
    this.waitKey = !keep;
    this.inFlight = undefined;
    this.drops++;
    this.onDrop();
  }

  // One frame at a time, in order. After a drop the frame being sent is
  // aborted by onDrop, so the next ones follow promptly.
  private async pump() {
    if (this.running) return;
    this.running = true;
    while (!this.closed && this.items.length) {
      const { c, at } = this.items.shift()!;
      this.inFlight = at;
      try {
        await this.send(c);
      } catch {
        // the Group was reset; later frames go on
      }
      if (this.inFlight === at) this.inFlight = undefined;
    }
    this.running = false;
    for (const w of this.idleWaiters.splice(0)) w();
  }
}

/** A video rendition the controller can give bitrate to. */
export interface Layer {
  rendition: string;
  /** 0 for the smallest rendition of its track, 1 for the next, ... */
  rank: number;
  screen: boolean;
  /** Bitrate the rendition is specified at (bps). */
  spec: number;
}

/** What a layer gets: its bitrate, whether at half frame rate, or nothing. */
export interface Allocation {
  bitrate: number;
  halfRate: boolean;
  suspend: boolean;
}

/** Shares of the specified bitrate a layer needs at full and at half frame rate. */
export const FULL_FLOOR = 0.4;
export const HALF_FLOOR = 0.25;

/**
 * Shares target (bps) among layers: the smallest layer of every track
 * first (screens before cameras), then the next size up. A layer that
 * can't have its floor runs at half frame rate, and below that is
 * suspended, except the smallest of each track, which always runs.
 */
export function allocate(target: number, layers: Layer[]): Map<string, Allocation> {
  const order = [...layers].sort((a, b) => a.rank - b.rank || Number(b.screen) - Number(a.screen) || a.spec - b.spec);
  const out = new Map<string, Allocation>();
  let left = target;
  for (const l of order) {
    const full = FULL_FLOOR * l.spec;
    const half = HALF_FLOOR * l.spec;
    if (left >= full) {
      out.set(l.rendition, { bitrate: full, halfRate: false, suspend: false });
      left -= full;
    } else if (left >= half || l.rank === 0) {
      // The smallest layer of a track keeps at least half its half-rate floor.
      const b = Math.max(Math.min(left, full), 0.5 * half);
      out.set(l.rendition, { bitrate: b, halfRate: true, suspend: false });
      left -= b;
    } else {
      out.set(l.rendition, { bitrate: 0, halfRate: false, suspend: true });
    }
  }
  for (const l of order) {
    const a = out.get(l.rendition)!;
    if (a.suspend || left <= 0) continue;
    const add = Math.min(left, l.spec - a.bitrate);
    a.bitrate += add;
    left -= add;
    a.halfRate = a.bitrate < FULL_FLOOR * l.spec;
  }
  return out;
}

/** The server's report on how this participant's media arrives. */
export interface Feedback {
  queuingDelayMs: number;
  receiveBitrate: number;
}

/** What the publisher sees locally, each tick. */
export interface LocalSignals {
  /** Longest a frame has waited in a send queue (ms). */
  queueDelayMs: number;
  /** Send queue drops so far (all renditions). */
  drops: number;
  /** Video bits per second handed to the transport. */
  sendRate: number;
  /** Audio bits per second sent. */
  audioRate: number;
  /** The most video it would send: every subscribed or suspended layer at its spec. */
  maxTarget: number;
}

const OVERUSE_DELAY_MS = 100;
const QUEUE_OVERUSE_MS = 150;
const CALM_DELAY_MS = 30;
const HOLD_MS = 2000;
const FEEDBACK_FRESH_MS = 1500;
const MIN_TARGET = 50_000;
const UNCONSTRAINED_AFTER_MS = 30_000;

/**
 * Fits the video bitrate to the uplink: a multiplicative decrease to 85%
 * of what got through when a queue builds up (the server's measured
 * queuing delay, or the local send queue), then a gentle increase while
 * the path is calm.
 */
export class SendController {
  /** Video bits per second to send, at most. */
  target = 0;
  private fb?: Feedback & { at: number };
  private overuse = 0;
  private holdUntil = 0;
  private lastTick?: number;
  private drops = 0;
  /** When the last decrease happened (ms, now() clock). */
  lastDecrease = -Infinity;

  constructor(private now: () => number = () => performance.now()) {}

  feedback(fb: Feedback) {
    this.fb = { ...fb, at: this.now() };
  }

  /** The server's queuing delay, if recent. */
  get queuingDelayMs(): number | undefined {
    return this.fb && this.now() - this.fb.at < FEEDBACK_FRESH_MS ? this.fb.queuingDelayMs : undefined;
  }

  /** Whether the path is calm: no queue building anywhere. */
  calm(s: LocalSignals): boolean {
    return (this.queuingDelayMs ?? 0) < CALM_DELAY_MS && s.queueDelayMs < CALM_DELAY_MS;
  }

  tick(s: LocalSignals) {
    const now = this.now();
    const dt = this.lastTick === undefined ? 0 : (now - this.lastTick) / 1000;
    this.lastTick = now;
    if (this.target === 0) this.target = s.maxTarget;
    const delay = this.queuingDelayMs;
    const dropped = s.drops > this.drops;
    this.drops = s.drops;
    const over = (delay ?? 0) > OVERUSE_DELAY_MS || s.queueDelayMs > QUEUE_OVERUSE_MS || dropped;
    this.overuse = over ? this.overuse + 1 : 0;
    if (this.overuse >= 2 && now >= this.holdUntil) {
      const through = delay !== undefined ? this.fb!.receiveBitrate - s.audioRate : s.sendRate;
      this.target = Math.max(MIN_TARGET, Math.min(this.target, 0.85 * through));
      this.holdUntil = now + HOLD_MS;
      this.lastDecrease = now;
    } else if (!over && now >= this.holdUntil && this.calm(s)) {
      // Long after the last decrease, nothing is held back: what is sent
      // follows what is subscribed, at its specified bitrates.
      if (now - this.lastDecrease >= UNCONSTRAINED_AFTER_MS) this.target = s.maxTarget;
      else this.target += Math.max(0.05 * this.target, 50_000) * dt;
    }
    this.target = Math.min(Math.max(this.target, MIN_TARGET), Math.max(s.maxTarget, MIN_TARGET));
  }
}

const SUSPEND_AFTER_MS = 2000;
const RESUME_BACKOFF_MS = 10_000;
const MAX_BACKOFF_MS = 120_000;
const FLAP_MS = 30_000;

/**
 * Suspension with hysteresis: a layer is suspended once the allocation has
 * wanted it for a while, and resumed only after calm long enough, with
 * room to spare; one that is suspended again soon after waits longer.
 */
export class Suspensions {
  readonly suspended = new Set<string>();
  private wanted = new Map<string, number>(); // since when allocation wants it suspended
  private suspendedAt = new Map<string, number>();
  private resumedAt = new Map<string, number>();
  private backoff = new Map<string, number>();

  constructor(private now: () => number = () => performance.now()) {}

  /**
   * Updates from an allocation of the running layers, and checks whether a
   * suspended layer can resume: after calm for its back-off, with target
   * covering every running layer's floor and its own by a quarter more.
   * Reports whether the set changed.
   */
  update(alloc: Map<string, Allocation>, running: Layer[], target: number, calmSince: number, candidates: Layer[]): boolean {
    const now = this.now();
    let changed = false;
    for (const [name, a] of alloc) {
      if (!a.suspend) {
        this.wanted.delete(name);
        continue;
      }
      const since = this.wanted.get(name) ?? now;
      this.wanted.set(name, since);
      if (now - since < SUSPEND_AFTER_MS) continue;
      this.wanted.delete(name);
      this.suspended.add(name);
      this.suspendedAt.set(name, now);
      const resumed = this.resumedAt.get(name);
      const flapped = resumed !== undefined && now - resumed < FLAP_MS;
      this.backoff.set(name, flapped ? Math.min(MAX_BACKOFF_MS, 2 * (this.backoff.get(name) ?? RESUME_BACKOFF_MS)) : RESUME_BACKOFF_MS);
      changed = true;
    }
    if (changed) return true;
    const needed = running.filter((l) => !this.suspended.has(l.rendition)).reduce((n, l) => n + FULL_FLOOR * l.spec, 0);
    for (const l of [...candidates].sort((a, b) => a.rank - b.rank || a.spec - b.spec)) {
      if (!this.suspended.has(l.rendition)) continue;
      const since = Math.max(this.suspendedAt.get(l.rendition) ?? 0, calmSince);
      if (now - since < (this.backoff.get(l.rendition) ?? RESUME_BACKOFF_MS)) continue;
      if (target < 1.25 * (needed + FULL_FLOOR * l.spec)) continue;
      this.suspended.delete(l.rendition);
      this.resumedAt.set(l.rendition, now);
      return true; // one at a time
    }
    return false;
  }
}
