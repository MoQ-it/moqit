import { describe, expect, test } from "vitest";
import { allocate, FULL_FLOOR, HALF_FLOOR, type Layer, type LocalSignals, SendController, SendQueue, Suspensions } from "./sendcontrol.ts";

const cam = (rendition: string, rank: number, spec: number): Layer => ({ rendition, rank, screen: false, spec });
const camH = cam("camera-h", 2, 1_700_000);
const camM = cam("camera-m", 1, 800_000);
const camL = cam("camera-l", 0, 150_000);

describe("SendQueue", () => {
  test("sends in order and drops a stale queue, resuming at a keyframe", async () => {
    let now = 0;
    const sent: string[] = [];
    let release!: () => void;
    let drops = 0;
    const q = new SendQueue<{ key: boolean; bytes: number; name: string }>(
      async (c) => {
        sent.push(c.name);
        if (c.name === "k1") await new Promise<void>((r) => (release = r)); // the uplink stalls
      },
      () => {
        drops++;
        release(); // abandoning the Group aborts the stalled write
      },
      () => now,
      500,
    );
    q.push({ key: true, bytes: 10, name: "k1" });
    for (let i = 0; i < 5; i++) {
      now += 100;
      q.push({ key: false, bytes: 10, name: `d${i}` });
    }
    expect(q.delay()).toBe(500);
    expect(q.queuedBytes).toBe(50);
    now += 100;
    q.push({ key: false, bytes: 10, name: "d5" }); // 600ms old: dropped, with everything waiting
    expect(drops).toBe(1);
    q.push({ key: false, bytes: 10, name: "d6" }); // still waiting for a keyframe
    q.push({ key: true, bytes: 10, name: "k2" });
    q.push({ key: false, bytes: 10, name: "d7" });
    await q.idle();
    expect(sent).toEqual(["k1", "k2", "d7"]);
    expect(q.droppedFrames).toBe(7);
    expect(q.delay()).toBe(0);
  });
});

describe("allocate", () => {
  test("everything at spec when the target allows", () => {
    const a = allocate(2_650_000, [camH, camM, camL]);
    for (const l of [camH, camM, camL]) expect(a.get(l.rendition)).toEqual({ bitrate: l.spec, halfRate: false, suspend: false });
  });

  test("smallest layers first, then half frame rate, then suspension", () => {
    // camera-l 150k at spec, camera-m 400k: camera-h can't have its floor.
    let a = allocate(550_000, [camH, camM, camL]);
    expect(a.get("camera-l")).toEqual({ bitrate: 150_000, halfRate: false, suspend: false });
    expect(a.get("camera-m")!.bitrate).toBe(400_000);
    expect(a.get("camera-h")!.suspend).toBe(true);
    // Between the floors, a layer runs at half frame rate.
    a = allocate(HALF_FLOOR * 800_000 + FULL_FLOOR * 150_000 + 10_000, [camM, camL]);
    expect(a.get("camera-m")!.halfRate).toBe(true);
    expect(a.get("camera-m")!.suspend).toBe(false);
    // The smallest layer of a track is never suspended.
    a = allocate(10_000, [camL]);
    expect(a.get("camera-l")).toMatchObject({ halfRate: true, suspend: false });
    expect(a.get("camera-l")!.bitrate).toBe(0.5 * HALF_FLOOR * 150_000);
  });

  test("screens come before cameras", () => {
    const screenL: Layer = { rendition: "screen-l", rank: 0, screen: true, spec: 600_000 };
    const a = allocate(300_000, [camL, screenL]);
    expect(a.get("screen-l")!.bitrate).toBeGreaterThanOrEqual(FULL_FLOOR * 600_000);
  });
});

const signals = (s: Partial<LocalSignals> = {}): LocalSignals => ({ queueDelayMs: 0, drops: 0, sendRate: 0, audioRate: 32_000, maxTarget: 2_650_000, ...s });

