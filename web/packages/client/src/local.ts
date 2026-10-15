// Publishing: capture, encode and send local tracks.

import type { OutgoingSubgroup, SubscribeRequest, TrackWriter } from "@moq-it/moqt";
import { type Rendition, RenditionSchema, TrackKind, TrackSource } from "./gen/moqit/v1/models_pb.ts";
import { create } from "@bufbuild/protobuf";
import { type Allocation, HALF_FLOOR, type Layer, SendQueue } from "./sendcontrol.ts";
import { captureClock, codecForRenditions, LocProperty, RateMeter, type RenditionSpec, Serial, TrackName, videoEncoderConfig } from "./util.ts";

const GOP_SECONDS = 2;
const DRAIN_MS = 4000; // a dropped rendition keeps sending this long, while subscribers switch

export type LocalTrack = LocalVideoTrack | LocalAudioTrack;

/** A rendition being sent: its encoded size and bitrate, and how its sending keeps up. */
export interface SendingRendition {
  rendition: string;
  width: number;
  height: number;
  /** Bits per second handed to the transport. */
  bitrate: number;
  /** How long the oldest frame not yet handed to the transport has waited, in ms. */
  sendDelayMs: number;
  /** Frames dropped because the uplink could not send them in time. */
  droppedFrames: number;
  /** The bitrate the encoder aims at; below the rendition's when the uplink is limited. */
  targetBitrate: number;
  /** Frames per second encoded. */
  framerate: number;
  /** Sent below its specified bitrate or frame rate because of the uplink. */
  limited: boolean;
}

/**
 * Transforms a captured track before it is encoded, e.g. a background
 * blur. The processor owns the tracks it returns; the captured track
 * stays the caller's.
 */
export interface TrackProcessor {
  /**
   * Starts processing `track` and resolves to the track to send, once its
   * output is processed: until then the track keeps sending what it sent
   * before, so unprocessed frames never go out by surprise. Called again
   * with a new track when the device changes, and after stop() when a
   * track is enabled again.
   */
  start(track: MediaStreamTrack): Promise<MediaStreamTrack>;
  /** Stops processing and releases what start() created. */
  stop(): void;
}

/** Base of local tracks. */
abstract class LocalTrackBase {
  /** The publisher's name for the track, e.g. "camera". */
  abstract readonly name: string;
  abstract readonly kind: TrackKind;
  abstract readonly source: TrackSource;
  abstract readonly codec: string;
  muted = false;
  sid?: string;
  private captured: MediaStreamTrack;
  private processed?: MediaStreamTrack;
  private processor_?: TrackProcessor;
  private changes = new Serial(); // device and processor changes, in order
  private ended = false;

  constructor(track: MediaStreamTrack) {
    this.captured = track;
  }

  /** The track being encoded and sent: the processor's output, or the captured track. */
  get mediaStreamTrack(): MediaStreamTrack {
    return this.processed ?? this.captured;
  }

  /** The track from the device, before any processor. */
  get capturedTrack(): MediaStreamTrack {
    return this.captured;
  }

  get processor(): TrackProcessor | undefined {
    return this.processor_;
  }

  /**
   * Captures from another track, e.g. a different camera, without
   * republishing: subscribers keep their subscriptions. With a processor,
   * the old device keeps sending until the processor has the new one.
   */
  replaceTrack(track: MediaStreamTrack): Promise<void> {
    return this.changes.run(async () => {
      const old = this.captured;
      if (track === old) return;
      let out: MediaStreamTrack | undefined;
      try {
        out = this.processor_ ? await this.processor_.start(track) : undefined;
      } catch (e) {
        track.stop();
        throw e;
      }
      if (this.ended) return void track.stop(); // stopped meanwhile: stop() already stopped the processor
      this.captured = track;
      this.setOutput(out);
      old.stop();
    });
  }

  /**
   * Processes the track before sending (undefined sends the captured track
   * as it is). Rejects, still sending as before, if the processor fails.
   */
  setProcessor(processor: TrackProcessor | undefined): Promise<void> {
    return this.changes.run(async () => {
      const old = this.processor_;
      if (processor === old) return;
      const out = processor ? await processor.start(this.captured) : undefined;
      if (this.ended) return processor?.stop();
      this.processor_ = processor;
      this.setOutput(out);
      old?.stop();
    });
  }

