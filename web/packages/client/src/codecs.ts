// Video codecs: codec strings, per-codec MoQ track names, capability probes
// and the order a publisher offers them in.

/** A video codec family. */
export type VideoCodec = "h264" | "h265" | "vp8" | "vp9" | "av1";

export const VideoCodecs: readonly VideoCodec[] = ["av1", "h265", "vp9", "h264", "vp8"];

/** Display names: "H.264", "AV1". */
export const VideoCodecNames: Record<VideoCodec, string> = { h264: "H.264", h265: "H.265", vp8: "VP8", vp9: "VP9", av1: "AV1" };

/** The display name of a WebCodecs codec string's family, e.g. "AV1"; the string itself if unknown. */
export function videoCodecName(codec: string): string {
  const c = codecOf(codec);
  return c ? VideoCodecNames[c] : codec;
}

/** What this browser and device can do with a codec. */
export interface CodecSupport {
  codec: VideoCodec;
  /** Can encode every rendition asked about. */
  encode: boolean;
  /** Encodes in hardware (prefer-hardware is supported). */
  hardware: boolean;
  /** Can decode it. */
  decode: boolean;
}

/** Suffix of a MoQ track carrying a codec: "camera-h.av1". */
const TAGS: Record<VideoCodec, string> = { h264: "avc", h265: "hevc", vp8: "vp8", vp9: "vp9", av1: "av1" };

/** The codec family of a WebCodecs codec string; undefined if not a known video codec. */
export function codecOf(codec: string): VideoCodec | undefined {
  const c = codec.toLowerCase();
  if (c.startsWith("avc1") || c.startsWith("avc3")) return "h264";
  if (c.startsWith("hev1") || c.startsWith("hvc1")) return "h265";
  if (c === "vp8") return "vp8";
  if (c.startsWith("vp09") || c === "vp9") return "vp9";
  if (c.startsWith("av01")) return "av1";
  return undefined;
}

/** The track name of a rendition in a codec, e.g. "camera-h.av1". */
export function encodingTrackName(rendition: string, codec: VideoCodec): string {
  return `${rendition}.${TAGS[codec]}`;
}

/** Splits an encoding's track name; a name without a codec suffix is the rendition itself. */
export function parseEncodingTrackName(name: string): { rendition: string; codec?: VideoCodec } {
  const dot = name.lastIndexOf(".");
  if (dot > 0) {
    const tag = name.slice(dot + 1);
    const codec = (Object.keys(TAGS) as VideoCodec[]).find((c) => TAGS[c] === tag);
    if (codec) return { rendition: name.slice(0, dot), codec };
  }
  return { rendition: name };
}

interface Size {
  width: number;
  height: number;
  framerate: number;
}

// [level, max luma picture size, max luma samples per second]
type LevelTable = [string, number, number][];

// H.264 levels in macroblocks: [level_idc, max frame size, max macroblocks/s].
const AVC_LEVELS: [number, number, number][] = [
  [0x1f, 3600, 108000], // 3.1: 1280x720@30
  [0x20, 5120, 216000], // 3.2
  [0x28, 8192, 245760], // 4.0: 1920x1080@30
  [0x2a, 8704, 522240], // 4.2
  [0x32, 22080, 589824], // 5.0
  [0x33, 36864, 983040], // 5.1: 4096x2160@30
  [0x34, 36864, 2073600], // 5.2
];

// H.265 Main tier, level_idc = 30 × level (ITU-T H.265 Table A.8).
const HEVC_LEVELS: LevelTable = [
  ["93", 983040, 33177600], // 3.1
  ["120", 2228224, 66846720], // 4
  ["123", 2228224, 133693440], // 4.1
  ["150", 8912896, 267386880], // 5
  ["153", 8912896, 534773760], // 5.1
  ["156", 8912896, 1069547520], // 5.2
];

