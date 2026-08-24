import { CalendarDays, Clock3, Eye } from "lucide-react";
import type { VideoDetail } from "@/types";
import { formatCount } from "@/lib/format";

type Props = {
  video: VideoDetail;
};

/**
 * 详情页标题块。
 *
 * 视觉：
 * - meta：一组小胶囊（来源、时长、观看数、发布时间）
 *   每个胶囊有自己的语义色彩，避免传统 "·" 分隔列表的列表感。
 * - 标题：大、粗、最高两行，位于 meta 下方
 */
export function VideoMetaHeader({ video }: Props) {
  const source = (video.sourceLabel ?? "").trim();
  const duration = (video.duration ?? "").trim();
  const published = (video.publishedAt ?? "").trim();
  const sourceKind = sourceKindFromLabel(source);

  return (
    <header className="vd-header">
      <div className="vd-header__row">
        <ul className="vd-meta" aria-label="视频信息">
          {source && (
            <li className="vd-meta__chip" data-tone={sourceKind || "neutral"}>
              {source}
            </li>
          )}
          {duration && (
            <li className="vd-meta__chip vd-meta__chip--plain">
              <Clock3 size={14} aria-hidden="true" />
              {duration}
            </li>
          )}
          <li className="vd-meta__chip vd-meta__chip--plain">
            <Eye size={14} aria-hidden="true" />
            <strong>{formatCount(video.views)}</strong> 次观看
          </li>
          {published && (
            <li className="vd-meta__chip vd-meta__chip--plain">
              <CalendarDays size={14} aria-hidden="true" />
              {published}
            </li>
          )}
        </ul>
      </div>

      <h1 className="vd-header__title" title={video.title}>
        {video.title}
      </h1>
    </header>
  );
}

// 根据 sourceLabel 识别网盘类型，用于胶囊配色。
function sourceKindFromLabel(label: string): string {
  const value = label.toLowerCase();
  if (value.includes("夸克") || value.includes("quark")) return "quark";
  if (value.includes("115") || value.includes("p115")) return "p115";
  if (value.includes("123") || value.includes("p123")) return "p123";
  if (value.includes("pikpak")) return "pikpak";
  if (value.includes("沃盘") || value.includes("wopan") || value.includes("联通"))
    return "wopan";
  if (value.includes("光鸭") || value.includes("guangyapan") || value.includes("guangya"))
    return "guangyapan";
  if (value.includes("onedrive") || value.includes("one drive")) return "onedrive";
  if (value.includes("webdav") || value.includes("web dav")) return "webdav";
  if (value.includes("本地") || value.includes("localstorage") || value.includes("local storage"))
    return "localstorage";
  return "";
}