  private setOutput(track: MediaStreamTrack | undefined) {
    this.processed = track;
    this.mediaStreamTrack.enabled = !this.muted;
    if (track) this.captured.enabled = true; // the processor needs input while muted output is black
    this.trackReplaced();
  }

  protected abstract trackReplaced(): void;

  /** MoQ track names this track serves. */
  abstract trackNames(): string[];
  /** Serve a SUBSCRIBE for one of trackNames(). */
  abstract serve(req: SubscribeRequest): Promise<void>;
  abstract renditions(): Rendition[];

  /** Pause sending (the track stays published, flagged muted). */
  mute() {
    this.muted = true;
    this.mediaStreamTrack.enabled = false;
  }

  unmute() {
    this.muted = false;
    this.mediaStreamTrack.enabled = true;
  }

  stop() {
    this.ended = true;
    this.processor_?.stop();
    this.captured.stop();
  }
}

/** Delivers raw VideoFrames from a camera or screen track. */
interface FrameSource {
  stop(): void;
}

function startFrameSource(track: MediaStreamTrack, onFrame: (f: VideoFrame) => void): FrameSource {
  const MSTP = (globalThis as { MediaStreamTrackProcessor?: new (o: { track: MediaStreamTrack }) => { readable: ReadableStream<VideoFrame> } })
    .MediaStreamTrackProcessor;
  if (MSTP) {
    // Chromium: frames straight from the capture pipeline.
    const reader = new MSTP({ track }).readable.getReader();
    let stopped = false;
    void (async () => {
      while (!stopped) {
        const { value, done } = await reader.read();
        if (done) return;
        onFrame(value);
      }
    })();
    return { stop: () => ((stopped = true), void reader.cancel()) };
  }
  // Firefox and Safari: paint the track into a <video> and grab each frame.
  const video = document.createElement("video");
  video.muted = true;
  video.playsInline = true;
  video.srcObject = new MediaStream([track]);
  let stopped = false;
  const step = () => {
    if (stopped) return;
    if (video.videoWidth > 0) onFrame(new VideoFrame(video, { timestamp: captureClock() }));
    video.requestVideoFrameCallback(step);
  };
  void video.play().then(() => video.requestVideoFrameCallback(step));
  return { stop: () => ((stopped = true), (video.srcObject = null)) };
}

/** Camera or screen video, published as one MoQ track per rendition. */
export class LocalVideoTrack extends LocalTrackBase {
  readonly kind = TrackKind.VIDEO;
  private encoders = new Map<string, RenditionEncoder>();
  private source_?: FrameSource;
  private nextGroup = 0;
  private previewEl?: HTMLVideoElement;
  private specs_: RenditionSpec[];
  private draining = new Map<string, ReturnType<typeof setTimeout>>();
  private suspended_ = new Set<string>();
  /** Published codec string; for H.264, with a level that fits every rendition. */
  codec: string;

  constructor(
    track: MediaStreamTrack,
    readonly source: TrackSource,
    private baseCodec: string,
    specs: RenditionSpec[],
  ) {
    super(track);
    this.specs_ = specs;
    this.codec = codecForRenditions(baseCodec, specs);
  }

  /** The renditions sent, largest first. */
  get specs(): readonly RenditionSpec[] {
    return this.specs_;
  }

  /**
   * Changes the renditions, e.g. to send a higher resolution. Renditions
   * kept by name re-encode at their new size; dropped ones keep going for
   * a few seconds, so subscribers can move to another first. The caller
   * announces the change (LocalParticipant does).
   */
  setRenditions(specs: RenditionSpec[]) {
    this.specs_ = specs;
    this.codec = codecForRenditions(this.baseCodec, specs);
    for (const [name, enc] of this.encoders) {
      const spec = specs.find((s) => s.trackName === name);
      clearTimeout(this.draining.get(name));
      this.draining.delete(name);
      if (spec) {
        enc.setSpec(spec);
        continue;
      }
      this.draining.set(name, setTimeout(() => {
        this.draining.delete(name);
        if (this.encoders.get(name) !== enc) return;
        this.encoders.delete(name);
        enc.close();
        if (this.encoders.size === 0) this.stopCapture();
      }, DRAIN_MS));
    }
  }

