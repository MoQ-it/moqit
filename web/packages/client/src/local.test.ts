import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { TrackSource } from "./gen/moqit/v1/models_pb.ts";
import { LocalVideoTrack } from "./local.ts";
import { CameraPresets } from "./util.ts";

class FakeEncoder {
  static all: FakeEncoder[] = [];
  state = "unconfigured";
  encodeQueueSize = 0;
  configs: VideoEncoderConfig[] = [];
  constructor(public init: VideoEncoderInit) {
    FakeEncoder.all.push(this);
  }
  configure(c: VideoEncoderConfig) {
    this.configs.push(c);
    this.state = "configured";
  }
  encode() {}
  close() {
    this.state = "closed";
  }
  fail() {
    this.state = "closed";
    this.init.error(new DOMException("encode failed", "EncodingError"));
  }
}

class FakeFrame {
  displayWidth = 1280;
  displayHeight = 720;
  format = "I420";
  timestamp = 0;
  close() {}
}

// Frames pushed by the test into the capture pipeline.
let push: (f: FakeFrame) => void = () => {};

beforeEach(() => {
  FakeEncoder.all = [];
  vi.stubGlobal("VideoEncoder", FakeEncoder);
  vi.stubGlobal("VideoFrame", FakeFrame);
  vi.stubGlobal("OffscreenCanvas", class {
    constructor(public width: number, public height: number) {}
    getContext() {
      return { drawImage() {} };
    }
  });
  vi.stubGlobal("MediaStreamTrackProcessor", class {
    readable = new ReadableStream<FakeFrame>({ start: (c) => void (push = (f) => c.enqueue(f)) });
  });
  vi.spyOn(console, "error").mockImplementation(() => {});
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

function request(name: string) {
  const abort = new AbortController();
  const req = {
    track: { name },
    rejected: undefined as string | undefined,
    reject: async (_code: number, reason: string) => void (req.rejected = reason),
    accept: async () => ({ forwarding: true, signal: abort.signal, openSubgroup: () => ({ writeObject: async () => {}, close: async () => {}, reset() {} }), close: async () => {} }),
    abort,
  };
  return req;
}

const media = { stop() {} } as unknown as MediaStreamTrack;

const flush = () => new Promise((r) => setTimeout(r, 0));

test("each rendition is announced and served in every codec offered, preferred first", async () => {
  const t = new LocalVideoTrack(media, TrackSource.CAMERA, ["av1", "h264"], CameraPresets["720p"].renditions);
  expect(t.codec).toBe("av01.0.05M.08");
  const [h] = t.renditions();
  expect(h.trackName).toBe("camera-h.av1");
  expect(h.encodings.map((e) => [e.trackName, e.codec])).toEqual([["camera-h.av1", "av01.0.05M.08"], ["camera-h.avc", "avc1.42e01f"]]);
  expect(t.trackNames()).toContain("camera-l.avc");

  for (const bad of ["camera-h", "camera-h.vp9", "camera-x.av1"]) {
    const r = request(bad);
    await t.serve(r as never);
    expect(r.rejected, bad).toBe("no such rendition");
  }
  await t.serve(request("camera-h.av1") as never);
  await t.serve(request("camera-h.avc") as never);
  push(new FakeFrame());
  await flush();
  expect(FakeEncoder.all.map((e) => e.configs[0]?.codec)).toEqual(["av01.0.05M.08", "avc1.42e01f"]);
  expect(t.sending().map((s) => [s.rendition, s.trackName, s.codec])).toEqual([["camera-h", "camera-h.av1", "av01.0.05M.08"], ["camera-h", "camera-h.avc", "avc1.42e01f"]]);
  // Both encodings of camera-h share its layer's bitrate.
  expect(t.layers()[0].layer.spec).toBe(2 * CameraPresets["720p"].renditions[0].bitrate);
  t.stop();
});

test("a codec no longer offered keeps sending while subscribers move, then stops", async () => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  const t = new LocalVideoTrack(media, TrackSource.CAMERA, ["h264", "vp8"], CameraPresets["360p"].renditions);
  await t.serve(request("camera-m.avc") as never);
  await t.serve(request("camera-m.vp8") as never);
  t.setCodecs(["vp8"]);
  expect(t.renditions()[0].encodings.map((e) => e.trackName)).toEqual(["camera-m.vp8"]);
  expect(t.sending()).toHaveLength(2);
  vi.advanceTimersByTime(5000);
  expect(t.sending().map((s) => s.trackName)).toEqual(["camera-m.vp8"]);
  expect(FakeEncoder.all[0].state).toBe("closed");
  t.stop();
});

test("a codec string given keeps its profile, with the level fitted", () => {
  const t = new LocalVideoTrack(media, TrackSource.CAMERA, "avc1.64001f", CameraPresets["1080p"].renditions);
  expect(t.codecs).toEqual(["h264"]);
  expect(t.codec).toBe("avc1.640028");
  expect(() => new LocalVideoTrack(media, TrackSource.CAMERA, "theora", [])).toThrow();
});

test("an encoder failing repeatedly reports its codec unusable", async () => {
  let now = 0;
  vi.spyOn(performance, "now").mockImplementation(() => now);
  const t = new LocalVideoTrack(media, TrackSource.CAMERA, ["h265", "h264"], CameraPresets["180p"].renditions);
  const failures: string[] = [];
  t.onCodecFailure = (codec, reason) => failures.push(`${codec}: ${reason}`);
  await t.serve(request("camera-l.hevc") as never);
  for (let i = 0; i < 3; i++) {
    push(new FakeFrame());
    await flush();
    FakeEncoder.all[FakeEncoder.all.length - 1].fail();
    now += 1500; // past the retry delay
  }
  expect(failures).toEqual(["h265: encoder error: encode failed"]);
  t.stop();
});
