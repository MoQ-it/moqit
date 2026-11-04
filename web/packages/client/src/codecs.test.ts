import { afterEach, expect, test, vi } from "vitest";
import {
  type CodecSupport, codecOf, codecString, encodingTrackName, parseEncodingTrackName, probeVideoCodecs, rankCodecs, resetCodecCaches, type VideoCodec, videoEncoderConfig,
} from "./codecs.ts";
import { CameraPresets, ScreenRenditions } from "./util.ts";

afterEach(() => {
  vi.unstubAllGlobals();
  resetCodecCaches();
});

const p180 = { width: 320, height: 180, framerate: 15 };
const p720 = { width: 1280, height: 720, framerate: 30 };
const p1080 = { width: 1920, height: 1080, framerate: 30 };
const p1440 = { width: 2560, height: 1440, framerate: 30 };
const p1080at60 = { width: 1920, height: 1080, framerate: 60 };

test("codec strings carry a level that fits the largest rendition, never below 3.1", () => {
  expect(codecString("h264", [p180])).toBe("avc1.42e01f");
  expect(codecString("h264", [p1080, p180])).toBe("avc1.42e028");
  expect(codecString("h265", [p180])).toBe("hev1.1.6.L93.B0");
  expect(codecString("h265", [p1080])).toBe("hev1.1.6.L120.B0");
  expect(codecString("h265", [p1080at60])).toBe("hev1.1.6.L123.B0");
  expect(codecString("h265", [p1440])).toBe("hev1.1.6.L150.B0");
  expect(codecString("vp9", [p720])).toBe("vp09.00.31.08");
  expect(codecString("vp9", [p1080])).toBe("vp09.00.40.08");
  expect(codecString("vp9", [p1080at60])).toBe("vp09.00.41.08");
  expect(codecString("av1", [p720])).toBe("av01.0.05M.08");
  expect(codecString("av1", [p1080])).toBe("av01.0.08M.08");
  expect(codecString("av1", [p1080at60])).toBe("av01.0.09M.08");
  expect(codecString("av1", [p1440])).toBe("av01.0.12M.08");
  expect(codecString("vp8", CameraPresets["1080p"].renditions)).toBe("vp8");
  expect(codecString("h264", ScreenRenditions)).toBe("avc1.42e028");
});

test("codec families and encoding track names", () => {
  const cases: [string, VideoCodec][] = [["avc1.42e01f", "h264"], ["hev1.1.6.L93.B0", "h265"], ["hvc1.1.6.L120.B0", "h265"], ["vp8", "vp8"], ["vp09.00.10.08", "vp9"], ["av01.0.05M.08", "av1"]];
  for (const [s, c] of cases) expect(codecOf(s)).toBe(c);
  expect(codecOf("opus")).toBeUndefined();
  for (const c of ["h264", "h265", "vp8", "vp9", "av1"] as VideoCodec[]) {
    expect(parseEncodingTrackName(encodingTrackName("camera-h", c))).toEqual({ rendition: "camera-h", codec: c });
  }
  expect(encodingTrackName("screen-l", "h265")).toBe("screen-l.hevc");
  expect(parseEncodingTrackName("camera-m")).toEqual({ rendition: "camera-m" });
  expect(parseEncodingTrackName("camera-m.xyz")).toEqual({ rendition: "camera-m.xyz" });
});

test("encoder config keeps H.264 and H.265 parameter sets in-band", () => {
  expect(videoEncoderConfig("avc1.42e01f", 640, 360, 1, 30).avc).toEqual({ format: "annexb" });
  expect((videoEncoderConfig("hev1.1.6.L93.B0", 640, 360, 1, 30) as { hevc?: unknown }).hevc).toEqual({ format: "annexb" });
  const av1 = videoEncoderConfig("av01.0.05M.08", 640, 360, 1, 30, "prefer-hardware");
  expect(av1.avc).toBeUndefined();
  expect(av1.hardwareAcceleration).toBe("prefer-hardware");
  expect(av1.latencyMode).toBe("realtime");
});

const sw = (codec: VideoCodec): CodecSupport => ({ codec, encode: true, hardware: false, decode: true });
const hw = (codec: VideoCodec): CodecSupport => ({ codec, encode: true, hardware: true, decode: true });
const none = (codec: VideoCodec): CodecSupport => ({ codec, encode: false, hardware: false, decode: true });

