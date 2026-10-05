export function formatCount(n: number | undefined): string {
  if (n === undefined || n === null) return "0";
  if (n < 1000) return String(n);
  if (n < 10000) return `${(n / 1000).toFixed(1)}k`;
  if (n < 1_000_000) return `${(n / 10000).toFixed(1)}w`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}

/** Normalize the API's minute-based duration for video card time badges. */
export function formatVideoDuration(value: string): string {
  const match = /^(?:(\d+):)?(\d+):([0-5]\d)$/.exec(value);
  if (!match) return value;

  const totalSeconds = Number(match[1] ?? 0) * 3600 + Number(match[2]) * 60 + Number(match[3]);
  if (!Number.isSafeInteger(totalSeconds)) return value;

  return [
    Math.floor(totalSeconds / 3600),
    Math.floor(totalSeconds / 60) % 60,
    totalSeconds % 60,
  ].map((part) => String(part).padStart(2, "0")).join(":");
}
