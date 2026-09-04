import {
  useEffect,
  useRef,
  useState,
  type CSSProperties,
  type MutableRefObject,
} from "react";
import Artplayer, { type Option, type SettingOption } from "artplayer";
import type Hls from "hls.js";
import {
  classifyTapZone,
  computeDoubleTapSeekTime,
  DOUBLE_TAP_CHAIN_WINDOW_MS,
  formatDoubleTapSeekLabel,
  reduceDoubleTap,
  type DoubleTapAction,
  type SeekChain,
  type SeekChainSide,
  type TapKind,
  type TapZone,
} from "@/lib/doubleTapSeek";
import {
  calculateFullscreenSubtitleBottom,
  getFullscreenPlayerOrientation,
} from "@/lib/fullscreenSubtitleLayout";
import { diagnosePlaybackSource } from "@/lib/playbackError";
import {
  formatPlaybackRateLabel,
  formatPlaybackRateOptionLabel,
  matchPlaybackRateOption,
  normalizePlaybackRate,
  NORMAL_PLAYBACK_RATE,
  PLAYBACK_RATE_OPTIONS,
} from "@/lib/playbackRate";
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

type VideoElementWithHls = HTMLVideoElement & {
  __hls?: Hls | null;
};

type MobileGestureMode = "seek" | "volume" | "brightness";
type MobileGestureSide = "left" | "right";
type PlayerGestureHudKind = "volume" | "brightness";
type KeyboardSeekKey = "ArrowLeft" | "ArrowRight";
type MobileGestureState = {
  startX: number;
  startY: number;
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
const NORMAL_RATE = NORMAL_PLAYBACK_RATE;
/** ArtPlayer 内部播放失败自动重连次数。 */
const ARTPLAYER_RECONNECT_TIME_MAX = 3;
/** 播放状态下控制栏无操作后自动隐藏的时间。 */
const ARTPLAYER_CONTROL_HIDE_TIME_MS = 2_000;
/** 键盘左右键单次快进/快退秒数。 */
const KEYBOARD_SEEK_SECONDS = 15;
/** 浏览器丢失 keyup 时，最后一次重复按键后的兜底提交延迟。 */
const KEYBOARD_SEEK_IDLE_COMMIT_MS = 1_500;

Artplayer.FAST_FORWARD_VALUE = FAST_RATE;
Artplayer.RECONNECT_TIME_MAX = ARTPLAYER_RECONNECT_TIME_MAX;
Artplayer.CONTROL_HIDE_TIME = ARTPLAYER_CONTROL_HIDE_TIME_MS;
// 控制栏倍速按钮、设置面板“播放速度”和桌面端右键菜单共用同一份档位。
Artplayer.PLAYBACK_RATE = PLAYBACK_RATE_OPTIONS;
// ArtPlayer 的双击动作写死在它的点击派发里：先 emit dblclick，再按自己的
// UA 判断执行“移动端切换播放 / 桌面端切换全屏”，回调里无法取消。它的移动端
// 判断口径又和本文件按指针类型判断的不一致（例如带触摸屏的桌面浏览器），
// 所以这里关掉两个内置开关，双击语义统一由 bindPlayerDoubleClickActions 实现。
Artplayer.MOBILE_DBCLICK_PLAY = false;
Artplayer.DBCLICK_FULLSCREEN = false;

const DEFAULT_SETTINGS: PlayerSettings = {
  volume: 1,
  muted: false,
  playbackRate: 1,
  brightness: 1,
  loop: true,
};
const DEFAULT_SETTING_LAYOUT = {
  width: Artplayer.SETTING_WIDTH,
  itemWidth: Artplayer.SETTING_ITEM_WIDTH,
  itemHeight: Artplayer.SETTING_ITEM_HEIGHT,
};
const COMPACT_SETTING_LAYOUT = {
  width: 172,
  itemWidth: 148,
  itemHeight: 30,
};
const ORIENTATION_CONTROL_NAME = "orientationToggle";
const PLAYBACK_RATE_CONTROL_NAME = "playbackRateToggle";
const PLAYBACK_RATE_VALUE_CLASS = "art-selector-value";
const PLAYBACK_RATE_ITEM_CLASS = "art-selector-item";
const PLAYBACK_RATE_LIST_CLASS = "art-selector-list";
const TRIPLE_SCREEN_CONTROL_NAME = "tripleScreen";
const TRIPLE_SCREEN_RELAY_QUERY = "tripleScreenRelay";
const MANUAL_ORIENTATION_CLASS = "art-manual-orientation";
const TRIPLE_SCREEN_ACTIVE_CLASS = "art-triple-screen-active";
const FAST_RATE_CLASS = "art-fast-rate-active";
const FAST_RATE_HINT_CLASS = "video-player__art-rate-hint";
const PLAYER_GESTURE_HUD_CLASS = "video-player__art-gesture-hud";
const PLAYER_GESTURE_HUD_ICON_CLASS = "video-player__art-gesture-hud-icon";
const PLAYER_GESTURE_HUD_VALUE_CLASS = "video-player__art-gesture-hud-value";
const SEEK_RIPPLE_CLASS = "video-player__art-seek-ripple";
const SEEK_RIPPLE_LEAVING_CLASS = "video-player__art-seek-ripple--leaving";
/** 与 CSS 里淡出动画的时长一致，动画结束后才移除节点。 */
const SEEK_RIPPLE_LEAVE_MS = 220;
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
const seekRippleTimers = new WeakMap<HTMLElement, number>();
const tripleScreenBindings = new WeakMap<
  Artplayer,
  { toggle: () => void; destroy: () => void }
>();

export function VideoPlayer({
  src,
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
    setPreviewHover(null);

    const cleanupPlayer = mountArtPlayer({
      mount,
      src,
      preferTypedMp4SourceOnIOS,
      poster,
      title,
      loadSubtitlesRef,
      artRef,
      playedRef,
      onFirstPlayRef,
      onFastChange: noop,
      onError: setPlayerError,
      onPreviewHover: setPreviewHover,
      onGestureHud: showGestureHud,
    });

    return cleanupPlayer;
  }, [poster, preferTypedMp4SourceOnIOS, retryNonce, src, title]);

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
    setRetryNonce((n) => n + 1);
  }

  async function copySource() {
    const absolute = new URL(src, window.location.href).href;
    try {
      await navigator.clipboard.writeText(absolute);
      showGestureHud("播放地址已复制", 900);
    } catch {
      fallbackCopyText(absolute);
      showGestureHud("播放地址已复制", 900);
    }
  }

  const previewStyle = previewHover
    ? ({ left: `${previewHover.x}px` } as CSSProperties)
    : undefined;

  return (
    <div className="video-player">
      <div ref={mountRef} className="video-player__mount" />

      {playerError && (
        <div className="video-player__error" role="alert">
          <div className="video-player__error-title">{playerError.title}</div>
          <div className="video-player__error-message">{playerError.message}</div>
          <div className="video-player__error-actions">
            <button type="button" onClick={retryPlayback}>
              重试
            </button>
            <button type="button" onClick={copySource}>
              复制地址
            </button>
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
  if (isBackendNativeVideoRoute(cleanPath)) return "mp4";
  return undefined;
}

function isBackendNativeVideoRoute(cleanPath: string) {
  const pathname = sourcePathname(cleanPath);
  return (
    pathname.startsWith("/p/stream/") ||
    pathname.startsWith("/p/upload/") ||
    pathname.startsWith("/p/share/")
  );
}

function sourcePathname(src: string) {
  if (src.startsWith("http://") || src.startsWith("https://")) {
    try {
      return new URL(src).pathname.toLowerCase();
    } catch {
      return src;
    }
  }
  return src;
}

function mountArtPlayer({
  mount,
  src,
  preferTypedMp4SourceOnIOS,
  poster,
  loadSubtitlesRef,
  title,
  artRef,
  playedRef,
  onFirstPlayRef,
  onFastChange,
  onError,
  onPreviewHover,
  onGestureHud,
}: {
  mount: HTMLDivElement;
  src: string;
  preferTypedMp4SourceOnIOS: boolean;
  poster: string;
  loadSubtitlesRef: MutableRefObject<Props["loadSubtitles"]>;
  title: string;
  artRef: MutableRefObject<Artplayer | null>;
  playedRef: MutableRefObject<boolean>;
  onFirstPlayRef: MutableRefObject<Props["onFirstPlay"]>;
  onFastChange: (active: boolean) => void;
  onError: (error: PlayerError | null) => void;
  onPreviewHover: (hover: PreviewHover | null) => void;
  onGestureHud: (label: string, duration?: number) => void;
}) {
  const useTypedMp4Source =
    preferTypedMp4SourceOnIOS && isIOSPlaybackDevice();
  const sourceType = useTypedMp4Source
    ? TYPED_MP4_SOURCE_TYPE
    : inferSourceType(src);
  const subtitleState: SubtitleLoadState = { status: "idle", subtitles: [] };
  const fastActiveRef = { current: false };
  const loadHlsSource = createHlsSourceLoader(onError);
  const enableOrientationControl = shouldEnableMobileOrientationControl();
  const enableTripleScreenControl = shouldEnableTripleScreenControl();
  const enableWebFullscreen = shouldEnableWebFullscreen(enableOrientationControl);
  let disposed = false;
  let playbackErrorActive = false;
  let diagnosticController: AbortController | null = null;
  let diagnosticPromise: Promise<string | null> | null = null;
  let diagnosticMessage: string | null = null;
  configureArtPlayerSettingLayout(
    shouldUseCompactPlayerSettings(mount, enableOrientationControl)
  );
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
  artRef.current = art;

  const video = art.video as VideoElementWithHls;
  video.setAttribute("referrerpolicy", MEDIA_REFERRER_POLICY);
  video.setAttribute("aria-label", title);
  video.setAttribute("controlsList", "nodownload");
  video.setAttribute("webkit-playsinline", "true");
  video.disablePictureInPicture = false;
  video.loop = DEFAULT_SETTINGS.loop;
  video.playbackRate = DEFAULT_SETTINGS.playbackRate;
  applyPlayerBrightness(art, DEFAULT_SETTINGS.brightness);

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
  const unbindDoubleClickActions = bindPlayerDoubleClickActions(art);
  const unbindPlaybackRateControl = bindPlaybackRateControl(art);
  const unbindProgressPreview = bindProgressPreview(
    art,
    video,
    mount,
    onPreviewHover
  );
  const unbindKeyboardHotkeys = bindPlayerKeyboardHotkeys(art);
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
  if (useTypedMp4Source) {
    // ArtPlayer 的 url getter 读取父级 video.src；使用子 <source> 时它为空。
    // 主动保存 option.url，确保 ArtPlayer 内置的失败重连仍使用真实地址。
    art.option.url = src;
    loadTypedMp4Source(video, src, art);
  } else {
    art.url = src;
  }

  return () => {
    disposed = true;
    clearPlaybackDiagnostic();
    // ArtPlayer 的网页全屏会把播放器节点临时移动到 document.body。
    // 路由回退时必须先退出网页全屏，把节点移回 mount，再执行 destroy；
    // 否则 React 卸载详情页后，脱离组件树的全屏节点会继续覆盖新页面。
    if (art.fullscreenWeb) art.fullscreenWeb = false;
    unbindFastRate();
    unbindMobileGestures();
    unbindDoubleClickActions();
    unbindPlaybackRateControl();
    unbindProgressPreview();
    unbindKeyboardHotkeys();
    unbindMobileFullscreenControlAutoHide();
    unbindFullscreenSubtitleLayout();
    unbindTripleScreen();
    unbindOrientationToggle();
    setPlayerFastRateHint(art, false);
    mount.removeEventListener("contextmenu", preventContextMenu);
    destroyHls(video);
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
  Artplayer.SETTING_ITEM_WIDTH = layout.itemWidth;
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

/** ArtPlayer 设置面板宽度默认为 200px，字幕列表需要更宽一些。 */
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

/**
 * 双击快进 / 快退的反馈：在被点的那一侧铺一层半圆遮罩，并从指尖位置扩散
 * 一圈水波纹，中间显示方向箭头和累计秒数。遮罩的生命周期就是连击窗口，
 * 每次点击刷新水波纹和文案，窗口结束提交 seek 时一起淡出。
 *
 * 用整块半圆而不是居中的小胶囊：胶囊靠 left 百分比定位，窄屏上可用宽度只
 * 剩另一半，文案会被压成两行；这里的文案在半圆内部居中，不受这个限制。
 */
function showPlayerSeekRipple(
  art: Artplayer,
  side: SeekChainSide,
  label: string,
  event: Event
) {
  const player = art.template.$player;
  clearSeekRippleTimer(player);

  let ripple = player.querySelector<HTMLElement>(`.${SEEK_RIPPLE_CLASS}`);
  if (!ripple) {
    ripple = document.createElement("div");
    ripple.setAttribute("aria-hidden", "true");
    player.appendChild(ripple);
  }
  // 整体重新赋值：既切换左右，也清掉可能正在播放的淡出状态。
  ripple.className = `${SEEK_RIPPLE_CLASS} ${SEEK_RIPPLE_CLASS}--${side}`;
  ripple.replaceChildren();

  const wave = document.createElement("span");
  wave.className = `${SEEK_RIPPLE_CLASS}-wave`;
  const origin = seekRippleOrigin(ripple, event);
  wave.style.left = `${origin.x}%`;
  wave.style.top = `${origin.y}%`;

  const icon = document.createElement("span");
  icon.className = `${SEEK_RIPPLE_CLASS}-icon`;
  icon.innerHTML = seekRippleIcon();

  const value = document.createElement("span");
  value.className = `${SEEK_RIPPLE_CLASS}-value`;
  value.textContent = label;

  // 重新创建子节点即可重放水波纹和箭头动画，不必手动重置 animation。
  ripple.append(wave, icon, value);
}

/** 连击结束：淡出后移除。 */
function hidePlayerSeekRipple(art: Artplayer) {
  const player = art.template.$player;
  const ripple = player.querySelector<HTMLElement>(`.${SEEK_RIPPLE_CLASS}`);
  if (!ripple) return;

  clearSeekRippleTimer(player);
  ripple.classList.add(SEEK_RIPPLE_LEAVING_CLASS);
  const timer = window.setTimeout(() => {
    seekRippleTimers.delete(player);
    ripple.remove();
  }, SEEK_RIPPLE_LEAVE_MS);
  seekRippleTimers.set(player, timer);
}

/** 播放器卸载：不放动画，直接清掉。 */
function clearPlayerSeekRipple(art: Artplayer) {
  const player = art.template.$player;
  clearSeekRippleTimer(player);
  player.querySelector<HTMLElement>(`.${SEEK_RIPPLE_CLASS}`)?.remove();
}

function clearSeekRippleTimer(player: HTMLElement) {
  const timer = seekRippleTimers.get(player);
  if (timer === undefined) return;
  window.clearTimeout(timer);
  seekRippleTimers.delete(player);
}

/** 水波纹从指尖扩散：把点击位置换算成遮罩内部的百分比。 */
function seekRippleOrigin(ripple: HTMLElement, event: Event) {
  const rect = ripple.getBoundingClientRect();
  if (!(event instanceof MouseEvent) || rect.width <= 0 || rect.height <= 0) {
    return { x: 50, y: 50 };
  }
  return {
    x: clamp(((event.clientX - rect.left) / rect.width) * 100, 0, 100),
    y: clamp(((event.clientY - rect.top) / rect.height) * 100, 0, 100),
  };
}

/** 三个箭头按 CSS 里的延迟依次点亮，左侧靠水平翻转复用同一段路径。 */
function seekRippleIcon() {
  return `
    <svg viewBox="0 0 34 24" fill="none">
      <path d="m5 7.4 4.6 4.6L5 16.6" stroke="currentColor" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round"/>
      <path d="m14.2 7.4 4.6 4.6-4.6 4.6" stroke="currentColor" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round"/>
      <path d="m23.4 7.4 4.6 4.6-4.6 4.6" stroke="currentColor" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round"/>
    </svg>
  `;
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
      <svg viewBox="0 0 24 24" fill="none">
        <circle cx="12" cy="12" r="4.2" stroke="currentColor" stroke-width="1.7"/>
        <path d="M12 2.8v2.1M12 19.1v2.1M4.9 4.9l1.5 1.5M17.6 17.6l1.5 1.5M2.8 12h2.1M19.1 12h2.1M4.9 19.1l1.5-1.5M17.6 6.4l1.5-1.5" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/>
      </svg>
    `;
  }

  if (value === "0%") {
    return `
      <svg viewBox="0 0 24 24" fill="none">
        <path d="M4.8 9.7h3l4.3-3.6v11.8l-4.3-3.6h-3V9.7Z" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/>
        <path d="m16.1 9.9 4.1 4.1M20.2 9.9 16.1 14" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/>
      </svg>
    `;
  }

  return `
    <svg viewBox="0 0 24 24" fill="none">
      <path d="M4.8 9.7h3l4.3-3.6v11.8l-4.3-3.6h-3V9.7Z" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/>
      <path d="M15.4 9.2a4.2 4.2 0 0 1 0 5.6M18 6.7a7.7 7.7 0 0 1 0 10.6" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/>
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
  const controls: PlayerControl[] = [createPlaybackRateControl()];
  if (enableTripleScreenControl) {
    controls.push(createTripleScreenControl());
  }
  if (enableOrientationControl) {
    controls.push(createOrientationControl());
  }
  return controls;
}

/**
 * 控制栏上的倍速入口。ArtPlayer 自带的倍速藏在设置面板二级菜单里，桌面端
 * 的右键菜单在移动端根本不初始化，所以这里补一个一步可达的按钮，选项沿用
 * ArtPlayer 控制栏 selector 的原生结构与样式。
 */
function createPlaybackRateControl(): PlayerControl {
  return {
    name: PLAYBACK_RATE_CONTROL_NAME,
    position: "right",
    index: 26,
    // 不用 ArtPlayer 的 hint 提示：它渲染在控件正上方，正好压住展开的最后一档。
    html: formatPlaybackRateLabel(NORMAL_RATE),
    selector: PLAYBACK_RATE_OPTIONS.map((rate) => ({
      value: rate,
      default: rate === NORMAL_RATE,
      html: formatPlaybackRateOptionLabel(rate),
    })),
    onSelect(this: Artplayer, selector) {
      const rate = normalizePlaybackRate(selector.value);
      this.playbackRate = rate;
      // 播放失败重连和切换清晰度都会走 video.load()，那时浏览器把
      // playbackRate 重置成 defaultPlaybackRate；一起写入才能保住用户选择。
      this.video.defaultPlaybackRate = rate;
      setPlaybackRateSelectorOpen(this, false);
      return formatPlaybackRateLabel(rate);
    },
    mounted(this: Artplayer, element) {
      element.setAttribute("role", "button");
      element.setAttribute("tabindex", "0");
      element.setAttribute("aria-haspopup", "listbox");
      element.setAttribute("aria-label", "播放速度");
      element.setAttribute("title", "播放速度");
      element.dataset.rateOpen = "false";
      this.events.proxy(element, "keydown", (event) => {
        const keyEvent = event as KeyboardEvent;
        if (keyEvent.key !== "Enter" && keyEvent.key !== " ") return;
        keyEvent.preventDefault();
        setPlaybackRateSelectorOpen(
          this,
          element.dataset.rateOpen !== "true",
          element
        );
      });
      updatePlaybackRateControl(this, element);
    },
    click(this: Artplayer, _component, event) {
      // 选项自身的点击交给 onSelect，这里只负责展开 / 收起。
      const target = event.target;
      if (
        target instanceof Element &&
        target.closest(`.${PLAYBACK_RATE_LIST_CLASS}`)
      ) {
        return;
      }
      const element = getPlaybackRateControl(this);
      setPlaybackRateSelectorOpen(this, element?.dataset.rateOpen !== "true");
    },
  };
}

function getPlaybackRateControl(art: Artplayer, mountedElement?: HTMLElement) {
  // 控件 mounted 时 art.controls 还没赋值到实例上，这时只能用回调里的节点。
  const controls = (art as Artplayer & {
    controls?: Record<string, HTMLElement | undefined>;
  }).controls;
  return mountedElement ?? controls?.[PLAYBACK_RATE_CONTROL_NAME];
}

/**
 * ArtPlayer 控制栏的 selector 只有 :hover 才展开，触摸设备上不可靠，
 * 所以额外用 data-rate-open 表达展开状态，由 CSS 在两种指针下分别生效。
 */
function setPlaybackRateSelectorOpen(
  art: Artplayer,
  open: boolean,
  mountedElement?: HTMLElement
) {
  const element = getPlaybackRateControl(art, mountedElement);
  if (!element) return;
  element.dataset.rateOpen = open ? "true" : "false";
  element.setAttribute("aria-expanded", open ? "true" : "false");
}

/** 把控制栏按钮文案与列表选中态同步到真实倍速。 */
function updatePlaybackRateControl(art: Artplayer, mountedElement?: HTMLElement) {
  const element = getPlaybackRateControl(art, mountedElement);
  if (!element) return;

  const rate = normalizePlaybackRate(art.video?.playbackRate);
  const value = element.querySelector<HTMLElement>(
    `.${PLAYBACK_RATE_VALUE_CLASS}`
  );
  if (value) value.textContent = formatPlaybackRateLabel(rate);

  const current = matchPlaybackRateOption(rate);
  element
    .querySelectorAll<HTMLElement>(`.${PLAYBACK_RATE_ITEM_CLASS}`)
    .forEach((item) => {
      const itemRate = normalizePlaybackRate(item.dataset.value);
      item.classList.toggle("art-current", current !== null && itemRate === current);
    });
}

function bindPlaybackRateControl(art: Artplayer) {
  function handleRateChange() {
    // 长按 2 倍速是临时状态，松手就恢复；期间不改按钮文案，
    // 免得用户以为自己选中了 2 倍速。
    if (art.template.$player.classList.contains(FAST_RATE_CLASS)) return;
    updatePlaybackRateControl(art);
  }

  function closeSelector() {
    setPlaybackRateSelectorOpen(art, false);
  }

  function handleControlChange(show: boolean) {
    // 控制栏在桌面端每次 mousemove 都会重新置为显示，只在隐藏时收起列表。
    if (!show) closeSelector();
  }

  art.on("video:ratechange", handleRateChange);
  art.on("video:loadedmetadata", handleRateChange);
  // 点到播放器外面、控制栏隐藏、进出全屏、打开设置面板时都要收起展开的列表。
  art.on("blur", closeSelector);
  art.on("control", handleControlChange);
  art.on("fullscreen", closeSelector);
  art.on("fullscreenWeb", closeSelector);
  art.on("setting", closeSelector);

  return () => {
    art.off("video:ratechange", handleRateChange);
    art.off("video:loadedmetadata", handleRateChange);
    art.off("blur", closeSelector);
    art.off("control", handleControlChange);
    art.off("fullscreen", closeSelector);
    art.off("fullscreenWeb", closeSelector);
    art.off("setting", closeSelector);
  };
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
  const relaySrc = tripleScreenRelaySrc(src);
  const canvas = document.createElement("canvas");
  canvas.className = "video-player__triple-screen-canvas";
  canvas.setAttribute("aria-hidden", "true");
  player.appendChild(canvas);

  let eligible = false;
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
      pendingEnableSuccessNotice = false;
      console.warn("[triple-screen] render failed", error);
      player.classList.remove(TRIPLE_SCREEN_ACTIVE_CLASS);
      updateTripleScreenControl(art, eligible, false);
      art.notice.show = tripleScreenErrorNotice(error);
    },
  });

  function rememberRequestedSource() {
    const requested = nonBlobPlaybackURL(art.url) ||
      nonBlobPlaybackURL(video.currentSrc);
    if (requested) activePlaybackSrc = requested;
  }

  function pendingSourceMatches(url: string) {
    const requested = nonBlobPlaybackURL(art.url);
    if (requested) return samePlaybackURL(requested, url);
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
    updateTripleScreenControl(art, eligible, renderer.enabled);
  }

  function enableRenderer(showSuccessNotice: boolean) {
    pendingEnableSuccessNotice = showSuccessNotice;
    const enabled = renderer.enable();
    if (!enabled) pendingEnableSuccessNotice = false;
    updateTripleScreenControl(art, true, enabled);
    return enabled;
  }

  function enableAfterRelayReady() {
    const pending = pendingRelayEnable;
    if (!pending) return;
    if (!eligible || !pendingSourceMatches(pending.url)) {
      clearPendingRelayEnable();
      return;
    }
    pendingRelayEnable = null;
    if (enableRenderer(true) && pending.shouldPlay) {
      void video.play().catch(() => undefined);
    }
  }

  function resetForNewSource() {
    rememberRequestedSource();
    if (
      pendingRelayEnable &&
      !pendingSourceMatches(pendingRelayEnable.url)
    ) {
      clearPendingRelayEnable();
    }
    pendingEnableSuccessNotice = false;
    eligible = false;
    renderer.disable();
    player.classList.remove(TRIPLE_SCREEN_ACTIVE_CLASS);
    updateTripleScreenControl(art, false, false);
  }

  const binding = {
    toggle() {
      if (!eligible) return;
      if (renderer.enabled) {
        pendingEnableSuccessNotice = false;
        renderer.disable();
        updateTripleScreenControl(art, true, false);
        art.notice.show = "已关闭三屏画面";
        return;
      }

      if (relaySrc && !isTripleScreenRelayURL(activePlaybackSrc)) {
        pendingRelayEnable = {
          time: Number.isFinite(video.currentTime) ? video.currentTime : 0,
          shouldPlay: !video.paused,
          url: relaySrc,
        };
        renderer.disable();
        updateTripleScreenControl(art, true, false);
        art.notice.show = "正在切换到三屏中转播放";
        activePlaybackSrc = relaySrc;
        art.url = relaySrc;
        return;
      }

      enableRenderer(true);
    },
    destroy() {
      clearPendingRelayEnable();
      renderer.destroy();
    },
  };

  tripleScreenBindings.set(art, binding);
  art.on("video:loadstart", resetForNewSource);
  art.on("video:loadedmetadata", updateEligibility);
  art.on("video:loadeddata", enableAfterRelayReady);
  art.on("video:canplay", enableAfterRelayReady);
  updateEligibility();

  return () => {
    art.off("video:loadstart", resetForNewSource);
    art.off("video:loadedmetadata", updateEligibility);
    art.off("video:loadeddata", enableAfterRelayReady);
    art.off("video:canplay", enableAfterRelayReady);
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

function applyPlayerBrightness(art: Artplayer, brightness: number) {
  art.template.$player.style.setProperty(
    "--video-player-brightness",
    clamp(brightness, BRIGHTNESS_MIN, BRIGHTNESS_MAX).toFixed(2)
  );
}

function getPlayerBrightness(art: Artplayer) {
  const raw = art.template.$player.style.getPropertyValue(
    "--video-player-brightness"
  );
  if (!raw.trim()) return DEFAULT_SETTINGS.brightness;
  const value = Number(raw);
  return Number.isFinite(value)
    ? clamp(value, BRIGHTNESS_MIN, BRIGHTNESS_MAX)
    : DEFAULT_SETTINGS.brightness;
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

function createHlsSourceLoader(
  onError: (error: PlayerError | null) => void
) {
  return function loadHlsSource(
    video: HTMLVideoElement,
    url: string,
    art: Artplayer
  ) {
    const target = video as VideoElementWithHls;
    destroyHls(target);
    onError(null);

    void import("hls.js/light")
      .then((hlsModule) => {
        if (art.isDestroy || !video.isConnected) return;
        loadHlsSourceWith(video, url, art, hlsModule.default, onError);
      })
      .catch(() => {
        if (art.isDestroy) return;
        onError({
          title: "HLS 内核加载失败",
          message: "播放器组件加载失败，请刷新页面后重试。",
        });
      });
  };
}

function loadHlsSourceWith(
  video: HTMLVideoElement,
  url: string,
  art: Artplayer,
  HlsCtor: typeof Hls,
  onError: (error: PlayerError | null) => void
) {
  const target = video as VideoElementWithHls;
  destroyHls(target);

  if (HlsCtor.isSupported()) {
    const hls = new HlsCtor({
      enableWorker: true,
      lowLatencyMode: true,
      backBufferLength: 90,
    });

    target.__hls = hls;
    art.hls = hls;
    hls.loadSource(url);
    hls.attachMedia(video);
    hls.on(HlsCtor.Events.ERROR, (_event, data) => {
      if (!data.fatal) return;

      if (data.type === HlsCtor.ErrorTypes.NETWORK_ERROR) {
        art.notice.show = "网络错误，正在重试";
        hls.startLoad();
        return;
      }

      if (data.type === HlsCtor.ErrorTypes.MEDIA_ERROR) {
        art.notice.show = "媒体错误，正在恢复";
        hls.recoverMediaError();
        return;
      }

      destroyHls(target);
      onError({
        title: "HLS 播放失败",
        message: "当前视频流无法解析，请稍后重试或复制播放地址排查。",
      });
    });
    return;
  }

  if (
    video.canPlayType("application/vnd.apple.mpegurl") ||
    video.canPlayType("application/x-mpegURL")
  ) {
    video.src = url;
    return;
  }

  onError({
    title: "当前浏览器不支持 HLS",
    message: "请换用新版 Chrome、Edge 或 Safari 后再试。",
  });
}

function destroyHls(video: VideoElementWithHls) {
  if (!video.__hls) return;
  video.__hls.destroy();
  video.__hls = null;
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

/**
 * 播放器的双击动作。触摸设备上双击左右侧边按 10 秒快退 / 快进（移动端
 * YouTube 的连击机制），双击中间仍是播放 / 暂停；鼠标设备保持双击全屏。
 *
 * 真实 seek 与键盘左右键一致：连击期间只累计目标时间并预览进度条，
 * 窗口结束后提交一次，避免一串点击把 HLS 缓冲反复冲掉。
 */
function bindPlayerDoubleClickActions(art: Artplayer) {
  const seekEnabled = shouldEnableMobileGestures();
  let chain: SeekChain | null = null;
  let seekTarget: number | null = null;
  let chainTimer: number | null = null;

  function clearChainTimer() {
    if (chainTimer === null) return;
    window.clearTimeout(chainTimer);
    chainTimer = null;
  }

  function scheduleChainCommit() {
    clearChainTimer();
    chainTimer = window.setTimeout(() => {
      chainTimer = null;
      commitChainSeek();
    }, DOUBLE_TAP_CHAIN_WINDOW_MS);
  }

  function renderSeekTarget() {
    const duration = art.duration;
    if (seekTarget === null || !Number.isFinite(duration) || duration <= 0) {
      return;
    }
    art.emit("setBar", "played", clamp(seekTarget, 0, duration) / duration);
  }

  function commitChainSeek() {
    clearChainTimer();
    chain = null;
    hidePlayerSeekRipple(art);
    if (seekTarget === null) return;

    const duration = art.duration;
    const hasDuration = Number.isFinite(duration) && duration > 0;
    const target = hasDuration ? clamp(seekTarget, 0, duration) : seekTarget;
    seekTarget = null;
    art.seek = target;
    if (hasDuration) art.emit("setBar", "played", target / duration);
  }

  function accumulateSeek(
    action: Extract<DoubleTapAction, { type: "seek" }>,
    event: Event
  ) {
    const duration = art.duration;
    if (!Number.isFinite(duration) || duration <= 0) return;

    seekTarget = computeDoubleTapSeekTime({
      baseTime: seekTarget ?? art.currentTime,
      stepSeconds: action.stepSeconds,
      duration,
    });
    renderSeekTarget();
    showPlayerSeekRipple(
      art,
      action.side,
      formatDoubleTapSeekLabel(action.totalSeconds),
      event
    );
    scheduleChainCommit();
  }

  function tapZoneFromEvent(event: Event): TapZone {
    const rect = art.template.$player.getBoundingClientRect();
    const clientX =
      event instanceof MouseEvent ? event.clientX : rect.left + rect.width / 2;
    return classifyTapZone(clientX - rect.left, rect.width);
  }

  function handleTap(kind: TapKind, event: Event) {
    if (!seekEnabled) {
      // 鼠标设备沿用 ArtPlayer 原本的双击全屏。
      if (kind === "dblclick") art.fullscreen = !art.fullscreen;
      return;
    }
    if (art.isLock) return;

    const result = reduceDoubleTap(chain, {
      kind,
      zone: tapZoneFromEvent(event),
      at: Date.now(),
    });
    chain = result.chain;

    if (result.action.type === "seek") {
      accumulateSeek(result.action, event);
      return;
    }
    if (result.action.type === "end") {
      commitChainSeek();
      if (result.action.toggle) art.toggle();
    }
  }

  const handleClick = (event: Event) => handleTap("click", event);
  const handleDoubleClick = (event: Event) => handleTap("dblclick", event);

  function handleTimeUpdate() {
    // 播放中的 timeupdate 会把进度条写回真实媒体时间，连击未提交时要盖回目标。
    if (seekTarget !== null) renderSeekTarget();
  }

  art.on("click", handleClick);
  art.on("dblclick", handleDoubleClick);
  art.on("video:timeupdate", handleTimeUpdate);
  art.on("video:pause", commitChainSeek);
  art.on("video:ended", commitChainSeek);
  art.on("blur", commitChainSeek);
  window.addEventListener("blur", commitChainSeek);

  return () => {
    clearChainTimer();
    chain = null;
    seekTarget = null;
    clearPlayerSeekRipple(art);
    art.off("click", handleClick);
    art.off("dblclick", handleDoubleClick);
    art.off("video:timeupdate", handleTimeUpdate);
    art.off("video:pause", commitChainSeek);
    art.off("video:ended", commitChainSeek);
    art.off("blur", commitChainSeek);
    window.removeEventListener("blur", commitChainSeek);
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
    if (event.touches.length !== 1 || art.isLock) return;

    const touch = event.touches[0];
    const rect = player.getBoundingClientRect();
    const localX = touch.clientX - rect.left;
    state = {
      startX: touch.clientX,
      startY: touch.clientY,
      startTime: video.currentTime || 0,
      startVolume: video.muted ? 0 : clamp(video.volume, 0, 1),
      startBrightness: getPlayerBrightness(art),
      side: localX < rect.width / 2 ? "left" : "right",
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
    if (event.touches.length !== 1) {
      resetGesture();
      return;
    }

    const touch = event.touches[0];
    const dx = touch.clientX - state.startX;
    const dy = touch.clientY - state.startY;

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
      handleVolumeGesture(touch.clientY);
      return;
    }

    handleBrightnessGesture(touch.clientY);
  }

  function handleSeekGesture(event: TouchEvent, dx: number) {
    if (!state) return;
    const duration = video.duration;
    if (!Number.isFinite(duration) || duration <= 0) return;
    const rect = player.getBoundingClientRect();
    const targetTime = clamp(
      state.startTime + (dx / Math.max(1, rect.width)) * duration,
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
    const rect = player.getBoundingClientRect();
    const delta = (state.startY - currentY) / Math.max(1, rect.height);
    const nextVolume = clamp(state.startVolume + delta, 0, 1);
    const normalized = Math.round(nextVolume * 100) / 100;
    video.volume = normalized;
    video.muted = normalized <= 0;
    showPlayerGestureHud(art, "volume", formatPercent(normalized));
  }

  function handleBrightnessGesture(currentY: number) {
    if (!state) return;
    const rect = player.getBoundingClientRect();
    const delta =
      ((state.startY - currentY) / Math.max(1, rect.height)) *
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
      return "视频加载已取消，请重试。";
    case MediaError.MEDIA_ERR_NETWORK:
      return "视频源网络连接失败，请稍后重试。";
    case MediaError.MEDIA_ERR_DECODE:
      return "视频编码无法解码，可尝试换用兼容的浏览器。";
    case MediaError.MEDIA_ERR_SRC_NOT_SUPPORTED:
      return "浏览器未能加载视频源，可能是网盘连接、地址失效或视频格式问题。";
    default:
      return "视频源暂时无法播放，请重试或复制地址排查。";
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

function fallbackCopyText(text: string) {
  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.setAttribute("readonly", "true");
  textarea.style.position = "fixed";
  textarea.style.left = "-9999px";
  document.body.appendChild(textarea);
  textarea.select();
  try {
    document.execCommand("copy");
  } catch {
    // ignore
  } finally {
    textarea.remove();
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
