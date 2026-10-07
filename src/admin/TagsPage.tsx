import { useEffect, useMemo, useState, type KeyboardEvent } from "react";
import { Film, Plus, RefreshCw, Trash2 } from "lucide-react";
import * as api from "./api";
import { useToast } from "@/components/ToastContext";
import { ConfirmModal } from "./ConfirmModal";
import { Modal } from "./Modal";
import { AdminEmptyVisual } from "./AdminEmptyVisual";
import { AdminPagination } from "./AdminPagination";
import { useAdminFloatingActionSpace } from "./useAdminFloatingActionSpace";
import { useAdminRouteActive } from "./AdminRouteCache";
import { useAdminResource } from "./useAdminResource";
import { useAuth } from "./AuthContext";

const DESKTOP_TAGS_PAGE_SIZE = 24;
const MOBILE_TAGS_PAGE_SIZE = 8;
const TAGS_MOBILE_QUERY = "(max-width: 640px)";
const TAG_SOURCE_FILTERS = ["user", "generated"];
const TAG_DISPLAY_GROUP_ORDER: Record<string, number> = {
  user: 0,
  crawler: 1,
  generated: 1,
};

type DeleteConfirmState =
  | { kind: "single"; tag: api.AdminTag }
  | { kind: "bulk"; ids: number[] }
  | null;