  get name(): string {
    return this.source === TrackSource.SCREEN ? "screen" : "camera";
  }

  trackNames(): string[] {
    return this.specs.map((s) => s.trackName);
  }

  /** The renditions announced: all but those suspended for a limited uplink. */
  renditions(): Rendition[] {
    return this.specs.filter((s) => !this.suspended_.has(s.trackName)).map((s) => create(RenditionSchema, s));
  }

  /** Renditions not sent because the uplink can't carry them. */
  suspended(): string[] {
    return this.specs.filter((s) => this.suspended_.has(s.trackName)).map((s) => s.trackName);
  }

  /** @internal The renditions as layers for send control, and which are being encoded. */
  layers(): { layer: Layer; encoding: boolean }[] {
    const n = this.specs_.length;
    return this.specs_.map((s, i) => ({
      layer: { rendition: s.trackName, rank: n - 1 - i, screen: this.source === TrackSource.SCREEN, spec: s.bitrate },
      encoding: this.encoders.has(s.trackName),
    }));
  }

  /** @internal Applies the send controller's shares to the encoders. */
  applyAllocation(alloc: Map<string, Allocation>) {
    for (const [name, enc] of this.encoders) {
      const a = alloc.get(name);
      if (!a) continue;
      // A layer about to be suspended runs at the least until it is.
      if (a.suspend) enc.setRate(0.5 * HALF_FLOOR * enc.specBitrate, true);
      else enc.setRate(a.bitrate, a.halfRate);
    }
  }

  /** @internal Stops (or resumes) encoding renditions the uplink can't carry; the caller re-announces the tracks. */
  setSuspended(names: Set<string>) {
    this.suspended_ = new Set([...names].filter((n) => this.specs_.some((s) => s.trackName === n)));
    for (const [name, enc] of this.encoders) enc.setSuspended(this.suspended_.has(name));
  }

  /** A local preview element. */
  attach(): HTMLVideoElement {
    if (!this.previewEl) {
      this.previewEl = document.createElement("video");
      this.previewEl.muted = true;
      this.previewEl.autoplay = true;
      this.previewEl.playsInline = true;
      this.previewEl.srcObject = new MediaStream([this.mediaStreamTrack]);
    }
    return this.previewEl;
  }

  protected trackReplaced() {
    if (this.previewEl) this.previewEl.srcObject = new MediaStream([this.mediaStreamTrack]);
    if (!this.source_) return;
    this.source_.stop();
    this.source_ = startFrameSource(this.mediaStreamTrack, (f) => this.onFrame(f));
    for (const e of this.encoders.values()) e.requestKeyFrame(); // the picture changes: start a new GOP
  }

  /**
   * The renditions being sent, largest first: their encoded size and
   * bitrate. A rendition nobody subscribes to is not encoded.
   */
  sending(): SendingRendition[] {
    const order = (name: string) => {
      const i = this.specs_.findIndex((s) => s.trackName === name);
      return i < 0 ? this.specs_.length : i;
    };
    return [...this.encoders.values()]
      .map((e) => ({
        rendition: e.name, ...(e.encodedSize ?? { width: 0, height: 0 }), bitrate: e.bitrate,
        sendDelayMs: e.sendDelayMs, droppedFrames: e.droppedFrames, targetBitrate: e.targetBitrate, framerate: e.framerate, limited: e.limited,
      }))
      .sort((a, b) => order(a.rendition) - order(b.rendition));
  }

  /** Group IDs increase across encoder restarts on this track. */
  takeGroup(): number {
    return this.nextGroup++;
  }

