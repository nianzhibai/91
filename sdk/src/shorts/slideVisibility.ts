type SlideVisibilityEntry = Pick<
  IntersectionObserverEntry,
  "target" | "intersectionRatio"
>;

/** 保留拖动期间每屏最新的观测结果，恢复判定时只消费一次。 */
export class ShortsSlideVisibility {
  private readonly pending = new Map<Element, SlideVisibilityEntry>();

  update(entries: SlideVisibilityEntry[], deferActivation: boolean): number {
    for (const entry of entries) this.pending.set(entry.target, entry);
    if (deferActivation) return -1;

    let bestIndex = -1;
    let bestRatio = 0.6;
    for (const entry of this.pending.values()) {
      // 队列可能已裁剪；保留节点的 index 也可能变化，消费时再读。
      if (!entry.target.isConnected || entry.intersectionRatio <= bestRatio) {
        continue;
      }
      const index = Number((entry.target as HTMLElement).dataset.index ?? -1);
      if (!Number.isInteger(index) || index < 0) continue;
      bestIndex = index;
      bestRatio = entry.intersectionRatio;
    }
    this.clear();
    return bestIndex;
  }

  clear() {
    this.pending.clear();
  }
}
