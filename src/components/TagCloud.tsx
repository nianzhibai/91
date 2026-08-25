import {
  memo,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Link, useSearchParams } from "react-router";
import { fetchTags, readCachedTags, type TagItem } from "@/data/videos";
import { withListingNavigation } from "@/lib/listingSearchParams";

const TAG_PLACEHOLDER_COUNT = 16;

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
  const [loaded, setLoaded] = useState(initialTagsRef.current !== null);
  const [hasMoreRight, setHasMoreRight] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);
  const visibleTags = useMemo(
    () => tags.filter((tag) => typeof tag.count !== "number" || tag.count > 0),
    [tags]
  );

  const updateScrollOverflow = useCallback(() => {
    const slider = containerRef.current;
    if (!slider) return;

    const remaining = slider.scrollWidth - slider.clientWidth - slider.scrollLeft;
    const nextHasMoreRight = remaining > 1;
    setHasMoreRight((current) => current === nextHasMoreRight ? current : nextHasMoreRight);
  }, []);

  useEffect(() => {
    if (initialTagsRef.current !== null) return;

    let active = true;
    fetchTags()
      .then((list) => {
        if (active) setTags(list);
      })
      .finally(() => {
        if (active) setLoaded(true);
      });
    return () => {
      active = false;
    };
  }, []);

  useLayoutEffect(() => {
    updateScrollOverflow();
  }, [updateScrollOverflow, visibleTags]);

  useEffect(() => {
    const slider = containerRef.current;
    if (!slider) return;

    let isDown = false;
    let startX = 0;
    let scrollLeft = 0;
    let isDragging = false;

    const handleMouseDown = (e: MouseEvent) => {
      isDown = true;
      isDragging = false;
      slider.classList.add("is-dragging");
      startX = e.pageX - slider.offsetLeft;
      scrollLeft = slider.scrollLeft;
    };

    const handleMouseLeave = () => {
      isDown = false;
      slider.classList.remove("is-dragging");
    };

    const handleMouseUp = () => {
      isDown = false;
      slider.classList.remove("is-dragging");
    };

    const handleMouseMove = (e: MouseEvent) => {
      if (!isDown) return;
      e.preventDefault();
      const x = e.pageX - slider.offsetLeft;
      const walk = (x - startX) * 1.5;
      if (Math.abs(x - startX) > 10) {
        isDragging = true;
      }
      slider.scrollLeft = scrollLeft - walk;
    };

    const handleWheel = (e: WheelEvent) => {
      if (e.deltaY !== 0) {
        e.preventDefault();
        slider.scrollLeft += e.deltaY;
      }
    };

    const handleClick = (e: MouseEvent) => {
      if (isDragging) {
        e.preventDefault();
        e.stopPropagation();
        isDragging = false;
      }
    };

    const resizeObserver = typeof ResizeObserver === "undefined"
      ? null
      : new ResizeObserver(updateScrollOverflow);

    slider.addEventListener("mousedown", handleMouseDown);
    slider.addEventListener("mouseleave", handleMouseLeave);
    slider.addEventListener("mouseup", handleMouseUp);
    slider.addEventListener("mousemove", handleMouseMove);
    slider.addEventListener("wheel", handleWheel, { passive: false });
    slider.addEventListener("click", handleClick, { capture: true });
    slider.addEventListener("scroll", updateScrollOverflow, { passive: true });
    window.addEventListener("resize", updateScrollOverflow);
    resizeObserver?.observe(slider);
    if (slider.firstElementChild) resizeObserver?.observe(slider.firstElementChild);
    updateScrollOverflow();

    return () => {
      slider.removeEventListener("mousedown", handleMouseDown);
      slider.removeEventListener("mouseleave", handleMouseLeave);
      slider.removeEventListener("mouseup", handleMouseUp);
      slider.removeEventListener("mousemove", handleMouseMove);
      slider.removeEventListener("wheel", handleWheel);
      slider.removeEventListener("click", handleClick, { capture: true });
      slider.removeEventListener("scroll", updateScrollOverflow);
      window.removeEventListener("resize", updateScrollOverflow);
      resizeObserver?.disconnect();
    };
  }, [updateScrollOverflow]);

  if (loaded && visibleTags.length === 0) return null;

  const loading = !loaded && visibleTags.length === 0;

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
      className={`tag-chip ${activeTag === tag.label ? "is-active" : ""}`}
      onClick={onTagSelect}
    >
      {tag.label}
    </Link>
  );

  return (
    <div
      className={`tag-cloud-container${loading ? " is-loading" : ""}${hasMoreRight ? " has-more-right" : ""}`}
      aria-label="热门标签"
      aria-busy={loading ? "true" : undefined}
    >
      <div className="tag-cloud__grid" ref={containerRef}>
        <div className="tag-cloud__row">
          {loading
            ? Array.from({ length: TAG_PLACEHOLDER_COUNT }, (_, item) => (
                <span
                  key={item}
                  className="tag-chip tag-chip--placeholder"
                  aria-hidden="true"
                />
              ))
            : visibleTags.map(renderTag)}
        </div>
      </div>
    </div>
  );
});
