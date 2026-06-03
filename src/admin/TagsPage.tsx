import { useEffect, useMemo, useState } from "react";
import { CheckSquare, Film, Plus, RefreshCw, Search, Tags, Trash2 } from "lucide-react";
import * as api from "./api";
import { useToast } from "./ToastContext";
import { ConfirmModal } from "./ConfirmModal";

const DESKTOP_TAGS_PAGE_SIZE = 25;
const MOBILE_TAGS_PAGE_SIZE = 8;
const TAGS_MOBILE_QUERY = "(max-width: 640px)";

type DeleteConfirmState =
  | { kind: "single"; tag: api.AdminTag }
  | { kind: "bulk"; ids: number[] }
  | null;

export function TagsPage() {
  const [tags, setTags] = useState<api.AdminTag[]>([]);
  const [label, setLabel] = useState("");
  const [aliases, setAliases] = useState("");
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [saving, setSaving] = useState(false);
  const [deletingId, setDeletingId] = useState<number | null>(null);
  const [togglingFrontendTagId, setTogglingFrontendTagId] = useState<number | null>(null);
  const [deleteConfirm, setDeleteConfirm] = useState<DeleteConfirmState>(null);
  const [searchQuery, setSearchQuery] = useState("");
  const [filterSource, setFilterSource] = useState<string>("all");
  const [selectMode, setSelectMode] = useState(false);
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const pageSize = useTagsPageSize();
  const [page, setPage] = useState(1);
  const { show } = useToast();

  async function refresh() {
    setLoading(true);
    setLoadError("");
    try {
      setTags(await api.listTags());
    } catch (e) {
      const message = e instanceof Error ? e.message : "加载标签失败";
      setLoadError(message);
      show(message, "error");
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    refresh();
  }, []);

  async function handleCreate() {
    const cleanLabel = label.trim();
    if (!cleanLabel) return;
    setSaving(true);
    try {
      const r = await api.createTag(cleanLabel, splitList(aliases));
      show(`已添加标签，自动归类 ${r.classified} 个视频`, "success");
      setLabel("");
      setAliases("");
      await refresh();
    } catch (e) {
      show(e instanceof Error ? e.message : "添加标签失败", "error");
    } finally {
      setSaving(false);
    }
  }

  async function handleToggleFrontendAccess(tag: api.AdminTag) {
    setTogglingFrontendTagId(tag.id);
    try {
      await api.setTagFrontendAccess(tag.id, !tag.frontendAccess);
      show(tag.frontendAccess ? "已取消前台访问标签" : "已设为前台可访问标签", "success");
      await refresh();
    } catch (e) {
      show(e instanceof Error ? e.message : "切换前台访问失败", "error");
    } finally {
      setTogglingFrontendTagId(null);
    }
  }

  function handleDelete(tag: api.AdminTag) {
    if (tag.source === "system") return;
    setDeleteConfirm({ kind: "single", tag });
  }

  function toggleSelectMode() {
    setSelectMode((m) => !m);
    setSelected(new Set());
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
      const results = await Promise.allSettled(
        ids.map((id) => api.deleteTag(id))
      );
      const ok = results.filter((r) => r.status === "fulfilled").length;
      const failed = ids.length - ok;
      show(failed ? `已删除 ${ok} 个，${failed} 个失败` : `已删除 ${ok} 个标签`, failed ? "error" : "success");
      setSelected(new Set());
      setSelectMode(false);
      setDeleteConfirm(null);
      await refresh();
    } finally {
      setBulkDeleting(false);
    }
  }

  const stats = useMemo(() => {
    let totalVideos = 0;
    let systemCount = 0;
    let userCount = 0;
    let collectionCount = 0;
    let frontendAccessCount = 0;

    tags.forEach((t) => {
      totalVideos += t.count ?? 0;
      if (t.frontendAccess) frontendAccessCount++;
      if (t.source === "system") systemCount++;
      else if (t.source === "user") userCount++;
      else if (t.source === "collection") collectionCount++;
      else if (t.source === "legacy") legacyCount++;
    });

    return {
      totalTags: tags.length,
      totalVideos,
      systemCount,
      userCount,
      collectionCount,
      legacyCount,
      frontendAccessCount,
    };
  }, [tags]);

  const filteredTags = useMemo(() => {
    return tags.filter((t) => {
      const query = searchQuery.trim().toLowerCase();
      const matchesSearch =
        !query ||
        t.label.toLowerCase().includes(query) ||
        (t.aliases ?? []).some((a) => a.toLowerCase().includes(query));
      const matchesSource = filterSource === "all" || t.source === filterSource;
      return matchesSearch && matchesSource;
    });
  }, [tags, searchQuery, filterSource]);

  const totalPages = Math.max(1, Math.ceil(filteredTags.length / pageSize));
  const currentPage = Math.min(page, totalPages);
  const pageStartIndex = (currentPage - 1) * pageSize;
  const pageEndIndex = pageStartIndex + pageSize;
  const pagedTags = useMemo(
    () => filteredTags.slice(pageStartIndex, pageEndIndex),
    [filteredTags, pageStartIndex, pageEndIndex]
  );
  const pageStart = filteredTags.length === 0 ? 0 : pageStartIndex + 1;
  const pageEnd = Math.min(filteredTags.length, pageEndIndex);

  useEffect(() => {
    setPage(1);
  }, [searchQuery, filterSource, pageSize]);

  useEffect(() => {
    setPage((p) => Math.min(Math.max(1, p), totalPages));
  }, [totalPages]);

  const deletablePageTags = useMemo(
    () => pagedTags.filter((t) => t.source !== "system"),
    [pagedTags]
  );
  const allSelected =
    deletablePageTags.length > 0 && deletablePageTags.every((t) => selected.has(t.id));

  function toggleSelectAll() {
    setSelected((prev) => {
      const next = new Set(prev);
      if (allSelected) deletablePageTags.forEach((t) => next.delete(t.id));
      else deletablePageTags.forEach((t) => next.add(t.id));
      return next;
    });
  }

  return (
    <section>
      <header className="admin-page__header">
        <h1 className="admin-page__title">标签管理</h1>
        <button type="button" className="admin-btn" onClick={refresh}>
          <RefreshCw size={13} /> 刷新
        </button>
      </header>

      <div className="admin-tags-layout">
        {/* 左栏：创建与统计 */}
        <div>
          <div className="admin-card">
            <div className="admin-card__title">
              <Plus size={15} /> 新增分类标签
            </div>
            <form
              className="admin-form"
              onSubmit={(e) => {
                e.preventDefault();
                handleCreate();
              }}
            >
              <div className="admin-form__row">
                <label htmlFor="admin-tag-label">标签名</label>
                <input
                  id="admin-tag-label"
                  value={label}
                  onChange={(e) => setLabel(e.target.value)}
                  placeholder="例如：清纯"
                />
              </div>
              <div className="admin-form__row">
                <label htmlFor="admin-tag-aliases">别名</label>
                <input
                  id="admin-tag-aliases"
                  value={aliases}
                  onChange={(e) => setAliases(e.target.value)}
                  placeholder="逗号分隔，例如：纯欲, 清新"
                />
                <div className="admin-form__help">
                  新增后会按别名和标签名匹配已有视频的标题、作者和目录并自动归类。
                </div>
              </div>
              <button
                type="submit"
                className="admin-btn is-primary"
                disabled={saving || !label.trim()}
              >
                <Plus size={13} /> {saving ? "添加中..." : "添加并自动归类"}
              </button>
            </form>
          </div>

          <div className="admin-card">
            <div className="admin-card__title">
              <Tags size={15} /> 标签总览
            </div>
            <div className="admin-tag-stats-list">
              <div className="admin-tag-stat-item">
                <span>总标签数</span>
                <strong>{stats.totalTags}</strong>
              </div>
              <div className="admin-tag-stat-item">
                <span>关联视频次</span>
                <strong>{stats.totalVideos}</strong>
              </div>
              <div className="admin-tag-stat-item">
                <span>前台可访问</span>
                <strong>{stats.frontendAccessCount}</strong>
              </div>
            </div>
          </div>
        </div>

        {/* 右栏：看板网格与搜索栏 */}
        <div>
          <div className="admin-tags-toolbar">
            <div className="admin-tags-search">
              <Search className="admin-tags-search__icon" size={14} />
              <input
                aria-label="搜索标签名或别名"
                type="text"
                value={searchQuery}
                onChange={(e) => setSearchQuery(e.target.value)}
                placeholder="搜索标签名或别名..."
              />
            </div>

            <div className="admin-tags-filter-tabs">
              <button
                type="button"
                className={`admin-tags-filter-tab ${filterSource === "all" ? "is-active" : ""}`}
                onClick={() => setFilterSource("all")}
              >
                全部 ({tags.length})
              </button>
              <button
                type="button"
                className={`admin-tags-filter-tab ${filterSource === "system" ? "is-active" : ""}`}
                onClick={() => setFilterSource("system")}
              >
                系统 ({stats.systemCount})
              </button>
              <button
                type="button"
                className={`admin-tags-filter-tab ${filterSource === "user" ? "is-active" : ""}`}
                onClick={() => setFilterSource("user")}
              >
                用户 ({stats.userCount})
              </button>
              <button
                type="button"
                className={`admin-tags-filter-tab ${filterSource === "collection" ? "is-active" : ""}`}
                onClick={() => setFilterSource("collection")}
              >
                合集 ({stats.collectionCount})
              </button>
              {stats.legacyCount > 0 && (
                <button
                  type="button"
                  className={`admin-tags-filter-tab ${filterSource === "legacy" ? "is-active" : ""}`}
                  onClick={() => setFilterSource("legacy")}
                >
                  旧数据 ({stats.legacyCount})
                </button>
              )}
            </div>

            <button
              type="button"
              className={`admin-btn ${selectMode ? "is-primary" : ""}`}
              onClick={toggleSelectMode}
            >
              <CheckSquare size={13} /> {selectMode ? "退出批量" : "批量删除"}
            </button>
          </div>

          {selectMode && (
            <div className="admin-tags-bulkbar">
              <label className="admin-check">
                <input type="checkbox" checked={allSelected} onChange={toggleSelectAll} />
                <span>全选本页 ({deletablePageTags.length})</span>
              </label>
              <span className="admin-tags-bulkbar__count">已选 {selected.size} 个</span>
              <button
                type="button"
                className="admin-btn"
                onClick={() => setSelected(new Set())}
                disabled={selected.size === 0}
              >
                清空
              </button>
              <button
                type="button"
                className="admin-btn is-danger"
                onClick={handleBulkDelete}
                disabled={selected.size === 0 || bulkDeleting}
              >
                <Trash2 size={13} /> {bulkDeleting ? "删除中..." : `删除选中 (${selected.size})`}
              </button>
            </div>
          )}

          {loading ? (
            <div className="admin-loading-state">
              <RefreshCw size={20} className="admin-spin" />
              <span>加载中...</span>
            </div>
          ) : loadError ? (
            <div className="admin-error-state">
              <strong>标签加载失败</strong>
              <span>{loadError}</span>
              <button type="button" className="admin-btn" onClick={refresh}>
                <RefreshCw size={13} /> 重试
              </button>
            </div>
          ) : filteredTags.length === 0 ? (
            <div className="admin-card admin-empty">
              没有找到匹配的标签。
            </div>
          ) : (
            <>
              <div className="admin-tags-grid">
                {pagedTags.map((tag) => {
                  const selectable = selectMode && tag.source !== "system";
                  const isSelected = selected.has(tag.id);
                  const cardClass = `admin-tag-card${selectable ? " is-selectable" : ""}${
                    selectable && isSelected ? " is-selected" : ""
                  }`;
                  const cardContent = (
                    <>
                      <div className="admin-tag-card__head">
                        {selectable && (
                          <input
                            type="checkbox"
                            className="admin-tag-card__check"
                            checked={isSelected}
                            onChange={() => toggleSelect(tag.id)}
                          />
                        )}
                        <span className="admin-tag-card__title">{tag.label}</span>
                        <span className="admin-tag-card__source-badge" data-source={tag.source}>
                          {sourceLabel(tag.source)}
                        </span>
                      </div>

                      {tag.aliases && tag.aliases.length > 0 && (
                        <div className="admin-tag-card__aliases">
                          {tag.aliases.map((alias) => (
                            <span key={alias} className="admin-tag-card__alias-pill">
                              {alias}
                            </span>
                          ))}
                        </div>
                      )}

	                      <div className="admin-tag-card__footer">
                        <span className="admin-tag-card__count">
                          <Film size={13} />
                          <strong>{tag.count}</strong> 视频
                        </span>
                        <div className="admin-tag-card__footer-actions">
                          <span className="admin-tag-card__id">#{tag.id}</span>
                          {!selectMode && (
                            <button
                              type="button"
                              className={`admin-btn ${tag.frontendAccess ? "is-primary" : ""}`}
                              onClick={() => handleToggleFrontendAccess(tag)}
                              disabled={togglingFrontendTagId === tag.id}
                              aria-label={`${tag.frontendAccess ? "取消" : "允许"}标签 ${tag.label} 前台访问`}
                            >
                              {togglingFrontendTagId === tag.id
                                ? "切换中"
                                : tag.frontendAccess
                                  ? "前台访问"
                                  : "设为前台"}
                            </button>
                          )}
                          {!selectMode && tag.source !== "system" && (
                            <button
                              type="button"
                              className="admin-tag-card__delete"
                              onClick={() => handleDelete(tag)}
                              disabled={deletingId === tag.id}
                              aria-label={`删除标签 ${tag.label}`}
                            >
                              <Trash2 size={11} />
                              <span>{deletingId === tag.id ? "删除中" : "删除"}</span>
                            </button>
                          )}
	                        </div>
	                      </div>
	                    </>
	                  );
                  return selectable ? (
                    <label key={tag.id} className={cardClass}>
                      {cardContent}
                    </label>
                  ) : (
                    <div key={tag.id} className={cardClass}>
                      {cardContent}
                    </div>
                  );
                })}
              </div>

              {totalPages > 1 && (
                <div className="admin-table-pagination admin-tags-pagination">
                  <button
                    type="button"
                    className="admin-btn"
                    onClick={() => setPage(1)}
                    disabled={currentPage <= 1}
                  >
                    首页
                  </button>
                  <button
                    type="button"
                    className="admin-btn"
                    onClick={() => setPage((p) => Math.max(1, p - 1))}
                    disabled={currentPage <= 1}
                  >
                    上一页
                  </button>
                  <span className="admin-table-pagination__info">
                    第 {currentPage} / {totalPages} 页，显示 {pageStart}-{pageEnd} / {filteredTags.length}，每页 {pageSize} 个
                  </span>
                  <button
                    type="button"
                    className="admin-btn"
                    onClick={() => setPage((p) => Math.min(totalPages, p + 1))}
                    disabled={currentPage >= totalPages}
                  >
                    下一页
                  </button>
                  <button
                    type="button"
                    className="admin-btn"
                    onClick={() => setPage(totalPages)}
                    disabled={currentPage >= totalPages}
                  >
                    末页
                  </button>
                </div>
              )}
            </>
          )}
        </div>
      </div>
      <ConfirmModal
        open={!!deleteConfirm}
        title={deleteConfirm?.kind === "bulk" ? "删除选中标签" : "删除标签"}
        message={
          deleteConfirm?.kind === "bulk"
            ? `确定删除选中的 ${deleteConfirm.ids.length} 个标签吗？`
            : `确定删除标签「${deleteConfirm?.tag.label ?? ""}」吗？`
        }
        details={["此操作会从所有视频上移除相关标签。"]}
        confirmText="确认删除"
        danger
        loading={deletingId !== null || bulkDeleting}
        onCancel={() => {
          if (deletingId === null && !bulkDeleting) setDeleteConfirm(null);
        }}
        onConfirm={confirmDelete}
      />
    </section>
  );
}

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

function splitList(s: string): string[] {
  return s
    .split(/[,，、\s]+/)
    .map((x) => x.trim())
    .filter(Boolean);
}

function sourceLabel(source: string): string {
  if (source === "system") return "系统";
  if (source === "collection") return "合集";
  if (source === "legacy") return "旧数据";
  return "用户";
}
