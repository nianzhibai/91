import type { Transport } from "./http.js";
import type { Schema } from "./contract.js";

export type PlaybackSource = Schema<"PlaybackSourceDTO">;
export type PlaybackManifest = Schema<"PlaybackManifestDTO">;
export type PlaybackOptions = { source?: string; refresh?: boolean; signal?: AbortSignal };

export function createPlaybackClient(http: Transport) {
  async function load(url: string, options: PlaybackOptions = {}, prepare = false) {
    const parsed = new URL(http.backendPath(url), "http://localhost");
    if (options.source) parsed.searchParams.set("source", options.source);
    if (options.refresh) parsed.searchParams.set("refresh", "1");
    const response = await http.send(parsed.pathname + parsed.search, {
      method: prepare ? "POST" : "GET", signal: options.signal,
    });
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
