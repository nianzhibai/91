import { useCallback, useEffect, useRef, useState } from "react";
import {
  useLocation,
  useNavigate,
  useNavigationType,
  useParams,
} from "react-router";
import { AppShell } from "@/components/AppShell";
import { VideoDetailLoading } from "@/components/VideoDetailLoading";
import { VideoPlayer } from "@/components/VideoPlayer";
import { VideoActions } from "@/components/VideoActions";
import { VideoMetaHeader } from "@/components/VideoMetaHeader";
import { VideoInfoPanel } from "@/components/VideoInfoPanel";
import { MobileVideoCollection } from "@/components/MobileVideoCollection";
import { RecommendedRail } from "@/components/RecommendedRail";
import {
  consumePrefetchedVideoDetail,
  deleteVideo,
  fetchTags,
  fetchVideoDetail,
  fetchVideoSubtitles,
  recordView,
  updateVideoTags,
} from "@/data/videos";
import { useAuth } from "@/admin/AuthContext";
import { useDocumentScrollLock } from "@/lib/useDocumentScrollLock";
import { resolveVideoReturnPath } from "@/lib/videoReturnPath";
import type { TagItem, VideoDetail } from "@/types";
import type { VideoReactionCounts } from "@/lib/videoReaction";

const DETAIL_CACHE_LIMIT = 20;
const RELATED_CACHE_LIMIT = 80;

type VideoDetailSnapshot = {
  detail: VideoDetail;
  tags: TagItem[];
};

const cachedVideoDetailsByID = new Map<string, VideoDetailSnapshot>();
const cachedRelatedVideosByID = new Map<string, VideoDetail["relatedVideos"]>();

function readCachedVideoDetail(id: string): VideoDetailSnapshot | null {
  return cachedVideoDetailsByID.get(id) ?? null;
}

function rememberVideoDetail(snapshot: VideoDetailSnapshot) {
  const id = snapshot.detail.id;
  cachedVideoDetailsByID.delete(id);
  cachedVideoDetailsByID.set(id, snapshot);

  if (cachedVideoDetailsByID.size > DETAIL_CACHE_LIMIT) {
    const oldestID = cachedVideoDetailsByID.keys().next().value;
    if (oldestID) cachedVideoDetailsByID.delete(oldestID);
  }
}

function forgetVideoDetail(id: string) {
  cachedVideoDetailsByID.delete(id);
  cachedRelatedVideosByID.delete(id);
}

function rememberRelatedVideos(id: string, videos: VideoDetail["relatedVideos"]) {
  if (cachedRelatedVideosByID.has(id)) return;
  if (cachedRelatedVideosByID.size >= RELATED_CACHE_LIMIT) {
    const oldestID = cachedRelatedVideosByID.keys().next().value;
    if (oldestID) cachedRelatedVideosByID.delete(oldestID);
  }
  cachedRelatedVideosByID.set(id, videos);
}

function withStableRelatedVideos(detail: VideoDetail | null): VideoDetail | null {
  if (!detail) return null;
  const cached = cachedRelatedVideosByID.get(detail.id);
  if (cached) return { ...detail, relatedVideos: cached };
  rememberRelatedVideos(detail.id, detail.relatedVideos ?? []);
  return detail;
}

export default function VideoDetailPage() {
  const { id } = useParams<{ id: string }>();

  // 参数变化时明确卸载上一台播放器；JSON 快照由下面的轻量缓存恢复。
  return <VideoDetailContent key={id ?? "missing"} id={id} />;
}

