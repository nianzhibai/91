import {
  useEffect,
  useRef,
  useState,
  type CSSProperties,
  type MutableRefObject,
} from "react";
import Artplayer, { type Option, type SettingOption } from "artplayer";
import {
  calculateFullscreenSubtitleBottom,
  getFullscreenPlayerOrientation,
} from "@/lib/fullscreenSubtitleLayout";
import { diagnosePlaybackSource } from "@/lib/playbackError";
import {
  attachVideoSource,
  disposeVideoSource,
  PLAYBACK_SOURCE_ERROR,
  PLAYBACK_SOURCE_CHANGE,
  playbackSourceURL,
  playbackSourceState,
  playbackModeLabel,
  PLAYBACK_MODES,
  retryVideoSource,
  selectPlaybackMode,
  setOriginalVideoDelivery,
  type PlaybackState,
  type PlaybackMode,
} from "@/lib/videoSource";
import {
  getPlayerGesturePoint,
  setPlayerGestureVolume,
  type PlayerGestureFrame,
  type PlayerGesturePoint,
} from "@/lib/playerGestures";
import { disablePlayerVolumePersistence } from "@/lib/playerVolume";
import { bindPlayerNativeBack } from "@/lib/playerNativeBack";
import {
  escapeHtml,
  formatSubtitleLabel,
  formatSubtitleOptionLabel,
  formatSubtitleTooltipLabel,
} from "@/lib/subtitleLabel";
import {
  isPortraitVideo,
  TripleScreenRenderer,
} from "@/lib/tripleScreen";
import type { VideoSubtitle } from "@/types";

type Props = {
  id?: string;
  src: string;
  playbackUrl?: string;
  /**
   * 仅由普通详情页对已确认的 PikPak MP4 开启。运行时仍会限制为
   * iOS/iPadOS；分享页及短视频页不传此参数。
   */
  preferTypedMp4SourceOnIOS?: boolean;
  poster: string;
  previewSrc?: string;
  loadSubtitles?: () => Promise<VideoSubtitle[]>;
  title: string;
  /**
   * 用户首次按下播放时触发。同一个 VideoPlayer 实例只会触发一次；
   * 后续暂停-继续不会重复触发。换 src 时会重置（详情页切换视频用）。
   */
  onFirstPlay?: () => void;
};

type PlayerError = {
  title: string;
  message: string;
};

type GestureHud = {
  key: number;
  label: string;
};

type PreviewHover = {
  x: number;
  ratio: number;
  time: number;
};

type PlayerSettings = {
  volume: number;
  muted: boolean;
  playbackRate: number;
  brightness: number;
  loop: boolean;
};

type MobileGestureMode = "seek" | "volume" | "brightness";
type MobileGestureSide = "left" | "right";
type PlayerGestureHudKind = "volume" | "brightness";
type KeyboardSeekKey = "ArrowLeft" | "ArrowRight";
type MobileGestureState = {
  frame: PlayerGestureFrame;
  startPoint: PlayerGesturePoint;
  startTime: number;
  startVolume: number;
  startBrightness: number;
  side: MobileGestureSide;
  mode: MobileGestureMode | null;
  targetTime: number;
  moved: boolean;
  fastActive: boolean;
  previousRate: number;
  pressTimer: number | null;
};

type OrientationMode = "landscape" | "portrait";
type OrientationKind = "native" | "web";
type FullscreenElement = HTMLElement & {
  webkitRequestFullscreen?: () => Promise<void> | void;
  mozRequestFullScreen?: () => Promise<void> | void;
  msRequestFullscreen?: () => Promise<void> | void;
};
type FullscreenDocument = Document & {
  webkitFullscreenElement?: Element | null;
  mozFullScreenElement?: Element | null;
  msFullscreenElement?: Element | null;
  webkitExitFullscreen?: () => Promise<void> | void;
  mozCancelFullScreen?: () => Promise<void> | void;
  msExitFullscreen?: () => Promise<void> | void;
};
type LockableScreenOrientation = ScreenOrientation & {
  lock?: (orientation: OrientationMode) => Promise<void>;
  unlock?: () => void;
};

/** 长按多少毫秒后进入 2 倍速。短按属于普通点击，交给 ArtPlayer 处理。 */
const LONG_PRESS_MS = 400;
/** 长按时使用的播放倍速。 */
const FAST_RATE = 2;
/** 默认倍速。 */
const NORMAL_RATE = 1;
/** ArtPlayer 内部播放失败自动重连次数。 */
const ARTPLAYER_RECONNECT_TIME_MAX = 2;
/** 播放状态下控制栏无操作后自动隐藏的时间。 */
const ARTPLAYER_CONTROL_HIDE_TIME_MS = 2_000;
/** 键盘左右键单次快进/快退秒数。 */
const KEYBOARD_SEEK_SECONDS = 15;
/** 浏览器丢失 keyup 时，最后一次重复按键后的兜底提交延迟。 */
const KEYBOARD_SEEK_IDLE_COMMIT_MS = 1_500;

Artplayer.FAST_FORWARD_VALUE = FAST_RATE;
Artplayer.RECONNECT_TIME_MAX = ARTPLAYER_RECONNECT_TIME_MAX;
Artplayer.CONTROL_HIDE_TIME = ARTPLAYER_CONTROL_HIDE_TIME_MS;

const DEFAULT_SETTINGS: PlayerSettings = {
  volume: 1,
  muted: false,
  playbackRate: 1,
  brightness: 1,
  loop: true,
};
const DEFAULT_SETTING_LAYOUT = {
  width: Artplayer.SETTING_WIDTH,
  itemHeight: Artplayer.SETTING_ITEM_HEIGHT,
};
const COMPACT_SETTING_LAYOUT = {
  width: 172,
  itemHeight: 30,
};
const ORIENTATION_CONTROL_NAME = "orientationToggle";
const PLAYER_SURFACE_CLASS = "video-player__art";
const TRIPLE_SCREEN_CONTROL_NAME = "tripleScreen";
const TRIPLE_SCREEN_RELAY_QUERY = "tripleScreenRelay";
const MANUAL_ORIENTATION_CLASS = "art-manual-orientation";
const TRIPLE_SCREEN_ACTIVE_CLASS = "art-triple-screen-active";
const FAST_RATE_CLASS = "art-fast-rate-active";
const FAST_RATE_HINT_CLASS = "video-player__art-rate-hint";
const PLAYER_GESTURE_HUD_CLASS = "video-player__art-gesture-hud";
const PLAYER_GESTURE_HUD_ICON_CLASS = "video-player__art-gesture-hud-icon";
const PLAYER_GESTURE_HUD_VALUE_CLASS = "video-player__art-gesture-hud-value";
const FULLSCREEN_SUBTITLE_BOTTOM_CSS_VAR =
  "--video-player-subtitle-bottom";
const FULLSCREEN_SUBTITLE_ORIENTATION_DATASET =
  "videoPlayerSubtitleOrientation";
const PLAYER_SPACE_HOTKEY_EXCLUDED_SELECTOR = [
  "input",
  "textarea",
  "select",
  "button",
  "a[href]",
  "[contenteditable]:not([contenteditable='false'])",
  "[role='button']",
  "[role='textbox']",
  "[role='combobox']",
  "[role='slider']",
  "[role='menuitem']",
  "[role='option']",
  "[role='dialog']",
].join(",");
const ACTIVE_MODAL_SELECTOR =
  'dialog[open], [role="dialog"][aria-modal="true"]';
const PREVIEW_WIDTH = 168;
const MEDIA_REFERRER_POLICY = "no-referrer";
const TYPED_MP4_SOURCE_TYPE = "ios-pikpak-mp4-source";
const TYPED_MP4_SOURCE_SELECTOR =
  'source[data-video-player-typed-source="mp4"]';
const BRIGHTNESS_MIN = 0.45;
const BRIGHTNESS_MAX = 1.35;
const GESTURE_ACTIVATION_PX = 12;
const GESTURE_DIRECTION_LOCK_RATIO = 1.2;
const GESTURE_VERTICAL_SCALE = 1.15;
const playerGestureHudTimers = new WeakMap<HTMLElement, number>();
const playerBrightness = new WeakMap<Artplayer, number>();
const tripleScreenBindings = new WeakMap<
  Artplayer,
  { toggle: () => void; destroy: () => void }
>();

export function VideoPlayer({
  src,
  playbackUrl,
  preferTypedMp4SourceOnIOS = false,
  poster,
  previewSrc,
  loadSubtitles,
  title,
  onFirstPlay,
}: Props) {
  const mountRef = useRef<HTMLDivElement | null>(null);
  const artRef = useRef<Artplayer | null>(null);
  const previewVideoRef = useRef<HTMLVideoElement | null>(null);
  const onFirstPlayRef = useRef<Props["onFirstPlay"]>(onFirstPlay);
  const loadSubtitlesRef = useRef<Props["loadSubtitles"]>(loadSubtitles);
  const playedRef = useRef(false);
  const [retryNonce, setRetryNonce] = useState(0);
  const [playerError, setPlayerError] = useState<PlayerError | null>(null);
  const [sourceState, setSourceState] = useState<PlaybackState | null>(null);
  const [gestureHud, setGestureHud] = useState<GestureHud | null>(null);
  const [previewHover, setPreviewHover] = useState<PreviewHover | null>(null);
  const gestureHudTimerRef = useRef<number | null>(null);

  useEffect(() => {
    onFirstPlayRef.current = onFirstPlay;
  }, [onFirstPlay]);

  useEffect(() => {
    loadSubtitlesRef.current = loadSubtitles;
  }, [loadSubtitles]);

  useEffect(() => {
    const mount = mountRef.current;
    if (!mount) return;

    playedRef.current = false;
    setPlayerError(null);
    setSourceState(null);
    setPreviewHover(null);

    const cleanupPlayer = mountArtPlayer({
      mount,
      src,
      playbackUrl,
      refreshSource: retryNonce > 0,
      preferTypedMp4SourceOnIOS,
      poster,
      title,
      loadSubtitlesRef,
      artRef,
      playedRef,
      onFirstPlayRef,
      onFastChange: noop,
      onError: setPlayerError,
      onSourceChange: setSourceState,
      onPreviewHover: setPreviewHover,
      onGestureHud: showGestureHud,
    });

    return cleanupPlayer;
  }, [poster, preferTypedMp4SourceOnIOS, retryNonce, src, playbackUrl, title]);

  useEffect(() => {
    return () => {
      if (gestureHudTimerRef.current !== null) {
        window.clearTimeout(gestureHudTimerRef.current);
      }
    };
  }, []);

  useEffect(() => {
    if (!previewSrc || !previewHover) return;
    syncPreviewVideo(previewVideoRef.current, previewHover.ratio);
  }, [previewHover, previewSrc]);

  function showGestureHud(label: string, duration = 700) {
    if (gestureHudTimerRef.current !== null) {
      window.clearTimeout(gestureHudTimerRef.current);
    }
    setGestureHud({ key: Date.now(), label });
    gestureHudTimerRef.current = window.setTimeout(() => {
      setGestureHud(null);
      gestureHudTimerRef.current = null;
    }, duration);
  }

  function retryPlayback() {
    setPlayerError(null);
    if (!artRef.current || !retryVideoSource(artRef.current.video)) {
      setRetryNonce((n) => n + 1);
    }
  }

  const previewStyle = previewHover
    ? ({ left: `${previewHover.x}px` } as CSSProperties)
    : undefined;
  const recoveryModes = sourceState?.error?.recoveryModes ?? [];

  return (
    <div className="video-player">
      <div ref={mountRef} className="video-player__mount" />

      {sourceState?.processing && !playerError && (
        <div className="video-player__processing" role="status" aria-live="polite">
          {sourceState.notice || "视频正在转码，完成后将自动播放"}
        </div>
      )}

      {playerError && (
        <div className="video-player__error" role="alert">
          <div className="video-player__error-title">{playerError.title}</div>
          <div className="video-player__error-message">{playerError.message}</div>
          <div className="video-player__error-actions">
            <button type="button" onClick={retryPlayback}>
              {sourceState?.error ? "重新获取" : "重试"}
            </button>
            {recoveryModes.map((mode) => (
              <button key={mode} type="button" onClick={() => {
                if (artRef.current) selectPlaybackMode(artRef.current.video, mode);
              }}>
                {mode === "original" ? "尝试原画播放" : "切换兼容播放"}
              </button>
            ))}
          </div>
        </div>
      )}

      {previewSrc && previewHover && (
        <div
          className="video-player__seek-preview"
          style={previewStyle}
          aria-hidden="true"
        >
          <video
            ref={previewVideoRef}
            src={previewSrc}
            poster={poster}
            muted
            playsInline
            preload="metadata"
            onLoadedMetadata={() =>
              syncPreviewVideo(previewVideoRef.current, previewHover.ratio)
            }
          />
          <span>{formatClock(previewHover.time)}</span>
        </div>
      )}

      {gestureHud && (
        <div
          key={gestureHud.key}
          className="video-player__gesture-hud"
          aria-hidden="true"
        >
          {gestureHud.label}
        </div>
      )}

    </div>
  );
}

