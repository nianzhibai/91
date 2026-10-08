import { useEffect, useMemo, useRef, useState } from "react";
import { Check, Share2, ThumbsDown, ThumbsUp, Trash2 } from "lucide-react";
import { useToast } from "./ToastContext";
import type { VideoDetail } from "@/types";
import { setVideoVisitReaction } from "@/data/videos";
import { formatCount } from "@/lib/format";
import {
  applyVideoReactionTransition,
  createVideoReactionVisitId,
  nextVideoReaction,
  type SelectableVideoReaction,
  type VideoReaction,
  type VideoReactionCounts,
} from "@/lib/videoReaction";
import {
  copyExistingVideoShareURL,
  createAndCopyVideoShare,
} from "@/lib/videoShareClipboard";

type Props = {
  video: VideoDetail;
  onDeleteVideo: () => void;
  deleteSaving?: boolean;
  canDelete?: boolean;
  onReactionCountsChange?: (counts: VideoReactionCounts) => void;
};

/**
 * 视频操作工具条。
 * - 整体是一张浮起的圆角玻璃卡，比上一版的横线分隔更"成体"。
 * - 每次详情页访问生成一张从 none 开始的匿名临时选票。
 * - 点赞 / 点踩在本次访问内互斥，可取消，也可直接切换。
 * - 删除是唯一的管理操作，hover 时露出 danger 色。
 */
