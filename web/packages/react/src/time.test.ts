import { expect, test } from "vitest";
import { formatClock } from "./time.ts";

test("formatClock", () => {
  expect(formatClock(0)).toBe("0:00");
  expect(formatClock(9.6)).toBe("0:10");
  expect(formatClock(75)).toBe("1:15");
  expect(formatClock(3725)).toBe("1:02:05");
  expect(formatClock(-3)).toBe("0:00");
});
