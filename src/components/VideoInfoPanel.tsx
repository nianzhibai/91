import { useEffect, useId, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Pencil, Tag, X } from "lucide-react";
import { useDocumentScrollLock } from "@/lib/useDocumentScrollLock";
import { useNativeBackHandler } from "@/lib/useNativeBack";
import type { TagItem, VideoDetail } from "@/types";

type Props = {
  video: VideoDetail;
  availableTags?: TagItem[];
  availableTagsLoading?: boolean;
  availableTagsError?: string;
  onRetryAvailableTags?: () => void;
  tagSaving?: boolean;
  onTagsChange?: (tags: string[]) => Promise<void>;
};

/** 视频标签卡，管理员可通过弹窗编辑标签。 */
export function VideoInfoPanel({
  video,
  availableTags = [],
  availableTagsLoading = false,
  availableTagsError = "",
  onRetryAvailableTags,
  tagSaving = false,
  onTagsChange,
}: Props) {
  const [editingTags, setEditingTags] = useState(false);
  const [draftTags, setDraftTags] = useState<string[]>(video.tags ?? []);
  const [tagError, setTagError] = useState("");
  const tagEditorTitleId = useId();
  const tagEditorRef = useRef<HTMLDivElement | null>(null);

  useDocumentScrollLock(editingTags);
  useNativeBackHandler(editingTags, () => {
    if (!tagSaving) closeTagEditor();
  });

  const tags = video.tags ?? [];

  const sortedAvailable = useMemo(() => {
    return [...availableTags].sort((a, b) => {
      const ac = a.count ?? 0;
      const bc = b.count ?? 0;
      if (bc !== ac) return bc - ac;
      return a.label.localeCompare(b.label, "zh-Hans-CN");
    });
  }, [availableTags]);

  useEffect(() => {
    if (!editingTags) return;

    function onKeyDown(e: KeyboardEvent) {
      if (e.key !== "Escape" || tagSaving) return;
      e.preventDefault();
      closeTagEditor();
    }

    const focusTimer = window.setTimeout(() => {
      const firstButton = tagEditorRef.current?.querySelector<HTMLButtonElement>(
        "button:not(:disabled)"
      );
      (firstButton ?? tagEditorRef.current)?.focus();
    }, 0);

    document.addEventListener("keydown", onKeyDown);
    return () => {
      window.clearTimeout(focusTimer);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [editingTags, tagSaving]);

  function openTagEditor() {
    setDraftTags(tags);
    setTagError("");
    setEditingTags(true);
  }

  function closeTagEditor() {
    setEditingTags(false);
    setTagError("");
  }

  async function saveTags() {
    if (!onTagsChange) return;
    setTagError("");
    try {
      await onTagsChange(draftTags);
      setEditingTags(false);
    } catch (e) {
      setTagError(e instanceof Error ? e.message : "保存标签失败");
    }
  }

  const tagEditor = editingTags
    ? createPortal(
        <div className="vd-tag-editor-modal" role="presentation">
          <div
            ref={tagEditorRef}
            className="vd-tag-editor"
            role="dialog"
            aria-modal="true"
            aria-labelledby={tagEditorTitleId}
            tabIndex={-1}
          >
            <header className="vd-tag-editor__head">
              <span id={tagEditorTitleId}>选择适用的标签</span>
              <button
                type="button"
                className="vd-tag-editor__close"
                onClick={closeTagEditor}
                disabled={tagSaving}
                aria-label="关闭"
              >
                <X size={16} />
              </button>
            </header>

            <div className="vd-tag-editor__grid">
              {availableTagsLoading ? (
                <div className="vd-tag-editor__empty" role="status">
                  正在加载标签…
                </div>
              ) : availableTagsError ? (
                <div className="vd-tag-editor__empty" role="alert">
                  <span>{availableTagsError}</span>
                  {onRetryAvailableTags && (
                    <button
                      type="button"
                      className="vd-tag-editor__retry"
                      onClick={onRetryAvailableTags}
                    >
                      重新加载
                    </button>
                  )}
                </div>
              ) : sortedAvailable.length === 0 ? (
                <div className="vd-tag-editor__empty">暂无可用标签</div>
              ) : (
                sortedAvailable.map((tag) => {
                  const checked = draftTags.includes(tag.label);
                  return (
                    <button
                      type="button"
                      key={tag.id}
                      className={`vd-tag-editor__chip${
                        checked ? " is-active" : ""
                      }`}
                      onClick={() =>
                        setDraftTags((prev) =>
                          prev.includes(tag.label)
                            ? prev.filter((t) => t !== tag.label)
                            : [...prev, tag.label]
                        )
                      }
                      disabled={tagSaving}
                      aria-pressed={checked}
                    >
                      <span>{tag.label}</span>
                    </button>
                  );
                })
              )}
            </div>

            {tagError && <div className="vd-tag-editor__error">{tagError}</div>}

            <div className="vd-tag-editor__actions">
              <button
                type="button"
                className="vd-tag-editor__btn"
                onClick={closeTagEditor}
                disabled={tagSaving}
              >
                取消
              </button>
              <button
                type="button"
                className="vd-tag-editor__btn is-primary"
                onClick={saveTags}
                disabled={
                  tagSaving || availableTagsLoading || !!availableTagsError
                }
              >
                {tagSaving ? "保存中..." : "保存"}
              </button>
            </div>
          </div>
        </div>,
        document.body
      )
    : null;

  return (
    <section className="vd-info" aria-label="视频信息">
      <div className="vd-info__tags">
        <div className="vd-info__section-head">
          <span className="vd-info__section-title">
            <Tag size={15} strokeWidth={2} aria-hidden="true" />
            标签
          </span>
          {onTagsChange && (
            <button
              type="button"
              className="vd-info__tags-edit"
              onClick={openTagEditor}
              aria-label="编辑标签"
            >
              <Pencil size={13} />
              <span>编辑</span>
            </button>
          )}
        </div>
        <div className="vd-info__tags-list">
          {tags.length === 0 ? (
            <span className="vd-info__tags-empty">暂无标签</span>
          ) : (
            tags.map((t) => (
              <span key={t} className="vd-tag">
                {t}
              </span>
            ))
          )}
        </div>
      </div>

      {tagEditor}
    </section>
  );
}