export function VideoActions({
  video,
  onDeleteVideo,
  deleteSaving,
  canDelete = true,
  onReactionCountsChange,
}: Props) {
  const { show } = useToast();
  const [likes, setLikes] = useState(video.likes ?? 0);
  const [dislikes, setDislikes] = useState(video.dislikes ?? 0);
  const [bursting, setBursting] = useState(false);
  const [reaction, setReaction] = useState<VideoReaction>("none");
  const [reactionPending, setReactionPending] = useState(false);
  const [shareState, setShareState] = useState<
    "idle" | "creating" | "copy-ready" | "copied" | "error"
  >("idle");
  const visitId = useMemo(createVideoReactionVisitId, [video.id]);
  const countsRef = useRef<VideoReactionCounts>({
    likes: video.likes ?? 0,
    dislikes: video.dislikes ?? 0,
  });
  const reactionRef = useRef<VideoReaction>("none");
  const reactionPendingRef = useRef(false);
  const reactionTouchedRef = useRef(false);
  const mountedRef = useRef(true);
  const burstResetTimer = useRef<number | null>(null);
  const shareResetTimer = useRef<number | null>(null);
  const pendingShareURL = useRef("");

  useEffect(() => {
    const initialCounts = {
      likes: video.likes ?? 0,
      dislikes: video.dislikes ?? 0,
    };
    countsRef.current = initialCounts;
    reactionRef.current = "none";
    reactionPendingRef.current = false;
    reactionTouchedRef.current = false;
    setLikes(initialCounts.likes);
    setDislikes(initialCounts.dislikes);
    setBursting(false);
    setReaction("none");
    setReactionPending(false);
    setShareState("idle");
    pendingShareURL.current = "";
    if (burstResetTimer.current !== null) {
      window.clearTimeout(burstResetTimer.current);
      burstResetTimer.current = null;
    }
    if (shareResetTimer.current !== null) {
      window.clearTimeout(shareResetTimer.current);
      shareResetTimer.current = null;
    }
  }, [video.id]);

  // 命中详情缓存时，父级会在后台刷新总数。用户尚未操作前跟随服务端；
  // 一旦本次选票开始切换，就由 reaction 接口的权威响应维护数字，避免旧快照
  // 覆盖刚完成的乐观更新。
  useEffect(() => {
    if (reactionTouchedRef.current) return;
    const nextCounts = {
      likes: video.likes ?? 0,
      dislikes: video.dislikes ?? 0,
    };
    countsRef.current = nextCounts;
    setLikes(nextCounts.likes);
    setDislikes(nextCounts.dislikes);
  }, [video.likes, video.dislikes]);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      if (burstResetTimer.current !== null) {
        window.clearTimeout(burstResetTimer.current);
      }
      if (shareResetTimer.current !== null) {
        window.clearTimeout(shareResetTimer.current);
      }
    };
  }, []);

  function applyCounts(next: VideoReactionCounts) {
    countsRef.current = next;
    setLikes(next.likes);
    setDislikes(next.dislikes);
  }

  async function handleReaction(selected: SelectableVideoReaction) {
    if (reactionPendingRef.current) return;

    const previousReaction = reactionRef.current;
    const nextReaction = nextVideoReaction(previousReaction, selected);
    const previousCounts = countsRef.current;
    const optimisticCounts = applyVideoReactionTransition(
      previousCounts,
      previousReaction,
      nextReaction
    );

    reactionPendingRef.current = true;
    reactionTouchedRef.current = true;
    reactionRef.current = nextReaction;
    setReactionPending(true);
    setReaction(nextReaction);
    applyCounts(optimisticCounts);

    if (burstResetTimer.current !== null) {
      window.clearTimeout(burstResetTimer.current);
      burstResetTimer.current = null;
    }
    const shouldBurst = selected === "like" && nextReaction === "like";
    setBursting(shouldBurst);
    if (shouldBurst) {
      burstResetTimer.current = window.setTimeout(() => {
        setBursting(false);
        burstResetTimer.current = null;
      }, 320);
    }

    try {
      const result = await setVideoVisitReaction(
        video.id,
        visitId,
        nextReaction
      );
      if (!mountedRef.current) return;
      reactionRef.current = result.reaction;
      setReaction(result.reaction);
      const confirmedCounts = {
        likes: result.likes,
        dislikes: result.dislikes,
      };
      applyCounts(confirmedCounts);
      onReactionCountsChange?.(confirmedCounts);
    } catch {
      if (!mountedRef.current) return;
      reactionRef.current = previousReaction;
      setReaction(previousReaction);
      applyCounts(previousCounts);
      setBursting(false);
      if (burstResetTimer.current !== null) {
        window.clearTimeout(burstResetTimer.current);
        burstResetTimer.current = null;
      }
      show("操作失败，请稍后重试", "error");
    } finally {
      reactionPendingRef.current = false;
      if (mountedRef.current) {
        setReactionPending(false);
      }
    }
  }

  async function handleShare() {
    if (shareState === "creating") return;
    if (shareResetTimer.current !== null) {
      window.clearTimeout(shareResetTimer.current);
      shareResetTimer.current = null;
    }
    setShareState("creating");
    try {
      if (pendingShareURL.current) {
        await copyExistingVideoShareURL(pendingShareURL.current);
      } else {
        const result = await createAndCopyVideoShare(video.id);
        if (!mountedRef.current) return;
        if (!result.copied) {
          pendingShareURL.current = result.url;
          setShareState("copy-ready");
          show("请再次点击分享按钮", "info");
          scheduleShareStateReset(2500);
          return;
        }
      }
      if (!mountedRef.current) return;
      pendingShareURL.current = "";
      setShareState("copied");
      show("已复制一次性分享链接", "success");
      scheduleShareStateReset(1500);
    } catch {
      if (!mountedRef.current) return;
      setShareState("error");
      show(pendingShareURL.current ? "复制失败，请重试" : "分享失败，请重试", "error");
    }
  }

  function scheduleShareStateReset(delay: number) {
    if (shareResetTimer.current !== null) {
      window.clearTimeout(shareResetTimer.current);
    }
    shareResetTimer.current = window.setTimeout(() => {
      setShareState("idle");
      shareResetTimer.current = null;
    }, delay);
  }

  return (
    <div className="vd-actions" role="toolbar" aria-label="视频操作">
      <div
        className="vd-actions__group"
        role="group"
        aria-label="点赞和点踩"
        aria-busy={reactionPending}
      >
        <button
          type="button"
          className={`vd-actions__pill vd-actions__like${
            reaction === "like" ? " is-active" : ""
          }${bursting ? " is-bursting" : ""}`}
          onClick={() => handleReaction("like")}
          disabled={reactionPending}
          aria-pressed={reaction === "like"}
          aria-label={reaction === "like" ? "取消点赞" : "点赞"}
        >
          <ThumbsUp
            size={18}
            fill={reaction === "like" ? "currentColor" : "none"}
          />
          <span className="vd-actions__count">{formatCount(likes)}</span>
        </button>
        <button
          type="button"
          className={`vd-actions__pill vd-actions__dislike${
            reaction === "dislike" ? " is-active" : ""
          }`}
          onClick={() => handleReaction("dislike")}
          disabled={reactionPending}
          aria-pressed={reaction === "dislike"}
          aria-label={reaction === "dislike" ? "取消点踩" : "点踩"}
        >
          <ThumbsDown
            size={18}
            fill={reaction === "dislike" ? "currentColor" : "none"}
          />
          <span className="vd-actions__count">{formatCount(dislikes)}</span>
        </button>
      </div>

      <button
        type="button"
        className={`vd-actions__btn vd-actions__share${
          shareState === "copied" ? " is-success" : ""
        }`}
        onClick={handleShare}
        disabled={shareState === "creating"}
        aria-label={
          pendingShareURL.current
            ? "复制已生成的一次性分享链接"
            : "生成并复制一次性分享链接"
        }
      >
        {shareState === "copied" ? <Check size={16} /> : <Share2 size={16} />}
        <span>
          {shareState === "creating"
            ? "生成中"
            : shareState === "copied"
              ? "链接已复制"
              : shareState === "copy-ready"
                ? "再次点击复制"
                : shareState === "error"
                  ? pendingShareURL.current
                    ? "复制失败，重试"
                    : "分享失败，重试"
                  : "分享"}
        </span>
      </button>

      {canDelete && (
        <button
          type="button"
          className="vd-actions__btn vd-actions__delete"
          onClick={onDeleteVideo}
          disabled={deleteSaving}
          aria-label="删除这个视频"
        >
          <Trash2 size={16} />
          <span>{deleteSaving ? "删除中" : "删除"}</span>
        </button>
      )}
    </div>
  );
}
