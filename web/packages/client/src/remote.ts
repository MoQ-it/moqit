// Subscribing: receive, decode and render remote tracks.

import { RequestError, type Session, type Subscription } from "@moq-it/moqt";
import { eachObject } from "./util.ts";
import { type TrackInfo, TrackKind, TrackSource } from "./gen/moqit/v1/models_pb.ts";
import { captureClock, Emitter, participantNamespace, RateMeter, retryDelay, sleep, type VideoQuality } from "./util.ts";

const SAMPLE_RATE = 48000;
const MAX_HOLD_MS = 500; // longest a video frame waits for its audio

type PublicationEvents = {
  subscribed: [RemoteTrack];
  unsubscribed: [RemoteTrack];
  muted: [];
  unmuted: [];
};

/** A track a remote participant publishes; subscribe to get its media. */
export class RemoteTrackPublication extends Emitter<PublicationEvents> {
  info: TrackInfo;
  track?: RemoteTrack;
  private wantSubscribed = false;
  private closed = false;
  // Rendition choice: an explicit setVideoQuality wins; otherwise the
  // rendered size picks it (adaptive stream), lowered by congestion.
  private manual?: VideoQuality;
  private bySize?: number; // rendition index fitting the element
  private renderedWidth = 0;
  private congestionSteps = 0;
  private resets: number[] = [];
  private relax?: ReturnType<typeof setTimeout>;
  private current?: string;

  constructor(
    info: TrackInfo,
    private ctx: { session: Session; room: string; identity: string; audio: AudioOutput; adaptive: boolean; audioClock: (video: TrackSource) => number | undefined },
    private defaultQuality: VideoQuality,
  ) {
    super();
    this.info = info;
  }

  get sid(): string {
    return this.info.sid;
  }
  get kind(): TrackKind {
    return this.info.kind;
  }
  get source(): TrackSource {
    return this.info.source;
  }
  get muted(): boolean {
    return this.info.muted;
  }
  get isSubscribed(): boolean {
    return this.track !== undefined;
  }

  /** The rendition (simulcast layer) received, e.g. "camera-m". */
  get currentRendition(): string | undefined {
    return this.track ? this.current : undefined;
  }

  /** The quality set with setVideoQuality; undefined is automatic. */
  get videoQuality(): VideoQuality | undefined {
    return this.manual;
  }

  /** @internal */
  update(info: TrackInfo) {
    const wasMuted = this.info.muted;
    const layers = (i: TrackInfo) => i.renditions.map((r) => `${r.trackName}:${r.width}`).join();
    const changed = layers(info) !== layers(this.info);
    this.info = info;
    if (wasMuted !== info.muted) this.emit(info.muted ? "muted" : "unmuted");
    if (changed) {
      // The publisher changed its resolution: choose again among what it sends.
      this.bySize = undefined;
      if (this.renderedWidth > 0) this.bySize = this.fitting(this.renderedWidth);
      void this.apply();
    }
  }

  async setSubscribed(subscribed: boolean): Promise<void> {
    this.wantSubscribed = subscribed;
    if (subscribed && !this.track) {
      const track = this.info.kind === TrackKind.AUDIO
        ? new RemoteAudioTrack(this.info, this.ctx.audio)
        : new RemoteVideoTrack(this.info);
      this.track = track;
      if (track instanceof RemoteVideoTrack) {
        const source = this.info.source;
        track.audioClock = () => this.ctx.audioClock(source);
        if (this.ctx.adaptive) track.onRendered = (width, visible) => this.rendered(width, visible);
        track.onReset = () => this.congested();
      }
      this.current = this.rendition();
      try {
        await track.start(this.ctx.session, participantNamespace(this.ctx.room, this.ctx.identity), this.current);
      } catch (e) {
        this.track = undefined;
        throw e;
      }
      if (!this.wantSubscribed) return this.setSubscribed(false);
      this.emit("subscribed", track);
    } else if (!subscribed && this.track) {
      const track = this.track;
      this.track = undefined;
      track.stop();
      this.emit("unsubscribed", track);
    }
  }

  /**
   * @internal Subscribes, retrying with backoff while the track is wanted
   * but can't be served yet (e.g. a viewer's node hasn't found its
   * publisher).
   */
  async subscribeRetrying(): Promise<void> {
    for (let attempt = 0; !this.closed && !this.track; attempt++) {
      try {
        return await this.setSubscribed(true);
      } catch (e) {
        if (!(e instanceof RequestError)) console.warn("moqit: subscribe failed", e);
      }
      await sleep(retryDelay(attempt));
      if (!this.wantSubscribed) return;
    }
  }

