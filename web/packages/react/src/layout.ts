export interface GridFit {
  cols: number;
  /** Tile width in pixels; tiles are `aspect` wide for each unit of height. */
  tileWidth: number;
}

/**
 * Lays out n tiles of the given aspect ratio in width × height: the column
 * count that makes the tiles largest while all of them fit. Below minWidth
 * the tiles stop shrinking, fill the width, and the grid scrolls.
 */
export function gridFit(width: number, height: number, n: number, { gap = 10, aspect = 16 / 9, minWidth = 160 } = {}): GridFit {
  if (n <= 0 || width <= 0 || height <= 0) return { cols: 1, tileWidth: 0 };
  let best: GridFit = { cols: 1, tileWidth: 0 };
  for (let cols = 1; cols <= n; cols++) {
    const rows = Math.ceil(n / cols);
    const w = Math.min((width - gap * (cols - 1)) / cols, ((height - gap * (rows - 1)) / rows) * aspect);
    if (w > best.tileWidth) best = { cols, tileWidth: w };
  }
  if (best.tileWidth < minWidth) {
    const cols = Math.max(1, Math.min(n, Math.floor((width + gap) / (minWidth + gap))));
    best = { cols, tileWidth: (width - gap * (cols - 1)) / cols };
  }
  return { cols: best.cols, tileWidth: Math.floor(best.tileWidth) };
}
