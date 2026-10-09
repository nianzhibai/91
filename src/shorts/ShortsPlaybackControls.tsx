import { useRef, type RefObject } from "react";
import { Maximize, Minimize, Pause, Play } from "lucide-react";
import { exitShortsFullscreen } from "./fullscreen";
import { ShortsVolumeControl } from "./ShortsVolumeControl";
import { ShortsControlButton } from "./ShortsControlButton";
import { SHORTS_CONTROL_SHORTCUTS } from "./controlShortcuts";

type Props = {
  paused: boolean;
  disabled: boolean;
  clockRef: RefObject<HTMLSpanElement>;
  clock: string;
  onTogglePlay: () => void;
  muted: boolean;
  volume: number;
  onToggleMute: () => void;
  onVolumeChange: (volume: number) => void;
  autoAdvance: boolean;
  onAutoAdvanceChange: (enabled: boolean) => void;
  clearScreen: boolean;
  onClearScreenChange: (enabled: boolean) => void;
  playbackRate: number;
  onPlaybackRateChange: (rate: number) => void;
  fullscreenSupported: boolean;
  isFullscreen: boolean;
  onRequestFullscreen: () => Promise<boolean>;
};

const PLAYBACK_RATES = [0.75, 1, 1.25, 1.5, 1.75, 2, 3];

function formatPlaybackRate(rate: number) {
  return `${Number.isInteger(rate) ? rate.toFixed(1) : rate}x`;
}

export function ShortsPlaybackControls(props: Props) {
  const speedMenuRef = useRef<HTMLDetailsElement>(null);
  const speedMenuKeyboardRef = useRef(false);
  function closeSpeedMenu() {
    const menu = speedMenuRef.current;
    if (!menu) return;
    menu.open = false;
    menu.querySelector("summary")?.focus();
  }

  return (
    <div className="shorts-slide__controls" data-shorts-no-swipe=""
      onClick={(event) => event.stopPropagation()}>
      <button type="button" aria-label={props.paused ? "播放视频" : "暂停视频"} aria-keyshortcuts="Space"
        disabled={props.disabled} onClick={props.onTogglePlay}>
        {props.paused ? <Play size={20} fill="currentColor" /> : <Pause size={20} fill="currentColor" />}
      </button>
      <span ref={props.clockRef} className="shorts-slide__clock">{props.clock}</span>
      <div className="shorts-slide__control-settings">
        <ShortsControlButton type="button" role="switch" aria-label="连播" aria-checked={props.autoAdvance}
          tooltip="自动连播" shortcut={SHORTS_CONTROL_SHORTCUTS.autoAdvance}
          onClick={() => props.onAutoAdvanceChange(!props.autoAdvance)}>
          <span className="shorts-control-switch" aria-hidden="true" /><span>连播</span>
        </ShortsControlButton>
        <ShortsControlButton type="button" role="switch" aria-label="清屏" aria-checked={props.clearScreen}
          tooltip={props.clearScreen ? "退出清屏" : "清屏"} shortcut={SHORTS_CONTROL_SHORTCUTS.clearScreen}
          onClick={() => props.onClearScreenChange(!props.clearScreen)}>
          <span className="shorts-control-switch" aria-hidden="true" /><span>清屏</span>
        </ShortsControlButton>
        <details ref={speedMenuRef} className="shorts-speed-menu"
          onPointerEnter={(event) => {
            if (event.pointerType !== "mouse") return;
            speedMenuKeyboardRef.current = false;
            event.currentTarget.open = true;
          }}
          onPointerLeave={(event) => {
            if (event.pointerType === "mouse" && !speedMenuKeyboardRef.current) {
              event.currentTarget.open = false;
            }
          }}
          onBlur={(event) => {
            if (!event.currentTarget.contains(event.relatedTarget as Node | null)) {
              event.currentTarget.open = false;
            }
          }}
          onKeyDown={(event) => {
            speedMenuKeyboardRef.current = true;
            if (event.key === "Escape") {
              event.preventDefault();
              event.stopPropagation();
              closeSpeedMenu();
            }
          }}>
          <summary role="button" aria-label="播放倍速" aria-haspopup="menu"
            onClick={(event) => {
              // 项目会在普通鼠标点击后释放控件焦点；菜单触发器需要保留
              // 焦点，才能由离开菜单的 blur 关闭，而不会刚打开就关闭。
              event.preventDefault();
              const menu = speedMenuRef.current;
              if (menu) {
                speedMenuKeyboardRef.current = event.detail === 0;
                menu.open = event.detail === 0 ? !menu.open : true;
              }
            }}>
            {props.playbackRate === 1 ? "倍速" : formatPlaybackRate(props.playbackRate)}
          </summary>
          <div className="shorts-speed-menu__panel" role="menu" aria-label="播放倍速"
            onKeyDown={(event) => {
              speedMenuKeyboardRef.current = true;
              const buttons = Array.from(event.currentTarget.querySelectorAll<HTMLButtonElement>("button"));
              const index = buttons.indexOf(document.activeElement as HTMLButtonElement);
              let next: number;
              switch (event.key) {
                case "ArrowUp": next = (index + buttons.length - 1) % buttons.length; break;
                case "ArrowDown": next = (index + 1) % buttons.length; break;
                case "Home": next = 0; break;
                case "End": next = buttons.length - 1; break;
                default: return;
              }
              event.preventDefault();
              event.stopPropagation();
              buttons[next]?.focus();
            }}>
            {PLAYBACK_RATES.map((rate) => (
              <button key={rate} type="button" role="menuitemradio"
                aria-checked={props.playbackRate === rate} onClick={() => {
                  props.onPlaybackRateChange(rate);
                  closeSpeedMenu();
                }}>{formatPlaybackRate(rate)}</button>
            ))}
          </div>
        </details>
        <ShortsVolumeControl muted={props.muted} volume={props.volume}
          onToggleMute={props.onToggleMute} onVolumeChange={props.onVolumeChange} />
        {props.fullscreenSupported && (
          <ShortsControlButton type="button" aria-label={props.isFullscreen ? "退出全屏播放" : "全屏播放"}
            tooltip={props.isFullscreen ? "退出全屏" : "进入全屏"}
            shortcut={SHORTS_CONTROL_SHORTCUTS.fullscreen} tooltipAlign="end"
            onClick={() => void (props.isFullscreen ? exitShortsFullscreen() : props.onRequestFullscreen())}>
            {props.isFullscreen ? <Minimize size={20} /> : <Maximize size={20} />}
          </ShortsControlButton>
        )}
      </div>
    </div>
  );
}