export function TagsPage() {
  const floatingActionPageRef = useAdminFloatingActionSpace<HTMLElement>();
  const routeActive = useAdminRouteActive();
  const { invalidateSession } = useAuth();
  const tagsResource = useAdminResource(api.listTags, {
    queryKey: "tags", active: routeActive, intervalMs: null, initialData: [], onUnauthorized: invalidateSession,
  });
  const { data: tags, loading, invalidate: refresh } = tagsResource;
  const loadError = tagsResource.ready ? "" : tagsResource.error;
  const [label, setLabel] = useState("");
  const [saving, setSaving] = useState(false);
  const [deletingId, setDeletingId] = useState<number | null>(null);
  const [deleteConfirm, setDeleteConfirm] = useState<DeleteConfirmState>(null);
  const [filterSource, setFilterSource] = useState<string>("all");
  const [selectMode, setSelectMode] = useState(false);
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const [editingTag, setEditingTag] = useState<api.AdminTag | null>(null);
  const [createModalOpen, setCreateModalOpen] = useState(false);
  const [activeTagActionsId, setActiveTagActionsId] = useState<number | null>(null);
  const pageSize = useTagsPageSize();
  const [page, setPage] = useState(1);
  const { show } = useToast();

  async function handleCreate() {
    const cleanLabel = label.trim();
    if (!cleanLabel) return;
    if (createLabelExists) return;
    setSaving(true);
    try {
      const r = await api.createTag(cleanLabel);
      show(`已添加标签「${r.label}」`, "success");
      setLabel("");
      setCreateModalOpen(false);
      await refresh();
    } catch (e) {
      show(e instanceof Error ? e.message : "添加标签失败", "error");
    } finally {
      setSaving(false);
    }
  }

  function handleDelete(tag: api.AdminTag) {
    setDeleteConfirm({ kind: "single", tag });
  }

  function openCreateModal() {
    setLabel("");
    setCreateModalOpen(true);
  }

  function toggleSelectMode() {
    setSelectMode((m) => !m);
    setSelected(new Set());
    setActiveTagActionsId(null);
  }

  function toggleSelect(id: number) {
    setSelected((prev) => {
      const next = new Set(prev);
      next.has(id) ? next.delete(id) : next.add(id);
      return next;
    });
  }

  async function handleBulkDelete() {
    const ids = [...selected];
    if (ids.length === 0) return;
    setDeleteConfirm({ kind: "bulk", ids });
  }

  async function confirmDelete() {
    if (!deleteConfirm) return;

    if (deleteConfirm.kind === "single") {
      const tag = deleteConfirm.tag;
      setDeletingId(tag.id);
      try {
        const r = await api.deleteTag(tag.id);
        show(`已删除标签，并从 ${r.removedVideos} 个视频移除`, "success");
        setDeleteConfirm(null);
        await refresh();
      } catch (e) {
        show(e instanceof Error ? e.message : "删除标签失败", "error");
      } finally {
        setDeletingId(null);
      }
      return;
    }

    const ids = deleteConfirm.ids;
    setBulkDeleting(true);
    try {
      let success = 0;
      for (const id of ids) {
        try {
          await api.deleteTag(id);
          success++;
        } catch {
          // Keep deleting the rest of the selected tags; report aggregate failure below.
        }
      }
      const failed = ids.length - success;
      show(
        failed ? `批量删除完成，成功 ${success} / ${ids.length} 个，失败 ${failed} 个` : `已删除 ${success} 个标签`,
        failed ? (success > 0 ? "info" : "error") : "success"
      );
      setSelected(new Set());
      setSelectMode(false);
      setDeleteConfirm(null);
      await refresh();
    } finally {
      setBulkDeleting(false);
    }
  }

  const stats = useMemo(() => {
    const sourceCounts: Record<string, number> = {};
    let total = 0;

    tags.forEach((t) => {
      if (!isSupportedTag(t)) return;
      total++;
      const key = tagSourceKey(t);
      sourceCounts[key] = (sourceCounts[key] ?? 0) + 1;
    });

    return {
      total,
      sourceCounts,
    };
  }, [tags]);

  const filteredTags = useMemo(() => {
    const matches = tags.filter((t) => {
      if (!isSupportedTag(t)) return false;
      return filterSource === "all" || tagSourceKey(t) === filterSource;
    });

    if (filterSource !== "all") return matches;

    return matches
      .map((tag, index) => ({ tag, index }))
      .sort((a, b) => {
        const rankDelta = tagDisplayGroupRank(a.tag) - tagDisplayGroupRank(b.tag);
        return rankDelta || a.index - b.index;
      })
      .map(({ tag }) => tag);
  }, [tags, filterSource]);
  const tagsEmpty = !loading && !loadError && stats.total === 0;
  const resultsEmpty = !tagsEmpty && !loading && !loadError && filteredTags.length === 0;

  const totalPages = Math.max(1, Math.ceil(filteredTags.length / pageSize));
  const currentPage = Math.min(page, totalPages);
  const pageStartIndex = (currentPage - 1) * pageSize;
  const pageEndIndex = pageStartIndex + pageSize;
  const pagedTags = useMemo(
    () => filteredTags.slice(pageStartIndex, pageEndIndex),
    [filteredTags, pageStartIndex, pageEndIndex]
  );
  const showPagination = filteredTags.length > pageSize;

  useEffect(() => {
    setPage(1);
  }, [filterSource, pageSize]);

  useEffect(() => {
    setPage((p) => Math.min(Math.max(1, p), totalPages));
  }, [totalPages]);

  const deletablePageTags = useMemo(
    () => pagedTags,
    [pagedTags]
  );
  const allSelected =
    deletablePageTags.length > 0 && deletablePageTags.every((t) => selected.has(t.id));
  const createLabelExists = useMemo(() => {
    const cleanLabel = label.trim().toLowerCase();
    if (!cleanLabel) return false;
    return tags.some((tag) => tag.label.trim().toLowerCase() === cleanLabel);
  }, [label, tags]);

  function selectPageTags() {
    setSelected((prev) => {
      const next = new Set(prev);
      deletablePageTags.forEach((t) => next.add(t.id));
      return next;
    });
  }

  function toggleTagActions(id: number) {
    setActiveTagActionsId((current) => (current === id ? null : id));
  }

  function handleTagCardKeyDown(event: KeyboardEvent<HTMLDivElement>, id: number) {
    if (event.target !== event.currentTarget) return;
    if (event.key !== "Enter" && event.key !== " ") return;
    event.preventDefault();
    toggleTagActions(id);
  }

  return (
    <section
      ref={floatingActionPageRef}
      className="admin-page admin-page--with-floating-actions admin-tags-page"
    >
      {tagsResource.ready && tagsResource.error && (
        <div className="admin-detail-error" role="alert">
          标签更新失败：{tagsResource.error}
          <button type="button" className="admin-btn" onClick={() => void tagsResource.refresh()}>重试</button>
        </div>
      )}
      <div className="admin-tags-layout">
        <div className="admin-tags-main">
          <div className="admin-tags-toolbar">
            <aside className="admin-tags-filter-panel" aria-label="标签分类">
              <div className="admin-tags-filter-tabs">
                <button
                  type="button"
                  className={`admin-tags-filter-tab ${filterSource === "all" ? "is-active" : ""}`}
                  onClick={() => setFilterSource("all")}
                  aria-label="全部"
                >
                  <span className="admin-tags-filter-tab__text">全部</span>
                </button>
                {TAG_SOURCE_FILTERS.filter((source) => (stats.sourceCounts[source] ?? 0) > 0).map((source) => {
                  const label = sourceLabel(source);
                  return (
                    <button
                      key={source}
                      type="button"
                      className={`admin-tags-filter-tab ${filterSource === source ? "is-active" : ""}`}
                      onClick={() => setFilterSource(source)}
                      aria-label={label}
                    >
                      <span className="admin-tags-filter-tab__text">{label}</span>
                    </button>
                  );
                })}
              </div>
            </aside>

            {!selectMode && (
              <div className="admin-tags-toolbar-actions" data-admin-floating-actions>
                <button
                  type="button"
                  className="admin-btn admin-tags-toolbar-actions__create"
                  onClick={openCreateModal}
                >
                  <Plus size="1em" aria-hidden="true" />
                  新增标签
                </button>
                {stats.total > 0 && (
                  <button
                    type="button"
                    className="admin-btn admin-tags-toolbar-actions__toggle"
                    onClick={toggleSelectMode}
                  >
                    <Trash2 size="1em" aria-hidden="true" />
                    批量删除
                  </button>
                )}
              </div>
            )}
          </div>

          {tagsEmpty ? (
            <AdminEmptyVisual
              variant="empty"
              text="当前没有标签"
              className="admin-empty-state admin-empty-state--plain admin-tags-empty-state"
            />
          ) : resultsEmpty ? (
            <AdminEmptyVisual
              variant="no-results"
              text="未查询到"
              className="admin-empty-state admin-empty-state--plain admin-tags-empty-state"
            />
          ) : (
            <div className="admin-tags-board" aria-busy={loading || tagsResource.refreshing || undefined}>
              <div className="admin-tags-cards">
                {loading ? null : loadError ? (
                  <div className="admin-error-state">
                    <strong>标签加载失败</strong>
                    <span>{loadError}</span>
                    <button type="button" className="admin-btn" onClick={() => void tagsResource.refresh()}>
                      <RefreshCw size={13} /> 重试
                    </button>
                  </div>
                ) : (
                  <>
                  <div className="admin-tags-grid">
                    {pagedTags.map((tag) => {
                      const selectable = selectMode;
                      const isSelected = selected.has(tag.id);
                      const actionsOpen = activeTagActionsId === tag.id;
                      const cardClass = `admin-tag-card${selectable ? " is-selectable" : ""}${
                        selectable && isSelected ? " is-selected" : ""
                      }${!selectable && actionsOpen ? " is-actions-open" : ""}`;
                      const cardContent = (
                        <>
                          <div className="admin-tag-card__head">
                            <span className="admin-tag-card__title">{tag.label}</span>
                            <span className="admin-tag-card__source-badge" data-source={tagCardSourceKey(tag)}>
                              {tagCardSourceLabel(tag)}
                            </span>
                          </div>

                          <div className="admin-tag-card__footer">
                            <span className="admin-tag-card__count">
                              <Film size={13} />
                              <strong>{tag.count}</strong> 视频
                            </span>
                            <div className="admin-tag-card__footer-actions">
                              {!selectMode && (
                                <button
                                  type="button"
                                  className="admin-tag-card__delete"
                                  onClick={(event) => {
                                    event.stopPropagation();
                                    handleDelete(tag);
                                  }}
                                  disabled={deletingId === tag.id}
                                  aria-label={`删除标签 ${tag.label}`}
                                >
                                  <span>{deletingId === tag.id ? "删除中" : "删除"}</span>
                                </button>
                              )}
                              {!selectMode && (
                                <button
                                  type="button"
                                  className="admin-tag-card__edit"
                                  onClick={(event) => {
                                    event.stopPropagation();
                                    setEditingTag(tag);
                                  }}
                                  aria-label={`编辑标签 ${tag.label}`}
                                >
                                  <span>编辑</span>
                                </button>
                              )}
                            </div>
                          </div>
                        </>
                      );
                      return selectable ? (
                        <button
                          key={tag.id}
                          type="button"
                          className={cardClass}
                          onClick={() => toggleSelect(tag.id)}
                          aria-pressed={isSelected}
                          aria-label={`${isSelected ? "取消选中" : "选中"}标签 ${tag.label}`}
                        >
                          {cardContent}
                        </button>
                      ) : (
                        <div
                          key={tag.id}
                          className={cardClass}
                          role="button"
                          tabIndex={0}
                          aria-expanded={actionsOpen}
                          aria-label={`标签 ${tag.label}`}
                          onClick={() => toggleTagActions(tag.id)}
                          onKeyDown={(event) => handleTagCardKeyDown(event, tag.id)}
                        >
                          {cardContent}
                        </div>
                      );
                    })}
                  </div>

                  {showPagination && (
                    <AdminPagination
                      page={currentPage}
                      totalPages={totalPages}
                      total={filteredTags.length}
                      itemLabel="标签"
                      onPage={setPage}
                    />
                  )}
                  </>
                )}
              </div>
            </div>
          )}
        </div>
      </div>
      {selectMode && (
        <div
          className="admin-tags-bulk-toolbar"
          data-admin-floating-actions
          role="region"
          aria-label="标签批量操作"
        >
          <div className="admin-tags-bulk-actions">
            <span className="admin-tags-bulk-actions__count">已选择 {selected.size} 项</span>
            <button
              type="button"
              className="admin-btn admin-tags-bulk-actions__btn admin-tags-bulk-actions__select-page"
              onClick={selectPageTags}
              disabled={deletablePageTags.length === 0 || allSelected}
            >
              全选本页
            </button>
            <button
              type="button"
              className="admin-btn admin-tags-bulk-actions__btn"
              onClick={() => setSelected(new Set())}
              disabled={selected.size === 0}
            >
              取消选中
            </button>
            <button
              type="button"
              className="admin-btn admin-tags-bulk-actions__btn"
              onClick={handleBulkDelete}
              disabled={selected.size === 0 || bulkDeleting}
            >
              {bulkDeleting ? "删除中..." : "删除选中"}
            </button>
            <button
              type="button"
              className="admin-btn admin-tags-bulk-actions__btn"
              onClick={toggleSelectMode}
            >
              退出批量
            </button>
          </div>
        </div>
      )}
      <Modal
        open={createModalOpen}
        title="新增标签"
        className="admin-modal--tag-rules admin-modal--tag-dialog admin-modal--tag-create"
        onClose={() => {
          if (!saving) setCreateModalOpen(false);
        }}
        footer={
          <>
            <button
              type="button"
              className="admin-btn"
              onClick={() => setCreateModalOpen(false)}
              disabled={saving}
            >
              取消
            </button>
            <button
              type="submit"
              form="admin-create-tag-form"
              className="admin-btn is-primary"
              disabled={saving || !label.trim() || createLabelExists}
            >
              {saving ? "添加中..." : "确认"}
            </button>
          </>
        }
      >
        <form
          id="admin-create-tag-form"
          className="admin-form admin-tag-rule-form"
          onSubmit={(e) => {
            e.preventDefault();
            handleCreate();
          }}
        >
          <div className="admin-form__row admin-tag-create-row">
            <input
              id="admin-tag-label"
              aria-label="输入标签名"
              aria-describedby={createLabelExists ? "admin-tag-create-warning" : undefined}
              value={label}
              onChange={(e) => setLabel(e.target.value)}
              placeholder="输入标签名"
            />
            <span
              className={`admin-tag-create-warning${createLabelExists ? " is-visible" : ""}`}
              id="admin-tag-create-warning"
              aria-hidden={!createLabelExists}
            >
              当前标签已存在
            </span>
          </div>
        </form>
      </Modal>
      {editingTag && (
        <EditTagModal
          tag={editingTag}
          onClose={() => setEditingTag(null)}
          onChanged={async () => {
            await refresh();
          }}
        />
      )}
      <ConfirmModal
        open={!!deleteConfirm}
        title={deleteConfirm?.kind === "bulk" ? "删除选中标签" : "删除标签"}
        message={
          deleteConfirm?.kind === "bulk"
            ? `确定要删除选中的 ${deleteConfirm.ids.length} 个标签吗？`
            : `确定要删除标签「${deleteConfirm?.tag.label ?? ""}」吗？`
        }
        confirmText="确认"
        danger
        hideIcon
        centerMessage
        modalClassName="admin-modal--delete-confirm admin-modal--tag-dialog admin-modal--tag-delete-confirm"
        loading={deletingId !== null || bulkDeleting}
        restoreFocus={false}
        onCancel={() => {
          if (deletingId === null && !bulkDeleting) setDeleteConfirm(null);
        }}
        onConfirm={confirmDelete}
      />
    </section>
  );
}