function VideoDetailContent({ id }: { id?: string }) {
  const navigate = useNavigate();
  const location = useLocation();
  const navigationType = useNavigationType();
  const { isAdmin } = useAuth();
  const locationState = location.state as { from?: unknown } | null;
  const [initialSnapshot] = useState<VideoDetailSnapshot | null>(() =>
    id ? readCachedVideoDetail(id) : null
  );
  const [detail, setDetail] = useState<VideoDetail | null>(
    initialSnapshot?.detail ?? null
  );
  const [tags, setTags] = useState<TagItem[]>(initialSnapshot?.tags ?? []);
  const [loading, setLoading] = useState(initialSnapshot === null);
  const [tagSaving, setTagSaving] = useState(false);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [deleteSource, setDeleteSource] = useState(false);
  const [deleteSaving, setDeleteSaving] = useState(false);
  const [deleteError, setDeleteError] = useState("");
  const reactionCountsRef = useRef<
    (VideoReactionCounts & { videoId: string }) | null
  >(null);

  useDocumentScrollLock(deleteOpen && isAdmin);

  useEffect(() => {
    if (!id) {
      setLoading(false);
      document.title = "视频不存在";
      return;
    }
    let active = true;
    if (navigationType !== "POP") {
      window.scrollTo({ top: 0, behavior: "auto" });
    }
    if (initialSnapshot) {
      // effect 中更新最近使用顺序，保持 React 的 state initializer 无副作用。
      rememberVideoDetail(initialSnapshot);
      document.title = initialSnapshot.detail.title;
    }

    // 命中快照时保留当前画面，在后台静默校验最新详情和标签。
    // 字幕只在用户打开播放器的字幕菜单后请求。
    const prefetchedDetail = consumePrefetchedVideoDetail(id);
    const detailRequest = prefetchedDetail
      ? prefetchedDetail.then((value) => value ?? fetchVideoDetail(id))
      : fetchVideoDetail(id);

    Promise.all([detailRequest, fetchTags()]).then(
      ([d, tagList]) => {
        if (!active) return;
        let stableDetail = withStableRelatedVideos(d);
        const localReactionCounts = reactionCountsRef.current;
        if (
          stableDetail &&
          localReactionCounts?.videoId === stableDetail.id
        ) {
          stableDetail = {
            ...stableDetail,
            likes: localReactionCounts.likes,
            dislikes: localReactionCounts.dislikes,
          };
        }

        // 请求短暂失败时 fetchVideoDetail 会返回 null；已有快照比错误空态更有用。
        if (!stableDetail && initialSnapshot) {
          setLoading(false);
          return;
        }

        if (stableDetail) {
          rememberVideoDetail({
            detail: stableDetail,
            tags: tagList,
          });
        }
        setDetail(stableDetail);
        setTags(tagList);
        setLoading(false);
        document.title = stableDetail ? stableDetail.title : "视频不存在";
      }
    );
    return () => {
      active = false;
    };
  }, [id, initialSnapshot, navigationType]);

  async function handleTagsChange(nextTags: string[]) {
    if (!detail) return;
    setTagSaving(true);
    try {
      const updated = await updateVideoTags(detail.id, nextTags);
      const nextDetail = { ...detail, tags: updated.tags ?? [] };
      setDetail(nextDetail);
      rememberVideoDetail({ detail: nextDetail, tags });
    } finally {
      setTagSaving(false);
    }
  }

  function handleOpenDelete() {
    if (!isAdmin || !detail || deleteSaving) return;
    setDeleteSource(false);
    setDeleteError("");
    setDeleteOpen(true);
  }

  function handleCloseDelete() {
    if (deleteSaving) return;
    setDeleteOpen(false);
    setDeleteError("");
  }

  async function handleConfirmDelete() {
    if (!detail || deleteSaving) return;
    setDeleteSaving(true);
    setDeleteError("");
    try {
      await deleteVideo(detail.id, { deleteSource });
      forgetVideoDetail(detail.id);
      const from = typeof locationState?.from === "string" ? locationState.from : null;
      navigate(resolveVideoReturnPath(from), { replace: true });
    } catch {
      setDeleteError(
        deleteSource
          ? "删除失败，源文件未能删除，请检查WebDAV是否有删除权限"
          : "删除失败，请稍后重试。"
      );
      setDeleteSaving(false);
    }
  }

  function handleFirstPlay() {
    if (!detail) return;
    // 失败静默忽略，不打扰用户播放体验
    recordView(detail.id).catch(() => undefined);
  }

  const handleReactionCountsChange = useCallback(
    (counts: VideoReactionCounts) => {
      if (id) {
        reactionCountsRef.current = { videoId: id, ...counts };
      }
      setDetail((current) => {
        if (!current) return current;
        const nextDetail = {
          ...current,
          likes: counts.likes,
          dislikes: counts.dislikes,
        };
        rememberVideoDetail({ detail: nextDetail, tags });
        return nextDetail;
      });
    },
    [tags]
  );

  const loadSubtitles = useCallback(() => {
    if (!id) return Promise.resolve([]);
    return fetchVideoSubtitles(id);
  }, [id]);

  if (loading) {
    return <VideoDetailLoading isAdmin={isAdmin} />;
  }

  if (!detail) {
    return (
      <AppShell mobileAutoHideNav>
        <div className="vd-page">
          <div className="container vd-page__inner">
            <div className="vd-empty">视频不存在或已被移除</div>
          </div>
        </div>
      </AppShell>
    );
  }

  return (
    <AppShell mobileAutoHideNav>
      <div className="vd-page">
        {/* Ambient 背景层：用海报作模糊底色，叠加渐变过渡到页面背景 */}
        <div
          className="vd-ambient"
          aria-hidden="true"
          style={{
            backgroundImage: detail.poster
              ? `url(${detail.poster})`
              : undefined,
          }}
        />

        <div className="container vd-page__inner">
          <div className="vd-layout">
            <div className="vd-main">
              <div className="vd-player-wrap">
                <div className="vd-player">
                  <VideoPlayer
                    id={detail.id}
                    src={detail.videoSrc}
                    preferTypedMp4SourceOnIOS={isPikPakMp4Detail(detail)}
                    poster={detail.poster}
                    previewSrc={detail.previewSrc}
                    loadSubtitles={loadSubtitles}
                    title={detail.title}
                    onFirstPlay={handleFirstPlay}
                  />
                </div>
              </div>

              <div className="vd-detail-panels">
                <section className="vd-summary" aria-label="当前视频">
                  <VideoMetaHeader video={detail} />

                  <VideoActions
                    video={detail}
                    onDeleteVideo={handleOpenDelete}
                    deleteSaving={deleteSaving}
                    canDelete={isAdmin}
                    onReactionCountsChange={handleReactionCountsChange}
                  />
                </section>

                {detail.collection && detail.collection.total > 1 && (
                  <MobileVideoCollection
                    videoId={detail.id}
                    collection={detail.collection}
                  />
                )}

                <VideoInfoPanel
                  video={detail}
                  availableTags={tags}
                  tagSaving={tagSaving}
                  onTagsChange={isAdmin ? handleTagsChange : undefined}
                />
              </div>
            </div>

            <RecommendedRail
              videos={detail.relatedVideos}
              videoId={detail.id}
              collection={detail.collection}
            />
          </div>
        </div>
      </div>

      {deleteOpen && isAdmin && (
        <div className="vd-delete-modal" role="presentation">
          <div
            className="vd-delete-dialog"
            role="dialog"
            aria-modal="true"
            aria-labelledby="vd-delete-title"
          >
            <div className="vd-delete-head">
              <h2 id="vd-delete-title" className="vd-delete-title">
                删除视频
              </h2>
              <p className="vd-delete-text">
                确定删除「{detail.title}」吗？
              </p>
            </div>

            <label className="vd-delete-option">
              <input
                type="checkbox"
                checked={deleteSource}
                disabled={deleteSaving}
                onChange={(e) => setDeleteSource(e.target.checked)}
              />
              <span>
                <strong>同时删除视频源文件</strong>
              </span>
            </label>

            {deleteError && <div className="vd-delete-error">{deleteError}</div>}

            <div className="vd-delete-actions">
              <button
                type="button"
                className="vd-delete-action vd-delete-cancel"
                onClick={handleCloseDelete}
                disabled={deleteSaving}
              >
                取消
              </button>
              <button
                type="button"
                className="vd-delete-action vd-delete-confirm"
                onClick={handleConfirmDelete}
                disabled={deleteSaving}
              >
                {deleteSaving ? "删除中..." : "删除"}
              </button>
            </div>
          </div>
        </div>
      )}
    </AppShell>
  );
}

function isPikPakMp4Detail(detail: VideoDetail) {
  return (
    detail.mediaType?.toLowerCase() === "video/mp4" &&
    detail.sourceLabel?.toLowerCase().includes("pikpak") === true
  );
}
