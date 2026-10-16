import { expect, test } from "vitest";
import { formatClock, formatLatency } from "./time.ts";

test("formatClock", () => {
  expect(formatClock(0)).toBe("0:00");
  expect(formatClock(9.6)).toBe("0:10");
  expect(formatClock(75)).toBe("1:15");
  expect(formatClock(3725)).toBe("1:02:05");
  expect(formatClock(-3)).toBe("0:00");
});

test("formatLatency", () => {
  expect(formatLatency(142.4)).toBe("142 ms");
  expect(formatLatency(-20)).toBe("0 ms");
  expect(formatLatency(-400)).toBe("clock skew");
  expect(formatLatency(999)).toBe("999 ms");
  expect(formatLatency(2345)).toBe("2.3 s");
});