  /**
   * Pins the rendition (simulcast layer), turning off size-based choice
   * for this track; pass undefined to return to automatic.
   */
  async setVideoQuality(q: VideoQuality | undefined): Promise<void> {
    this.manual = q;
    await this.apply();
  }

  /** The rendition the current constraints select. */
  private rendition(): string {
    const r = this.info.renditions;
    if (r.length === 0) return "";
    let idx: number;
    if (this.manual) idx = { high: 0, medium: 1, low: 2 }[this.manual];
    else idx = this.bySize ?? { high: 0, medium: 1, low: 2 }[this.defaultQuality];
    idx = Math.min(r.length - 1, idx + this.congestionSteps);
    return r[idx].trackName;
  }

  private async apply() {
    const next = this.rendition();
    if (next === this.current || !(this.track instanceof RemoteVideoTrack)) return;
    this.current = next;
    await this.track.switchTo(next);
  }

  // Renditions are ordered largest first; pick the smallest that is at
  // least as wide as the element's pixels.
  private rendered(width: number, visible: boolean) {
    if (this.track instanceof RemoteVideoTrack) void this.track.setPaused(!visible);
    if (!visible || width <= 0) return;
    this.renderedWidth = width;
    const idx = this.fitting(width);
    if (idx !== this.bySize) {
      this.bySize = idx;
      void this.apply();
    }
  }

  private fitting(width: number): number {
    const r = this.info.renditions;
    let idx = 0;
    for (let i = 0; i < r.length; i++) if (r[i].width >= width || r[i].width === 0) idx = i;
    return idx;
  }

  // The relay reset a group: this subscriber is not keeping up. Two resets
  // within 5s step down one rendition; 20s without one steps back up.
  private congested() {
    const now = performance.now();
    this.resets = this.resets.filter((t) => now - t < 5000);
    this.resets.push(now);
    if (this.resets.length >= 2 && this.congestionSteps < this.info.renditions.length - 1) {
      this.congestionSteps++;
      this.resets = [];
      void this.apply();
    }
    clearTimeout(this.relax);
    this.relax = setTimeout(() => {
      if (this.congestionSteps > 0) {
        this.congestionSteps--;
        void this.apply();
      }
    }, 20000);
  }

  /** @internal */
  close() {
    this.closed = true;
    this.wantSubscribed = false;
    clearTimeout(this.relax);
    void this.setSubscribed(false);
  }
}

export type RemoteTrack = RemoteVideoTrack | RemoteAudioTrack;

/** Subscribe with the current Group, and ask for a fresh one (keyframe). */
const startParams = { locationFilter: { type: 1, startGroup: 1 }, newGroupRequest: 0 };

function timestampOf(props?: { type: number; int?: number }[]): number {
  return props?.find((p) => p.type === 0x10)?.int ?? 0;
}

/** Remote camera or screen video. */
export class RemoteVideoTrack {
  readonly kind = TrackKind.VIDEO;
  /** Frames decoded so far (for stats and tests). */
  framesDecoded = 0;
  /**
   * Capture-to-arrival delay of the latest object in ms, from its LOC
   * timestamp. Meaningful when the publisher's and subscriber's clocks
   * agree (e.g. NTP-synced machines).
   */
  latencyMs = 0;
  width = 0;
  height = 0;
  private meter = new RateMeter();

  /** Bits per second received (media payload, all renditions in flight). */
  get bitrate(): number {
    return this.meter.bitrate;
  }
  private decoder: VideoDecoder;
  private sub?: Subscription;
  private session?: Session;
  private ns: string[] = [];
  private element?: HTMLVideoElement | HTMLCanvasElement;
  private draw?: (f: VideoFrame) => void;
  private stopped = false;
  private generation = 0;
  private trackName = "";
  private paused = false;
  private inFlight?: { cancel(): void };
  private observers: { disconnect(): void }[] = [];
  /**
   * @internal Lip sync: the capture timestamp of the participant's audible
   * audio, from its audio track.
   */
  audioClock?: () => number | undefined;
  /** How far video leads (+) or lags (-) the audio as shown, in ms; NaN without audio. */
  avOffsetMs = Number.NaN;
  private held: VideoFrame[] = [];
  private holdTimer?: ReturnType<typeof setTimeout>;

