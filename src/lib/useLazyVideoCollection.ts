import { useCallback, useEffect, useState } from "react";
import { fetchVideoCollection } from "@/data/videos";
import type { VideoCollection } from "@/types";

const COLLECTION_CACHE_LIMIT = 8;

type CachedCollection = {
  data: VideoCollection;
  includesPreview: boolean;
};

const cachedCollectionsByVideoID = new Map<string, CachedCollection>();

function readCachedCollection(videoId: string, requirePreview: boolean) {
  const cached = cachedCollectionsByVideoID.get(videoId);
  if (!cached || (requirePreview && !cached.includesPreview)) return null;
  return cached.data;
}

function rememberCollection(
  videoId: string,
  collection: VideoCollection,
  includesPreview: boolean
) {
  const existing = cachedCollectionsByVideoID.get(videoId);
  if (existing?.includesPreview && !includesPreview) {
    return existing.data;
  }

  cachedCollectionsByVideoID.delete(videoId);
  cachedCollectionsByVideoID.set(videoId, {
    data: collection,
    includesPreview,
  });

  if (cachedCollectionsByVideoID.size > COLLECTION_CACHE_LIMIT) {
    const oldestVideoID = cachedCollectionsByVideoID.keys().next().value;
    if (oldestVideoID) cachedCollectionsByVideoID.delete(oldestVideoID);
  }
  return collection;
}

/**
 * Loads a video's complete directory collection only when its UI is opened.
 * Successful responses are shared by the mobile sheet and desktop rail so a
 * breakpoint change does not request the same large directory twice.
 */
export function useLazyVideoCollection(
  videoId: string,
  enabled: boolean,
  options: { includePreview?: boolean } = {}
) {
  const includePreview = options.includePreview === true;
  const [data, setData] = useState<VideoCollection | null>(() =>
    readCachedCollection(videoId, includePreview)
  );
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [reloadVersion, setReloadVersion] = useState(0);

  useEffect(() => {
    const dataHasRequiredFields =
      !!data &&
      (!includePreview ||
        data.items.every((item) => typeof item.previewSrc === "string"));
    if (!enabled || dataHasRequiredFields) return;

    const cached = readCachedCollection(videoId, includePreview);
    if (cached) {
      setData(cached);
      setError("");
      return;
    }

    const controller = new AbortController();
    let active = true;
    setLoading(true);
    setError("");
    fetchVideoCollection(videoId, {
      signal: controller.signal,
      includePreview,
    })
      .then((next) => {
        if (!active) return;
        setData(rememberCollection(videoId, next, includePreview));
      })
      .catch((reason: unknown) => {
        if (!active || isAbortError(reason)) return;
        setError("合集加载失败，请稍后重试");
      })
      .finally(() => {
        if (active) setLoading(false);
      });

    return () => {
      active = false;
      controller.abort();
    };
  }, [data, enabled, includePreview, reloadVersion, videoId]);

  const retry = useCallback(() => {
    cachedCollectionsByVideoID.delete(videoId);
    setData(null);
    setError("");
    setReloadVersion((version) => version + 1);
  }, [videoId]);

  return { data, loading, error, retry };
}

function isAbortError(error: unknown) {
  return error instanceof DOMException && error.name === "AbortError";
}