function inferSourceType(src: string) {
  const lower = src.toLowerCase();
  const cleanPath = lower.split("#")[0].split("?")[0];
  if (cleanPath.endsWith(".m3u8") || lower.includes(".m3u8")) return "m3u8";
  return undefined;
}

function mountArtPlayer({
  mount,
  src,
  playbackUrl,
  refreshSource,
  preferTypedMp4SourceOnIOS,
  poster,
  loadSubtitlesRef,
  title,
  artRef,
  playedRef,
  onFirstPlayRef,
  onFastChange,
  onError,
  onSourceChange,
  onPreviewHover,
  onGestureHud,
}: {
  mount: HTMLDivElement;
  src: string;
  playbackUrl?: string;
  refreshSource: boolean;
  preferTypedMp4SourceOnIOS: boolean;
  poster: string;
  loadSubtitlesRef: MutableRefObject<Props["loadSubtitles"]>;
  title: string;
  artRef: MutableRefObject<Artplayer | null>;
  playedRef: MutableRefObject<boolean>;
  onFirstPlayRef: MutableRefObject<Props["onFirstPlay"]>;
  onFastChange: (active: boolean) => void;
  onError: (error: PlayerError | null) => void;
  onSourceChange: (state: PlaybackState) => void;
  onPreviewHover: (hover: PreviewHover | null) => void;
  onGestureHud: (label: string, duration?: number) => void;
}) {
  const useTypedMp4Source =
    preferTypedMp4SourceOnIOS && isIOSPlaybackDevice();
  const sourceType = playbackUrl ? "resolved-source" : useTypedMp4Source
    ? TYPED_MP4_SOURCE_TYPE
    : inferSourceType(src);
  const subtitleState: SubtitleLoadState = { status: "idle", subtitles: [] };
  const fastActiveRef = { current: false };
  const loadHlsSource = createHlsSourceLoader();
  const enableOrientationControl = shouldEnableMobileOrientationControl();
  const enableTripleScreenControl = shouldEnableTripleScreenControl();
  const enableWebFullscreen = shouldEnableWebFullscreen(enableOrientationControl);
  let disposed = false;
  let playbackErrorActive = false;
  let diagnosticController: AbortController | null = null;
  let diagnosticPromise: Promise<string | null> | null = null;
  let diagnosticMessage: string | null = null;
  const compactSettings = shouldUseCompactPlayerSettings(mount, enableOrientationControl);
  configureArtPlayerSettingLayout(compactSettings);
  const option: Option = {
    id: "91-detail-player",
    container: mount,
    url: "",
    poster,
    theme: "var(--video-player-progress)",
    lang: "zh-cn",
    volume: DEFAULT_SETTINGS.volume,
    muted: DEFAULT_SETTINGS.muted,
    autoplay: false,
    autoSize: false,
    playbackRate: true,
    aspectRatio: true,
    setting: true,
    // 左右键需要在松键时只提交一次真实 seek，不能使用 ArtPlayer
    // 每个 keydown 都改 currentTime 的内置实现。其它快捷键在下方重新绑定。
    hotkey: false,
    pip: true,
    mutex: true,
    fullscreen: true,
    fullscreenWeb: enableWebFullscreen,
    miniProgressBar: true,
    backdrop: false,
    playsInline: true,
    lock: true,
    gesture: false,
    fastForward: false,
    airplay: true,
    customType: {
      "resolved-source": (video) => {
        attachVideoSource(video, src, playbackUrl, {
          refresh: refreshSource,
          loadNative: useTypedMp4Source ? (url) => loadTypedMp4Source(video, url, art) : undefined,
        });
      },
      hls: loadHlsSource,
      m3u8: loadHlsSource,
      [TYPED_MP4_SOURCE_TYPE]: loadTypedMp4Source,
    },
    moreVideoAttr: {
      preload: "metadata",
      playsInline: true,
    },
    settings: createPlayerSettings(subtitleState, requestSubtitles),
    controls: createPlayerControls(
      enableOrientationControl,
      enableTripleScreenControl
    ),
    contextmenu: [],
    cssVar: {
      "--art-theme": "var(--video-player-progress)",
    },
  };
  if (sourceType) {
    option.type = sourceType;
  }

  const art = new Artplayer(option);
  // The shared source controller owns recovery. ArtPlayer's delayed reconnect
  // would otherwise reload an old source after a successful automatic switch.
  if (playbackUrl || sourceType === "m3u8") art.off("video:error");
  artRef.current = art;
  art.template.$player.classList.add(PLAYER_SURFACE_CLASS);
  art.template.$player.dataset.settingsLayout = compactSettings ? "compact" : "default";

  const video = art.video;
  disablePlayerVolumePersistence(art.storage);
  // ArtPlayer may have restored a cached volume during its constructor.
  video.volume = DEFAULT_SETTINGS.volume;
  video.muted = DEFAULT_SETTINGS.muted;
  video.setAttribute("referrerpolicy", MEDIA_REFERRER_POLICY);
  video.setAttribute("aria-label", title);
  video.setAttribute("controlsList", "nodownload");
  video.setAttribute("webkit-playsinline", "true");
  video.disablePictureInPicture = false;
  video.loop = DEFAULT_SETTINGS.loop;
  video.playbackRate = DEFAULT_SETTINGS.playbackRate;
  const unbindBrightness = bindPlayerBrightness(art);

  async function requestSubtitles() {
    if (
      subtitleState.status === "loading" ||
      subtitleState.status === "loaded" ||
      subtitleState.status === "empty"
    ) {
      return;
    }
    const loader = loadSubtitlesRef.current;
    if (!loader) {
      subtitleState.status = "empty";
      updateSubtitleSetting(art, subtitleState, requestSubtitles);
      return;
    }
    subtitleState.status = "loading";
    setSubtitleSettingTooltip(art, "加载中");
    art.notice.show = "正在加载字幕";
    try {
      const subtitles = playableSubtitles(await loader());
      if (disposed) return;
      subtitleState.subtitles = subtitles;
      subtitleState.status = subtitles.length > 0 ? "loaded" : "empty";
      updateSubtitleSetting(art, subtitleState, requestSubtitles);
      art.notice.show =
        subtitles.length > 0
          ? `已加载 ${subtitles.length} 条字幕`
          : "暂无可用字幕";
    } catch {
      if (disposed) return;
      subtitleState.status = "error";
      updateSubtitleSetting(art, subtitleState, requestSubtitles);
      art.notice.show = "字幕加载失败，点击字幕重试";
    }
  }

  function preventContextMenu(event: Event) {
    event.preventDefault();
  }

  function handlePlay() {
    if (!playedRef.current) {
      playedRef.current = true;
      onFirstPlayRef.current?.();
    }
    playbackErrorActive = false;
    clearPlaybackDiagnostic();
    onError(null);
  }

  function handleLoadStart() {
    playbackErrorActive = false;
    onError(null);
  }

  function handleReady() {
    playbackErrorActive = false;
    clearPlaybackDiagnostic();
    onError(null);
  }

  function handleVideoError() {
    if (playbackUrl) return;
    playbackErrorActive = true;
    const fallbackMessage = mediaErrorMessage(video.error);
    onError({
      title: "视频源加载失败",
      message: diagnosticMessage || fallbackMessage,
    });
    if (diagnosticPromise) return;

    diagnosticController = new AbortController();
    diagnosticPromise = diagnosePlaybackSource(src, {
      signal: diagnosticController.signal,
    });
    void diagnosticPromise.then((message) => {
      diagnosticMessage = message;
      if (!disposed && playbackErrorActive && message) {
        onError({ title: "视频源加载失败", message });
      }
    });
  }

  function handleSourceError() {
    const failure = playbackSourceState(video)?.error;
    if (!failure) return;
    const unsupportedOriginal = failure.reason === "unsupported" && failure.sourceID === "original";
    onError({
      title: unsupportedOriginal ? "视频无法播放" : "视频源暂不可用",
      message: failure.message,
    });
  }

  let previousSourceNotice: string | null = null;
  let previousPlaybackSetting = "";
  function handleSourceChange() {
    const state = playbackSourceState(video);
    if (!state) return;
    onSourceChange(state);
    if (!state.error) onError(null);
    const hasCloudSource = state.sources.some((source) => source.kind === "transcoded");
    const settingKey = `${state.mode}:${hasCloudSource}`;
    if (settingKey !== previousPlaybackSetting && hasCloudSource) {
      art.setting.update(createPlaybackSetting(state));
    } else if (!hasCloudSource && art.setting.find("playback-mode")) {
      art.setting.remove("playback-mode");
    }
    updatePlaybackSourceStatus(art, state);
    previousPlaybackSetting = settingKey;
    if (state.notice && state.notice !== previousSourceNotice) art.notice.show = state.notice;
    previousSourceNotice = state.notice;
  }

  function clearPlaybackDiagnostic() {
    diagnosticController?.abort();
    diagnosticController = null;
    diagnosticPromise = null;
    diagnosticMessage = null;
  }

  function resetFastRate() {
    fastActiveRef.current = false;
    setPlayerFastRateHint(art, false);
    onFastChange(false);
  }

  const handleFastChange = (active: boolean) => {
    fastActiveRef.current = active;
    setPlayerFastRateHint(art, active);
    onFastChange(active);
  };

  const unbindFastRate = bindLongPressFast(video, handleFastChange);
  const unbindMobileGestures = bindMobilePlayerGestures(
    art,
    video,
    handleFastChange,
    onGestureHud
  );
  const unbindProgressPreview = bindProgressPreview(
    art,
    video,
    mount,
    onPreviewHover
  );
  const unbindKeyboardHotkeys = bindPlayerKeyboardHotkeys(art);
  const unbindNativeBack = bindPlayerNativeBack(art);
  const unbindMobileFullscreenControlAutoHide =
    bindMobileFullscreenControlAutoHide(art);
  const unbindFullscreenSubtitleLayout = bindFullscreenSubtitleLayout(
    art,
    video
  );
  const unbindTripleScreen = enableTripleScreenControl
    ? bindTripleScreen(art, video, src)
    : noop;
  const unbindOrientationToggle = enableOrientationControl
    ? bindOrientationToggle(art)
    : noop;

  mount.addEventListener("contextmenu", preventContextMenu);

  art.on("video:loadstart", handleLoadStart);
  video.addEventListener(PLAYBACK_SOURCE_ERROR, handleSourceError);
  video.addEventListener(PLAYBACK_SOURCE_CHANGE, handleSourceChange);
  art.on("video:loadeddata", handleReady);
  art.on("video:canplay", handleReady);
  art.on("video:playing", handleReady);
  art.on("video:error", handleVideoError);
  art.on("error", handleVideoError);
  art.on("video:play", handlePlay);
  art.on("video:pause", resetFastRate);
  art.on("video:ended", resetFastRate);
  // 所有基于媒体生命周期的监听都挂好后再设置真实地址，避免本地缓存
  // 极快返回 metadata 时错过方向识别或错误恢复事件。
  if (useTypedMp4Source && !playbackUrl) {
    // ArtPlayer 的 url getter 读取父级 video.src；使用子 <source> 时它为空。
    // 主动保存 option.url，确保 ArtPlayer 内置的失败重连仍使用真实地址。
    art.option.url = src;
    loadTypedMp4Source(video, src, art);
  } else {
    art.url = src;
  }

  return () => {
    disposed = true;
    video.removeEventListener(PLAYBACK_SOURCE_ERROR, handleSourceError);
    video.removeEventListener(PLAYBACK_SOURCE_CHANGE, handleSourceChange);
    disposeVideoSource(video);
    clearPlaybackDiagnostic();
    // ArtPlayer 的网页全屏会把播放器节点临时移动到 document.body。
    // 路由回退时必须先退出网页全屏，把节点移回 mount，再执行 destroy；
    // 否则 React 卸载详情页后，脱离组件树的全屏节点会继续覆盖新页面。
    if (art.fullscreenWeb) art.fullscreenWeb = false;
    unbindBrightness();
    unbindFastRate();
    unbindMobileGestures();
    unbindProgressPreview();
    unbindKeyboardHotkeys();
    unbindNativeBack();
    unbindMobileFullscreenControlAutoHide();
    unbindFullscreenSubtitleLayout();
    unbindTripleScreen();
    unbindOrientationToggle();
    setPlayerFastRateHint(art, false);
    mount.removeEventListener("contextmenu", preventContextMenu);
    if (useTypedMp4Source) clearTypedMp4Source(video);
    art.off("video:loadstart", handleLoadStart);
    art.off("video:loadeddata", handleReady);
    art.off("video:canplay", handleReady);
    art.off("video:playing", handleReady);
    art.off("video:error", handleVideoError);
    art.off("error", handleVideoError);
    art.off("video:play", handlePlay);
    art.off("video:pause", resetFastRate);
    art.off("video:ended", resetFastRate);
    art.destroy(true);
    if (artRef.current === art) {
      artRef.current = null;
    }
    onPreviewHover(null);
  };
}

