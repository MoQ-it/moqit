import { expect, test } from "vitest";
import { CameraRenditions, codecForRenditions, Emitter, jwtClaims, participantNamespace, RateMeter, ScreenRenditions, Serial } from "./util.ts";

test("emitter on/once/off/next", async () => {
  class E extends Emitter<{ ping: [number] }> {
    fire(n: number) {
      this.emit("ping", n);
    }
  }
  const e = new E();
  const seen: number[] = [];
  const h = (n: number) => seen.push(n);
  e.on("ping", h);
  e.once("ping", (n) => seen.push(n * 10));
  e.fire(1);
  e.fire(2);
  e.off("ping", h);
  e.fire(3);
  expect(seen).toEqual([1, 10, 2]);
  const next = e.next("ping");
  e.fire(4);
  expect(await next).toEqual([4]);
});

test("jwt claims", () => {
  const payload = btoa(JSON.stringify({ sub: "alice", grants: { room: { room: "r1" } } })).replace(/=+$/, "").replace(/\+/g, "-").replace(/\//g, "_");
  const c = jwtClaims(`x.${payload}.y`);
  expect(c.sub).toBe("alice");
  expect(c.grants?.room?.room).toBe("r1");
});

test("naming matches the server", () => {
  expect(participantNamespace("r1", "alice")).toEqual(["moqit", "r1", "p", "alice"]);
});

test("serial runs tasks in order even after failures", async () => {
  const s = new Serial();
  const order: number[] = [];
  const a = s.run(async () => {
    await new Promise((r) => setTimeout(r, 20));
    order.push(1);
  });
  const b = s.run(async () => {
    order.push(2);
    throw new Error("x");
  });
  const c = s.run(async () => order.push(3));
  await a;
  await expect(b).rejects.toThrow();
  await c;
  expect(order).toEqual([1, 2, 3]);
});

test("H.264 level fits the largest rendition", () => {
  expect(codecForRenditions("avc1.42e01f", CameraRenditions)).toBe("avc1.42e01f"); // 720p30: level 3.1
  expect(codecForRenditions("avc1.42e01f", ScreenRenditions)).toBe("avc1.42e028"); // 1080p15: level 4.0
  expect(codecForRenditions("avc1.42e01f", [{ width: 2560, height: 1440, framerate: 30 }])).toBe("avc1.42e032");
  expect(codecForRenditions("avc1.42e033", CameraRenditions)).toBe("avc1.42e033"); // never lowered
  expect(codecForRenditions("vp8", ScreenRenditions)).toBe("vp8");
  expect(codecForRenditions("vp09.00.10.08", ScreenRenditions)).toBe("vp09.00.10.08");
});

test("rate meter counts the last window only", () => {
  let now = 0;
  const m = new RateMeter(2000, () => now);
  for (let i = 0; i < 20; i++) {
    m.add(1250); // 10 kbit every 100 ms: 100 kbit/s
    now += 100;
  }
  expect(m.bitrate).toBe(100_000);
  now += 1000;
  expect(m.bitrate).toBe(50_000); // half the window is now empty
  now += 1000;
  expect(m.bitrate).toBe(0);
});
