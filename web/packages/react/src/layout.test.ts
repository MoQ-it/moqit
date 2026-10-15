import { describe, expect, it } from "vitest";
import { gridFit } from "./layout.ts";

// Whether n tiles fit inside width × height with the 10px gap.
function fits(width: number, height: number, n: number) {
  const { cols, tileWidth } = gridFit(width, height, n);
  const rows = Math.ceil(n / cols);
  return cols * tileWidth + (cols - 1) * 10 <= width && rows * (tileWidth * 9 / 16) + (rows - 1) * 10 <= height;
}

describe("gridFit", () => {
  it("puts two side by side in a wide stage and stacks them in a tall one", () => {
    expect(gridFit(1600, 700, 2).cols).toBe(2);
    expect(gridFit(500, 900, 2).cols).toBe(1);
  });

  it("uses one row for three in a very wide stage, two rows in a 16:9 one", () => {
    expect(gridFit(2400, 500, 3).cols).toBe(3);
    expect(gridFit(1256, 680, 3).cols).toBe(2);
    expect(gridFit(1256, 680, 4).cols).toBe(2);
  });

  it("keeps every tile inside the stage", () => {
    for (const [w, h] of [[1256, 680], [1576, 480], [776, 780], [360, 600], [1920, 1000]]) {
      for (let n = 1; n <= 12; n++) expect(fits(w, h, n), `${n} tiles in ${w}x${h}`).toBe(true);
    }
  });

  it("stops shrinking at the minimum width and fills the row instead", () => {
    const f = gridFit(360, 300, 9);
    expect(f.cols).toBe(2);
    expect(f.tileWidth).toBe(175);
  });

  it("handles an empty or unmeasured stage", () => {
    expect(gridFit(800, 600, 0).tileWidth).toBe(0);
    expect(gridFit(0, 0, 3).tileWidth).toBe(0);
  });
});