  /** @internal Reports the element's width in device pixels and visibility. */
  onRendered?: (width: number, visible: boolean) => void;
  /** @internal Called when the relay resets a group (delivery timeout). */
  onReset?: () => void;

  constructor(readonly info: TrackInfo) {
    this.decoder = this.newDecoder();
  }

  private newDecoder(): VideoDecoder {
    const d = new VideoDecoder({
      output: (frame) => {
        this.framesDecoded++;
        this.width = frame.displayWidth;
        this.height = frame.displayHeight;
        this.present(frame);
      },
      error: (e) => console.warn("moqit: video decoder error", e),
    });
    d.configure({ codec: this.info.codec, optimizeForLatency: true });
    return d;
  }

  // Shows frames no earlier than the audio playing with them. Frames
  // ahead of the audio wait (up to MAX_HOLD); late ones show at once.
  private present(frame: VideoFrame) {
    this.held.push(frame);
    while (this.held.length > 15) this.held.shift()!.close(); // decoder far ahead: drop
    this.flush();
  }

  private flush() {
    clearTimeout(this.holdTimer);
    while (this.held.length) {
      const frame = this.held[0];
      const audio = this.audioClock?.();
      const lead = audio === undefined ? 0 : (frame.timestamp - audio) / 1000;
      if (lead > 5 && lead < MAX_HOLD_MS) {
        this.holdTimer = setTimeout(() => this.flush(), lead);
        return;
      }
      this.held.shift();
      this.avOffsetMs = audio === undefined ? Number.NaN : lead;
      if (this.draw) this.draw(frame);
      else frame.close();
    }
  }

  /** @internal */
  async start(session: Session, ns: string[], trackName: string) {
    this.session = session;
    this.ns = ns;
    await this.switchTo(trackName);
  }

  /** @internal Subscribes to another rendition; the old one is dropped once the new one delivers. */
  async switchTo(trackName: string) {
    if (!this.session || this.stopped) return;
    const old = this.sub;
    const gen = ++this.generation;
    this.trackName = trackName;
    const sub = await this.session.subscribe({ namespace: this.ns, name: trackName }, startParams);
    this.sub = sub;
    void this.consume(sub, gen, () => old?.unsubscribe()).catch(() => {}).finally(() => {
      // The track ended without our asking: its publisher moved, or the
      // node serving it went away. Subscribe again while it is wanted.
      if (gen === this.generation && !this.stopped) void this.resubscribe(gen);
    });
  }

  private async resubscribe(gen: number) {
    for (let attempt = 0; gen === this.generation && !this.stopped; attempt++) {
      await sleep(retryDelay(attempt));
      if (gen !== this.generation || this.stopped) return;
      try {
        return await this.switchTo(this.trackName);
      } catch {
        // not served yet; try again
      }
    }
  }

  private async consume(sub: Subscription, gen: number, onFirst: () => void) {
    let first = true;
    for await (const group of sub) {
      if (gen !== this.generation || this.stopped) {
        group.cancel();
        continue;
      }
      if (first) {
        first = false;
        onFirst();
        // A new rendition may differ in size or codec state: start clean.
        try {
          this.decoder.close();
        } catch {
          // already closed
        }
        this.decoder = this.newDecoder();
      }
      let key = true; // each Group starts with a keyframe
      this.inFlight = group;
      await eachObject(group, (o) => {
        this.meter.add(o.payload.byteLength);
        if (gen !== this.generation || this.paused || this.decoder.state !== "configured") return false;
        const ts = timestampOf(o.properties);
        this.latencyMs = (captureClock() - ts) / 1000;
        this.decoder.decode(new EncodedVideoChunk({ type: key ? "key" : "delta", timestamp: ts, data: o.payload }));
        key = false;
        return true;
      }, () => {
        // Only the relay dropping the layer being watched means congestion;
        // groups of a layer switched away from, or paused, end by reset too.
        if (gen === this.generation && !this.paused && !this.stopped) this.onReset?.();
      });
    }
  }

  /** @internal Stops (or resumes) forwarding while the element is not visible. */
  async setPaused(paused: boolean) {
    if (paused === this.paused || !this.sub) return;
    this.paused = paused;
    if (paused) this.inFlight?.cancel(); // stop the group in progress too
    // Resuming asks for a fresh Group so decoding restarts at a keyframe.
    await this.sub.update(paused ? { forward: 0 } : { forward: 1, newGroupRequest: 0 }).catch(() => {});
  }