  async serve(req: SubscribeRequest): Promise<void> {
    const spec = this.specs.find((s) => s.trackName === req.track.name);
    if (!spec) return req.reject(0x10, "no such rendition");
    const writer = await req.accept({ properties: [{ type: 0x30, int: 1 }] }); // DYNAMIC_GROUPS: honours NEW_GROUP_REQUEST
    const enc = new RenditionEncoder(this, spec, writer);
    enc.setSuspended(this.suspended_.has(spec.trackName));
    this.encoders.get(spec.trackName)?.close();
    this.encoders.set(spec.trackName, enc);
    writer.signal.addEventListener("abort", () => {
      if (this.encoders.get(spec.trackName) === enc) this.encoders.delete(spec.trackName);
      enc.close();
      if (this.encoders.size === 0) this.stopCapture();
    });
    this.source_ ??= startFrameSource(this.mediaStreamTrack, (f) => this.onFrame(f));
  }

  private onFrame(frame: VideoFrame) {
    if (this.muted) {
      frame.close();
      return;
    }
    const timestamp = captureClock();
    for (const enc of this.encoders.values()) enc.encode(frame, timestamp);
    frame.close();
  }

  private stopCapture() {
    this.source_?.stop();
    this.source_ = undefined;
  }

  override stop() {
    for (const t of this.draining.values()) clearTimeout(t);
    for (const e of this.encoders.values()) e.close();
    this.encoders.clear();
    this.stopCapture();
    super.stop();
  }
}

/** Encodes one rendition while it has a subscriber. */
class RenditionEncoder {
  private encoder: VideoEncoder;
  private configured?: { width: number; height: number };
  private canvas?: OffscreenCanvas;
  private ctx?: OffscreenCanvasRenderingContext2D;
  private forceKey = true;
  private lastKeyAt = 0; // capture µs of the last keyframe
  private lastFrameAt = 0; // capture µs of the last frame encoded
  private rate?: number; // bitrate set by send control; the spec's otherwise
  private rateAt = 0;
  private reconfigure = false;
  private halfRate = false;
  private suspended = false;
  private group?: { sg: OutgoingSubgroup; next: number };
  private queue = new SendQueue<EncodedFrame>((f) => this.write(f), () => this.abandonGroup());
  private closed = false;
  private failedAt = 0;
  private meter = new RateMeter();

  constructor(private track: LocalVideoTrack, private spec: RenditionSpec, private writer: TrackWriter) {
    this.encoder = this.newEncoder();
    writer.onUpdate = (p) => {
      if (p.newGroupRequest !== undefined) this.forceKey = true;
    };
  }

  private newEncoder(): VideoEncoder {
    return new VideoEncoder({
      output: (chunk) => this.output(chunk),
      error: (e) => {
        // A failed encoder closes itself; encode() makes a new one.
        console.error(`moqit: ${this.spec.trackName} encoder error (${this.track.codec} ${this.configured?.width}x${this.configured?.height})`, e);
        this.failedAt = performance.now();
      },
    });
  }

  requestKeyFrame() {
    this.forceKey = true;
  }

  get name(): string {
    return this.spec.trackName;
  }

  /** The size being encoded. */
  get encodedSize(): { width: number; height: number } | undefined {
    return this.configured;
  }

  /** Bits per second handed to the transport. */
  get bitrate(): number {
    return this.meter.bitrate;
  }

  /** New size, bitrate or frame rate: the encoder is reconfigured on the next frame. */
  setSpec(spec: RenditionSpec) {
    this.spec = spec;
    this.rate = undefined;
    this.configured = undefined;
  }

  get targetBitrate(): number {
    return this.rate ?? this.spec.bitrate;
  }

  get specBitrate(): number {
    return this.spec.bitrate;
  }

  get framerate(): number {
    return this.halfRate ? this.spec.framerate / 2 : this.spec.framerate;
  }

  get limited(): boolean {
    return this.halfRate || this.targetBitrate < 0.95 * this.spec.bitrate;
  }

