/** Formats seconds as m:ss, or h:mm:ss from an hour. */
export function formatClock(seconds: number): string {
  const s = Math.max(0, Math.round(seconds));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const ss = String(s % 60).padStart(2, "0");
  return h > 0 ? `${h}:${String(m).padStart(2, "0")}:${ss}` : `${m}:${ss}`;
}

/**
 * Formats a capture-to-arrival latency: "140 ms", "2.3 s". Clearly negative
 * means the sender's clock runs ahead of this one.
 */
export function formatLatency(ms: number): string {
  if (ms < -50) return "clock skew";
  if (ms < 1000) return `${Math.max(0, Math.round(ms))} ms`;
  return `${(ms / 1000).toFixed(1)} s`;
}