function loadTypedMp4Source(
  video: HTMLVideoElement,
  url: string,
  art: Artplayer
) {
  if (art.isDestroy || !video.isConnected) return;
  video.removeAttribute("src");
  clearTypedMp4Source(video);

  const source = document.createElement("source");
  source.src = url;
  source.type = "video/mp4";
  source.dataset.videoPlayerTypedSource = "mp4";
  source.addEventListener(
    "error",
    (event) => {
      // 使用 <source> 时，WebKit 会把资源选择错误派发到 source 节点；
      // 转成 ArtPlayer 的媒体错误，沿用现有重试和诊断界面。
      if (!art.isDestroy && source.isConnected) {
        art.emit("video:error", event);
        video.dispatchEvent(new Event("error"));
      }
    },
    { once: true }
  );
  // ArtPlayer 已经放入了字幕 <track>；source 必须排在 track 前面。
  video.insertBefore(source, video.firstChild);
  video.load();
}

function clearTypedMp4Source(video: HTMLVideoElement) {
  video
    .querySelectorAll<HTMLSourceElement>(TYPED_MP4_SOURCE_SELECTOR)
    .forEach((source) => source.remove());
}

function bindFullscreenSubtitleLayout(
  art: Artplayer,
  video: HTMLVideoElement
) {
  const player = art.template.$player;
  let active = true;
  let updateFrame: number | null = null;

  function updateLayout() {
    updateFrame = null;
    if (!active) return;
    const orientation = getFullscreenPlayerOrientation(
      player.clientWidth,
      player.clientHeight
    );
    if (orientation === null) {
      delete player.dataset[FULLSCREEN_SUBTITLE_ORIENTATION_DATASET];
    } else {
      player.dataset[FULLSCREEN_SUBTITLE_ORIENTATION_DATASET] = orientation;
    }
    const bottom = calculateFullscreenSubtitleBottom(
      player.clientWidth,
      player.clientHeight,
      video.videoWidth,
      video.videoHeight
    );
    if (bottom === null) {
      player.style.removeProperty(FULLSCREEN_SUBTITLE_BOTTOM_CSS_VAR);
      return;
    }
    player.style.setProperty(
      FULLSCREEN_SUBTITLE_BOTTOM_CSS_VAR,
      `${bottom.toFixed(2)}px`
    );
  }

  function scheduleUpdate() {
    if (!active) return;
    if (updateFrame !== null) window.cancelAnimationFrame(updateFrame);
    updateFrame = window.requestAnimationFrame(updateLayout);
  }

  const resizeObserver =
    typeof ResizeObserver === "undefined"
      ? null
      : new ResizeObserver(scheduleUpdate);
  resizeObserver?.observe(player);

  art.on("video:loadedmetadata", scheduleUpdate);
  art.on("resize", scheduleUpdate);
  art.on("fullscreen", scheduleUpdate);
  art.on("fullscreenWeb", scheduleUpdate);
  art.on("aspectRatio", scheduleUpdate);
  scheduleUpdate();

  return () => {
    active = false;
    if (updateFrame !== null) window.cancelAnimationFrame(updateFrame);
    resizeObserver?.disconnect();
    art.off("video:loadedmetadata", scheduleUpdate);
    art.off("resize", scheduleUpdate);
    art.off("fullscreen", scheduleUpdate);
    art.off("fullscreenWeb", scheduleUpdate);
    art.off("aspectRatio", scheduleUpdate);
    player.style.removeProperty(FULLSCREEN_SUBTITLE_BOTTOM_CSS_VAR);
    delete player.dataset[FULLSCREEN_SUBTITLE_ORIENTATION_DATASET];
  };
}

function bindPlayerKeyboardHotkeys(art: Artplayer) {
  let keyboardSeekTarget: number | null = null;
  let keyboardSeekIdleTimer: number | null = null;
  const heldSeekKeys = new Set<KeyboardSeekKey>();

  function clearKeyboardSeekIdleTimer() {
    if (keyboardSeekIdleTimer === null) return;
    window.clearTimeout(keyboardSeekIdleTimer);
    keyboardSeekIdleTimer = null;
  }

  function scheduleKeyboardSeekIdleCommit() {
    clearKeyboardSeekIdleTimer();
    keyboardSeekIdleTimer = window.setTimeout(() => {
      keyboardSeekIdleTimer = null;
      commitKeyboardSeek();
    }, KEYBOARD_SEEK_IDLE_COMMIT_MS);
  }

  function renderKeyboardSeekTarget(showNotice: boolean) {
    const duration = art.duration;
    if (
      keyboardSeekTarget === null ||
      !Number.isFinite(duration) ||
      duration <= 0
    ) {
      return;
    }

    keyboardSeekTarget = clamp(keyboardSeekTarget, 0, duration);
    art.emit("setBar", "played", keyboardSeekTarget / duration);
    if (showNotice) {
      art.notice.show = `${formatClock(keyboardSeekTarget)} / ${formatClock(duration)}`;
    }
  }

  function previewKeyboardSeek(delta: number, key: KeyboardSeekKey) {
    const duration = art.duration;
    if (!Number.isFinite(duration) || duration <= 0) return;

    heldSeekKeys.add(key);
    const baseTime = keyboardSeekTarget ?? art.currentTime;
    keyboardSeekTarget = clamp(baseTime + delta, 0, duration);
    renderKeyboardSeekTarget(true);
    scheduleKeyboardSeekIdleCommit();
  }

  function commitKeyboardSeek() {
    if (keyboardSeekTarget === null) return;

    clearKeyboardSeekIdleTimer();
    const duration = art.duration;
    const target = Number.isFinite(duration) && duration > 0
      ? clamp(keyboardSeekTarget, 0, duration)
      : keyboardSeekTarget;
    keyboardSeekTarget = null;
    heldSeekKeys.clear();

    art.seek = target;
    if (Number.isFinite(duration) && duration > 0) {
      art.emit("setBar", "played", target / duration);
    }
  }

  const handleEscape = () => {
    commitKeyboardSeek();
    if (art.fullscreenWeb) art.fullscreenWeb = false;
  };
  const handleSpace = (event: KeyboardEvent) => {
    if (event.repeat) return;
    commitKeyboardSeek();
    art.toggle();
  };
  const handleArrowUp = () => {
    commitKeyboardSeek();
    art.volume += Artplayer.VOLUME_STEP;
  };
  const handleArrowDown = () => {
    commitKeyboardSeek();
    art.volume -= Artplayer.VOLUME_STEP;
  };
  const handleArrowLeft = () => {
    previewKeyboardSeek(-KEYBOARD_SEEK_SECONDS, "ArrowLeft");
  };
  const handleArrowRight = () => {
    previewKeyboardSeek(KEYBOARD_SEEK_SECONDS, "ArrowRight");
  };

  function handlePageSpaceKeyDown(event: KeyboardEvent) {
    if (event.code !== "Space" && event.key !== " ") return;
    if (shouldIgnorePageSpaceHotkey(event)) return;

    // ArtPlayer 的 hotkey 只有在播放器被点击后才生效。详情播放器挂载时
    // 直接接管页面空格键，让首次按键也能播放，同时阻止浏览器翻页滚动。
    event.preventDefault();
    handleSpace(event);
  }

  function handleKeyUp(event: KeyboardEvent) {
    if (event.code !== "ArrowLeft" && event.code !== "ArrowRight") return;
    if (keyboardSeekTarget === null) return;

    event.preventDefault();
    heldSeekKeys.delete(event.code);
    if (heldSeekKeys.size === 0) commitKeyboardSeek();
  }

  function handleTimeUpdate() {
    // 播放中的 timeupdate 会把进度条写回真实媒体时间；按键尚未松开时，
    // 再写回累计目标，确保长按预览不抖动。
    if (keyboardSeekTarget !== null) renderKeyboardSeekTarget(false);
  }

  function handleVisibilityChange() {
    if (document.hidden) commitKeyboardSeek();
  }

  art.hotkey.add("Escape", handleEscape);
  art.hotkey.add("ArrowUp", handleArrowUp);
  art.hotkey.add("ArrowDown", handleArrowDown);
  art.hotkey.add("ArrowLeft", handleArrowLeft);
  art.hotkey.add("ArrowRight", handleArrowRight);
  art.on("video:timeupdate", handleTimeUpdate);
  art.on("blur", commitKeyboardSeek);
  document.addEventListener("keydown", handlePageSpaceKeyDown);
  document.addEventListener("keyup", handleKeyUp);
  document.addEventListener("visibilitychange", handleVisibilityChange);
  window.addEventListener("blur", commitKeyboardSeek);

  return () => {
    clearKeyboardSeekIdleTimer();
    keyboardSeekTarget = null;
    heldSeekKeys.clear();
    art.hotkey.remove("Escape", handleEscape);
    art.hotkey.remove("ArrowUp", handleArrowUp);
    art.hotkey.remove("ArrowDown", handleArrowDown);
    art.hotkey.remove("ArrowLeft", handleArrowLeft);
    art.hotkey.remove("ArrowRight", handleArrowRight);
    art.off("video:timeupdate", handleTimeUpdate);
    art.off("blur", commitKeyboardSeek);
    document.removeEventListener("keydown", handlePageSpaceKeyDown);
    document.removeEventListener("keyup", handleKeyUp);
    document.removeEventListener("visibilitychange", handleVisibilityChange);
    window.removeEventListener("blur", commitKeyboardSeek);
  };
}