test("ranking: hardware first by efficiency, then cheap software codecs", () => {
  // Software only (e.g. headless Linux): H.264 as before, VP8 on Firefox.
  const soft = ["av1", "h265", "vp9", "h264", "vp8"].map((c) => (c === "h265" ? none : sw)(c as VideoCodec));
  expect(rankCodecs(soft, { firefox: false })).toEqual(["h264", "vp8", "vp9", "av1"]);
  expect(rankCodecs(soft, { firefox: true })).toEqual(["vp8", "vp9", "av1", "h264"]);
  // Hardware AV1 and HEVC lead; a baseline stays among the alternatives.
  const gpu = [hw("av1"), hw("h265"), sw("vp9"), hw("h264"), sw("vp8")];
  expect(rankCodecs(gpu, { firefox: false })).toEqual(["av1", "h265", "h264", "vp8"]);
  // Firefox's H.264 holds frames even in hardware.
  expect(rankCodecs([hw("h264"), sw("vp8")], { firefox: true })).toEqual(["vp8", "h264"]);
});

test("ranking: preference, room policy, broken codecs and the baseline", () => {
  const gpu = [hw("av1"), hw("h265"), hw("vp9"), sw("h264"), sw("vp8")];
  expect(rankCodecs(gpu, { preference: "vp8", firefox: false })).toEqual(["vp8", "av1", "h265", "vp9"]);
  expect(rankCodecs(gpu, { preference: "auto", firefox: false })[0]).toBe("av1");
  // At most three alternatives, one of them H.264 or VP8.
  expect(rankCodecs(gpu, { firefox: false })).toEqual(["av1", "h265", "vp9", "h264"]);
  expect(rankCodecs(gpu, { allowed: ["vp9", "vp8"], firefox: false })).toEqual(["vp9", "vp8"]);
  expect(rankCodecs(gpu, { allowed: ["h265"], preference: "av1", firefox: false })).toEqual(["h265"]);
  expect(rankCodecs(gpu, { broken: new Set(["av1"] as VideoCodec[]), firefox: false })).toEqual(["h265", "vp9", "h264", "vp8"]);
  // A preference that can't be encoded is ignored.
  expect(rankCodecs([sw("h264"), none("av1")], { preference: "av1", firefox: false })).toEqual(["h264"]);
  expect(rankCodecs([none("h264")], { firefox: false })).toEqual([]);
});

test("probing asks for every rendition, in hardware and then at all, and caches", async () => {
  const asked: string[] = [];
  vi.stubGlobal("VideoEncoder", {
    isConfigSupported: async (c: VideoEncoderConfig) => {
      asked.push(`${c.codec}@${c.width}${c.hardwareAcceleration === "prefer-hardware" ? ":hw" : ""}`);
      const family = codecOf(c.codec);
      if (family === "h265") return { supported: c.hardwareAcceleration === "prefer-hardware" };
      if (family === "av1") return { supported: c.width <= 640 }; // too slow for the top layer
      if (family === "vp9") throw new TypeError("bad config");
      return { supported: c.hardwareAcceleration !== "prefer-hardware" };
    },
  });
  vi.stubGlobal("VideoDecoder", { isConfigSupported: async (c: VideoDecoderConfig) => ({ supported: codecOf(c.codec) !== "h265" }) });
  const specs = CameraPresets["720p"].renditions;
  const support = await probeVideoCodecs(specs);
  const by = Object.fromEntries(support.map((s) => [s.codec, s]));
  expect(by.h265).toEqual({ codec: "h265", encode: true, hardware: true, decode: false });
  expect(by.av1).toMatchObject({ encode: false, hardware: false, decode: true });
  expect(by.vp9).toMatchObject({ encode: false, decode: true });
  expect(by.h264).toEqual({ codec: "h264", encode: true, hardware: false, decode: true });
  expect(asked).toContain("hev1.1.6.L93.B0@1280:hw");
  expect(asked).toContain("avc1.42e01f@320");
  const n = asked.length;
  expect(await probeVideoCodecs(specs)).toBe(support);
  expect(asked.length).toBe(n);
});
