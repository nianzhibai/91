import {
  cloneElement, useEffect, useId, useRef, useState,
  type AriaAttributes, type CSSProperties, type ReactElement,
} from "react";

type Props = {
  label: string;
  shortcut?: string;
  placement?: "top" | "left";
  align?: "center" | "end";
  enabled?: boolean;
  disabled?: boolean;
  anchorIcon?: boolean;
  children: ReactElement<AriaAttributes>;
};

/** 播放器提示共用悬停、键盘聚焦和 Escape 行为，外观由放置位置决定。 */
export function ShortsTooltip({
  label, shortcut, placement = "top", align = "center", enabled = true,
  disabled = false, anchorIcon = false, children,
}: Props) {
  const tooltipId = useId();
  const [open, setOpen] = useState(false);
  const [anchorTop, setAnchorTop] = useState<number>();
  const hoveredRef = useRef(false);
  const keyboardFocusRef = useRef(false);
  const visible = enabled && !disabled && open;
  const className = placement === "left" ? "shorts-action-tooltip" : "shorts-control-tooltip";

  useEffect(() => {
    setOpen(false);
    hoveredRef.current = false;
    keyboardFocusRef.current = false;
  }, [enabled]);

  function showTooltip(element: HTMLElement) {
    if (!enabled || disabled) return;
    if (anchorIcon) {
      const icon = element.querySelector("button > svg, a > svg");
      if (icon) {
        const iconBounds = icon.getBoundingClientRect();
        setAnchorTop(iconBounds.top + iconBounds.height / 2 - element.getBoundingClientRect().top);
      }
    }
    setOpen(true);
  }

  return (
    <div className={className} data-align={align} data-shorts-no-swipe=""
      onPointerEnter={(event) => {
        if (event.pointerType !== "mouse" || !enabled || disabled) return;
        hoveredRef.current = true;
        showTooltip(event.currentTarget);
      }}
      onPointerLeave={(event) => {
        if (event.pointerType !== "mouse") return;
        hoveredRef.current = false;
        if (!keyboardFocusRef.current) setOpen(false);
      }}
      onFocus={(event) => {
        keyboardFocusRef.current = event.target.matches(":focus-visible");
        if (keyboardFocusRef.current) showTooltip(event.currentTarget);
      }}
      onBlur={() => {
        keyboardFocusRef.current = false;
        if (!hoveredRef.current) setOpen(false);
      }}
      onKeyDown={(event) => {
        if (event.key !== "Escape" || !visible) return;
        event.preventDefault();
        event.stopPropagation();
        setOpen(false);
      }}>
      {cloneElement(children, { "aria-describedby": visible ? tooltipId : undefined })}
      <span id={tooltipId} className={`${className}__content`} role="tooltip" hidden={!visible}
        data-has-shortcut={Boolean(shortcut)}
        style={anchorTop === undefined ? undefined : { "--shorts-tooltip-anchor": `${anchorTop}px` } as CSSProperties}>
        <span>{label}</span>{shortcut && <kbd>{shortcut}</kbd>}
        {placement === "left" && (
          <svg className="shorts-action-tooltip__arrow" width="7" height="24" viewBox="0 0 7 24"
            fill="currentColor" aria-hidden="true" focusable="false">
            <path d="M0 0H1C1 4 2 5.5 4 7.5S7 10 7 12 6 14.5 4 16.5 1 20 1 24H0Z" />
          </svg>
        )}
      </span>
    </div>
  );
}