function shouldIgnorePageSpaceHotkey(event: KeyboardEvent) {
  if (
    event.defaultPrevented ||
    event.isComposing ||
    event.altKey ||
    event.ctrlKey ||
    event.metaKey ||
    event.shiftKey
  ) {
    return true;
  }

  // 弹窗打开但尚未把焦点移入弹窗时，也不能让空格控制背后的视频。
  if (document.querySelector(ACTIVE_MODAL_SELECTOR)) return true;

  return [event.target, document.activeElement].some(
    (target) =>
      target instanceof Element &&
      target.closest(PLAYER_SPACE_HOTKEY_EXCLUDED_SELECTOR) !== null
  );
}

function shouldEnableMobileOrientationControl() {
  return isMobilePlaybackDevice() && !isApplePhoneDevice();
}

function shouldEnableTripleScreenControl() {
  return !isMobilePlaybackDevice();
}

function shouldEnableWebFullscreen(enableOrientationControl: boolean) {
  return !enableOrientationControl && !isAppleDevice();
}

function isMobilePlaybackDevice() {
  const coarsePointer = window.matchMedia?.(
    "(hover: none) and (pointer: coarse)"
  ).matches;
  if (coarsePointer) return true;

  return /Android|iPhone|iPad|iPod|Mobile/i.test(navigator.userAgent);
}

function isApplePhoneDevice() {
  return /iPhone|iPod/i.test(navigator.userAgent);
}

function isIOSPlaybackDevice() {
  return (
    /iPhone|iPad|iPod/i.test(navigator.userAgent) ||
    (/Macintosh/i.test(navigator.userAgent) && navigator.maxTouchPoints > 1)
  );
}

function isAppleDevice() {
  return /iPhone|iPad|iPod|Macintosh/i.test(navigator.userAgent);
}

function shouldUseCompactPlayerSettings(
  mount: HTMLElement,
  mobileControls: boolean
) {
  const narrowViewport =
    window.matchMedia?.("(max-width: 640px)").matches ??
    window.innerWidth <= 640;
  return mobileControls || narrowViewport || mount.clientWidth <= 640;
}

function configureArtPlayerSettingLayout(compact: boolean) {
  const layout = compact ? COMPACT_SETTING_LAYOUT : DEFAULT_SETTING_LAYOUT;
  Artplayer.SETTING_WIDTH = layout.width;
  Artplayer.SETTING_ITEM_WIDTH = layout.width;
  Artplayer.SETTING_ITEM_HEIGHT = layout.itemHeight;
}

function shouldEnableMobileGestures() {
  return isMobilePlaybackDevice();
}

function createLoopSetting() {
  return {
    name: "mind-loop",
    html: "洗脑循环",
    tooltip: DEFAULT_SETTINGS.loop ? "开" : "关",
    switch: DEFAULT_SETTINGS.loop,
    onSwitch(this: Artplayer, item: SettingOption) {
      const next = !item.switch;
      this.video.loop = next;
      item.tooltip = next ? "开" : "关";
      return next;
    },
  };
}

/** 字幕文件名使用独立宽度；短选项子菜单继承主菜单宽度。 */
const SUBTITLE_PANEL_WIDTH = 240;

type PlayerSubtitle = VideoSubtitle & { type: "vtt" | "srt" | "ass" };
type PlayerSetting = NonNullable<Option["settings"]>[number];
type SubtitleLoadStatus = "idle" | "loading" | "loaded" | "empty" | "error";
type SubtitleLoadState = {
  status: SubtitleLoadStatus;
  subtitles: PlayerSubtitle[];
};
type ArtSettingWithRender = Artplayer["setting"] & {
  render: (settings?: SettingOption[]) => void;
};

function playableSubtitles(subtitles: VideoSubtitle[]): PlayerSubtitle[] {
  return subtitles.filter(
    (subtitle): subtitle is PlayerSubtitle =>
      Boolean(subtitle.url) &&
      (subtitle.type === "vtt" ||
        subtitle.type === "srt" ||
        subtitle.type === "ass")
  );
}

function subtitleOption(
  subtitle: PlayerSubtitle
): NonNullable<Option["subtitle"]> {
  return {
    url: subtitle.url,
    name: formatSubtitleLabel(subtitle),
    type: subtitle.type,
    encoding: "utf-8",
    escape: true,
  };
}

function createPlayerSettings(
  state: SubtitleLoadState,
  requestSubtitles: () => Promise<void>
) {
  return [createLoopSetting(), createSubtitleSetting(state, requestSubtitles)];
}

function playbackSourceStatus(state: PlaybackState) {
  if (state.error) return "播放源暂不可用";
  if (state.processing) return "视频正在转码";
  if (state.loading) return "正在获取播放源";
  return state.activeSource ? `当前：${state.activeSource.label}` : "等待播放源";
}

function updatePlaybackSourceStatus(art: Artplayer, state: PlaybackState) {
  const label = playbackSourceStatus(state);
  art.template.$setting.querySelectorAll(".video-player__playback-source-status").forEach((element) => {
    element.textContent = label;
    element.setAttribute("title", label);
  });
}

function createPlaybackSetting(state: PlaybackState): PlayerSetting {
  return {
    name: "playback-mode",
    html: `<span class="video-player__playback-heading"><span>播放方式</span><span class="video-player__playback-source-status">${escapeHtml(playbackSourceStatus(state))}</span></span>`,
    tooltip: playbackModeLabel(state.mode),
    // Inherit the root menu width; only subtitle names need a wider panel.
    selector: PLAYBACK_MODES.map((mode) => ({
      name: `playback-mode-${mode.value}`,
      html: mode.label,
      value: mode.value,
      default: state.mode === mode.value,
    })),
    onSelect(this: Artplayer, item: SettingOption) {
      selectPlaybackMode(this.video, item.value as PlaybackMode);
      return playbackModeLabel(playbackSourceState(this.video)?.mode ?? "auto");
    },
  };
}

function updateSubtitleSetting(
  art: Artplayer,
  state: SubtitleLoadState,
  requestSubtitles: () => Promise<void>
) {
  const keepOpen = art.setting.show;
  const setting = art.setting as ArtSettingWithRender;
  setting.update(createSubtitleSetting(state, requestSubtitles));
  if (!keepOpen) return;
  const subtitleSetting = setting.find("online-subtitle");
  if (subtitleSetting?.selector?.length) {
    setting.render(subtitleSetting.selector);
  }
  setting.show = true;
}

function setSubtitleSettingTooltip(art: Artplayer, tooltip: string) {
  const setting = art.setting.find("online-subtitle");
  if (!setting) return;
  setting.tooltip = tooltip;
  if (setting.$tooltip) setting.$tooltip.textContent = tooltip;
}

function createSubtitleSetting(
  state: SubtitleLoadState,
  requestSubtitles: () => Promise<void>
): PlayerSetting {
  const subtitles = state.subtitles;
  const tooltip =
    state.status === "empty"
      ? "无字幕"
      : state.status === "error"
        ? "加载失败"
        : state.status === "loading"
          ? "加载中"
          : state.status === "idle"
            ? ""
            : "关闭";
  return {
    name: "online-subtitle",
    html: "字幕",
    tooltip,
    // 二级面板要放下字幕名，比 ArtPlayer 默认的 200px 宽一些；
    // 240px 在 360px 宽的手机竖屏里仍然放得下。
    width: SUBTITLE_PANEL_WIDTH,
    selector: [
      {
        name: "online-subtitle-option-off",
        html: "关闭",
        value: "off",
        default: true,
      },
      ...subtitles.map((subtitle, index) => ({
        name: `online-subtitle-option-${index}`,
        html: subtitleOptionHtml(subtitle, index),
        value: String(index),
        default: false,
      })),
    ],
    mounted(panel) {
      panel.addEventListener("click", () => {
        if (state.status === "idle" || state.status === "error") {
          void requestSubtitles();
        }
      });
    },
    onSelect(this: Artplayer, item: SettingOption) {
      const value = String(item.value ?? "");
      if (value === "off") {
        setSubtitleVisible(this, false);
        return "关闭";
      }

      const index = Number(value);
      const subtitle = subtitles[index];
      if (!subtitle) {
        return escapeHtml(this.option.subtitle?.name || "字幕");
      }

      setSubtitleVisible(this, true);
      void this.subtitle.switch(subtitle.url, subtitleOption(subtitle));
      return escapeHtml(formatSubtitleTooltipLabel(subtitle, index));
    },
  };
}

/**
 * ArtPlayer 用 insertAdjacentHTML / innerHTML 渲染设置项，而字幕名字来自
 * 第三方接口，必须转义后再交给它。完整名字放在 title 里，悬停仍可查看。
 */
function subtitleOptionHtml(subtitle: PlayerSubtitle, index: number) {
  const label = escapeHtml(formatSubtitleOptionLabel(subtitle, index));
  const title = escapeHtml(formatSubtitleLabel(subtitle, index));
  return `<span class="video-player__subtitle-option" title="${title}">${label}</span>`;
}

function setSubtitleVisible(art: Artplayer, visible: boolean) {
  (art.subtitle as typeof art.subtitle & { show: boolean }).show = visible;
}

function isPlayerExpanded(art: Artplayer) {
  return Boolean(
    art.fullscreen || art.fullscreenWeb || getNativeFullscreenElement()
  );
}

