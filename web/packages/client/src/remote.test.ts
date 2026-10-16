import { create } from "@bufbuild/protobuf";
import { afterEach, expect, test, vi } from "vitest";
import { TrackInfoSchema, TrackKind, TrackSource } from "./gen/moqit/v1/models_pb.ts";
import { RemoteVideoTrack } from "./remote.ts";

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