  private observe(el: Element) {
    let width = 0;
    let visible = true;
    const report = () => this.onRendered?.(width, visible && document.visibilityState === "visible");
    if (typeof ResizeObserver === "function") {
      const ro = new ResizeObserver((entries) => {
        const box = entries[entries.length - 1].contentRect;
        width = Math.round(box.width * (globalThis.devicePixelRatio || 1));
        report();
      });
      ro.observe(el);
      this.observers.push(ro);
    }
    if (typeof IntersectionObserver === "function") {
      const io = new IntersectionObserver((entries) => {
        visible = entries[entries.length - 1].isIntersecting;
        report();
      });
      io.observe(el);
      this.observers.push(io);
    }
    document.addEventListener("visibilitychange", report);
    this.observers.push({ disconnect: () => document.removeEventListener("visibilitychange", report) });
  }

  /** The element showing this track; created on first call. */
  attach(): HTMLVideoElement | HTMLCanvasElement {
    if (this.element) return this.element;
    queueMicrotask(() => this.element && this.observe(this.element));
    const MSTG = (globalThis as { MediaStreamTrackGenerator?: new (o: { kind: string }) => MediaStreamTrack & { writable: WritableStream<VideoFrame> } })
      .MediaStreamTrackGenerator;
    if (MSTG) {
      const gen = new MSTG({ kind: "video" });
      const writer = gen.writable.getWriter();
      const video = document.createElement("video");
      video.autoplay = true;
      video.muted = true;
      video.playsInline = true;
      video.srcObject = new MediaStream([gen]);
      this.draw = (f) => void writer.write(f).catch(() => f.close());
      this.element = video;
    } else {
      const canvas = document.createElement("canvas");
      const ctx = canvas.getContext("2d")!;
      this.draw = (f) => {
        if (canvas.width !== f.displayWidth) canvas.width = f.displayWidth;
        if (canvas.height !== f.displayHeight) canvas.height = f.displayHeight;
        ctx.drawImage(f, 0, 0);
        f.close();
      };
      this.element = canvas;
    }
    return this.element;
  }

  detach() {
    this.draw = undefined;
    this.element = undefined;
    for (const o of this.observers) o.disconnect();
    this.observers = [];
  }

  /** @internal */
  stop() {
    for (const o of this.observers) o.disconnect();
    clearTimeout(this.holdTimer);
    for (const f of this.held) f.close();
    this.held = [];
    this.stopped = true;
    this.sub?.unsubscribe();
    try {
      this.decoder.close();
    } catch {
      // already closed
    }
  }
}

/** Maps a frame's RMS to 0..1 on a -60..0 dBFS scale. */
export function levelOf(pcm: Float32Array): number {
  let sum = 0;
  for (let i = 0; i < pcm.length; i++) sum += pcm[i] * pcm[i];
  const rms = Math.sqrt(sum / Math.max(1, pcm.length));
  if (rms <= 0) return 0;
  return Math.max(0, Math.min(1, (20 * Math.log10(rms) + 60) / 60));
}

/** Remote microphone or screen audio, played through the room's AudioOutput. */
export class RemoteAudioTrack {
  readonly kind = TrackKind.AUDIO;
  framesDecoded = 0;
  /** Capture-to-arrival delay of the latest object in ms; see RemoteVideoTrack.latencyMs. */
  latencyMs = 0;
  private level_ = 0;
  private lastFrameAt = 0;
  private meter = new RateMeter();

  /** Bits per second received (Opus payload). */
  get bitrate(): number {
    return this.meter.bitrate;
  }
  private captureTs: number[] = [];

  /** Capture timestamp (µs, publisher clock) of the audio audible now. */
  clock(): number | undefined {
    return this.sink?.clock();
  }

  /**
   * Smoothed level of the decoded audio, 0 (silent) to 1. It drops to 0
   * when no audio has arrived recently (muted publisher, lost stream).
   */
  get level(): number {
    return performance.now() - this.lastFrameAt > 300 ? 0 : this.level_;
  }
  private decoder?: AudioDecoder;
  private sub?: Subscription;
  private sink?: AudioSink;
  private stopped = false;

  constructor(readonly info: TrackInfo, private output: AudioOutput) {}

