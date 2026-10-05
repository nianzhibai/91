import { useCallback, useEffect, useRef, useState } from "react";
import { useLocation } from "react-router";
import type { PreviewState } from "@/types";
import { previewController } from "./previewController";
import { usePreviewEnabled } from "./usePreviewEnabled";

type Options = {
  id: string;
  src?: string;
  inView: boolean;
  active?: boolean;
};

/** Each card owns its media; only the enabled policy is shared between cards. */
export function useCardPreview({ id, src, inView, active = true }: Options) {
  const [previewState, setPreviewState] = useState<PreviewState>("idle");
  const [shouldRenderPreview, setShouldRenderPreview] = useState(false);
  const [showPreviewLoader, setShowPreviewLoader] = useState(false);
  const videoRef = useRef<HTMLVideoElement | null>(null);
  const requestedRef = useRef(false);
  const previewEnabled = usePreviewEnabled();
  const { pathname } = useLocation();

  const stopPreview = useCallback(() => {
    requestedRef.current = false;
    const media = videoRef.current;
    if (media) {
      media.pause();
      media.removeAttribute("src");
      media.load();
    }
    setShouldRenderPreview(false);
    setShowPreviewLoader(false);
    setPreviewState("idle");
  }, []);

  const startPreview = useCallback(() => {
    if (!previewController.isEnabled() || !src || !active) return;
    if (requestedRef.current) return;
    requestedRef.current = true;
    setShouldRenderPreview(true);
    setShowPreviewLoader(true);
    setPreviewState("loading");
  }, [active, src]);

  useEffect(() => {
    if (!previewEnabled || !active || !inView) stopPreview();
  }, [active, inView, previewEnabled, stopPreview]);

  // Navigation can retain the card tree, and tab switches can merely hide it.
  // Release media on either transition as well as source changes and unmount.
  useEffect(() => stopPreview, [id, pathname, src, stopPreview]);

  const handlePreviewPlay = useCallback(() => setPreviewState("playing"), []);
  const finishPreviewLoader = useCallback(() => setShowPreviewLoader(false), []);

  return {
    previewEnabled,
    previewState,
    shouldRenderPreview,
    showPreviewLoader,
    videoRef,
    startPreview,
    stopPreview,
    handlePreviewPlay,
    finishPreviewLoader,
  };
}
