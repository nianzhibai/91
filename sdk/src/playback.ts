import { responseError, type Transport } from "./http.js";

export type PlaybackSource = {
  id: string;
  label: string;
  kind: "original" | "transcoded";
  protocol: "file" | "hls";
  status: "unknown" | "ready" | "processing" | "unavailable";
  url?: string;
  resolveUrl?: string;
  prepareUrl?: string;
  mediaType?: string;
  message?: string;
  expiresAt?: string;
  resolution?: string;
  width?: number;
  height?: number;
  bitrate?: number;
  codec?: string;
  urlSource?: string;
  sourceInfoUrl?: string;
};

export type PlaybackManifest = { sources: PlaybackSource[] };
export type PlaybackOptions = { source?: string; refresh?: boolean; signal?: AbortSignal };

export function createPlaybackClient(http: Transport) {
  async function load(url: string, options: PlaybackOptions = {}, prepare = false) {
    const parsed = new URL(http.backendPath(url), "http://localhost");
    if (options.source) parsed.searchParams.set("source", options.source);
    if (options.refresh) parsed.searchParams.set("refresh", "1");
    const response = await http.fetch(parsed.pathname + parsed.search, {
      method: prepare ? "POST" : "GET", signal: options.signal, cache: "no-store",
      headers: { Accept: "application/json" },
    });
    if (!response.ok) throw await responseError(response);
    const manifest = await response.json() as PlaybackManifest;
    if (!manifest || !Array.isArray(manifest.sources) || !manifest.sources.length || manifest.sources.some(source =>
      !source || typeof source.id !== "string" || !source.id || typeof source.label !== "string" ||
      !["original", "transcoded"].includes(source.kind) || !["file", "hls"].includes(source.protocol) ||
      !["unknown", "ready", "processing", "unavailable"].includes(source.status) ||
      (source.status === "ready" && (typeof source.url !== "string" || !source.url)) ||
      [source.resolveUrl, source.prepareUrl, source.sourceInfoUrl, source.urlSource].some(value => value !== undefined && typeof value !== "string")
    ) || new Set(manifest.sources.map(source => source.id)).size !== manifest.sources.length) {
      throw new Error("播放源返回了无效数据");
    }
    return manifest;
  }

  return {
    get: (videoID: string, options?: PlaybackOptions) => load(`/api/video/${encodeURIComponent(videoID)}/playback`, options),
    resolve: (url: string, options?: PlaybackOptions) => load(url, options),
    /** Preparation may enqueue work. It is always explicit and never automatically retried. */
    prepare: (url: string, signal?: AbortSignal) => load(url, { signal }, true),
  };
}
