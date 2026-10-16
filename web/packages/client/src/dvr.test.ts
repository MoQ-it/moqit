import { create, toBinary } from "@bufbuild/protobuf";
import type { FetchObject, Params, Session } from "@moq-it/moqt";
import { describe, expect, test } from "vitest";
import { ArchivePlayer, chunkEnd, type Checkpoint, epochOf, ParticipantIndex, pickStart, Playhead, Rewind, type RewindTrack, trackOfSource } from "./dvr.ts";
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

  test("a paused playhead holds its position, also when moved, and runs on from it", () => {
    let now = 1000;
    const p = new Playhead(() => now);
    p.start(5e6);
    now += 100;
    p.pause();
    now += 5000;
    expect(p.position()).toBe(5.1e6);
    p.start(2e6); // a seek while paused stays paused
    now += 1000;
    expect(p.position()).toBe(2e6);
    p.resume();
    now += 250;
    expect(p.position()).toBe(2.25e6);
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

describe("ArchivePlayer paused", () => {
  // One second of media around t0: a Group per 100 ms, a keyframe and a delta frame each.
  function setup(t0: number, track: string) {
    const enc = new TextEncoder();
    const objs: FetchObject[] = [];
    const cps: Checkpoint[] = [];
    for (let i = 0; i < 10; i++) {
      const at = t0 - 500e3 + i * 100e3;
      objs.push(obj(E + i, 0, enc.encode(`k${i}`), at), obj(E + i, 1, enc.encode(`d${i}`), at + 50e3));
      cps.push(cp(track, E + i, at, at));
    }
    const minutes = new Map<number, Uint8Array>();
    for (const c of cps) minutes.set(Math.floor(c.at / 60e6), indexMinute(cps.filter((x) => Math.floor(x.at / 60e6) === Math.floor(c.at / 60e6))));
    const session = fakeSession(minutes, new Map([[track, objs]]), []);
    return { session, ix: new ParticipantIndex(session, ["moqit", "r", "p", "a"]) };
  }
  const wait = (ms: number) => new Promise((r) => setTimeout(r, ms));

  test("video decodes up to the paused position, then nothing until resumed", async () => {
    const t0 = Date.now() * 1000 - 60e6;
    const { session, ix } = setup(t0, "camera-m");
    const playhead = new Playhead();
    playhead.start(t0);
    playhead.pause();
    const got: string[] = [];
    const player = new ArchivePlayer(session, ["moqit", "r", "p", "a"], TrackSource.CAMERA, ix, playhead, { epoch() {}, object: (p) => got.push(new TextDecoder().decode(p)) }, false);
    const run = player.run();
    await wait(300);
    expect(got).toEqual(["k5"]); // the frame at t0, from its keyframe
    playhead.resume();
    await wait(250);
    player.stop();
    await run;
    expect(got.slice(0, 4)).toEqual(["k5", "d5", "k6", "d6"]);
  });

  test("audio plays nothing while paused", async () => {
    const t0 = Date.now() * 1000 - 60e6;
    const { session, ix } = setup(t0, "mic");
    const playhead = new Playhead();
    playhead.start(t0);
    playhead.pause();
    const got: string[] = [];
    const player = new ArchivePlayer(session, ["moqit", "r", "p", "a"], TrackSource.MICROPHONE, ix, playhead, { epoch() {}, object: (p) => got.push(new TextDecoder().decode(p)) }, true);
    const run = player.run();
    await wait(300);
    expect(got).toEqual([]);
    playhead.resume();
    await wait(150);
    player.stop();
    await run;
    expect(got[0]).toBe("k5");
  });
});

describe("Rewind", () => {
  test("pausing live takes tracks off live without fetching; resume plays from the pause; goLive returns them", async () => {
    const log: string[] = [];
    const rw = new Rewind(fakeSession(new Map(), new Map(), log), (id) => ["moqit", "r", "p", id]);
    const calls: string[] = [];
    const track: RewindTrack = {
      kind: 1,
      enterArchive: () => calls.push("enter"),
      leaveArchive: async () => void calls.push("leave"),
      archiveEpoch() {},
      archiveObject() {},
    };
    rw.pauseLive(0.3, [{ identity: "a", source: TrackSource.CAMERA, track }]);
    const at = rw.playhead.position();
    await new Promise((r) => setTimeout(r, 50));
    expect(calls).toEqual(["enter"]);
    expect(log).toEqual([]);
    expect(rw.timeline(true)).toMatchObject({ live: false, paused: true });
    expect(rw.playhead.position()).toBe(at);
    rw.resume();
    await new Promise((r) => setTimeout(r, 50));
    expect(log.some((l) => l.startsWith("dvr "))).toBe(true); // the player reads the index
    expect(rw.timeline(true).paused).toBe(false);
    await rw.goLive();
    expect(calls).toEqual(["enter", "leave"]);
    expect(rw.timeline(true).live).toBe(true);
  });
});
