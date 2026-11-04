import { create } from "@bufbuild/protobuf";
import { afterEach, expect, test, vi } from "vitest";
import { TrackInfoSchema, TrackKind, TrackSource } from "./gen/moqit/v1/models_pb.ts";
import { resetCodecCaches } from "./codecs.ts";
import { RemoteTrackPublication, RemoteVideoTrack } from "./remote.ts";

afterEach(() => vi.unstubAllGlobals());

class FakeDecoder {
  static all: FakeDecoder[] = [];
  state = "unconfigured";
  config?: VideoDecoderConfig;
  decoded: string[] = [];
  constructor(public init: VideoDecoderInit) {
    FakeDecoder.all.push(this);
  }
  configure(c: VideoDecoderConfig) {
    this.config = c;
    this.state = "configured";
  }
  decode(c: { type: string }) {
    this.decoded.push(c.type);
  }
  close() {
    this.state = "closed";
  }
  fail() {
    this.state = "closed";
    this.init.error(new DOMException("decode failed", "EncodingError"));
  }
}

test("a failed video decoder is replaced, resuming at a keyframe, and repeated failures switch to software", () => {
  FakeDecoder.all = [];
  vi.stubGlobal("VideoDecoder", FakeDecoder);
  vi.stubGlobal("EncodedVideoChunk", class { type: string; constructor(o: { type: string }) { this.type = o.type; } });
  vi.spyOn(console, "warn").mockImplementation(() => {});
  const track = new RemoteVideoTrack(create(TrackInfoSchema, { sid: "v", kind: TrackKind.VIDEO, source: TrackSource.CAMERA, codec: "avc1.42e01f" }));
  track.enterArchive(); // archived objects are fed directly, without a subscription
  const data = new Uint8Array(4);
  const current = () => FakeDecoder.all[FakeDecoder.all.length - 1];

  track.archiveObject(data, 1, true);
  track.archiveObject(data, 2, false);
  current().fail();
  const second = current();
  expect(FakeDecoder.all).toHaveLength(3); // constructor, enterArchive, replacement
  expect(second.config?.hardwareAcceleration).toBeUndefined();
  track.archiveObject(data, 3, false); // undecodable without its keyframe
  track.archiveObject(data, 4, true);
  track.archiveObject(data, 5, false);
  expect(second.decoded).toEqual(["key", "delta"]);

  second.fail();
  expect(current().config?.hardwareAcceleration).toBe("prefer-software");
  expect(current().config?.codec).toBe("avc1.42e01f");

  // A decoder replaced on purpose reporting an error later changes nothing.
  let n = FakeDecoder.all.length;
  second.init.error(new DOMException("late", "EncodingError"));
  expect(FakeDecoder.all).toHaveLength(n);

  // One that fails every time is given up on rather than replaced forever.
  for (let i = 0; i < 10; i++) current().fail();
  n = FakeDecoder.all.length;
  expect(n).toBe(7); // 2 + five replacements
  expect(current().state).toBe("closed");
});

test("a subscription superseded while it was being set up is dropped", async () => {
  FakeDecoder.all = [];
  vi.stubGlobal("VideoDecoder", FakeDecoder);
  type FakeSub = { name: string; unsubscribed: boolean; unsubscribe(): void; [Symbol.asyncIterator](): AsyncIterator<never> };
  const pending: { name: string; resolve: (s: FakeSub) => void }[] = [];
  const subs: FakeSub[] = [];
  const session = {
    subscribe: (track: { name: string }) => new Promise<FakeSub>((resolve) => pending.push({ name: track.name, resolve })),
  };
  const make = (name: string): FakeSub => {
    const s: FakeSub = {
      name, unsubscribed: false,
      unsubscribe() { s.unsubscribed = true; },
      [Symbol.asyncIterator]: () => ({ next: () => new Promise(() => {}) }),
    };
    subs.push(s);
    return s;
  };
  const track = new RemoteVideoTrack(create(TrackInfoSchema, { sid: "v", kind: TrackKind.VIDEO, source: TrackSource.CAMERA, codec: "vp8" }));
  const first = track.start(session as never, ["moqit", "r", "p", "a"], "camera-m");
  const second = track.switchTo("camera-h"); // the element's size came first
  expect(pending.map((p) => p.name)).toEqual(["camera-m", "camera-h"]);
  pending[1].resolve(make("camera-h"));
  pending[0].resolve(make("camera-m"));
  await Promise.all([first, second]);
  expect(subs.find((s) => s.name === "camera-m")!.unsubscribed).toBe(true);
  expect(subs.find((s) => s.name === "camera-h")!.unsubscribed).toBe(false);

  // A stop while subscribing drops the late subscription too.
  const third = track.switchTo("camera-l");
  track.stop();
  pending[2].resolve(make("camera-l"));
  await third;
  expect(subs.find((s) => s.name === "camera-l")!.unsubscribed).toBe(true);
});

