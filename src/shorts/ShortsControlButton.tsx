import { type ButtonHTMLAttributes } from "react";
import { ShortsTooltip } from "./ShortsTooltip";

type Props = ButtonHTMLAttributes<HTMLButtonElement> & {
  tooltip: string;
  shortcut: string;
  tooltipAlign?: "center" | "end";
};

/** 控制栏共用的悬停提示，也支持键盘聚焦和 Escape 关闭。 */
export function ShortsControlButton({ tooltip, shortcut, tooltipAlign = "center", ...props }: Props) {
  return (
    <ShortsTooltip label={tooltip} shortcut={shortcut} align={tooltipAlign} disabled={props.disabled}>
      <button {...props} aria-keyshortcuts={shortcut} />
    </ShortsTooltip>
  );
}
