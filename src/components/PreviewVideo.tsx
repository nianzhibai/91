import { forwardRef, useEffect, useImperativeHandle, useRef } from "react";
import type { PreviewState } from "@/types";

type Props = {
  src: string;
  state: PreviewState;
  onPlay: () => void;
  onEnded: () => void;
  onError: () => void;
};

// The card owns activation; the media element owns releasing its source on unmount.
export const PreviewVideo = forwardRef<HTMLVideoElement, Props>(
  function PreviewVideo(
    { src, state, onPlay, onEnded, onError },
    ref
  ) {
    const mediaRef = useRef<HTMLVideoElement>(null);
    useImperativeHandle(ref, () => mediaRef.current!, []);
    useEffect(() => {
      const media = mediaRef.current;
      // React StrictMode may run cleanup and setup again on the same element.
      if (media && media.getAttribute("src") !== src) media.src = src;
      return () => {
        if (!media) return;
        media.pause();
        media.removeAttribute("src");
        media.load();
      };
    }, [src]);
    return (
      <video
        ref={mediaRef}
        className={`preview-video ${state === "playing" ? "is-visible" : ""}`}
        src={src}
        muted
        autoPlay
        playsInline
        preload="metadata"
        onPlay={onPlay}
        onEnded={onEnded}
        onError={onError}
        aria-hidden="true"
      />
    );
  }
);
