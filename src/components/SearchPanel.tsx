import { memo, type FormEvent, useEffect, useRef, useState } from "react";
import { useNavigate, useSearchParams } from "react-router";
import { Search } from "lucide-react";
import { withListingNavigation } from "@/lib/listingSearchParams";

type SearchPanelProps = {
  value?: string;
  onSearch?: (keyword: string) => void;
  loading?: boolean;
  navigationPath?: string;
  variant?: "default" | "uiverse";
  placeholder?: string;
  className?: string;
};

export const SearchPanel = memo(function SearchPanel({
  value,
  onSearch,
  loading = false,
  navigationPath = "/list",
  variant = "default",
  placeholder = "搜索视频标题或作者",
  className,
}: SearchPanelProps = {}) {
  const isUiverse = variant === "uiverse";
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const urlKeyword = params.get("q") ?? "";
  const committedKeyword = value ?? urlKeyword;
  const [keyword, setKeyword] = useState(committedKeyword);
  const [submittedKeyword, setSubmittedKeyword] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const composingRef = useRef(false);
  const hasChanges = keyword.trim() !== committedKeyword.trim();
  const submitUnavailable = loading && !hasChanges;
  const searching = loading && submittedKeyword === committedKeyword.trim();

  function commitSearch(value: string) {
    const q = value.trim();
    if (onSearch) {
      onSearch(q);
      return;
    }
    const next = withListingNavigation(params, {
      q,
      tag: q ? null : undefined,
      page: 1,
    });
    const query = next.toString();
    navigate(query ? `${navigationPath}?${query}` : navigationPath);
  }

  useEffect(() => {
    setKeyword(committedKeyword);
  }, [committedKeyword]);

  useEffect(() => {
    if (!loading && submittedKeyword === committedKeyword.trim()) {
      setSubmittedKeyword(null);
    }
  }, [committedKeyword, loading, submittedKeyword]);

  function handleSubmit(e: FormEvent) {
    e.preventDefault();
    if (composingRef.current || submitUnavailable) return;
    const q = keyword.trim();
    setKeyword(q);
    setSubmittedKeyword(q);
    inputRef.current?.blur();
    commitSearch(q);
  }

  function handleReset() {
    setKeyword("");
    inputRef.current?.focus();
  }

  const searchInput = (
    <input
      ref={inputRef}
      className={isUiverse ? "search-panel__uiverse-input" : "search-panel__input"}
      type="text"
      enterKeyHint="search"
      value={keyword}
      onChange={(e) => setKeyword(e.target.value)}
      onCompositionStart={() => { composingRef.current = true; }}
      onCompositionEnd={() => { composingRef.current = false; }}
      onKeyDown={(e) => {
        // Safari may clear isComposing before the IME's confirmation Enter.
        if (e.key === "Enter" && (composingRef.current || e.nativeEvent.isComposing || e.keyCode === 229)) {
          e.preventDefault();
        }
      }}
      placeholder={placeholder}
      aria-label="搜索关键词"
    />
  );

  return (
    <form
      className={`search-panel${isUiverse ? " search-panel--uiverse" : ""}${className ? ` ${className}` : ""}`}
      onSubmit={handleSubmit}
      role="search"
      data-loading={loading || undefined}
      data-searching={searching || undefined}
    >
      {isUiverse ? (
        <>
          <button className="search-panel__uiverse-submit" type="submit" aria-label="搜索" aria-disabled={submitUnavailable || undefined}>
            <svg
              width="17"
              height="16"
              fill="none"
              xmlns="http://www.w3.org/2000/svg"
              aria-hidden="true"
            >
              <path
                d="M7.667 12.667A5.333 5.333 0 107.667 2a5.333 5.333 0 000 10.667zM14.334 14l-2.9-2.9"
                stroke="currentColor"
                strokeWidth="1.333"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
            </svg>
          </button>
          {searchInput}
          <button
            className="search-panel__reset"
            type="button"
            onClick={handleReset}
            aria-label="清空搜索"
            disabled={!keyword}
          >
            <svg
              xmlns="http://www.w3.org/2000/svg"
              fill="none"
              viewBox="0 0 24 24"
              stroke="currentColor"
              strokeWidth="2"
              aria-hidden="true"
            >
              <path
                strokeLinecap="round"
                strokeLinejoin="round"
                d="M6 18L18 6M6 6l12 12"
              />
            </svg>
          </button>
        </>
      ) : (
        <div className="search-panel__form">
          <div className="search-panel__input-wrapper">
            <Search size={16} className="search-panel__search-icon" aria-hidden="true" />
            {searchInput}
          </div>
          <button className="search-panel__submit" type="submit" aria-label="搜索" aria-disabled={submitUnavailable || undefined}>
            <Search size={16} className="search-panel__submit-icon" aria-hidden="true" />
            <span className="search-panel__submit-text">搜索</span>
          </button>
        </div>
      )}
      <span className="sr-only" role="status">{loading ? "正在加载搜索结果" : ""}</span>
    </form>
  );
});
