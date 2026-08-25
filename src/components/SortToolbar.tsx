import { memo } from "react";
import { LayoutGrid, List } from "lucide-react";
import type { SortKey } from "@/types";

type ViewMode = "grid" | "compact";

type Props = {
  sort: SortKey;
  view: ViewMode;
  onSortChange: (s: SortKey) => void;
  onViewChange: (v: ViewMode) => void;
  sortDisabled?: boolean;
};

const sortOptions: { key: SortKey; label: string }[] = [
  { key: "hot", label: "最热" },
  { key: "latest", label: "最新" },
  { key: "recent", label: "最近观看" },
];

export const SortToolbar = memo(function SortToolbar({
  sort,
  view,
  onSortChange,
  onViewChange,
  sortDisabled = false,
}: Props) {
  return (
    <div className="sort-toolbar" role="toolbar" aria-label="排序和视图">
      <div className="sort-toolbar__group">
        {sortOptions.map((o) => (
          <button
            type="button"
            key={o.key}
            className={`sort-toolbar__btn ${sort === o.key ? "is-active" : ""}`}
            onClick={() => onSortChange(o.key)}
            disabled={sortDisabled}
            aria-pressed={sort === o.key}
          >
            {o.label}
          </button>
        ))}
      </div>
      <div className="sort-toolbar__spacer" />
      <div className="sort-toolbar__group" aria-label="视图切换">
        <button
          type="button"
          className={`sort-toolbar__btn ${view === "grid" ? "is-active" : ""}`}
          onClick={() => onViewChange("grid")}
          aria-pressed={view === "grid"}
          aria-label="基础视图"
        >
          <LayoutGrid size={14} />
        </button>
        <button
          type="button"
          className={`sort-toolbar__btn ${
            view === "compact" ? "is-active" : ""
          }`}
          onClick={() => onViewChange("compact")}
          aria-pressed={view === "compact"}
          aria-label="详细视图"
        >
          <List size={14} />
        </button>
      </div>
    </div>
  );
});

export type { ViewMode };