// VP9 levels (VP9 bitstream spec, Annex A).
const VP9_LEVELS: LevelTable = [
  ["31", 983040, 36864000], // 3.1
  ["40", 2228224, 83558400], // 4
  ["41", 2228224, 160432128], // 4.1
  ["50", 8912896, 311951360], // 5
  ["51", 8912896, 588251136], // 5.1
  ["52", 8912896, 1176502272], // 5.2
];

// AV1 seq_level_idx (AV1 spec, Annex A.3): MaxPicSize, MaxDisplayRate.
const AV1_LEVELS: LevelTable = [
  ["05", 1065024, 31950720], // 3.1
  ["08", 2359296, 70778880], // 4.0
  ["09", 2359296, 141557760], // 4.1
  ["12", 8912896, 267386880], // 5.0
  ["13", 8912896, 534773760], // 5.1
  ["14", 8912896, 1069547520], // 5.2
];

function fit(table: LevelTable, specs: Size[]): string {
  let ps = 0, sr = 0;
  for (const s of specs) {
    ps = Math.max(ps, s.width * s.height);
    sr = Math.max(sr, s.width * s.height * s.framerate);
  }
  return (table.find(([, maxPs, maxSr]) => ps <= maxPs && sr <= maxSr) ?? table[table.length - 1])[0];
}

/**
 * The codec string to publish for a set of renditions. Encoders refuse
 * sizes above the level in the codec string, so the level is raised to fit
 * the largest rendition (never below 3.1). For H.264 the given string's
 * profile is kept and its level never lowered; other strings pass through.
 */