function EditTagModal({
  tag,
  onClose,
  onChanged,
}: {
  tag: api.AdminTag;
  onClose: () => void;
  onChanged: () => void | Promise<void>;
}) {
  const [draft, setDraft] = useState(() => tagRuleDraft(tag));
  const [saving, setSaving] = useState(false);
  const { show } = useToast();
  const isAV = isAVTag(tag);

  async function persistDraft(nextDraft: RuleDraft) {
    const parsedRules = matchRulesFromDraft(nextDraft, isAV);
    if (!isAV && !hasRuleTerms(parsedRules)) {
      show("至少保留一个包含词", "error");
      return;
    }
    setSaving(true);
    try {
      await api.updateTag(tag.id, parsedRules);
      setDraft(nextDraft);
      show("标签已保存", "success");
      await onChanged();
    } catch (e) {
      show(e instanceof Error ? e.message : "保存标签失败", "error");
    } finally {
      setSaving(false);
    }
  }

  return (
    <Modal
      open
      title={tag.label}
      className="admin-modal--tag-rules admin-modal--tag-dialog"
      onClose={onClose}
      restoreFocus={false}
    >
      <div className="admin-form admin-tag-rule-form">
        {isAV ? (
          <div className="admin-form__row">
            <PrefixPillEditor
              value={draft.avCodePrefixes}
              onCommit={(value) => void persistDraft({ ...draft, avCodePrefixes: value })}
              disabled={saving}
            />
          </div>
        ) : (
          <div className="admin-form__row">
            <KeywordPillEditor
              value={draft.keywords}
              onCommit={(value) => void persistDraft({ ...draft, keywords: value })}
              disabled={saving}
            />
          </div>
        )}
      </div>
    </Modal>
  );
}

