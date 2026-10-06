import {
  memo,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Link, useSearchParams } from "react-router";
import { fetchTags, readCachedTags, type TagItem } from "@/data/videos";
import { withListingNavigation } from "@/lib/listingSearchParams";

type TagCloudStatus = "loading" | "ready" | "error";

type TagCloudProps = {
  linkBasePath?: string;
  onTagSelect?: () => void;
};

export const TagCloud = memo(function TagCloud({
  linkBasePath = "/list",
  onTagSelect,
}: TagCloudProps) {
  const [params] = useSearchParams();
  const activeTag = params.get("tag")?.trim() ?? "";
  const initialTagsRef = useRef<TagItem[] | null>(readCachedTags());
  const [tags, setTags] = useState<TagItem[]>(initialTagsRef.current ?? []);
  const [status, setStatus] = useState<TagCloudStatus>(
    initialTagsRef.current === null ? "loading" : "ready"
  );
  const [retryVersion, setRetryVersion] = useState(0);
  const visibleTags = useMemo(
    () => tags.filter((tag) => typeof tag.count !== "number" || tag.count > 0),
    [tags]
  );

  useEffect(() => {
    if (initialTagsRef.current !== null && retryVersion === 0) return;

    let active = true;
    setStatus("loading");
    fetchTags()
      .then((list) => {
        if (!active) return;
        setTags(list);
        setStatus("ready");
      })
      .catch(() => {
        if (active) setStatus("error");
      });
    return () => {
      active = false;
    };
  }, [retryVersion]);

  if (status === "loading") return null;
  if (status === "ready" && visibleTags.length === 0) return null;

  const failed = status === "error" && visibleTags.length === 0;

  const buildTagHref = (label: string) => {
    const nextTag = activeTag === label ? null : label;
    const next = withListingNavigation(params, { tag: nextTag, page: 1 });
    const query = next.toString();
    return query ? `${linkBasePath}?${query}` : linkBasePath;
  };

  const renderTag = (tag: TagItem) => (
    <Link
      key={tag.id}
      to={buildTagHref(tag.label)}
      className={`tag-cloud__link${activeTag === tag.label ? " is-active" : ""}`}
      aria-current={activeTag === tag.label ? "true" : undefined}
      onClick={onTagSelect}
    >
      {tag.label}
    </Link>
  );

  return (
    <nav
      className="tag-cloud-container"
      aria-label="热门标签"
    >
      {failed ? (
        <div className="tag-cloud__error" role="status">
          <span>标签加载失败</span>
          <button
            type="button"
            className="tag-cloud__retry"
            onClick={() => setRetryVersion((current) => current + 1)}
          >
            重新加载
          </button>
        </div>
      ) : (
        visibleTags.map(renderTag)
      )}
    </nav>
  );
});