export function codecForRenditions(codec: string, specs: Size[]): string {
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

/** The WebCodecs codec string of a codec family at a level fitting specs. */
export function codecString(codec: VideoCodec, specs: Size[]): string {
  switch (codec) {
    case "h264":
      return codecForRenditions("avc1.42e01f", specs); // Constrained Baseline
    case "h265":
      return `hev1.1.6.L${fit(HEVC_LEVELS, specs)}.B0`; // Main; hev1 keeps parameter sets in-band
    case "vp8":
      return "vp8";
    case "vp9":
      return `vp09.00.${fit(VP9_LEVELS, specs)}.08`; // profile 0, 8-bit
    case "av1":
      return `av01.0.${fit(AV1_LEVELS, specs)}M.08`; // Main, 8-bit
  }
}

export function videoEncoderConfig(
  codec: string, width: number, height: number, bitrate: number, framerate: number, hardwareAcceleration?: HardwareAcceleration,
): VideoEncoderConfig {
  const c: VideoEncoderConfig = { codec, width, height, bitrate, framerate, latencyMode: "realtime" };
  if (hardwareAcceleration) c.hardwareAcceleration = hardwareAcceleration;
  // Annex B keeps parameter sets in-band on keyframes, so decoders need no description.
  const family = codecOf(codec);
  if (family === "h264") c.avc = { format: "annexb" };
  if (family === "h265") (c as VideoEncoderConfig & { hevc?: { format: string } }).hevc = { format: "annexb" };
  return c;
}

const isFirefox = () => typeof navigator !== "undefined" && /Firefox\//.test(navigator.userAgent);

async function supported(f: () => Promise<{ supported?: boolean }>): Promise<boolean> {
  try {
    return (await f()).supported === true;
  } catch {
    return false;
  }
}

const decodeCache = new Map<string, Promise<boolean>>();

/** Whether this browser can decode a codec string (cached). */
export function canDecode(codec: string): Promise<boolean> {
  let p = decodeCache.get(codec);
  if (!p) {
    p = typeof VideoDecoder === "undefined" ? Promise.resolve(false) : supported(() => VideoDecoder.isConfigSupported({ codec, optimizeForLatency: true }));
    decodeCache.set(codec, p);
  }
  return p;
}

/** @internal For tests. */
export function resetCodecCaches() {
  decodeCache.clear();
  probeCache.clear();
}

const probeCache = new Map<string, Promise<CodecSupport[]>>();

/**
 * Probes each codec for the given renditions: whether every size encodes
 * (in hardware, or at all) and whether it decodes. Results are cached.
 */
export function probeVideoCodecs(specs: { width: number; height: number; bitrate: number; framerate: number }[]): Promise<CodecSupport[]> {
  const key = specs.map((s) => `${s.width}x${s.height}@${s.framerate}`).join();
  let p = probeCache.get(key);
  if (!p) {
    p = Promise.all(VideoCodecs.map((c) => probeCodec(c, specs)));
    probeCache.set(key, p);
  }
  return p;
}

async function probeCodec(codec: VideoCodec, specs: { width: number; height: number; bitrate: number; framerate: number }[]): Promise<CodecSupport> {
  const str = codecString(codec, specs);
  const decode = await canDecode(str);
  if (typeof VideoEncoder === "undefined" || specs.length === 0) return { codec, encode: false, hardware: false, decode };
  const all = (hw?: HardwareAcceleration) =>
    Promise.all(specs.map((s) => supported(() => VideoEncoder.isConfigSupported(videoEncoderConfig(str, s.width, s.height, s.bitrate, s.framerate, hw)))))
      .then((r) => r.every(Boolean));
  const hardware = await all("prefer-hardware");
  const encode = hardware || (await all());
  return { codec, encode, hardware, decode };
}

/** How a publisher chooses its codec: automatically, or one asked for. */
export type VideoCodecPreference = VideoCodec | "auto";

/** Most alternatives offered besides the primary codec. */
const MAX_ALTERNATIVES = 3;

/**
 * The codecs to offer, primary first: a codec asked for, then hardware
 * encoders by efficiency, then software ones by cost. Firefox's H.264
 * encoder holds frames for over a second, so there it comes last. At most
 * three alternatives, one of them H.264 or VP8 (which every browser decodes).
 */
export function rankCodecs(
  support: CodecSupport[], opts: { preference?: VideoCodecPreference; allowed?: readonly string[]; broken?: ReadonlySet<VideoCodec>; firefox?: boolean } = {},
): VideoCodec[] {
  const firefox = opts.firefox ?? isFirefox();
  const allowed = opts.allowed?.length ? new Set(opts.allowed) : undefined;
  const usable = support.filter((s) => s.encode && !opts.broken?.has(s.codec) && (!allowed || allowed.has(s.codec)));
  const hwOrder: VideoCodec[] = ["av1", "h265", "vp9", "h264", "vp8"];
  const swOrder: VideoCodec[] = firefox ? ["vp8", "vp9", "av1", "h265"] : ["h264", "vp8", "vp9", "av1", "h265"];
  const score = (s: CodecSupport) => {
    if (firefox && s.codec === "h264") return 100;
    return s.hardware ? hwOrder.indexOf(s.codec) : 10 + swOrder.indexOf(s.codec);
  };
  const ranked = usable.sort((a, b) => score(a) - score(b)).map((s) => s.codec);
  const pref = opts.preference && opts.preference !== "auto" ? opts.preference : undefined;
  if (pref && ranked.includes(pref)) ranked.splice(0, 0, ...ranked.splice(ranked.indexOf(pref), 1));
  const out = ranked.slice(0, 1 + MAX_ALTERNATIVES);
  const baseline = ranked.find((c) => c === "h264" || c === "vp8");
  if (baseline && !out.some((c) => c === "h264" || c === "vp8")) out[out.length - 1] = baseline;
  return out;
}

/** The primary codec string for a 720p camera on this device. */
export async function pickVideoCodec(): Promise<string> {
  const spec = { width: 1280, height: 720, bitrate: 1_000_000, framerate: 30 };
  const [codec] = rankCodecs(await probeVideoCodecs([spec]));
  if (!codec) throw new Error("no supported video encoder");
  return codecString(codec, [spec]);
}
