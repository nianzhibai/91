import { useEffect, useLayoutEffect, useRef, useState } from "react";

type Props = {
  src: string;
  eager?: boolean;
  highPriority?: boolean;
  enabled?: boolean;
};

type ThumbnailState = "loading" | "retrying" | "ready" | "failed";

const MAX_LOCAL_THUMBNAIL_RETRIES = 8;

export function VideoThumbnail({
  src,
  eager = false,
  highPriority = false,
  enabled = true,
}: Props) {
  if (!enabled) {
    return (
      <span
        className="thumb-placeholder"
        data-state="deferred"
        aria-hidden="true"
      />
    );
  }

  // One component instance owns exactly one source lifecycle. This makes a src
  // change synchronous instead of resetting state later in an effect, which can
  // otherwise overwrite an already-fired load event from the browser cache.
  return (
    <ThumbnailResource
      key={src}
      src={src}
      eager={eager}
      highPriority={highPriority}
    />
  );
}

function ThumbnailResource({
  src,
  eager = false,
  highPriority = false,
}: Props) {
  const [state, setState] = useState<ThumbnailState>(src ? "loading" : "failed");
  const [retry, setRetry] = useState(0);
  const retryTimerRef = useRef<number | null>(null);
  const imageRef = useRef<HTMLImageElement | null>(null);

  useEffect(() => {
    return clearRetryTimer;
  }, []);

  function clearRetryTimer() {
    if (retryTimerRef.current !== null) {
      window.clearTimeout(retryTimerRef.current);
      retryTimerRef.current = null;
    }
  }

  function handleLoad() {
    clearRetryTimer();
    setState("ready");
  }

  function handleError() {
    const canRetry =
      src.startsWith("/p/thumb/") && retry < MAX_LOCAL_THUMBNAIL_RETRIES;
    if (!canRetry) {
      clearRetryTimer();
      setState("failed");
      return;
    }
    if (retryTimerRef.current !== null) return;

    setState("retrying");
    retryTimerRef.current = window.setTimeout(() => {
      retryTimerRef.current = null;
      setRetry((current) => current + 1);
    }, Math.min(1000 + retry * 750, 5000));
  }

  const thumbnailSrc = retry === 0 ? src : withRetryParam(src, retry);

  // A cached image can finish between DOM insertion and React's passive effects,
  // and some browsers do not replay that load event. Reconcile the DOM state
  // before paint so a completed image can never remain hidden at opacity: 0.
  useLayoutEffect(() => {
    const image = imageRef.current;
    if (!image?.complete) return;
    if (image.naturalWidth > 0) {
      handleLoad();
    } else {
      handleError();
    }
  }, [thumbnailSrc]);

  return (
    <>
      <span
        className="thumb-placeholder"
        data-state={state}
        aria-hidden="true"
      />
      {src && (
        <img
          ref={imageRef}
          key={thumbnailSrc}
          className={`thumb-image ${state === "ready" ? "is-ready" : ""}`}
          src={thumbnailSrc}
          alt=""
          loading={eager || highPriority ? "eager" : "lazy"}
          fetchPriority={highPriority ? "high" : "auto"}
          decoding="async"
          onLoad={handleLoad}
          onError={handleError}
        />
      )}
    </>
  );
}

function withRetryParam(src: string, retry: number): string {
  const sep = src.includes("?") ? "&" : "?";
  return `${src}${sep}r=${retry}`;
}