/**
 * 安卓触摸全屏时可能留下一个不会再更新的 mousemove/hover 状态，导致
 * ArtPlayer 内置的控制栏隐藏判断一直认为用户仍悬停在进度条上。
 * 这里沿用 ArtPlayer 的隐藏延迟，但不依赖鼠标悬停状态做移动端兜底。
 */
function bindMobileFullscreenControlAutoHide(art: Artplayer) {
  if (!isMobilePlaybackDevice()) return noop;

  let hideTimer: number | null = null;

  function clearHideTimer() {
    if (hideTimer === null) return;
    window.clearTimeout(hideTimer);
    hideTimer = null;
  }

  function shouldWaitForInteraction() {
    return (
      art.setting.show ||
      art.isInput ||
      art.template.$player.classList.contains(FAST_RATE_CLASS)
    );
  }

  function hideControls() {
    hideTimer = null;
    if (!isPlayerExpanded(art) || !art.playing || !art.controls.show) return;
    if (shouldWaitForInteraction()) {
      scheduleHide();
      return;
    }
    art.controls.show = false;
  }

  function scheduleHide() {
    clearHideTimer();
    if (!isPlayerExpanded(art) || !art.playing || !art.controls.show) return;
    hideTimer = window.setTimeout(hideControls, Artplayer.CONTROL_HIDE_TIME);
  }

  function handleExpandedChange() {
    if (isPlayerExpanded(art)) {
      scheduleHide();
    } else {
      clearHideTimer();
    }
  }

  function handleControlChange(show: boolean) {
    if (show) {
      scheduleHide();
    } else {
      clearHideTimer();
    }
  }

  function handleSettingChange(show: boolean) {
    if (show) {
      clearHideTimer();
    } else {
      scheduleHide();
    }
  }

  function handlePlayerInteractionEnd(event: Event) {
    const target = event.target;
    if (!(target instanceof Node)) return;
    if (!art.template.$player.contains(target)) return;
    scheduleHide();
  }

  art.on("fullscreen", handleExpandedChange);
  art.on("fullscreenWeb", handleExpandedChange);
  art.on("video:playing", scheduleHide);
  art.on("video:pause", clearHideTimer);
  art.on("video:ended", clearHideTimer);
  art.on("control", handleControlChange);
  art.on("setting", handleSettingChange);
  art.on("document:pointerup", handlePlayerInteractionEnd);
  art.on("document:touchend", handlePlayerInteractionEnd);

  return () => {
    clearHideTimer();
    art.off("fullscreen", handleExpandedChange);
    art.off("fullscreenWeb", handleExpandedChange);
    art.off("video:playing", scheduleHide);
    art.off("video:pause", clearHideTimer);
    art.off("video:ended", clearHideTimer);
    art.off("control", handleControlChange);
    art.off("setting", handleSettingChange);
    art.off("document:pointerup", handlePlayerInteractionEnd);
    art.off("document:touchend", handlePlayerInteractionEnd);
  };
}

function setPlayerFastRateHint(art: Artplayer, active: boolean) {
  const player = art.template.$player;
  player.classList.toggle(FAST_RATE_CLASS, active);

  let hint = player.querySelector<HTMLElement>(`.${FAST_RATE_HINT_CLASS}`);
  if (!active) {
    hint?.remove();
    return;
  }

  if (!hint) {
    hint = document.createElement("div");
    hint.className = FAST_RATE_HINT_CLASS;
    hint.setAttribute("aria-hidden", "true");
    hint.textContent = `${FAST_RATE}x`;
    player.appendChild(hint);
  }
}

function showPlayerGestureHud(
  art: Artplayer,
  kind: PlayerGestureHudKind,
  value: string,
  duration = 680
) {
  const player = art.template.$player;
  const currentTimer = playerGestureHudTimers.get(player);
  if (currentTimer !== undefined) {
    window.clearTimeout(currentTimer);
  }

  let hud = player.querySelector<HTMLElement>(`.${PLAYER_GESTURE_HUD_CLASS}`);
  if (!hud) {
    hud = document.createElement("div");
    hud.setAttribute("aria-hidden", "true");
    player.appendChild(hud);
  }

  hud.className = [
    PLAYER_GESTURE_HUD_CLASS,
    `${PLAYER_GESTURE_HUD_CLASS}--${kind}`,
    kind === "volume" && value === "0%" ? `${PLAYER_GESTURE_HUD_CLASS}--muted` : "",
  ]
    .filter(Boolean)
    .join(" ");
  hud.replaceChildren();

  const icon = document.createElement("span");
  icon.className = PLAYER_GESTURE_HUD_ICON_CLASS;
  icon.innerHTML = playerGestureHudIcon(kind, value);

  const valueElement = document.createElement("span");
  valueElement.className = PLAYER_GESTURE_HUD_VALUE_CLASS;
  valueElement.textContent = value;

  hud.append(icon, valueElement);

  const timer = window.setTimeout(() => {
    hud?.remove();
    playerGestureHudTimers.delete(player);
  }, duration);
  playerGestureHudTimers.set(player, timer);
}

function clearPlayerGestureHud(art: Artplayer) {
  const player = art.template.$player;
  const currentTimer = playerGestureHudTimers.get(player);
  if (currentTimer !== undefined) {
    window.clearTimeout(currentTimer);
    playerGestureHudTimers.delete(player);
  }
  player.querySelector<HTMLElement>(`.${PLAYER_GESTURE_HUD_CLASS}`)?.remove();
}

function playerGestureHudIcon(kind: PlayerGestureHudKind, value: string) {
  if (kind === "brightness") {
    return `
      <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 640 640">
        <!--! Font Awesome Pro 7.3.1 by @fontawesome - https://fontawesome.com License - https://fontawesome.com/license (Commercial License) Copyright 2026 Fonticons, Inc. -->
        <path opacity=".4" fill="currentColor" d="M64 320C64 337.7 78.3 352 96 352L144 352C161.7 352 176 337.7 176 320C176 302.3 161.7 288 144 288L96 288C78.3 288 64 302.3 64 320zM137.4 137.4C124.9 149.9 124.9 170.2 137.4 182.7L169.4 214.7C181.9 227.2 202.2 227.2 214.7 214.7C227.2 202.2 227.2 181.9 214.7 169.4L182.7 137.4C170.2 124.9 149.9 124.9 137.4 137.4zM137.4 457.4C124.9 469.9 124.9 490.2 137.4 502.7C149.9 515.2 170.2 515.2 182.7 502.7L214.7 470.7C227.2 458.2 227.2 437.9 214.7 425.4C202.2 412.9 181.9 412.9 169.4 425.4L137.4 457.4zM288 96L288 144C288 161.7 302.3 176 320 176C337.7 176 352 161.7 352 144L352 96C352 78.3 337.7 64 320 64C302.3 64 288 78.3 288 96zM288 496L288 544C288 561.7 302.3 576 320 576C337.7 576 352 561.7 352 544L352 496C352 478.3 337.7 464 320 464C302.3 464 288 478.3 288 496zM425.4 169.4C412.9 181.9 412.9 202.2 425.4 214.7C437.9 227.2 458.2 227.2 470.7 214.7L502.7 182.7C515.2 170.2 515.2 149.9 502.7 137.4C490.2 124.9 469.9 124.9 457.4 137.4L425.4 169.4zM425.4 425.4C412.9 437.9 412.9 458.2 425.4 470.7L457.4 502.7C469.9 515.2 490.2 515.2 502.7 502.7C515.2 490.2 515.2 469.9 502.7 457.4L470.7 425.4C458.2 412.9 437.9 412.9 425.4 425.4zM464 320C464 337.7 478.3 352 496 352L544 352C561.7 352 576 337.7 576 320C576 302.3 561.7 288 544 288L496 288C478.3 288 464 302.3 464 320z"/>
        <path fill="currentColor" d="M224 320C224 267 267 224 320 224C373 224 416 267 416 320C416 373 373 416 320 416C267 416 224 373 224 320z"/>
      </svg>
    `;
  }

  const volumeMark = value === "0%"
    ? `<path d="M432 264L544 376M544 264L432 376" fill="none" stroke="currentColor" stroke-width="48" stroke-linecap="round"/>`
    : `<path fill="currentColor" d="M471.3 174.5C479.7 164.2 494.8 162.7 505.1 171C548.3 206.2 576 259.9 576 320C576 380.1 548.3 433.8 505.1 469C494.8 477.4 479.7 475.8 471.3 465.5C462.9 455.2 464.5 440.1 474.8 431.7C507.3 405.3 528 365.1 528 320C528 274.9 507.3 234.7 474.8 208.2C464.5 199.8 463 184.7 471.3 174.4zM410.8 249C419.2 238.7 434.3 237.2 444.6 245.5C466.2 263.1 480 289.9 480 320C480 350.1 466.1 376.9 444.6 394.5C434.3 402.9 419.2 401.3 410.8 391C402.4 380.7 404 365.6 414.3 357.2C425.1 348.4 432 335 432 320C432 305 425.1 291.6 414.3 282.7C404 274.3 402.5 259.2 410.8 248.9z"/>`;

  return `
    <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 640 640">
      <!--! Font Awesome Pro 7.3.1 by @fontawesome - https://fontawesome.com License - https://fontawesome.com/license (Commercial License) Copyright 2026 Fonticons, Inc. -->
      <path opacity=".4" fill="currentColor" d="M64 272L64 368C64 394.5 85.5 416 112 416L160 416L294.1 535.2C300.5 540.9 308.7 544 317.2 544C336.4 544 352 528.4 352 509.2L352 130.8C352 111.6 336.4 96 317.2 96C308.7 96 300.5 99.1 294.1 104.8L160 224L112 224C85.5 224 64 245.5 64 272z"/>
      ${volumeMark}
    </svg>
  `;
}

function noop() {
  // noop
}

type PlayerControl = NonNullable<Option["controls"]>[number];

function createPlayerControls(
  enableOrientationControl: boolean,
  enableTripleScreenControl: boolean
): PlayerControl[] {
  const controls: PlayerControl[] = [];
  if (enableTripleScreenControl) {
    controls.push(createTripleScreenControl());
  }
  if (enableOrientationControl) {
    controls.push(createOrientationControl());
  }
  return controls;
}

function createTripleScreenControl(): PlayerControl {
  return {
    name: TRIPLE_SCREEN_CONTROL_NAME,
    position: "right",
    index: 29,
    tooltip: "三屏画面",
    html: `
      <span class="video-player__triple-screen-control-icon" aria-hidden="true">
        <svg width="30" height="30" viewBox="6 6 20 20" fill="none" xmlns="http://www.w3.org/2000/svg">
          <path d="M10.5 9.875h3.125v12.25H10.5A1.125 1.125 0 0 1 9.375 21V11c0-.621.504-1.125 1.125-1.125z" stroke="currentColor" stroke-width="1.75"/>
          <path d="M18.25 9.75v12.5h-4.5V9.75h4.5z" stroke="currentColor" stroke-width="1.5"/>
          <path d="M21.5 9.875c.621 0 1.125.504 1.125 1.125v10c0 .621-.504 1.125-1.125 1.125h-3.125V9.875H21.5z" stroke="currentColor" stroke-width="1.75"/>
        </svg>
      </span>
    `,
    mounted(element) {
      element.dataset.tripleScreenControl = "";
      element.dataset.tripleScreenEligible = "false";
      element.setAttribute("role", "button");
      element.setAttribute("tabindex", "0");
      element.setAttribute("aria-pressed", "false");
      element.setAttribute("aria-label", "三屏画面");
      element.setAttribute("title", "三屏画面");
      this.events.proxy(element, "keydown", (event) => {
        const keyEvent = event as KeyboardEvent;
        if (keyEvent.key !== "Enter" && keyEvent.key !== " ") return;
        keyEvent.preventDefault();
        tripleScreenBindings.get(this)?.toggle();
      });
    },
    click() {
      tripleScreenBindings.get(this)?.toggle();
    },
  };
}