// A publication of a camera in AV1 with an H.264 alternative, subscribed
// through a session whose subscriptions never deliver.
function publication(decodable: (codec: string) => boolean, encodings = [["camera-m.av1", "av01.0.05M.08"], ["camera-m.avc", "avc1.42e01f"]]) {
  FakeDecoder.all = [];
  vi.stubGlobal("VideoDecoder", Object.assign(FakeDecoder, { isConfigSupported: async (c: VideoDecoderConfig) => ({ supported: decodable(c.codec) }) }));
  vi.spyOn(console, "warn").mockImplementation(() => {});
  resetCodecCaches();
  const subscribed: string[] = [];
  const session = {
    subscribe: async (track: { name: string }) => {
      subscribed.push(track.name);
      return { unsubscribe() {}, update: async () => {}, [Symbol.asyncIterator]: () => ({ next: () => new Promise(() => {}) }) };
    },
  };
  const info = (encs: string[][]) => create(TrackInfoSchema, {
    sid: "v", kind: TrackKind.VIDEO, source: TrackSource.CAMERA, codec: encs[0][1],
    renditions: [{ trackName: encs[0][0], width: 640, height: 360, encodings: encs.map(([trackName, codec]) => ({ trackName, codec })) }],
  });
  const ctx = { session, room: "r", identity: "alice", audio: {}, adaptive: false, audioClock: () => undefined, volume: () => 1 };
  const pub = new RemoteTrackPublication(info(encodings), ctx as never, "medium");
  const unsupported: boolean[] = [];
  pub.on("unsupported", (u) => unsupported.push(u));
  return { pub, subscribed, info, unsupported };
}

const settle = () => new Promise((r) => setTimeout(r, 0));

test("a subscriber takes the publisher's preferred codec when it can decode it", async () => {
  const { pub, subscribed } = publication(() => true);
  await pub.setSubscribed(true);
  expect(subscribed).toEqual(["camera-m.av1"]);
  expect(pub.currentRendition).toBe("camera-m");
  expect(pub.currentTrackName).toBe("camera-m.av1");
  expect((pub.track as RemoteVideoTrack).codec).toBe("av01.0.05M.08");
  expect(FakeDecoder.all.map((d) => d.config?.codec)).toEqual(["av01.0.05M.08"]);
});

test("a subscriber that can't decode the preferred codec takes an alternative", async () => {
  const { pub, subscribed } = publication((c) => !c.startsWith("av01"));
  await pub.setSubscribed(true);
  expect(subscribed).toEqual(["camera-m.avc"]);
  expect(FakeDecoder.all.every((d) => d.config?.codec === "avc1.42e01f")).toBe(true);
  expect(pub.unsupported).toBe(false);
});

test("a live codec switch moves subscribers; a codec failing to decode moves them to the next", async () => {
  const { pub, subscribed, info } = publication(() => true, [["camera-m.avc", "avc1.42e01f"], ["camera-m.vp8", "vp8"]]);
  await pub.setSubscribed(true);
  pub.update(info([["camera-m.av1", "av01.0.05M.08"], ["camera-m.avc", "avc1.42e01f"], ["camera-m.vp8", "vp8"]]));
  await settle();
  expect(subscribed).toEqual(["camera-m.avc", "camera-m.av1"]);
  expect((pub.track as RemoteVideoTrack).info.codec).toBe("av01.0.05M.08");

  // A device that reports AV1 decodable but fails on every frame.
  (pub.track as RemoteVideoTrack).onUndecodable!("av01.0.05M.08");
  await settle();
  expect(subscribed.at(-1)).toBe("camera-m.avc");
  (pub.track as RemoteVideoTrack).onUndecodable!("avc1.42e01f");
  await settle();
  expect(subscribed.at(-1)).toBe("camera-m.vp8");
});

test("a video no codec of which decodes is flagged, until one is offered", async () => {
  const { pub, subscribed, info, unsupported } = publication((c) => c === "vp8", [["camera-m.hevc", "hev1.1.6.L93.B0"]]);
  await pub.setSubscribed(true);
  expect(pub.isSubscribed).toBe(true);
  expect(pub.unsupported).toBe(true);
  expect((pub.track as RemoteVideoTrack).unsupported).toBe(true);
  expect(subscribed).toEqual([]);
  pub.update(info([["camera-m.hevc", "hev1.1.6.L93.B0"], ["camera-m.vp8", "vp8"]]));
  await settle();
  expect(subscribed).toEqual(["camera-m.vp8"]);
  expect(unsupported).toEqual([true, false]);
});

test("a codec string changing under the same track reconfigures at the next Group", async () => {
  const { pub, subscribed, info } = publication(() => true, [["camera-m.avc", "avc1.42e01f"]]);
  await pub.setSubscribed(true);
  pub.update(info([["camera-m.avc", "avc1.42e028"]]));
  await settle();
  expect(subscribed).toEqual(["camera-m.avc"]);
  // Applied on the next Group, so the decoder in use keeps the old one until then.
  expect((pub.track as RemoteVideoTrack).codec).toBe("avc1.42e01f");
});
