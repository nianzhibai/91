import {
  forwardRef,
  useCallback,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
} from "react";
import type { PointerEvent as ReactPointerEvent } from "react";
import { createPortal } from "react-dom";
import { Link, useLocation, useNavigate } from "react-router";
import { ArrowUpDown, ChevronRight, Eye, X } from "lucide-react";
import type {
  VideoCollectionItem,
  VideoCollectionSummary,
} from "@/types";
import { formatCount } from "@/lib/format";
import { useDocumentScrollLock } from "@/lib/useDocumentScrollLock";
import { supportsNativeBack } from "@/lib/nativeBack";
import { useNativeBackHandler } from "@/lib/useNativeBack";
import { useInViewport } from "@/lib/useInViewport";
import { useCardPreview } from "@/lib/useCardPreview";
import { useLazyVideoCollection } from "@/lib/useLazyVideoCollection";
import {
  resolveVideoReturnPath,
  routeToPath,
} from "@/lib/videoReturnPath";
import {
  continueVideoDetailNavigationState,
  type VideoDetailNavigationState,
} from "@/lib/videoListingBackground";
import { PreviewVideo } from "./PreviewVideo";
import { PreviewLoader } from "./PreviewLoader";
import { VideoThumbnail } from "./VideoThumbnail";

type Props = {
  videoId: string;
  collection: VideoCollectionSummary;
};

type SheetDragState = {
  active: boolean;
  pointerId: number;
  startY: number;
  lastY: number;
  surface: HTMLDivElement | null;
};

type ListPullState = {
  tracking: boolean;
  activated: boolean;
  touchId: number;
  startX: number;
  startY: number;
};

const LIST_PULL_ACTIVATION_DISTANCE = 8;
const SHEET_DISMISS_HEIGHT_RATIO = 0.25;
const SHEET_DISMISS_MIN_DISTANCE = 96;
const SHEET_DISMISS_MAX_DISTANCE = 160;
const SHEET_DISMISS_ANIMATION_MS = 180;
const COLLECTION_SHEET_HISTORY_STATE_KEY = "mobileVideoCollection";