describe("SendController", () => {
  test("decreases to 85% of what got through, holds, then increases while calm", () => {
    let now = 0;
    const c = new SendController(() => now);
    c.tick(signals());
    expect(c.target).toBe(2_650_000);
    c.feedback({ queuingDelayMs: 300, receiveBitrate: 532_000 });
    now += 250;
    c.tick(signals());
    expect(c.target).toBe(2_650_000); // one tick of overuse is not enough
    now += 250;
    c.feedback({ queuingDelayMs: 300, receiveBitrate: 532_000 });
    c.tick(signals());
    expect(c.target).toBeCloseTo(0.85 * 500_000);
    // Held for 2s even though the delay is gone.
    c.feedback({ queuingDelayMs: 0, receiveBitrate: 400_000 });
    now += 1000;
    c.tick(signals());
    expect(c.target).toBeCloseTo(425_000);
    // Then it grows by at least 50 kbps a second while calm.
    now += 1250;
    c.feedback({ queuingDelayMs: 5, receiveBitrate: 400_000 });
    c.tick(signals());
    now += 1000;
    c.feedback({ queuingDelayMs: 5, receiveBitrate: 400_000 });
    c.tick(signals());
    expect(c.target).toBeGreaterThanOrEqual(425_000 + 50_000);
    // 30s after the last decrease, nothing is held back.
    now += 30_000;
    c.feedback({ queuingDelayMs: 5, receiveBitrate: 400_000 });
    c.tick(signals());
    expect(c.target).toBe(2_650_000);
  });

  test("without feedback, the local send queue and drops count", () => {
    let now = 0;
    const c = new SendController(() => now);
    c.tick(signals());
    now += 250;
    c.tick(signals({ queueDelayMs: 200, sendRate: 1_000_000 }));
    now += 250;
    c.tick(signals({ queueDelayMs: 200, sendRate: 1_000_000 }));
    expect(c.target).toBe(850_000);
    expect(c.queuingDelayMs).toBeUndefined();
    // Stale feedback is ignored.
    c.feedback({ queuingDelayMs: 500, receiveBitrate: 100_000 });
    now += 5000;
    expect(c.queuingDelayMs).toBeUndefined();
  });
});

describe("Suspensions", () => {
  test("suspends after 2s, resumes after calm with room, and backs off when it flaps", () => {
    let now = 0;
    const s = new Suspensions(() => now);
    const want = new Map([["camera-h", { bitrate: 0, halfRate: false, suspend: true }], ["camera-m", { bitrate: 400_000, halfRate: false, suspend: false }]]);
    expect(s.update(want, [camH, camM], 400_000, 0, [])).toBe(false);
    now += 2000;
    expect(s.update(want, [camH, camM], 400_000, 0, [])).toBe(true);
    expect([...s.suspended]).toEqual(["camera-h"]);
    const running = new Map([["camera-m", { bitrate: 800_000, halfRate: false, suspend: false }]]);
    // Not before 10s of calm, nor without room for both floors and a quarter.
    now += 5000;
    expect(s.update(running, [camM], 2_500_000, 0, [camH])).toBe(false);
    now += 6000;
    expect(s.update(running, [camM], 1_000_000, 0, [camH])).toBe(false);
    const enough = 1.25 * FULL_FLOOR * (800_000 + 1_700_000);
    expect(s.update(running, [camM], enough, 0, [camH])).toBe(true);
    expect(s.suspended.size).toBe(0);
    // Suspended again soon after resuming: it waits twice as long.
    now += 1000;
    s.update(want, [camH, camM], 400_000, now, []);
    now += 2000;
    expect(s.update(want, [camH, camM], 400_000, now, [])).toBe(true);
    now += 15_000;
    expect(s.update(running, [camM], 2_650_000, 0, [camH])).toBe(false);
    now += 6000;
    expect(s.update(running, [camM], 2_650_000, 0, [camH])).toBe(true);
  });
});