  /**
   * Send control's share: the encoder's bitrate (applied without a
   * keyframe; increases at most once a second, changes under 10% ignored)
   * and whether to encode at half the frame rate.
   */
  setRate(bitrate: number, halfRate: boolean) {
    this.halfRate = halfRate;
    const b = Math.round(Math.min(bitrate, this.spec.bitrate));
    const cur = this.targetBitrate;
    if (Math.abs(b - cur) < 0.1 * cur) return;
    const now = performance.now();
    if (b > cur && now - this.rateAt < 1000) return;
    this.rate = b;
    this.rateAt = now;
    this.reconfigure = true;
  }

  setSuspended(suspended: boolean) {
    if (suspended === this.suspended) return;
    this.suspended = suspended;
    if (suspended) this.abandonGroup();
  }

  /** Output size: the rendition's box, never upscaling the source. */
  private size(frame: VideoFrame): { width: number; height: number } {
    const sw = frame.displayWidth, sh = frame.displayHeight;
    const scale = Math.min(1, this.spec.width / sw, this.spec.height / sh);
    return { width: Math.max(2, Math.round((sw * scale) / 2) * 2), height: Math.max(2, Math.round((sh * scale) / 2) * 2) };
  }

  encode(frame: VideoFrame, timestamp: number) {
    if (this.closed || this.suspended || !this.writer.forwarding) return;
    // Half frame rate: skip frames closer than two frame intervals.
    if (this.halfRate && timestamp - this.lastFrameAt < 0.9 * (2e6 / this.spec.framerate)) return;
    if (this.encoder.state === "closed") {
      if (performance.now() - this.failedAt < 1000) return; // retry at most once a second
      this.encoder = this.newEncoder();
      this.configured = undefined;
    }
    if (this.encoder.encodeQueueSize > 2) return; // encoder behind: drop rather than add latency
    const { width, height } = this.size(frame);
    const resized = !this.configured || this.configured.width !== width || this.configured.height !== height;
    if (resized || this.reconfigure) {
      this.configured = { width, height };
      // A new size needs a keyframe; a new bitrate alone doesn't.
      if (resized) this.forceKey = true;
      this.reconfigure = false;
      try {
        this.encoder.configure(videoEncoderConfig(this.track.codec, width, height, this.targetBitrate, this.spec.framerate));
      } catch (e) {
        console.error(`moqit: ${this.spec.trackName} encoder configure failed (${this.track.codec} ${width}x${height})`, e);
        this.failedAt = performance.now();
        return;
      }
    }
    let input: VideoFrame;
    // Frames in an opaque format (format null, e.g. GPU-backed camera
    // frames) can fail in the encoder; drawn through a canvas they encode.
    if (width === frame.displayWidth && height === frame.displayHeight && frame.format !== null) {
      input = new VideoFrame(frame, { timestamp });
    } else {
      if (!this.canvas || this.canvas.width !== width || this.canvas.height !== height) {
        this.canvas = new OffscreenCanvas(width, height);
        this.ctx = this.canvas.getContext("2d")!;
      }
      this.ctx!.drawImage(frame, 0, 0, width, height);
      input = new VideoFrame(this.canvas, { timestamp });
    }
    const keyFrame = this.forceKey || timestamp - this.lastKeyAt >= GOP_SECONDS * 1e6;
    this.forceKey = false;
    if (keyFrame) this.lastKeyAt = timestamp;
    this.lastFrameAt = timestamp;
    this.encoder.encode(input, { keyFrame });
    input.close();
  }

  private output(chunk: EncodedVideoChunk) {
    const payload = new Uint8Array(chunk.byteLength);
    chunk.copyTo(payload);
    this.queue.push({ key: chunk.type === "key", bytes: payload.byteLength, payload, timestamp: chunk.timestamp });
  }

  private async write(f: EncodedFrame) {
    if (this.closed) return;
    if (f.key) {
      // Each GOP is a Group, starting with its keyframe.
      await this.group?.sg.close();
      this.group = { sg: this.writer.openSubgroup(this.track.takeGroup(), 0, { properties: true }), next: 0 };
    }
    const g = this.group;
    if (!g) return; // no keyframe yet
    try {
      await g.sg.writeObject({ id: g.next++, status: 0, payload: f.payload, properties: [{ type: LocProperty.Timestamp, int: f.timestamp }] });
      this.meter.add(f.payload.byteLength);
    } catch (e) {
      if (this.group === g) this.abandonGroup();
      throw e;
    }
  }