function asRouterState(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

function collectionSheetVideoId(state: Record<string, unknown>): string | null {
  const sheetState = state[COLLECTION_SHEET_HISTORY_STATE_KEY];
  if (
    sheetState === null ||
    typeof sheetState !== "object" ||
    Array.isArray(sheetState)
  ) {
    return null;
  }
  const videoId = (sheetState as Record<string, unknown>).videoId;
  return typeof videoId === "string" ? videoId : null;
}

/**
 * Mobile-only directory collection entry and bottom sheet.
 *
 * The detail payload contains only the summary. Items are fetched when the
 * sheet first opens, keeping ordinary playback navigation lightweight even for
 * directories with many videos.
 */
export function MobileVideoCollection({ videoId, collection }: Props) {
  const navigate = useNavigate();
  const location = useLocation();
  const locationState = asRouterState(location.state);
  const historyOpen = collectionSheetVideoId(locationState) === videoId;
  const [nativeOpenKey, setNativeOpenKey] = useState<string | null>(null);
  const open = historyOpen || nativeOpenKey === location.key;
  const { data, loading, error, retry } = useLazyVideoCollection(
    videoId,
    open,
    { includePreview: true }
  );
  const [ascending, setAscending] = useState(true);
  const titleId = useId();
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  const closeRef = useRef<HTMLButtonElement | null>(null);
  const sheetRef = useRef<HTMLElement | null>(null);
  const listRef = useRef<HTMLUListElement | null>(null);
  const currentItemRef = useRef<HTMLLIElement | null>(null);
  const dismissTimerRef = useRef<number | null>(null);
  const closePendingRef = useRef(false);
  const dragRef = useRef<SheetDragState>({
    active: false,
    pointerId: -1,
    startY: 0,
    lastY: 0,
    surface: null,
  });
  const listPullRef = useRef<ListPullState>({
    tracking: false,
    activated: false,
    touchId: -1,
    startX: 0,
    startY: 0,
  });
  const returnPath =
    typeof locationState.from === "string"
      ? resolveVideoReturnPath(locationState.from)
      : resolveVideoReturnPath(routeToPath(location));
  const detailNavigationState = useMemo(
    () => continueVideoDetailNavigationState(returnPath, location.state),
    [location.state, returnPath]
  );

  useDocumentScrollLock(open);
  useNativeBackHandler(open, () => closeSheet(true, false));

  useEffect(() => {
    return () => {
      if (dismissTimerRef.current !== null) {
        window.clearTimeout(dismissTimerRef.current);
      }
    };
  }, []);

  // Navigation can close the sheet without calling closeSheet. Cancel any
  // pending dismissal so its old timer cannot navigate back a second time.
  useEffect(() => {
    closePendingRef.current = false;
    if (open) return;
    setNativeOpenKey(null);
    if (dismissTimerRef.current !== null) {
      window.clearTimeout(dismissTimerRef.current);
      dismissTimerRef.current = null;
    }
    releaseDragCapture();
  }, [open]);

  const items = useMemo(() => {
    const loaded = data?.items ?? [];
    return ascending ? loaded : [...loaded].reverse();
  }, [ascending, data]);

  useEffect(() => {
    if (!open) return;

    const focusTimer = window.setTimeout(() => closeRef.current?.focus(), 0);
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      event.preventDefault();
      closeSheet();
    };
    document.addEventListener("keydown", handleKeyDown);
    return () => {
      window.clearTimeout(focusTimer);
      document.removeEventListener("keydown", handleKeyDown);
    };
  }, [historyOpen, open]);

  // CSS hides the feature above the mobile breakpoint. Also close an already
  // open sheet after rotation/resizing so its document scroll lock is released.
  useEffect(() => {
    const media = window.matchMedia("(max-width: 768px)");
    const handleChange = () => {
      if (!media.matches && open) closeSheet(false, false);
    };
    media.addEventListener("change", handleChange);
    return () => media.removeEventListener("change", handleChange);
  }, [open]);

  useEffect(() => {
    if (!open || items.length === 0) return;
    const frame = window.requestAnimationFrame(() => {
      const list = listRef.current;
      const current = currentItemRef.current;
      if (!list || !current) return;
      list.scrollTop = Math.max(
        0,
        current.offsetTop - list.clientHeight / 2 + current.clientHeight / 2
      );
    });
    return () => window.cancelAnimationFrame(frame);
  }, [ascending, items, open]);

  // A scrollable list normally owns every vertical touch. At its upper
  // boundary, intercept only a new downward gesture and hand it to the sheet;
  // upward and horizontal gestures remain native list scrolling.
  useEffect(() => {
    const list = listRef.current;
    if (!open || !list) return;

    const resetPull = () => {
      listPullRef.current = {
        tracking: false,
        activated: false,
        touchId: -1,
        startX: 0,
        startY: 0,
      };
    };
    const findTouch = (touches: TouchList, identifier: number) => {
      for (let index = 0; index < touches.length; index += 1) {
        const touch = touches.item(index);
        if (touch?.identifier === identifier) return touch;
      }
      return null;
    };
    const handleTouchStart = (event: TouchEvent) => {
      if (
        event.touches.length !== 1 ||
        list.scrollTop > 1 ||
        dragRef.current.active
      ) {
        resetPull();
        return;
      }
      const touch = event.touches.item(0);
      if (!touch) return;
      listPullRef.current = {
        tracking: true,
        activated: false,
        touchId: touch.identifier,
        startX: touch.clientX,
        startY: touch.clientY,
      };
    };
    const handleTouchMove = (event: TouchEvent) => {
      const pull = listPullRef.current;
      if (!pull.tracking) return;
      if (event.touches.length !== 1) {
        if (pull.activated) {
          finishSheetDragAt(dragRef.current.lastY, true);
        }
        resetPull();
        return;
      }
      const touch = findTouch(event.touches, pull.touchId);
      if (!touch) {
        if (pull.activated) {
          finishSheetDragAt(dragRef.current.lastY, true);
        }
        resetPull();
        return;
      }

      if (!pull.activated) {
        const deltaX = touch.clientX - pull.startX;
        const deltaY = touch.clientY - pull.startY;
        if (
          Math.max(Math.abs(deltaX), Math.abs(deltaY)) <
          LIST_PULL_ACTIVATION_DISTANCE
        ) {
          return;
        }
        if (deltaY <= 0 || Math.abs(deltaX) > Math.abs(deltaY)) {
          resetPull();
          return;
        }
        if (!event.cancelable) {
          resetPull();
          return;
        }
        event.preventDefault();
        beginSheetDragAt(pull.touchId, pull.startY, null);
        pull.activated = dragRef.current.active;
        if (!pull.activated) {
          resetPull();
          return;
        }
      } else if (event.cancelable) {
        event.preventDefault();
      }

      moveSheetDragAt(touch.clientY);
    };
    const finishTouch = (event: TouchEvent, cancelled: boolean) => {
      const pull = listPullRef.current;
      if (!pull.tracking) return;
      const touch = findTouch(event.changedTouches, pull.touchId);
      if (pull.activated) {
        if (event.cancelable) event.preventDefault();
        finishSheetDragAt(
          touch?.clientY ?? dragRef.current.lastY,
          cancelled
        );
      }
      resetPull();
    };
    const handleTouchEnd = (event: TouchEvent) => finishTouch(event, false);
    const handleTouchCancel = (event: TouchEvent) => finishTouch(event, true);

    list.addEventListener("touchstart", handleTouchStart, { passive: true });
    list.addEventListener("touchmove", handleTouchMove, { passive: false });
    list.addEventListener("touchend", handleTouchEnd, { passive: false });
    list.addEventListener("touchcancel", handleTouchCancel, { passive: true });
    return () => {
      list.removeEventListener("touchstart", handleTouchStart);
      list.removeEventListener("touchmove", handleTouchMove);
      list.removeEventListener("touchend", handleTouchEnd);
      list.removeEventListener("touchcancel", handleTouchCancel);
      if (listPullRef.current.activated && dragRef.current.active) {
        finishSheetDragAt(dragRef.current.lastY, true);
      }
      resetPull();
    };
  }, [items.length, open]);

  function openSheet() {
    if (open) return;
    if (supportsNativeBack()) {
      setNativeOpenKey(location.key);
      return;
    }
    navigate(routeToPath(location), {
      state: {
        ...locationState,
        [COLLECTION_SHEET_HISTORY_STATE_KEY]: { videoId },
      },
    });
  }

  function releaseDragCapture() {
    const drag = dragRef.current;
    const surface = drag.surface;
    const pointerId = drag.pointerId;
    // Mark inactive before releasing capture because lostpointercapture may be
    // dispatched synchronously and must not finish the same gesture twice.
    drag.active = false;
    drag.pointerId = -1;
    drag.surface = null;
    if (
      surface &&
      pointerId >= 0 &&
      surface.hasPointerCapture(pointerId)
    ) {
      surface.releasePointerCapture(pointerId);
    }
  }

  function closeSheet(restoreFocus = true, animate = true) {
    if (!open || closePendingRef.current) return;
    closePendingRef.current = true;
    if (dismissTimerRef.current !== null) {
      window.clearTimeout(dismissTimerRef.current);
      dismissTimerRef.current = null;
    }
    releaseDragCapture();
    const finishClose = () => {
      dismissTimerRef.current = null;
      if (historyOpen) navigate(-1);
      else setNativeOpenKey(null);
      if (restoreFocus) {
        window.setTimeout(() => triggerRef.current?.focus(), 0);
      }
    };
    const sheet = sheetRef.current;
    if (!animate || !sheet) {
      finishClose();
      return;
    }

    sheet.classList.remove("is-entering", "is-dragging");
    // Commit the current position before animating, including a released drag.
    void sheet.offsetHeight;
    sheet.classList.add("is-dismissing");
    sheet.style.setProperty(
      "--vd-collection-sheet-drag-y",
      `${sheet.offsetHeight + 24}px`
    );
    dismissTimerRef.current = window.setTimeout(
      finishClose,
      SHEET_DISMISS_ANIMATION_MS
    );
  }

  function beginSheetDragAt(
    pointerId: number,
    startY: number,
    surface: HTMLDivElement | null
  ) {
    const sheet = sheetRef.current;
    if (!sheet || dragRef.current.active || closePendingRef.current) return;
    dragRef.current = {
      active: true,
      pointerId,
      startY,
      lastY: startY,
      surface,
    };
    // The opening animation is one-shot. Removing it before a gesture keeps a
    // snap-back from replaying the sheet's bottom-to-top entrance.
    sheet.classList.remove("is-entering", "is-dismissing");
    sheet.classList.add("is-dragging");
    sheet.style.setProperty("--vd-collection-sheet-drag-y", "0px");
    surface?.setPointerCapture(pointerId);
  }

  function moveSheetDragAt(clientY: number) {
    const drag = dragRef.current;
    if (!drag.active) return null;

    const offset = Math.max(0, clientY - drag.startY);
    drag.lastY = clientY;
    const sheet = sheetRef.current;
    if (!sheet) return null;
    const clampedOffset = Math.min(offset, sheet.offsetHeight);
    sheet.style.setProperty(
      "--vd-collection-sheet-drag-y",
      `${clampedOffset}px`
    );
    return offset;
  }

  function finishSheetDragAt(clientY: number, cancelled = false) {
    const drag = dragRef.current;
    if (!drag.active) return;

    const sheet = sheetRef.current;
    const offset = Math.max(0, clientY - drag.startY);
    releaseDragCapture();
    if (!sheet) return;

    const sheetHeight = sheet.offsetHeight;
    const distanceThreshold = Math.min(
      SHEET_DISMISS_MAX_DISTANCE,
      Math.max(
        SHEET_DISMISS_MIN_DISTANCE,
        sheetHeight * SHEET_DISMISS_HEIGHT_RATIO
      )
    );
    const shouldDismiss = !cancelled && offset >= distanceThreshold;

    sheet.classList.remove("is-dragging");
    // Commit the last drag frame before re-enabling the CSS transition so both
    // the snap-back and dismissal animate from the finger's release position.
    void sheet.offsetHeight;
    if (!shouldDismiss) {
      sheet.style.setProperty("--vd-collection-sheet-drag-y", "0px");
      return;
    }

    closeSheet();
  }

  function beginSheetDrag(event: ReactPointerEvent<HTMLDivElement>) {
    if (
      !event.isPrimary ||
      (event.pointerType === "mouse" && event.button !== 0)
    ) {
      return;
    }
    const target = event.target as HTMLElement;
    if (target.closest("button, a, input, select, textarea")) return;
    beginSheetDragAt(
      event.pointerId,
      event.clientY,
      event.currentTarget
    );
  }

  function moveSheetDrag(event: ReactPointerEvent<HTMLDivElement>) {
    const drag = dragRef.current;
    if (!drag.active || drag.pointerId !== event.pointerId) return;
    const offset = moveSheetDragAt(event.clientY);
    if (offset !== null && offset > 0) event.preventDefault();
  }

  function finishSheetDrag(
    event: ReactPointerEvent<HTMLDivElement>,
    cancelled = false
  ) {
    const drag = dragRef.current;
    if (!drag.active || drag.pointerId !== event.pointerId) return;
    finishSheetDragAt(event.clientY, cancelled);
  }

  const shownSummary = data ?? collection;
  const sheet = open
    ? createPortal(
        <div
          className="vd-collection-sheet-modal"
          role="presentation"
          onClick={() => closeSheet()}
        >
          <section
            ref={sheetRef}
            className="vd-collection-sheet is-entering"
            role="dialog"
            aria-modal="true"
            aria-labelledby={titleId}
            onAnimationEnd={(event) => {
              if (event.target === event.currentTarget) {
                event.currentTarget.classList.remove("is-entering");
              }
            }}
            onClick={(event) => event.stopPropagation()}
          >
            <div
              className="vd-collection-sheet__drag-zone"
              onPointerDown={beginSheetDrag}
              onPointerMove={moveSheetDrag}
              onPointerUp={finishSheetDrag}
              onPointerCancel={(event) => finishSheetDrag(event, true)}
              onLostPointerCapture={(event) => finishSheetDrag(event, true)}
            >
              <div className="vd-collection-sheet__handle" aria-hidden="true" />
              <header className="vd-collection-sheet__head">
                <h2 id={titleId} className="vd-collection-sheet__title">
                  {shownSummary.name || "同目录视频"}
                </h2>
                <button
                  ref={closeRef}
                  type="button"
                  className="vd-collection-sheet__close"
                  onClick={() => closeSheet()}
                  aria-label="关闭合集"
                >
                  <X size={18} strokeWidth={2} />
                </button>
              </header>
              <div className="vd-collection-sheet__toolbar">
                <span>
                  选集
                  {shownSummary.total > 0 && (
                    <small>{shownSummary.total} 个视频</small>
                  )}
                </span>
                <button
                  type="button"
                  className="vd-collection-sheet__sort"
                  onClick={() => setAscending((value) => !value)}
                  aria-label={`切换为${ascending ? "倒序" : "正序"}`}
                >
                  <ArrowUpDown size={17} aria-hidden="true" />
                  {ascending ? "正序" : "倒序"}
                </button>
              </div>
            </div>

            {loading && !data ? (
              <CollectionLoading />
            ) : error && !data ? (
              <div className="vd-collection-sheet__state" role="alert">
                <span>{error}</span>
                <button type="button" onClick={retry}>
                  重新加载
                </button>
              </div>
            ) : items.length === 0 ? (
              <div className="vd-collection-sheet__state" role="status">
                <span>当前目录暂无其他视频</span>
              </div>
            ) : (
              <ul ref={listRef} className="vd-collection-sheet__list">
                {items.map((video) => {
                  const current = video.id === videoId;
                  return (
                    <CollectionItem
                      key={video.id}
                      ref={current ? currentItemRef : undefined}
                      video={video}
                      current={current}
                      navigationState={detailNavigationState}
                      replaceHistory={historyOpen}
                      onSelect={(event) => {
                        if (!current) return;
                        event.preventDefault();
                        closeSheet();
                      }}
                    />
                  );
                })}
              </ul>
            )}
          </section>
        </div>,
        document.body
      )
    : null;

  return (
    <div className="vd-mobile-collection">
      <button
        ref={triggerRef}
        type="button"
        className="vd-collection-entry"
        onClick={openSheet}
        aria-haspopup="dialog"
        aria-expanded={open}
      >
        <span className="vd-collection-entry__label">合集</span>
        <span className="vd-collection-entry__separator" aria-hidden="true">
          ·
        </span>
        <span className="vd-collection-entry__name">{collection.name}</span>
        <span className="vd-collection-entry__position">
          <span>
            {collection.currentIndex}/{collection.total}
          </span>
          <ChevronRight size={19} aria-hidden="true" />
        </span>
      </button>
      {sheet}
    </div>
  );
}

