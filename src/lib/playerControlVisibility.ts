import type Artplayer from "artplayer";

const IDLE_MS = 3_000;

/** Own control visibility in both inline and fullscreen playback. */
export function bindPlayerControlVisibility(art: Artplayer, touchDevice: boolean, longPressMs: number) {
  const { $player: player, $bottom: bottom, $mask: mask } = art.template;
  const video = art.video;
  let initialLoading = video.readyState < video.HAVE_FUTURE_DATA;
  let visible = initialLoading || art.controls.show;
  let touchInput = touchDevice;
  let mouseOutside = false;
  let hoveredControl = false;
  let keyboardInput = false;
  let focusedControl = false;
  let disposed = false;
  let hideTimer: number | undefined;
  let surfaceTouch: { id: number; time: number; x: number; y: number } | null = null;
  let suppressTap = false;
  const pointers = new Set<number>();
  const keys = new Set<string>();
  const removeListeners: (() => void)[] = [];

  // Replace the built-in mouse and tap requests. Its playback shortcuts still
  // run independently; its timeupdate-based timeout is disabled by the caller.
  art.off("mousemove");
  art.off("click");

  function listen(target: EventTarget, name: string, listener: EventListener) {
    target.addEventListener(name, listener);
    removeListeners.push(() => target.removeEventListener(name, listener));
  }

  function isControl(target: EventTarget | null) {
    return target instanceof Node && (
      art.template.$controls.contains(target) ||
      art.template.$progress.contains(target) ||
      art.template.$setting.contains(target)
    );
  }

  function clearTimer() {
    window.clearTimeout(hideTimer);
    hideTimer = undefined;
  }

  function syncVisibility() {
    // Loadstart and other library lifecycle handlers must not reopen controls
    // that the viewer hid. Playback and user events below decide visibility.
    if (art.controls.show !== visible) art.controls.show = visible;
    const hidden = !visible || art.isLock;
    bottom.toggleAttribute("inert", hidden);
    bottom.setAttribute("aria-hidden", String(hidden));
    mask.toggleAttribute("inert", hidden);
  }

  function setVisible(next: boolean) {
    visible = initialLoading || next;
    syncVisibility();
  }

  function isInteracting() {
    return focusedControl || art.setting.show || pointers.size > 0 || keys.size > 0;
  }

  function scheduleHide() {
    clearTimer();
    if (
      disposed || initialLoading || !visible || document.hidden ||
      hoveredControl || isInteracting()
    ) return;
    hideTimer = window.setTimeout(() => {
      hideTimer = undefined;
      setVisible(false);
    }, IDLE_MS);
  }

  function finishInteraction() {
    if (mouseOutside && !touchInput && !isInteracting()) {
      clearTimer();
      setVisible(false);
    } else scheduleHide();
  }

  function reveal() {
    setVisible(true);
    scheduleHide();
  }

  function handlePointerMove(event: Event) {
    const pointer = event as PointerEvent;
    if (pointer.pointerType === "touch") {
      if (surfaceTouch && Math.hypot(pointer.clientX - surfaceTouch.x, pointer.clientY - surfaceTouch.y) > 8) {
        suppressTap = true;
      }
      return;
    }
    touchInput = false;
    mouseOutside = false;
    hoveredControl = isControl(pointer.target);
    reveal();
  }

  function handlePointerLeave(event: Event) {
    if ((event as PointerEvent).pointerType === "touch") return;
    touchInput = false;
    mouseOutside = true;
    hoveredControl = false;
    finishInteraction();
  }

  function handlePointerDown(event: Event) {
    const pointer = event as PointerEvent;
    if (pointer.button !== 0) return;
    touchInput = pointer.pointerType === "touch";
    mouseOutside = false;
    if (touchInput) hoveredControl = false;
    suppressTap = false;
    surfaceTouch = touchInput && pointer.target === video
      ? { id: pointer.pointerId, time: pointer.timeStamp, x: pointer.clientX, y: pointer.clientY }
      : null;
    keyboardInput = false;
    focusedControl = false;
    pointers.add(pointer.pointerId);
    clearTimer();
    if (isControl(pointer.target)) reveal();
  }

  function handlePointerUp(event: Event) {
    const pointer = event as PointerEvent;
    if (!pointers.delete(pointer.pointerId)) return;
    if (surfaceTouch?.id === pointer.pointerId) {
      suppressTap ||= pointer.timeStamp - surfaceTouch.time >= longPressMs;
      surfaceTouch = null;
    }
    finishInteraction();
  }

  function handleSurfaceClick() {
    // A tap controls the overlay. ArtPlayer retains desktop click-to-pause
    // and mobile double-tap behavior on the video itself.
    if (touchInput && suppressTap) {
      suppressTap = false;
      return;
    }
    if (art.setting.show) {
      art.setting.show = false;
      reveal();
    } else if (touchInput) {
      clearTimer();
      setVisible(!visible);
      scheduleHide();
    } else reveal();
  }

  function handleHotkey(event: KeyboardEvent) {
    keyboardInput = true;
    touchInput = false;
    keys.add(event.code);
    reveal();
  }

  function handleKeyDown(event: Event) {
    const key = event as KeyboardEvent;
    keyboardInput = true;
    if (!isControl(document.activeElement)) return;
    focusedControl = true;
    if (key.code !== "Tab") keys.add(key.code);
    reveal();
  }

  function handleKeyUp(event: Event) {
    if (keys.delete((event as KeyboardEvent).code)) finishInteraction();
  }

  function handleFocusIn(event: Event) {
    focusedControl = keyboardInput && isControl(event.target);
    if (focusedControl) reveal();
  }

  function handleFocusOut(event: Event) {
    focusedControl = keyboardInput && isControl((event as FocusEvent).relatedTarget);
    finishInteraction();
  }

  function handleSetting(show: boolean) {
    if (show) reveal(); else finishInteraction();
  }

  function handleScrub(bar: string, _percentage: number, event?: Event) {
    if (bar !== "played" || !(event?.target instanceof Node) || !player.contains(event.target)) return;
    if (touchInput) suppressTap = true;
    reveal();
  }

  function handleFullscreen() {
    hoveredControl = false;
    surfaceTouch = null;
    pointers.clear();
    reveal();
  }

  function handleInitialReady() {
    // Keep controls visible after startup until a subsequent user interaction.
    // Later buffering must not pin them open again.
    initialLoading = false;
  }

  function clearInteractions() {
    hoveredControl = false;
    focusedControl = false;
    surfaceTouch = null;
    pointers.clear();
    keys.clear();
    finishInteraction();
  }

  listen(player, "pointerenter", handlePointerMove);
  listen(player, "pointermove", handlePointerMove);
  listen(player, "pointerleave", handlePointerLeave);
  listen(player, "pointerdown", handlePointerDown);
  listen(document, "pointerup", handlePointerUp);
  listen(document, "pointercancel", handlePointerUp);
  listen(document, "keydown", handleKeyDown);
  listen(document, "keyup", handleKeyUp);
  listen(player, "focusin", handleFocusIn);
  listen(player, "focusout", handleFocusOut);
  listen(window, "blur", clearInteractions);
  listen(document, "visibilitychange", clearInteractions);
  art.on("click", handleSurfaceClick);
  art.on("hotkey", handleHotkey);
  art.on("control", syncVisibility);
  art.on("setting", handleSetting);
  art.on("setBar", handleScrub);
  art.on("lock", reveal);
  art.on("fullscreen", handleFullscreen);
  art.on("fullscreenWeb", handleFullscreen);
  art.on("video:canplay", handleInitialReady);
  art.on("video:playing", handleInitialReady);
  art.on("video:play", reveal);
  art.on("video:pause", reveal);
  art.on("video:ended", reveal);
  art.on("video:error", reveal);
  syncVisibility();
  scheduleHide();

  return () => {
    disposed = true;
    clearTimer();
    removeListeners.forEach(remove => remove());
    art.off("click", handleSurfaceClick);
    art.off("hotkey", handleHotkey);
    art.off("control", syncVisibility);
    art.off("setting", handleSetting);
    art.off("setBar", handleScrub);
    art.off("lock", reveal);
    art.off("fullscreen", handleFullscreen);
    art.off("fullscreenWeb", handleFullscreen);
    art.off("video:canplay", handleInitialReady);
    art.off("video:playing", handleInitialReady);
    art.off("video:play", reveal);
    art.off("video:pause", reveal);
    art.off("video:ended", reveal);
    art.off("video:error", reveal);
  };
}