function KeywordPillEditor({
  value,
  onCommit,
  disabled,
}: {
  value: string;
  onCommit: (value: string) => void;
  disabled: boolean;
}) {
  return (
    <RulePillEditor
      value={value}
      onCommit={onCommit}
      disabled={disabled}
      inputId="admin-tag-rule-keywords"
      inputLabel="添加包含词"
      listLabel="当前包含词"
      emptyText="暂无包含词"
      duplicateText="当前包含词已存在"
      warningId="admin-tag-rule-keyword-warning"
      removeLabelPrefix="移除包含词"
      splitTerms={splitRuleTerms}
    />
  );
}

function PrefixPillEditor({
  value,
  onCommit,
  disabled,
}: {
  value: string;
  onCommit: (value: string) => void;
  disabled: boolean;
}) {
  return (
    <RulePillEditor
      value={value}
      onCommit={onCommit}
      disabled={disabled}
      inputId="admin-tag-rule-prefixes"
      inputLabel="添加车牌前缀"
      listLabel="当前车牌前缀"
      emptyText="暂无车牌前缀"
      duplicateText="当前车牌前缀已存在"
      warningId="admin-tag-rule-prefix-warning"
      removeLabelPrefix="移除车牌前缀"
      splitTerms={splitPrefixTerms}
      allowEmpty
    />
  );
}