type CollectionItemProps = {
  video: VideoCollectionItem;
  current: boolean;
  navigationState: VideoDetailNavigationState;
  replaceHistory: boolean;
  onSelect: (event: React.MouseEvent<HTMLAnchorElement>) => void;
};

const CollectionItem = forwardRef<HTMLLIElement, CollectionItemProps>(
  function CollectionItem(
    { video, current, navigationState, replaceHistory, onSelect },
    forwardedRef
  ) {
    const [titleClicked, setTitleClicked] = useState(false);
    const rootRef = useRef<HTMLLIElement | null>(null);
    const inView = useInViewport(rootRef);
    const {
      previewEnabled, previewState, shouldRenderPreview, showPreviewLoader,
      videoRef, startPreview, stopPreview, handlePreviewPlay, finishPreviewLoader,
    } = useCardPreview({ id: video.id, src: video.previewSrc, inView });
    const setRootRef = useCallback(
      (node: HTMLLIElement | null) => {
        rootRef.current = node;
        if (typeof forwardedRef === "function") {
          forwardedRef(node);
        } else if (forwardedRef) {
          forwardedRef.current = node;
        }
      },
      [forwardedRef]
    );

    useEffect(() => {
      if (!titleClicked) return;
      const timeout = window.setTimeout(() => setTitleClicked(false), 180);
      return () => window.clearTimeout(timeout);
    }, [titleClicked]);

    function handlePointerEnter(event: React.PointerEvent<HTMLDivElement>) {
      if (event.pointerType !== "touch") startPreview();
    }

    function handlePointerLeave(event: React.PointerEvent<HTMLDivElement>) {
      if (event.pointerType !== "touch") stopPreview();
    }

    return (
      <li
        ref={setRootRef}
        className="vd-collection-item"
        data-preview-enabled={previewEnabled}
      >
        <Link
          to={video.href}
          replace={replaceHistory}
          state={navigationState}
          className="vd-collection-item__link"
          data-title-clicked={titleClicked || undefined}
          aria-current={current ? "page" : undefined}
          onClick={(event) => {
            stopPreview();
            onSelect(event);
          }}
        >
          <div
            className="vd-collection-item__thumb"
            onPointerEnter={handlePointerEnter}
            onPointerLeave={handlePointerLeave}
            onTouchStart={startPreview}
          >
            <VideoThumbnail src={video.thumbnail} />
            {previewEnabled && shouldRenderPreview && video.previewSrc && (
              <PreviewVideo
                ref={videoRef}
                src={video.previewSrc}
                state={previewState}
                onPlay={handlePreviewPlay}
                onEnded={stopPreview}
                onError={stopPreview}
              />
            )}
            {previewEnabled && shouldRenderPreview && showPreviewLoader && (
              <PreviewLoader onFinish={finishPreviewLoader} />
            )}
            {video.duration && (
              <span className="vd-collection-item__duration">
                {video.duration}
              </span>
            )}
            {current && (
              <span className="vd-collection-item__current-thumb">
                当前视频
              </span>
            )}
          </div>
          <div className="vd-collection-item__body">
            <h3
              className="vd-collection-item__title"
              onClick={() => setTitleClicked(true)}
            >
              {video.title}
            </h3>
            <div className="vd-collection-item__meta">
              {video.publishedAt && <span>{video.publishedAt}</span>}
              <span>
                <Eye size={12} aria-hidden="true" />
                {formatCount(video.views)} 次观看
              </span>
            </div>
          </div>
        </Link>
      </li>
    );
  }
);

function CollectionLoading() {
  return (
    <div
      className="vd-collection-sheet__loading"
      aria-busy="true"
      aria-label="合集加载中"
    >
      {Array.from({ length: 5 }).map((_, index) => (
        <div key={index} className="vd-collection-sheet__skeleton-row">
          <span className="vd-collection-sheet__skeleton-thumb" />
          <span className="vd-collection-sheet__skeleton-body">
            <span />
            <span />
          </span>
        </div>
      ))}
    </div>
  );
}