function bindTripleScreen(art: Artplayer, video: HTMLVideoElement, src: string) {
  const player = art.template.$player;
  const canvas = document.createElement("canvas");
  canvas.className = "video-player__triple-screen-canvas";
  canvas.setAttribute("aria-hidden", "true");
  player.appendChild(canvas);

  let eligible = false;
  // Keep display intent while a source is loading; the renderer only owns the
  // current video frames and may temporarily disable itself on emptied events.
  let requested = false;
  let pendingRelayEnable:
    | { time: number; shouldPlay: boolean; url: string }
    | null = null;
  let activePlaybackSrc = src;
  let pendingEnableSuccessNotice = false;
  const renderer = new TripleScreenRenderer({
    container: player,
    video,
    canvas,
    onVisibilityChange(visible) {
      player.classList.toggle(TRIPLE_SCREEN_ACTIVE_CLASS, visible);
      if (visible && pendingEnableSuccessNotice) {
        pendingEnableSuccessNotice = false;
        art.notice.show = "已开启三屏画面";
      }
    },
    onError(error) {
      requested = false;
      pendingRelayEnable = null;
      pendingEnableSuccessNotice = false;
      console.warn("[triple-screen] render failed", error);
      player.classList.remove(TRIPLE_SCREEN_ACTIVE_CLASS);
      updateTripleScreenControl(art, eligible, false);
      art.notice.show = tripleScreenErrorNotice(error);
    },
  });

  function rememberRequestedSource() {
    const controlled = playbackSourceState(video);
    const requestedURL = (controlled && playbackSourceURL(video)) || nonBlobPlaybackURL(art.url) ||
      nonBlobPlaybackURL(video.currentSrc);
    if (requestedURL) activePlaybackSrc = requestedURL;
  }

  function pendingSourceMatches(url: string) {
    if (playbackSourceState(video)) return samePlaybackURL(playbackSourceURL(video), url);
    const requestedURL = nonBlobPlaybackURL(art.url);
    if (requestedURL) return samePlaybackURL(requestedURL, url);
    const current = nonBlobPlaybackURL(video.currentSrc);
    if (current) return samePlaybackURL(current, url);
    return samePlaybackURL(activePlaybackSrc, url);
  }

  function clearPendingRelayEnable() {
    pendingRelayEnable = null;
    pendingEnableSuccessNotice = false;
  }

  function updateEligibility() {
    rememberRequestedSource();
    eligible = isPortraitVideo(video.videoWidth, video.videoHeight);
    if (
      pendingRelayEnable &&
      (!eligible || !pendingSourceMatches(pendingRelayEnable.url))
    ) {
      clearPendingRelayEnable();
    }
    if (!eligible) {
      renderer.disable();
      pendingEnableSuccessNotice = false;
    }
    if (
      pendingRelayEnable &&
      Number.isFinite(pendingRelayEnable.time)
    ) {
      video.currentTime = pendingRelayEnable.time;
    }
    updateTripleScreenControl(art, eligible, requested);
  }

  function enableRenderer(showSuccessNotice: boolean) {
    pendingEnableSuccessNotice = showSuccessNotice;
    const enabled = renderer.enable();
    if (!enabled) pendingEnableSuccessNotice = false;
    updateTripleScreenControl(art, true, enabled);
    return enabled;
  }

  function resetForNewSource() {
    rememberRequestedSource();
    eligible = false;
    renderer.disable();
    player.classList.remove(TRIPLE_SCREEN_ACTIVE_CLASS);
    updateTripleScreenControl(art, false, requested);
  }

  function resumeForCurrentSource() {
    if (!requested || !eligible) return;
    const state = playbackSourceState(video);
    if (state && (state.loading || state.error)) return;
    rememberRequestedSource();
    const source = state?.activeSource;
    const originalFile = !source || (source.kind === "original" && source.protocol === "file");
    const relaySrc = originalFile ? tripleScreenRelaySrc(activePlaybackSrc) : null;
    if (relaySrc && !isTripleScreenRelayURL(activePlaybackSrc)) {
      pendingRelayEnable = {
        time: Number.isFinite(video.currentTime) ? video.currentTime : 0,
        shouldPlay: !video.paused,
        url: relaySrc,
      };
      renderer.disable();
      updateTripleScreenControl(art, true, requested);
      art.notice.show = "正在切换到三屏中转播放";
      activePlaybackSrc = relaySrc;
      if (!setOriginalVideoDelivery(video, relaySrc)) art.url = relaySrc;
      return;
    }
    if (renderer.enabled || video.readyState < HTMLMediaElement.HAVE_CURRENT_DATA) return;
    const pending = pendingRelayEnable;
    pendingRelayEnable = null;
    if (enableRenderer(pendingEnableSuccessNotice) && pending?.shouldPlay) {
      void video.play().catch(() => undefined);
    }
  }

  function handleSourceChange() {
    const state = playbackSourceState(video);
    if (state?.loading || state?.error) resetForNewSource();
    else rememberRequestedSource();
  }

  const binding = {
    toggle() {
      if (!eligible) return;
      requested = !requested;
      if (!requested) {
        clearPendingRelayEnable();
        renderer.disable();
        updateTripleScreenControl(art, true, false);
        setOriginalVideoDelivery(video, null);
        art.notice.show = "已关闭三屏画面";
        return;
      }
      pendingEnableSuccessNotice = true;
      resumeForCurrentSource();
    },
    destroy() {
      clearPendingRelayEnable();
      renderer.destroy();
    },
  };

  tripleScreenBindings.set(art, binding);
  art.on("video:loadstart", resetForNewSource);
  art.on("video:loadedmetadata", updateEligibility);
  art.on("video:loadeddata", resumeForCurrentSource);
  art.on("video:canplay", resumeForCurrentSource);
  video.addEventListener("emptied", resetForNewSource);
  video.addEventListener(PLAYBACK_SOURCE_CHANGE, handleSourceChange);
  updateEligibility();

  return () => {
    art.off("video:loadstart", resetForNewSource);
    art.off("video:loadedmetadata", updateEligibility);
    art.off("video:loadeddata", resumeForCurrentSource);
    art.off("video:canplay", resumeForCurrentSource);
    video.removeEventListener("emptied", resetForNewSource);
    video.removeEventListener(PLAYBACK_SOURCE_CHANGE, handleSourceChange);
    tripleScreenBindings.delete(art);
    binding.destroy();
    player.classList.remove(TRIPLE_SCREEN_ACTIVE_CLASS);
  };
}

function tripleScreenRelaySrc(src: string) {
  if (typeof window === "undefined") return null;
  let url: URL;
  try {
    url = new URL(src, window.location.href);
  } catch {
    return null;
  }
  if (url.origin !== window.location.origin) return null;
  const pathname = url.pathname.toLowerCase();
  const canRelay =
    pathname.startsWith("/p/stream/") ||
    (pathname.startsWith("/p/share/") && pathname.endsWith("/stream"));
  if (!canRelay) return null;
  url.searchParams.set(TRIPLE_SCREEN_RELAY_QUERY, "1");
  return `${url.pathname}${url.search}${url.hash}`;
}

function isTripleScreenRelayURL(src: string) {
  if (!src || typeof window === "undefined") return false;
  try {
    return new URL(src, window.location.href).searchParams.get(
      TRIPLE_SCREEN_RELAY_QUERY
    ) === "1";
  } catch {
    return false;
  }
}

function nonBlobPlaybackURL(src: unknown) {
  if (typeof src !== "string" || !src || src.startsWith("blob:")) return "";
  return src;
}

function samePlaybackURL(left: string, right: string) {
  if (!left || !right || typeof window === "undefined") return left === right;
  try {
    return new URL(left, window.location.href).href ===
      new URL(right, window.location.href).href;
  } catch {
    return left === right;
  }
}

function tripleScreenErrorNotice(error: Error) {
  if (
    error.name === "SecurityError" ||
    /cross-origin|cross origin|origin-clean|tainted|security/i.test(error.message)
  ) {
    return "当前视频源受跨域限制，无法开启三屏画面";
  }
  return "当前浏览器或视频源不支持三屏画面";
}

function updateTripleScreenControl(
  art: Artplayer,
  eligible: boolean,
  enabled: boolean
) {
  const controls = (art as Artplayer & {
    controls?: Record<string, HTMLElement | undefined>;
  }).controls;
  const element = controls?.[TRIPLE_SCREEN_CONTROL_NAME];
  if (!element) return;

  element.dataset.tripleScreenEligible = eligible ? "true" : "false";
  element.setAttribute("aria-pressed", enabled ? "true" : "false");
  element.setAttribute("aria-label", "三屏画面");
  element.setAttribute("title", "三屏画面");
}

function createOrientationControl(): NonNullable<Option["controls"]>[number] {
  return {
    name: ORIENTATION_CONTROL_NAME,
    position: "right",
    index: 55,
    tooltip: "横竖屏切换",
    html: `
      <span class="video-player__orientation-control-icon video-player__orientation-control-icon--to-landscape" aria-hidden="true">
        <svg width="24" height="24" viewBox="0 0 24 24" fill="none">
          <path d="M14.4 11.2h2.7c1.7 0 3 1.3 3 3v4.1c0 1.7-1.3 3-3 3h-3.8" fill="none" stroke="currentColor" stroke-opacity=".42" stroke-width="2.3" stroke-linecap="round" stroke-linejoin="round"/>
          <rect x="3.1" y="6.7" width="9.7" height="14.1" rx="2.4" fill="none" stroke="currentColor" stroke-width="2.3"/>
          <path d="M11.8 2.8h2.9c2.6 0 4.7 1.8 5 4.2" fill="none" stroke="currentColor" stroke-width="2.3" stroke-linecap="round"/>
          <path d="M17.4 4.6 19.8 7 22 4.5" fill="none" stroke="currentColor" stroke-width="2.3" stroke-linecap="round" stroke-linejoin="round"/>
        </svg>
      </span>
      <span class="video-player__orientation-control-icon video-player__orientation-control-icon--to-portrait" aria-hidden="true">
        <svg width="24" height="24" viewBox="0 0 24 24" fill="none">
          <g transform="rotate(180 12 12)">
            <path d="M12.8 14.4v2.7c0 1.7-1.3 3-3 3H5.7c-1.7 0-3-1.3-3-3v-3.8" fill="none" stroke="currentColor" stroke-opacity=".42" stroke-width="2.3" stroke-linecap="round" stroke-linejoin="round"/>
            <rect x="3.2" y="3.1" width="14.1" height="9.7" rx="2.4" fill="none" stroke="currentColor" stroke-width="2.3"/>
            <path d="M21.2 11.8v2.9c0 2.6-1.8 4.7-4.2 5" fill="none" stroke="currentColor" stroke-width="2.3" stroke-linecap="round"/>
            <path d="M19.4 17.4 17 19.8 19.5 22" fill="none" stroke="currentColor" stroke-width="2.3" stroke-linecap="round" stroke-linejoin="round"/>
          </g>
        </svg>
      </span>
    `,
    mounted(element) {
      element.setAttribute("role", "button");
      element.setAttribute("tabindex", "0");
      updateOrientationControl(this, element);
      this.events.proxy(element, "keydown", (event) => {
        const keyEvent = event as KeyboardEvent;
        if (keyEvent.key !== "Enter" && keyEvent.key !== " ") return;
        keyEvent.preventDefault();
        void togglePlayerOrientation(this);
      });
    },
    click() {
      void togglePlayerOrientation(this);
    },
  };
}