function RulePillEditor({
  value,
  onCommit,
  disabled,
  inputId,
  inputLabel,
  listLabel,
  emptyText,
  duplicateText,
  warningId,
  removeLabelPrefix,
  splitTerms,
  allowEmpty = false,
}: {
  value: string;
  onCommit: (value: string) => void;
  disabled: boolean;
  inputId: string;
  inputLabel: string;
  listLabel: string;
  emptyText: string;
  duplicateText: string;
  warningId: string;
  removeLabelPrefix: string;
  splitTerms: (value: string) => string[];
  allowEmpty?: boolean;
}) {
  const [input, setInput] = useState("");
  const terms = splitTerms(value);
  const pendingTerm = singleRuleTerm(input, splitTerms);
  const pendingExists = terms.some((term) => term.toLowerCase() === pendingTerm.toLowerCase());
  const showDuplicateWarning = pendingTerm !== "" && pendingExists;
  const canRemoveTerm = allowEmpty || terms.length > 1;

  function commitTerms(nextTerms: string[]) {
    onCommit(joinRuleTerms(splitTerms(nextTerms.join("\n"))));
  }

  function addInputTerm() {
    if (!pendingTerm || pendingExists) return;
    commitTerms([...terms, pendingTerm]);
    setInput("");
  }

  function removeTerm(term: string) {
    if (!canRemoveTerm) return;
    commitTerms(terms.filter((item) => item !== term));
  }

  return (
    <div className="admin-tag-rule-keyword-editor">
      <div className="admin-tag-rule-keyword-list" aria-label={listLabel}>
        {terms.length > 0 ? (
          terms.map((term) => (
            <span key={term} className="admin-tag-rule-keyword-pill">
              <span>{term}</span>
              <button
                type="button"
                onClick={() => removeTerm(term)}
                disabled={disabled || !canRemoveTerm}
                aria-label={`${removeLabelPrefix} ${term}`}
              >
                移除
              </button>
            </span>
          ))
        ) : (
          <span className="admin-tag-rule-keyword-empty">{emptyText}</span>
        )}
      </div>
      <div className="admin-tag-rule-keyword-input-row">
        <input
          id={inputId}
          value={input}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={(e) => {
            if (e.key !== "Enter") return;
            e.preventDefault();
            addInputTerm();
          }}
          disabled={disabled}
          aria-describedby={showDuplicateWarning ? warningId : undefined}
          aria-label={inputLabel}
          placeholder={inputLabel}
        />
        <button
          type="button"
          className="admin-btn"
          onClick={addInputTerm}
          disabled={disabled || !pendingTerm || pendingExists}
        >
          添加
        </button>
      </div>
      {showDuplicateWarning && (
        <span className="admin-tag-rule-keyword-warning" id={warningId}>
          {duplicateText}
        </span>
      )}
    </div>
  );
}

