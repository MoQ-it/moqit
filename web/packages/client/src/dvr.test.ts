import { create, toBinary } from "@bufbuild/protobuf";
import type { FetchObject, Params, Session } from "@moq-it/moqt";
import { describe, expect, test } from "vitest";
import { ArchivePlayer, chunkEnd, type Checkpoint, epochOf, ParticipantIndex, pickStart, Playhead, trackOfSource } from "./dvr.ts";
import { DvrCheckpointSchema, DvrEpochSchema, DvrIndexSchema, TrackSource } from "./gen/moqit/v1/models_pb.ts";

const E = 2 ** 32;
const cp = (track: string, group: number, at: number, loc = 0): Checkpoint => ({ track, group, at, loc });

describe("planning", () => {
  test("playhead runs at 1x from its start", () => {
    let now = 1000;
    const p = new Playhead(() => now);
    p.start(5e6);
    now += 250;
    expect(p.position()).toBe(5e6 + 250e3);
  });

  test("pickStart takes the last checkpoint at or before t", () => {
    const cps = [cp("mic", 1, 10), cp("mic", 2, 20), cp("mic", 3, 30)];
    expect(pickStart(cps, 25)?.group).toBe(2);
    expect(pickStart(cps, 5)?.group).toBe(1);
    expect(pickStart(cps, 99)?.group).toBe(3);
    expect(pickStart([], 1)).toBeUndefined();
  });

  test("chunkEnd stops before a later checkpoint of the same track and epoch", () => {
    const cps = [cp("camera-m", E + 1, 0), cp("camera-m", E + 4, 6e6), cp("camera-h", E + 9, 11e6), cp("camera-m", E + 7, 12e6), cp("camera-m", 2 * E + 1, 30e6)];
    expect(chunkEnd(cps, "camera-m", E + 1, 0, 10e6)).toBe(E + 6);
    expect(chunkEnd(cps, "camera-m", E + 7, 12e6, 10e6)).toBeUndefined(); // next is another epoch
    expect(epochOf(2 * E + 1)).toBe(2);
  });

  test("archived tracks map to sources", () => {
    expect(trackOfSource("camera-m", TrackSource.CAMERA)).toBe(true);
    expect(trackOfSource("screen-h", TrackSource.SCREEN)).toBe(true);
    expect(trackOfSource("screen-audio", TrackSource.SCREEN)).toBe(false);
    expect(trackOfSource("screen-audio", TrackSource.SCREEN_AUDIO)).toBe(true);
    expect(trackOfSource("mic", TrackSource.MICROPHONE)).toBe(true);
    expect(trackOfSource("mic", TrackSource.CAMERA)).toBe(false);
  });
});

// A fake session serving one participant's index and archive.
function fakeSession(minutes: Map<number, Uint8Array>, media: Map<string, FetchObject[]>, log: string[]): Session {
  return {
    async fetch(track: { name: string }, params: Params) {
      const f = params.locationFilter!;
      const start = f.startGroup ?? 0;
      const end = f.type === 4 ? start + (f.endGroupDelta ?? 0) : Number.MAX_SAFE_INTEGER;
      log.push(`${track.name} ${start}..${f.type === 4 ? end : ""}`);
      const objs: FetchObject[] =
        track.name === "dvr"
          ? [...minutes].filter(([m]) => m >= start && m <= end).map(([m, payload]) => obj(m, 0, payload))
          : (media.get(track.name) ?? []).filter((o) => o.group >= start && o.group <= end);
      return { cancel() {}, async *[Symbol.asyncIterator]() { yield* objs; } };
    },
  } as unknown as Session;
}

function obj(group: number, object: number, payload: Uint8Array, loc?: number): FetchObject {
  return { range: 0, group, object, datagram: false, subgroupId: 0, priority: 0, payload, properties: loc ? [{ type: 0x10, int: loc }] : undefined };
}