  /** @internal */
  async start(session: Session, ns: string[], trackName: string) {
    this.sink = this.output.sink();
    this.decoder = new AudioDecoder({
      output: (data) => {
        this.framesDecoded++;
        const pcm = new Float32Array(data.numberOfFrames);
        data.copyTo(pcm, { planeIndex: 0, format: "f32-planar" });
        const ts = this.captureTs.shift() ?? data.timestamp;
        data.close();
        this.level_ = Math.max(levelOf(pcm), this.level * 0.85); // fast attack, slow release
        this.lastFrameAt = performance.now();
        this.sink?.push(pcm, ts);
      },
      error: (e) => console.warn("moqit: audio decoder error", e),
    });
    this.decoder.configure({ codec: "opus", sampleRate: SAMPLE_RATE, numberOfChannels: 1 });
    await this.follow(session, ns, trackName);
  }

  // Subscribes, and again whenever the subscription ends while playing.
  private async follow(session: Session, ns: string[], trackName: string) {
    if (this.stopped) return;
    const sub = await session.subscribe({ namespace: ns, name: trackName }, startParams);
    this.sub = sub;
    void this.consume(sub).catch(() => {}).finally(async () => {
      for (let attempt = 0; !this.stopped && this.sub === sub; attempt++) {
        await sleep(retryDelay(attempt));
        if (this.stopped || this.sub !== sub) return;
        try {
          return await this.follow(session, ns, trackName);
        } catch {
          // not served yet; try again
        }
      }
    });
  }

  private async consume(sub: Subscription) {
    for await (const group of sub) {
      if (this.decoder?.state !== "configured") return;
      await eachObject(group, (o) => {
        this.meter.add(o.payload.byteLength);
        if (this.decoder?.state !== "configured") return false;
        const ts = timestampOf(o.properties);
        this.latencyMs = (captureClock() - ts) / 1000;
        // Opus decoders do not preserve timestamps exactly; outputs come in
        // order, so the capture timestamps queue alongside.
        this.captureTs.push(ts);
        this.decoder.decode(new EncodedAudioChunk({ type: "key", timestamp: ts, data: o.payload }));
        return true;
      });
    }
  }

  setVolume(v: number) {
    this.sink?.setVolume(v);
  }

  /** @internal */
  stop() {
    this.stopped = true;
    this.sub?.unsubscribe();
    try {
      this.decoder?.close();
    } catch {
      // already closed
    }
    this.sink?.close();
  }
}

// Playout worklet: a jitter buffer per remote track. It waits for
// TARGET samples before playing, plays silence on underrun (and refills to
// TARGET), and drops audio when the buffer exceeds MAX to bound latency.
// Chunks carry their capture timestamp (µs, publisher clock); about every
// 50 ms it reports the timestamp of the sample being played, which video
// uses for lip sync.
const playoutWorklet = `
const TARGET = ${SAMPLE_RATE * 0.06}, MAX = ${SAMPLE_RATE * 0.2}, RATE = ${SAMPLE_RATE};
class MoqitPlayout extends AudioWorkletProcessor {
  constructor() {
    super();
    this.queue = []; this.offset = 0; this.buffered = 0; this.playing = false; this.sinceReport = 0;
    this.port.onmessage = (e) => {
      this.queue.push(e.data); this.buffered += e.data.pcm.length;
      while (this.buffered > MAX && this.queue.length > 1) {
        const dropped = this.queue.shift(); this.buffered -= dropped.pcm.length - this.offset; this.offset = 0;
      }
    };
  }
  process(_, outputs) {
    const out = outputs[0][0];
    if (!this.playing) {
      if (this.buffered < TARGET) { out.fill(0); return true; }
      this.playing = true;
    }
    const head = this.queue[0];
    const playingTs = head ? head.ts + (this.offset / RATE) * 1e6 : undefined;
    let i = 0;
    while (i < out.length && this.queue.length) {
      const h = this.queue[0];
      const n = Math.min(out.length - i, h.pcm.length - this.offset);
      out.set(h.pcm.subarray(this.offset, this.offset + n), i);
      i += n; this.offset += n; this.buffered -= n;
      if (this.offset === h.pcm.length) { this.queue.shift(); this.offset = 0; }
    }
    if (i < out.length) { out.fill(0, i); this.playing = false; }
    this.sinceReport += out.length;
    if (playingTs !== undefined && this.sinceReport >= RATE * 0.05) {
      this.sinceReport = 0;
      this.port.postMessage({ playingTs, at: currentTime });
    }
    return true;
  }
}
registerProcessor("moqit-playout", MoqitPlayout);
`;