type RuleDraft = {
  keywords: string;
  avCodePrefixes: string;
};

function useTagsPageSize() {
  const [pageSize, setPageSize] = useState(() =>
    window.matchMedia(TAGS_MOBILE_QUERY).matches
      ? MOBILE_TAGS_PAGE_SIZE
      : DESKTOP_TAGS_PAGE_SIZE
  );

  useEffect(() => {
    const media = window.matchMedia(TAGS_MOBILE_QUERY);
    const update = () => {
      setPageSize(media.matches ? MOBILE_TAGS_PAGE_SIZE : DESKTOP_TAGS_PAGE_SIZE);
    };
    update();
    media.addEventListener("change", update);
    return () => media.removeEventListener("change", update);
  }, []);

  return pageSize;
}

function tagRuleDraft(tag: api.AdminTag): RuleDraft {
  const rules = tag.matchRules ?? {};
  return {
    keywords: joinRuleTerms(rules.keywords),
    avCodePrefixes: joinRuleTerms(rules.avCodePrefixes),
  };
}

function matchRulesFromDraft(draft: RuleDraft, isAV: boolean): api.TagMatchRules {
  if (isAV) {
    return {
      matchAvCode: true,
      avCodePrefixes: splitPrefixTerms(draft.avCodePrefixes),
    };
  }
  return {
    keywords: splitRuleTerms(draft.keywords),
  };
}