  // The rest of the Group can't be sent in time: reset it and start the
  // next from a keyframe.
  private abandonGroup() {
    this.group?.sg.reset();
    this.group = undefined;
    this.forceKey = true;
  }

  /** How long the oldest unsent frame has waited (ms), and frames dropped so far. */
  get sendDelayMs(): number {
    return this.queue.delay();
  }
  get droppedFrames(): number {
    return this.queue.droppedFrames;
  }

  close() {
    if (this.closed) return;
    this.closed = true;
    try {
      this.encoder.close();
    } catch {
      // already closed
    }
    this.queue.close();
    void (async () => {
      await this.queue.idle();
      await this.group?.sg.close();
      await this.writer.close();
    })().catch(() => {});
  }
}

interface EncodedFrame {
  key: boolean;
  bytes: number;
  payload: Uint8Array;
  timestamp: number;
}

const SAMPLE_RATE = 48000;
const FRAME_SAMPLES = 960; // 20 ms
const MAX_PENDING_AUDIO = 25; // 500 ms of audio waiting for the transport

// Capture worklet: slices the microphone into 20 ms frames and measures
// their level.
const captureWorklet = `
class MoqitCapture extends AudioWorkletProcessor {
  constructor() { super(); this.buf = new Float32Array(${FRAME_SAMPLES}); this.n = 0; }
  process(inputs) {
    const ch = inputs[0] && inputs[0][0];
    if (!ch) return true;
    for (let i = 0; i < ch.length; i++) {
      this.buf[this.n++] = ch[i];
      if (this.n === this.buf.length) {
        let sum = 0;
        for (let j = 0; j < this.n; j++) sum += this.buf[j] * this.buf[j];
        const rms = Math.sqrt(sum / this.n);
        const dbov = rms > 0 ? Math.max(-127, 20 * Math.log10(rms)) : -127;
        this.port.postMessage({ samples: this.buf, level: Math.round(-dbov) }, [this.buf.buffer]);
        this.buf = new Float32Array(${FRAME_SAMPLES});
        this.n = 0;
      }
    }
    return true;
  }
}
registerProcessor("moqit-capture", MoqitCapture);
`;

/** Microphone or screen audio, published as one MoQ track ("mic"). */
export class LocalAudioTrack extends LocalTrackBase {
  readonly kind = TrackKind.AUDIO;
  readonly codec = "opus";
  /** Latest level, 0 (silent) to 1. */
  level = 0;
  /** Time the latest frame spent in the encoder, in ms. */
  encodeDelayMs = 0;
  /** Time from encoder output until the latest frame was handed to the transport, in ms. */
  sendDelayMs = 0;
  // Capture times of frames in the encoder, oldest first. Opus emits one
  // packet per 20 ms frame in order, but does not preserve timestamps
  // exactly, so LOC timestamps come from here.
  private inFlight: { ts: number; at: number }[] = [];
  private writers = new Set<TrackWriter>();
  private ctx?: AudioContext;
  private capture?: { src: MediaStreamAudioSourceNode; node: AudioWorkletNode };
  private encoder?: AudioEncoder;
  private nextGroup = 0;
  private lastLevel = 127;
  private pending = 0;
  private meter = new RateMeter();

  /** Bits per second handed to the transport (Opus payload). */
  get bitrate(): number {
    return this.meter.bitrate;
  }

  constructor(track: MediaStreamTrack, readonly source: TrackSource) {
    super(track);
  }

  get name(): string {
    return this.source === TrackSource.SCREEN_AUDIO ? "screen-audio" : "microphone";
  }

  trackNames(): string[] {
    return [this.source === TrackSource.SCREEN_AUDIO ? TrackName.ScreenAudio : TrackName.Mic];
  }

  renditions(): Rendition[] {
    return [create(RenditionSchema, { trackName: this.trackNames()[0], bitrate: 32000 })];
  }