function bindOrientationToggle(art: Artplayer) {
  function handleResize() {
    updateManualWebOrientation(art);
    updateOrientationControl(art);
  }

  function handleFullscreenWeb(state: boolean) {
    if (!state && getManualOrientationKind(art) === "web") {
      clearManualOrientation(art);
      return;
    }
    handleResize();
  }

  function handleFullscreen(state: boolean) {
    if (!state && getManualOrientationKind(art) === "native") {
      clearManualOrientation(art);
      return;
    }
    updateOrientationControl(art);
  }

  window.addEventListener("resize", handleResize);
  window.addEventListener("orientationchange", handleResize);
  getScreenOrientation()?.addEventListener?.("change", handleResize);
  art.on("fullscreenWeb", handleFullscreenWeb);
  art.on("fullscreen", handleFullscreen);
  updateOrientationControl(art);

  return () => {
    clearManualOrientation(art);
    window.removeEventListener("resize", handleResize);
    window.removeEventListener("orientationchange", handleResize);
    getScreenOrientation()?.removeEventListener?.("change", handleResize);
    art.off("fullscreenWeb", handleFullscreenWeb);
    art.off("fullscreen", handleFullscreen);
  };
}

async function togglePlayerOrientation(art: Artplayer) {
  const target = nextOrientationTarget(art);
  const locked = await lockNativeOrientation(art, target);
  if (locked) {
    clearManualWebRotation(art);
    setManualOrientation(art, target, "native");
    art.notice.show = `已切换${orientationLabel(target)}`;
    updateOrientationControl(art);
    return;
  }

  await exitNativeFullscreen();
  if (!art.fullscreenWeb) {
    art.fullscreenWeb = true;
  }
  setManualOrientation(art, target, "web");
  updateManualWebOrientation(art);
  art.notice.show = `已切换${orientationLabel(target)}`;
  updateOrientationControl(art);
}

async function lockNativeOrientation(
  art: Artplayer,
  target: OrientationMode
) {
  const orientation = getScreenOrientation();
  if (!orientation?.lock) return false;

  try {
    const fullscreen = await requestNativeFullscreen(art.template.$player);
    if (!fullscreen) return false;
    await orientation.lock(target);
    return true;
  } catch {
    return false;
  }
}

async function requestNativeFullscreen(element: HTMLElement) {
  if (getNativeFullscreenElement()) return true;
  const target = element as FullscreenElement;
  try {
    if (target.requestFullscreen) {
      await target.requestFullscreen({ navigationUI: "hide" });
      return true;
    }
    const request =
      target.webkitRequestFullscreen ||
      target.mozRequestFullScreen ||
      target.msRequestFullscreen;
    if (!request) return false;
    await maybePromise(request.call(target));
    return true;
  } catch {
    return false;
  }
}

async function exitNativeFullscreen() {
  if (!getNativeFullscreenElement()) return;
  const doc = document as FullscreenDocument;
  const exit =
    doc.exitFullscreen ||
    doc.webkitExitFullscreen ||
    doc.mozCancelFullScreen ||
    doc.msExitFullscreen;
  if (!exit) return;
  try {
    await maybePromise(exit.call(document));
  } catch {
    // ignore
  }
}

function getNativeFullscreenElement() {
  const doc = document as FullscreenDocument;
  return (
    document.fullscreenElement ||
    doc.webkitFullscreenElement ||
    doc.mozFullScreenElement ||
    doc.msFullscreenElement ||
    null
  );
}

function getScreenOrientation() {
  return window.screen?.orientation as LockableScreenOrientation | undefined;
}

async function maybePromise(value: Promise<void> | void) {
  if (value && typeof value.then === "function") {
    await value;
  }
}

function nextOrientationTarget(art: Artplayer): OrientationMode {
  const active = getManualOrientationTarget(art) ?? getViewportOrientation();
  return active === "landscape" ? "portrait" : "landscape";
}

function getViewportOrientation(): OrientationMode {
  const type = getScreenOrientation()?.type;
  if (type?.startsWith("landscape")) return "landscape";
  if (type?.startsWith("portrait")) return "portrait";
  return window.innerWidth > window.innerHeight ? "landscape" : "portrait";
}

function setManualOrientation(
  art: Artplayer,
  target: OrientationMode,
  kind: OrientationKind
) {
  const { dataset } = art.template.$player;
  dataset.videoPlayerOrientationTarget = target;
  dataset.videoPlayerOrientationKind = kind;
}

function getManualOrientationTarget(art: Artplayer) {
  const value = art.template.$player.dataset.videoPlayerOrientationTarget;
  return value === "landscape" || value === "portrait" ? value : null;
}

function getManualOrientationKind(art: Artplayer) {
  const value = art.template.$player.dataset.videoPlayerOrientationKind;
  return value === "native" || value === "web" ? value : null;
}

function clearManualOrientation(art: Artplayer) {
  const kind = getManualOrientationKind(art);
  delete art.template.$player.dataset.videoPlayerOrientationTarget;
  delete art.template.$player.dataset.videoPlayerOrientationKind;
  clearManualWebRotation(art);
  if (kind === "native") {
    try {
      getScreenOrientation()?.unlock?.();
    } catch {
      // ignore
    }
  }
  updateOrientationControl(art);
}

function updateManualWebOrientation(art: Artplayer) {
  if (getManualOrientationKind(art) !== "web") return;
  const target = getManualOrientationTarget(art);
  if (!target) return;
  if (!art.fullscreenWeb) {
    clearManualOrientation(art);
    return;
  }
  if (target !== getViewportOrientation()) {
    applyManualWebRotation(art);
  } else {
    clearManualWebRotation(art);
  }
}

function applyManualWebRotation(art: Artplayer) {
  const player = art.template.$player;
  const viewWidth = document.documentElement.clientWidth;
  const viewHeight = document.documentElement.clientHeight;
  player.style.width = `${viewHeight}px`;
  player.style.height = `${viewWidth}px`;
  player.style.transformOrigin = "0 0";
  player.style.transform = `rotate(90deg) translate(0, -${viewWidth}px)`;
  player.classList.add(MANUAL_ORIENTATION_CLASS);
  art.emit("resize");
}

function clearManualWebRotation(art: Artplayer) {
  const player = art.template.$player;
  player.classList.remove(MANUAL_ORIENTATION_CLASS);
  player.style.transform = "";
  player.style.transformOrigin = "";
  if (art.fullscreenWeb) {
    player.style.width = "100%";
    player.style.height = "100%";
  } else {
    player.style.width = "";
    player.style.height = "";
  }
  art.emit("resize");
}

function updateOrientationControl(art: Artplayer, mountedElement?: HTMLElement) {
  const controls = (art as Artplayer & {
    controls?: Record<string, HTMLElement | undefined>;
  }).controls;
  const element = mountedElement ?? controls?.[ORIENTATION_CONTROL_NAME];
  if (!element) return;
  const next = nextOrientationTarget(art);
  const label = `切换${orientationLabel(next)}`;
  element.dataset.nextOrientation = next;
  element.setAttribute("aria-label", label);
  element.setAttribute("title", label);
}

function orientationLabel(mode: OrientationMode) {
  return mode === "landscape" ? "横屏" : "竖屏";
}

function bindPlayerBrightness(art: Artplayer) {
  applyPlayerBrightness(art, DEFAULT_SETTINGS.brightness);
  function restoreBrightness() {
    // ArtPlayer restores the pre-fullscreen inline styles when leaving web fullscreen.
    applyPlayerBrightness(art, getPlayerBrightness(art));
  }
  art.on("fullscreenWeb", restoreBrightness);
  return () => {
    art.off("fullscreenWeb", restoreBrightness);
    playerBrightness.delete(art);
  };
}

function applyPlayerBrightness(art: Artplayer, brightness: number) {
  const value = Number(clamp(brightness, BRIGHTNESS_MIN, BRIGHTNESS_MAX).toFixed(2));
  playerBrightness.set(art, value);
  art.template.$player.style.setProperty(
    "--video-player-brightness",
    value.toFixed(2)
  );
}

function getPlayerBrightness(art: Artplayer) {
  return playerBrightness.get(art) ?? DEFAULT_SETTINGS.brightness;
}

function seekGestureLabel(
  startTime: number,
  targetTime: number,
  duration: number
) {
  const action = targetTime >= startTime ? "快进" : "快退";
  return `${action} ${formatClock(targetTime)} / ${formatClock(duration)}`;
}

function formatBrightnessPercent(brightness: number) {
  const normalized =
    (clamp(brightness, BRIGHTNESS_MIN, BRIGHTNESS_MAX) - BRIGHTNESS_MIN) /
    (BRIGHTNESS_MAX - BRIGHTNESS_MIN);
  return formatPercent(normalized);
}

function createHlsSourceLoader() {
  return (video: HTMLVideoElement, url: string) => attachVideoSource(video, url);
}

function bindLongPressFast(
  video: HTMLVideoElement,
  onFastChange: (active: boolean) => void
) {
  let pressTimer: number | null = null;
  let fastActive = false;
  let previousRate = NORMAL_RATE;
  let suppressNextClick = false;

  function clearPressTimer() {
    if (pressTimer !== null) {
      window.clearTimeout(pressTimer);
      pressTimer = null;
    }
  }

  function setFast(next: boolean) {
    if (fastActive === next) return;
    if (next) {
      previousRate =
        Number.isFinite(video.playbackRate) && video.playbackRate > 0
          ? video.playbackRate
          : NORMAL_RATE;
    }
    fastActive = next;
    video.playbackRate = next ? FAST_RATE : previousRate;
    onFastChange(next);
  }

  function activateFast() {
    if (video.paused || video.ended) return;
    setFast(true);
  }

  function startPress() {
    if (video.paused || video.ended) return;
    clearPressTimer();
    pressTimer = window.setTimeout(() => {
      pressTimer = null;
      activateFast();
    }, LONG_PRESS_MS);
  }

  function endPress(suppressClick = false) {
    clearPressTimer();
    const wasFastActive = fastActive;
    setFast(false);
    if (wasFastActive && suppressClick) {
      suppressNextClick = true;
    }
  }

  function handleMouseDown(event: MouseEvent) {
    if (event.button !== 0) return;
    startPress();
  }

  function handleMouseUp(event: MouseEvent) {
    if (event.button !== 0) return;
    endPress(true);
  }

  function handlePressEnd() {
    endPress();
  }

  function handleClick(event: MouseEvent) {
    if (!suppressNextClick) return;
    suppressNextClick = false;
    event.preventDefault();
    event.stopPropagation();
    event.stopImmediatePropagation();
  }

  video.addEventListener("mousedown", handleMouseDown);
  video.addEventListener("mouseup", handleMouseUp);
  video.addEventListener("click", handleClick, true);
  video.addEventListener("mouseleave", handlePressEnd);
  video.addEventListener("pause", handlePressEnd);
  video.addEventListener("ended", handlePressEnd);

  return () => {
    clearPressTimer();
    setFast(false);
    video.removeEventListener("mousedown", handleMouseDown);
    video.removeEventListener("mouseup", handleMouseUp);
    video.removeEventListener("click", handleClick, true);
    video.removeEventListener("mouseleave", handlePressEnd);
    video.removeEventListener("pause", handlePressEnd);
    video.removeEventListener("ended", handlePressEnd);
  };
}