/** Room-wide audio output: one AudioContext mixing every remote track. */
export class AudioOutput {
  private ctx?: AudioContext;
  private ready?: Promise<void>;
  private dest?: MediaStreamAudioDestinationNode;
  private element?: HTMLAudioElement;
  private sinkId = "";

  /** Whether the browser can choose the audio output device (not Safari). */
  static get canSelectOutput(): boolean {
    return typeof HTMLMediaElement !== "undefined" && "setSinkId" in HTMLMediaElement.prototype;
  }

  /** Plays through another output device; "" is the system default. */
  async setSinkId(deviceId: string): Promise<void> {
    if (!AudioOutput.canSelectOutput) throw new Error("this browser cannot choose the audio output device");
    if (this.element) await this.element.setSinkId(deviceId);
    this.sinkId = deviceId;
  }

  get outputDevice(): string {
    return this.sinkId;
  }

  private init(): Promise<void> {
    this.ready ??= (async () => {
      this.ctx = new AudioContext({ sampleRate: SAMPLE_RATE, latencyHint: "interactive" });
      const url = URL.createObjectURL(new Blob([playoutWorklet], { type: "text/javascript" }));
      await this.ctx.audioWorklet.addModule(url);
      URL.revokeObjectURL(url);
      // Play through an <audio> element rather than ctx.destination, which
      // gives browsers' echo cancellers a better chance to see the output.
      this.dest = this.ctx.createMediaStreamDestination();
      this.element = new Audio();
      this.element.autoplay = true;
      this.element.srcObject = this.dest.stream;
      if (this.sinkId) await this.element.setSinkId(this.sinkId).catch(() => {});
      void this.element.play().catch(() => {});
    })();
    return this.ready;
  }

  /** Resumes playback; call from a user gesture if autoplay was blocked. */
  async start(): Promise<void> {
    await this.init();
    await this.ctx!.resume();
    await this.element!.play().catch(() => {});
  }

  get canPlay(): boolean {
    return this.ctx?.state === "running";
  }

  /** @internal */
  sink(): AudioSink {
    const sink = new AudioSink();
    void this.init().then(() => {
      if (sink.closed) return;
      const node = new AudioWorkletNode(this.ctx!, "moqit-playout", { numberOfInputs: 0, numberOfOutputs: 1, outputChannelCount: [1] });
      const gain = this.ctx!.createGain();
      node.connect(gain).connect(this.dest!);
      sink.attach(node, gain, this.ctx!);
      void this.ctx!.resume().catch(() => {});
    });
    return sink;
  }

  close() {
    void this.ctx?.close();
    if (this.element) this.element.srcObject = null;
  }
}

/** One remote track's input into the AudioOutput. */
class AudioSink {
  closed = false;
  private node?: AudioWorkletNode;
  private gain?: GainNode;
  private ctx?: AudioContext;
  private pending: { pcm: Float32Array; ts: number }[] = [];
  private playing?: { ts: number; at: number }; // capture ts playing at ctx time `at`

  attach(node: AudioWorkletNode, gain: GainNode, ctx: AudioContext) {
    this.node = node;
    this.gain = gain;
    this.ctx = ctx;
    node.port.onmessage = (e: MessageEvent<{ playingTs: number; at: number }>) => (this.playing = { ts: e.data.playingTs, at: e.data.at });
    for (const p of this.pending) node.port.postMessage(p, [p.pcm.buffer]);
    this.pending = [];
  }

  push(pcm: Float32Array, ts: number) {
    if (this.node) this.node.port.postMessage({ pcm, ts }, [pcm.buffer]);
    else if (this.pending.length < 50) this.pending.push({ pcm, ts });
  }

  /**
   * The capture timestamp (µs, publisher clock) of the audio audible now,
   * or undefined when nothing is playing.
   */
  clock(): number | undefined {
    if (!this.playing || !this.ctx || this.ctx.state !== "running") return undefined;
    const elapsed = this.ctx.currentTime - this.playing.at;
    if (elapsed > 0.25) return undefined; // stalled: no recent report
    const outputLatency = (this.ctx.outputLatency || 0) + (this.ctx.baseLatency || 0);
    return this.playing.ts + (elapsed - outputLatency) * 1e6;
  }

  setVolume(v: number) {
    if (this.gain) this.gain.gain.value = v;
  }

  close() {
    this.closed = true;
    this.node?.disconnect();
    this.gain?.disconnect();
  }
}