  async serve(req: SubscribeRequest): Promise<void> {
    const writer = await req.accept({ properties: [{ type: 0x0e, int: 16 }] }); // audio ahead of video
    this.writers.add(writer);
    writer.signal.addEventListener("abort", () => {
      this.writers.delete(writer);
      if (this.writers.size === 0) this.stopCapture();
    });
    await this.startCapture();
  }

  private async startCapture() {
    if (this.ctx) return;
    this.ctx = new AudioContext({ sampleRate: SAMPLE_RATE });
    const url = URL.createObjectURL(new Blob([captureWorklet], { type: "text/javascript" }));
    await this.ctx.audioWorklet.addModule(url);
    URL.revokeObjectURL(url);
    const src = this.ctx.createMediaStreamSource(new MediaStream([this.mediaStreamTrack]));
    const node = new AudioWorkletNode(this.ctx, "moqit-capture", { numberOfInputs: 1, numberOfOutputs: 0, channelCount: 1, channelCountMode: "explicit" });
    src.connect(node);
    this.capture = { src, node };
    this.encoder = new AudioEncoder({ output: (c) => this.output(c), error: (e) => console.warn("moqit: audio encoder error", e) });
    this.encoder.configure({ codec: "opus", sampleRate: SAMPLE_RATE, numberOfChannels: 1, bitrate: 32000, opus: { frameDuration: 20000 } } as AudioEncoderConfig);
    node.port.onmessage = (e: MessageEvent<{ samples: Float32Array<ArrayBuffer>; level: number }>) => {
      this.lastLevel = e.data.level;
      // Same -60..0 dBFS scale as RemoteAudioTrack.level.
      this.level = Math.max(Math.max(0, Math.min(1, (60 - e.data.level) / 60)), this.level * 0.85);
      if (this.muted || this.encoder?.state !== "configured") return;
      const ts = captureClock();
      const data = new AudioData({ format: "f32-planar", sampleRate: SAMPLE_RATE, numberOfFrames: FRAME_SAMPLES, numberOfChannels: 1, timestamp: ts, data: e.data.samples });
      this.inFlight.push({ ts, at: performance.now() });
      this.encoder.encode(data);
      data.close();
    };
    void this.ctx.resume();
  }

  protected trackReplaced() {
    if (!this.ctx || !this.capture) return;
    // Same worklet and encoder: the stream continues from the new device.
    this.capture.src.disconnect();
    this.capture.src = this.ctx.createMediaStreamSource(new MediaStream([this.mediaStreamTrack]));
    this.capture.src.connect(this.capture.node);
  }

  private stopCapture() {
    void this.ctx?.close();
    this.ctx = undefined;
    this.capture = undefined;
    try {
      this.encoder?.close();
    } catch {
      // already closed
    }
    this.encoder = undefined;
  }

  private output(chunk: EncodedAudioChunk) {
    const src = this.inFlight.shift();
    const now = performance.now();
    if (src) this.encodeDelayMs = now - src.at;
    const payload = new Uint8Array(chunk.byteLength);
    chunk.copyTo(payload);
    const group = this.nextGroup++;
    const properties = [{ type: LocProperty.Timestamp, int: src?.ts ?? chunk.timestamp }, { type: LocProperty.AudioLevel, int: this.lastLevel }];
    // One Object per Group (LOC), each on its own short stream. Streams are
    // independent, so frames are sent concurrently: waiting for each
    // stream before opening the next would queue audio behind slow stream
    // creation. Frames beyond MAX_PENDING are dropped rather than delayed.
    for (const w of this.writers) {
      if (!w.forwarding || this.pending >= MAX_PENDING_AUDIO) continue;
      this.pending++;
      const sg = w.openSubgroup(group, 0, { properties: true, endOfGroup: true });
      void sg.writeObject({ id: 0, status: 0, payload, properties })
        .then(() => {
          this.sendDelayMs = performance.now() - now;
          this.meter.add(payload.byteLength);
          return sg.close();
        })
        .catch(() => sg.reset())
        .finally(() => this.pending--);
    }
  }

  override stop() {
    for (const w of this.writers) void w.close();
    this.writers.clear();
    this.stopCapture();
    super.stop();
  }
}