function bindMobilePlayerGestures(
  art: Artplayer,
  video: HTMLVideoElement,
  onFastChange: (active: boolean) => void,
  onGestureHud: (label: string, duration?: number) => void
) {
  if (!shouldEnableMobileGestures()) return noop;

  const player = art.template.$player;
  let state: MobileGestureState | null = null;

  function clearPressTimer() {
    if (!state || state.pressTimer === null) return;
    window.clearTimeout(state.pressTimer);
    state.pressTimer = null;
  }

  function setTouchFast(next: boolean) {
    if (!state || state.fastActive === next) return;
    if (next) {
      state.previousRate =
        Number.isFinite(video.playbackRate) && video.playbackRate > 0
          ? video.playbackRate
          : NORMAL_RATE;
      state.fastActive = true;
      onFastChange(true);
      video.playbackRate = FAST_RATE;
      return;
    }

    const previousRate = state.previousRate;
    state.fastActive = false;
    onFastChange(false);
    video.playbackRate = previousRate;
  }

  function resetGesture() {
    clearPressTimer();
    if (state?.fastActive) {
      setTouchFast(false);
    }
    state = null;
  }

  function handleTouchStart(event: TouchEvent) {
    resetGesture();
    if (event.touches.length !== 1 || art.isLock) return;

    const touch = event.touches[0];
    const rect = player.getBoundingClientRect();
    const frame: PlayerGestureFrame = {
      left: rect.left,
      top: rect.top,
      width: rect.width,
      height: rect.height,
      rotated: player.classList.contains(MANUAL_ORIENTATION_CLASS) || art.isRotate,
    };
    const startPoint = getPlayerGesturePoint(touch, frame);
    state = {
      frame,
      startPoint,
      startTime: video.currentTime || 0,
      startVolume: video.muted ? 0 : clamp(video.volume, 0, 1),
      startBrightness: getPlayerBrightness(art),
      side: startPoint.x < startPoint.width / 2 ? "left" : "right",
      mode: null,
      targetTime: video.currentTime || 0,
      moved: false,
      fastActive: false,
      previousRate: video.playbackRate || NORMAL_RATE,
      pressTimer: null,
    };

    state.pressTimer = window.setTimeout(() => {
      if (!state || state.mode || state.moved || video.paused || video.ended) {
        return;
      }
      setTouchFast(true);
    }, LONG_PRESS_MS);
  }

  function lockGestureMode(dx: number, dy: number) {
    if (!state) return;
    const absX = Math.abs(dx);
    const absY = Math.abs(dy);
    if (absX < GESTURE_ACTIVATION_PX && absY < GESTURE_ACTIVATION_PX) return;

    state.moved = true;
    clearPressTimer();

    if (absX >= absY * GESTURE_DIRECTION_LOCK_RATIO) {
      state.mode = "seek";
      return;
    }

    if (absY >= absX * GESTURE_DIRECTION_LOCK_RATIO) {
      if (!isPlayerExpanded(art)) {
        resetGesture();
        return;
      }
      state.mode = state.side === "right" ? "volume" : "brightness";
    }
  }

  function handleTouchMove(event: TouchEvent) {
    if (!state) return;
    if (event.touches.length !== 1 || art.isLock) {
      resetGesture();
      return;
    }

    const touch = event.touches[0];
    const point = getPlayerGesturePoint(touch, state.frame);
    const dx = point.x - state.startPoint.x;
    const dy = point.y - state.startPoint.y;

    if (state.fastActive) {
      event.preventDefault();
      return;
    }

    if (!state.mode) {
      lockGestureMode(dx, dy);
      if (!state || !state.mode) return;
    }

    event.preventDefault();

    if (state.mode === "seek") {
      handleSeekGesture(event, dx);
      return;
    }

    if (state.mode === "volume") {
      handleVolumeGesture(point.y);
      return;
    }

    handleBrightnessGesture(point.y);
  }

  function handleSeekGesture(event: TouchEvent, dx: number) {
    if (!state) return;
    const duration = video.duration;
    if (!Number.isFinite(duration) || duration <= 0) return;
    const targetTime = clamp(
      state.startTime + (dx / Math.max(1, state.startPoint.width)) * duration,
      0,
      duration
    );
    state.targetTime = targetTime;
    art.emit("setBar", "played", targetTime / duration, event);
    if (!isPlayerExpanded(art)) return;
    onGestureHud(seekGestureLabel(state.startTime, targetTime, duration), 560);
  }

  function handleVolumeGesture(currentY: number) {
    if (!state) return;
    const delta =
      (state.startPoint.y - currentY) / Math.max(1, state.startPoint.height);
    const volume = setPlayerGestureVolume(video, state.startVolume + delta);
    if (volume === null) {
      clearPlayerGestureHud(art);
      art.notice.show = "当前浏览器不支持音量调节";
      resetGesture();
      return;
    }
    showPlayerGestureHud(art, "volume", formatPercent(volume));
  }

  function handleBrightnessGesture(currentY: number) {
    if (!state) return;
    const delta =
      ((state.startPoint.y - currentY) / Math.max(1, state.startPoint.height)) *
      GESTURE_VERTICAL_SCALE;
    const nextBrightness = clamp(
      state.startBrightness + delta,
      BRIGHTNESS_MIN,
      BRIGHTNESS_MAX
    );
    applyPlayerBrightness(art, nextBrightness);
    showPlayerGestureHud(art, "brightness", formatBrightnessPercent(nextBrightness));
  }

  function handleTouchEnd() {
    if (!state) return;

    if (state.mode === "seek") {
      const duration = video.duration;
      if (Number.isFinite(duration) && duration > 0) {
        art.seek = clamp(state.targetTime, 0, duration);
        if (isPlayerExpanded(art)) {
          onGestureHud(
            seekGestureLabel(state.startTime, state.targetTime, duration),
            720
          );
        }
      }
    }

    resetGesture();
  }

  video.addEventListener("touchstart", handleTouchStart, { passive: true });
  video.addEventListener("touchmove", handleTouchMove, { passive: false });
  video.addEventListener("touchend", handleTouchEnd);
  video.addEventListener("touchcancel", resetGesture);
  video.addEventListener("pause", resetGesture);
  video.addEventListener("ended", resetGesture);
  window.addEventListener("blur", resetGesture);
  art.on("resize", resetGesture);
  art.on("fullscreen", resetGesture);
  art.on("fullscreenWeb", resetGesture);

  return () => {
    clearPlayerGestureHud(art);
    resetGesture();
    video.removeEventListener("touchstart", handleTouchStart);
    video.removeEventListener("touchmove", handleTouchMove);
    video.removeEventListener("touchend", handleTouchEnd);
    video.removeEventListener("touchcancel", resetGesture);
    video.removeEventListener("pause", resetGesture);
    video.removeEventListener("ended", resetGesture);
    window.removeEventListener("blur", resetGesture);
    art.off("resize", resetGesture);
    art.off("fullscreen", resetGesture);
    art.off("fullscreenWeb", resetGesture);
  };
}

function bindProgressPreview(
  art: Artplayer,
  video: HTMLVideoElement,
  mount: HTMLDivElement,
  onPreviewHover: (hover: PreviewHover | null) => void
) {
  const progress = art.query<HTMLElement>(".art-progress");
  if (!progress) return () => undefined;
  const progressEl = progress;

  function update(event: PointerEvent | MouseEvent) {
    if ("pointerType" in event && event.pointerType === "touch") return;
    const duration = video.duration;
    if (!Number.isFinite(duration) || duration <= 0) return;
    const rect = progressEl.getBoundingClientRect();
    const hostRect = mount.getBoundingClientRect();
    const ratio = clamp((event.clientX - rect.left) / Math.max(1, rect.width), 0, 1);
    const edge = Math.min(PREVIEW_WIDTH / 2 + 8, hostRect.width / 2);
    const maxX = Math.max(edge, hostRect.width - edge);
    onPreviewHover({
      x: clamp(event.clientX - hostRect.left, edge, maxX),
      ratio,
      time: ratio * duration,
    });
  }

  function hide() {
    onPreviewHover(null);
  }

  progressEl.addEventListener("pointermove", update);
  progressEl.addEventListener("pointerdown", update);
  progressEl.addEventListener("pointerleave", hide);
  window.addEventListener("pointerup", hide);
  window.addEventListener("blur", hide);

  return () => {
    progressEl.removeEventListener("pointermove", update);
    progressEl.removeEventListener("pointerdown", update);
    progressEl.removeEventListener("pointerleave", hide);
    window.removeEventListener("pointerup", hide);
    window.removeEventListener("blur", hide);
  };
}

function mediaErrorMessage(error: MediaError | null) {
  switch (error?.code) {
    case MediaError.MEDIA_ERR_ABORTED:
      return "视频加载已取消，请重试";
    case MediaError.MEDIA_ERR_NETWORK:
      return "视频源网络连接失败，请稍后重试";
    case MediaError.MEDIA_ERR_DECODE:
      return "视频编码无法解码，可尝试换用兼容的浏览器";
    case MediaError.MEDIA_ERR_SRC_NOT_SUPPORTED:
      return "浏览器未能加载视频源，可能是网盘连接、地址失效或视频格式问题";
    default:
      return "视频源暂时无法播放，请重试";
  }
}

function syncPreviewVideo(video: HTMLVideoElement | null, ratio: number) {
  if (!video || !Number.isFinite(video.duration) || video.duration <= 0) return;
  const target = clamp(ratio * video.duration, 0, Math.max(0, video.duration - 0.05));
  if (Math.abs(video.currentTime - target) > 0.25) {
    try {
      video.currentTime = target;
    } catch {
      // ignore
    }
  }
}

function clamp(n: number, min: number, max: number) {
  return n < min ? min : n > max ? max : n;
}

function formatClock(seconds: number) {
  if (!Number.isFinite(seconds) || seconds < 0) return "00:00";
  const total = Math.floor(seconds);
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  if (h > 0) {
    return `${String(h).padStart(2, "0")}:${String(m).padStart(
      2,
      "0"
    )}:${String(s).padStart(2, "0")}`;
  }
  return `${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
}

function formatPercent(value: number) {
  return `${Math.round(clamp(value, 0, 1) * 100)}%`;
}
