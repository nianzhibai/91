/** 视口顶部为下拉系统栏预留的触摸起点范围（CSS px）。 */
export const SHORTS_SYSTEM_GESTURE_TOP_PX = 32;

/** 按起点分配整段手势，手指进入视频区域后也不能再接管。 */
export function isShortsSystemGestureStart(
  event: Pick<PointerEvent, "pointerType" | "clientY">
): boolean {
  if (event.pointerType !== "touch") return false;
  const viewportTop = window.visualViewport?.offsetTop ?? 0;
  return event.clientY <= viewportTop + SHORTS_SYSTEM_GESTURE_TOP_PX;
}
