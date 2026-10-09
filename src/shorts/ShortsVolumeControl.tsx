import { useRef, useState, type CSSProperties } from "react";
import { Volume1, Volume2, VolumeX } from "lucide-react";

type Props = {
  muted: boolean;
  volume: number;
  onToggleMute: () => void;
  onVolumeChange: (volume: number) => void;
};

export function ShortsVolumeControl({ muted, volume, onToggleMute, onVolumeChange }: Props) {
  const [open, setOpen] = useState(false);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const draggingRef = useRef(false);
  const keyboardRef = useRef(false);
  const percent = muted ? 0 : Math.round(volume * 100);
  const Icon = percent === 0 ? VolumeX : percent <= 50 ? Volume1 : Volume2;

  return (
    <div className="shorts-volume-control"
      onPointerEnter={(event) => {
        if (event.pointerType !== "mouse") return;
        keyboardRef.current = false;
        setOpen(true);
      }}
      onPointerLeave={(event) => {
        if (event.pointerType === "mouse" && !draggingRef.current && !keyboardRef.current) setOpen(false);
      }}
      onFocus={() => setOpen(true)}
      onBlur={(event) => {
        if (!event.currentTarget.contains(event.relatedTarget as Node | null)) setOpen(false);
      }}
      onKeyDown={(event) => {
        keyboardRef.current = true;
        if (event.key === "Escape") {
          event.preventDefault();
          event.stopPropagation();
          triggerRef.current?.focus();
          setOpen(false);
        }
      }}>
      <button ref={triggerRef} type="button"
        aria-label={percent === 0 ? "开启视频声音" : "关闭视频声音"}
        aria-keyshortcuts="M"
        aria-expanded={open} aria-controls="shorts-volume-panel"
        onClick={(event) => {
          // 保留焦点，让指针点击静音后仍可继续操作浮层。
          event.preventDefault();
          onToggleMute();
        }}>
        <Icon size={21} />
      </button>
      <div id="shorts-volume-panel" className="shorts-volume-control__panel" hidden={!open}>
        <span className="shorts-volume-control__value" aria-hidden="true">{percent}</span>
        <div className="shorts-volume-control__range"
          style={{ "--shorts-volume-percent": `${percent}%` } as CSSProperties}>
          <div className="shorts-volume-control__track" aria-hidden="true"><span /></div>
          <input type="range" min="0" max="100" step="1" value={percent}
            aria-label="视频音量" aria-orientation="vertical" aria-valuetext={`${percent}%`}
            onChange={(event) => onVolumeChange(Number(event.currentTarget.value) / 100)}
            onPointerDown={() => {
              keyboardRef.current = false;
              draggingRef.current = true;
            }}
            onPointerUp={(event) => {
              draggingRef.current = false;
              if (!event.currentTarget.closest(".shorts-volume-control")?.matches(":hover")) setOpen(false);
            }}
            onPointerCancel={() => { draggingRef.current = false; setOpen(false); }} />
        </div>
      </div>
    </div>
  );
}