function indexMinute(checkpoints: Checkpoint[], earliest = checkpoints[0].at, codec = "vp8") {
  return toBinary(DvrIndexSchema, create(DvrIndexSchema, {
    earliest: BigInt(earliest),
    latest: BigInt(checkpoints[checkpoints.length - 1].at),
    epochs: [...new Set(checkpoints.map((c) => epochOf(c.group)))].map((epoch) => create(DvrEpochSchema, { epoch, track: checkpoints[0].track, codec })),
    checkpoints: checkpoints.map((c) => create(DvrCheckpointSchema, { track: c.track, group: BigInt(c.group), at: BigInt(c.at), loc: BigInt(c.loc) })),
  }));
}

describe("ParticipantIndex", () => {
  test("fetches missing minutes once and refreshes the growing ones", async () => {
    const t0 = 1000 * 60e6;
    const log: string[] = [];
    const minutes = new Map([[1000, indexMinute([cp("mic", E + 1, t0 + 1e6)])], [1002, indexMinute([cp("mic", E + 9, t0 + 121e6)], t0 + 1e6)]]);
    const ix = new ParticipantIndex(fakeSession(minutes, new Map(), log), ["moqit", "r", "p", "a"]);
    await ix.load(t0, t0 + 179e6);
    await ix.load(t0, t0 + 179e6);
    expect(log).toEqual(["dvr 1000..1002"]);
    expect(ix.checkpoints(TrackSource.MICROPHONE).map((c) => c.group)).toEqual([E + 1, E + 9]);
    expect(ix.earliest).toBe(t0 + 1e6);
    await ix.load(t0, t0 + 179e6, true);
    expect(log).toEqual(["dvr 1000..1002", "dvr 1002..1002"]);
  });
});

describe("ArchivePlayer", () => {
  test("plays from the checkpoint before the playhead, in order, across epochs", async () => {
    const now = Date.now() * 1000;
    const t0 = now - 60e6; // a minute ago, server time
    const enc = new TextEncoder();
    const media = new Map([["camera-m", [
      obj(E + 1, 0, enc.encode("k1"), t0 - 500e3), obj(E + 1, 1, enc.encode("d1"), t0 - 480e3),
      obj(E + 2, 0, enc.encode("k2"), t0 + 20e3),
      obj(2 * E, 1, enc.encode("partial"), t0 + 40e3), obj(2 * E + 1, 0, enc.encode("k3"), t0 + 60e3),
    ]]]);
    const cps = [cp("camera-m", E + 1, t0 - 500e3, t0 - 500e3), cp("camera-m", 2 * E + 1, t0 + 60e3, t0 + 60e3)];
    const minutes = new Map<number, Uint8Array>();
    for (const c of cps) minutes.set(Math.floor(c.at / 60e6), indexMinute(cps.filter((x) => Math.floor(x.at / 60e6) === Math.floor(c.at / 60e6))));
    const log: string[] = [];
    const session = fakeSession(minutes, media, log);
    const ix = new ParticipantIndex(session, ["moqit", "r", "p", "a"]);
    const playhead = new Playhead();
    playhead.start(t0);
    const got: string[] = [];
    let done!: () => void;
    const finished = new Promise<void>((r) => (done = r));
    const player = new ArchivePlayer(session, ["moqit", "r", "p", "a"], TrackSource.CAMERA, ix, playhead, {
      epoch: (e) => got.push(`epoch ${e?.epoch} ${e?.codec}`),
      object: (payload, ts, key) => {
        got.push(`${new TextDecoder().decode(payload)}${key ? "*" : ""}@${(ts - t0) / 1e3}`);
        if (got.length === 6) done();
      },
    }, false);
    const run = player.run();
    await finished;
    player.stop();
    await run;
    // The second epoch's index lists codec vp8 too; its partial first Group is skipped.
    expect(got).toEqual(["epoch 1 vp8", "k1*@-500", "d1@-480", "k2*@20", "epoch 2 vp8", "k3*@60"]);
  });
});
