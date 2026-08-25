/**
 * 虚拟网格的纯计算层：把"一维的视频列表"折成"二维的行"，以及决定何时
 * 续下一批。窗口本身由 @tanstack/react-virtual 负责，这里只留可以在没有
 * 浏览器的单元测试里覆盖到每个分支的算术。
 */

function toCount(value: number): number {
  if (!Number.isFinite(value) || value <= 0) return 0;
  return Math.floor(value);
}

/** 与视频网格 CSS 断点一致；首帧即可按正确列数折行。 */
export function virtualGridColumns(input: {
  compact: boolean;
  mobile: boolean;
  tablet: boolean;
}): number {
  if (input.compact) return 1;
  if (input.mobile) return 2;
  if (input.tablet) return 3;
  return 4;
}

/** 虚拟单元是"整行"，行数决定 virtualizer 的 count。 */
export function virtualRowCount(itemCount: number, columns: number): number {
  const items = toCount(itemCount);
  if (items === 0) return 0;
  return Math.ceil(items / Math.max(1, toCount(columns) || 1));
}

/** 某一行覆盖的下标区间 [start, end)，末行不足一整行时按实际条数收口。 */
export function virtualRowRange(
  rowIndex: number,
  columns: number,
  itemCount: number
): { start: number; end: number } {
  const items = toCount(itemCount);
  const perRow = Math.max(1, toCount(columns) || 1);
  const row = Number.isFinite(rowIndex) ? Math.floor(rowIndex) : -1;
  if (row < 0 || items === 0) return { start: 0, end: 0 };
  const start = Math.min(row * perRow, items);
  return { start, end: Math.min(start + perRow, items) };
}

export type LoadMoreInput = {
  /** 当前渲染窗口的结束下标（不含）。 */
  endIndex: number;
  itemCount: number;
  columns: number;
  hasMore: boolean;
  loading: boolean;
  /** 距列表尾部还有几行时开始预取。 */
  prefetchRows?: number;
};

/**
 * 加载更多的触发条件直接来自渲染窗口，而不是另加一个哨兵节点：
 * 虚拟列表里哨兵本身可能被回收，两套机制也会各自漂移。
 */
export function shouldLoadMore(input: LoadMoreInput): boolean {
  if (!input.hasMore || input.loading) return false;
  const columns = Math.max(1, toCount(input.columns) || 1);
  const prefetchRows = Math.max(1, toCount(input.prefetchRows ?? 1) || 1);
  return input.endIndex >= toCount(input.itemCount) - prefetchRows * columns;
}