function hasRuleTerms(rules: api.TagMatchRules): boolean {
  return [
    ...(rules.keywords ?? []),
    ...(rules.avCodePrefixes ?? []),
  ].length > 0;
}

function joinRuleTerms(terms?: string[]): string {
  return (terms ?? []).join("\n");
}

function splitRuleTerms(value: string): string[] {
  return uniqueTerms(value.split(/[\n,，、;；]+/));
}

function singleRuleTerm(value: string, splitTerms: (value: string) => string[] = splitRuleTerms): string {
  return splitTerms(value)[0] ?? "";
}

function splitPrefixTerms(value: string): string[] {
  return uniqueTerms(value.split(/[\s,，、;；]+/).map((term) => term.toUpperCase()));
}

function uniqueTerms(terms: string[]): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const term of terms) {
    const clean = term.trim();
    const key = clean.toLowerCase();
    if (!clean || seen.has(key)) continue;
    seen.add(key);
    out.push(clean);
  }
  return out;
}

function isAVTag(tag: api.AdminTag): boolean {
  return tag.label.trim().toUpperCase() === "AV";
}

function sourceLabel(source: string): string {
  if (source === "crawler" || source === "generated") return "自动生成";
  if (source === "user") return "自定义";
  return source || "未知";
}

function tagCardSourceLabel(tag: api.AdminTag): string {
  if (tag.crawlerOwned || tag.source === "crawler") return "爬虫脚本";
  return sourceLabel(tag.source);
}

function tagCardSourceKey(tag: api.AdminTag): string {
  if (tag.crawlerOwned || tag.source === "crawler") return "crawler";
  return tag.source || "";
}

function tagDisplayGroupKey(tag: api.AdminTag): string {
  if (tag.source === "user") return tag.source;
  if (tag.crawlerOwned || tag.source === "crawler") return "crawler";
  return tag.source || "";
}

function tagDisplayGroupRank(tag: api.AdminTag): number {
  return TAG_DISPLAY_GROUP_ORDER[tagDisplayGroupKey(tag)] ?? 99;
}

function tagSourceKey(tag: api.AdminTag): string {
  return tag.crawlerOwned || tag.source === "generated" ? "generated" : tag.source;
}

function isSupportedTag(tag: api.AdminTag): boolean {
  return tag.source === "user" || tag.source === "generated" || tag.crawlerOwned === true;
}
