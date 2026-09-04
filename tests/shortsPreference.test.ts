import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const shortsPageSource = readFileSync(
  new URL("../src/pages/ShortsPage.tsx", import.meta.url),
  "utf8"
);
const shortsCssSource = readFileSync(
  new URL("../src/styles/shorts.css", import.meta.url),
  "utf8"
);
const videosDataSource = readFileSync(
  new URL("../src/data/videos.ts", import.meta.url),
  "utf8"
);
const shareClipboardSource = readFileSync(
  new URL("../src/lib/videoShareClipboard.ts", import.meta.url),
  "utf8"
);
const shortsFeedSource = readFileSync(
  new URL("../src/shorts/shortsFeed.ts", import.meta.url),
  "utf8"
);
const useShortsFeedSource = readFileSync(
  new URL("../src/shorts/useShortsFeed.ts", import.meta.url),
  "utf8"
);
const mediaBufferSource = readFileSync(
  new URL("../src/shorts/mediaBuffer.ts", import.meta.url),
  "utf8"
);
const shortsPlatformSource = readFileSync(
  new URL("../src/shorts/platform.ts", import.meta.url),
  "utf8"
);
const useShortsKeyboardSource = readFileSync(
  new URL("../src/shorts/useShortsKeyboard.tsx", import.meta.url),
  "utf8"
);
const slideGesturesSource = readFileSync(
  new URL("../src/shorts/useShortsSlideGestures.ts", import.meta.url),
  "utf8"
);
const shortsHudSource = readFileSync(
  new URL("../src/shorts/ShortsDebugHud.tsx", import.meta.url),
  "utf8"
);
const shortsFeedGoSource = readFileSync(
  new URL("../backend/internal/api/shorts_feed.go", import.meta.url),
  "utf8"
);

test("shorts does not keep recommendation preference from likes or watch time", () => {
  assert.doesNotMatch(shortsPageSource, /currentTime\s*>=\s*3/);
  assert.doesNotMatch(shortsPageSource, /onPreferenceReady/);
  assert.doesNotMatch(shortsPageSource, /preferredFromVideoId/);
  assert.doesNotMatch(videosDataSource, /preferredFromVideoId/);
  // feed 层同样不允许偷偷记录偏好画像
  assert.doesNotMatch(shortsFeedSource + useShortsFeedSource, /preferred|watchTime|seenIds/i);

  const match = /const handleLikeToggle[\s\S]*?const hasLiked/.exec(
    shortsPageSource
  );
  assert.ok(match, "handleLikeToggle block should be present");

  assert.doesNotMatch(match[0], /preferred/i);
  assert.doesNotMatch(videosDataSource, /JSON\.stringify\(\{ seenIds, count \}\)/);
  assert.match(videosDataSource, /const params = new URLSearchParams\(\{\s*cursor: String\(cursor\),\s*count: String\(count\),/);
  assert.match(videosDataSource, /apiGet<ShortsNextResponse>/);
});

test("shorts progress dragging uses immediate pointer state", () => {
  assert.match(shortsPageSource, /const scrubbingRef = useRef\(false\)/);
  assert.match(slideGesturesSource, /scrubbingRef\.current = true;/);
  assert.match(slideGesturesSource, /if \(!options\.scrubbingRef\.current\) return;/);
  assert.doesNotMatch(shortsPageSource + slideGesturesSource, /if \(!scrubbing\) return;/);
  assert.match(shortsPageSource, /function getSeekDuration/);
  assert.match(shortsPageSource, /onLostPointerCapture=\{handleProgressPointerEnd\}/);
});

test("mobile shorts scrubbing time is shown at the top", () => {
  assert.match(
    shortsPageSource,
    /\{scrubbing &&\s*!playbackFailure &&\s*isActive &&\s*shouldLoad &&\s*!isMarkedHidden && \(\s*<div\s*ref=\{progressTimeRef\}\s*className="shorts-slide__progress-time"\s*aria-live="polite"\s*>\s*\{formatClock\(currentTimeRef\.current\)\} \/ \{formatClock\(duration\)\}/
  );
  assert.doesNotMatch(
    shortsPageSource,
    /className=\{`shorts-slide__progress[\s\S]*?<div className="shorts-slide__progress-time"/
  );
  assert.match(
    shortsCssSource,
    /@media \(hover: none\) and \(pointer: coarse\) \{\s*\.shorts-slide__progress-time \{\s*top: calc\(env\(safe-area-inset-top\) \+ 76px\);\s*bottom: auto;/
  );
});

test("mobile shorts title stays plain text without creating a gesture dead zone", () => {
  assert.match(
    shortsPageSource,
    /<h2 className="shorts-slide__title">\{item\.title\}<\/h2>/
  );
  assert.match(shortsPageSource, /<div className="shorts-slide__overlay">/);
  assert.doesNotMatch(shortsPageSource, /shorts-slide__title-link/);
  assert.doesNotMatch(shortsCssSource, /\.shorts-slide__overlay \* \{/);
});

test("the shorts drive badge is the only video detail link", () => {
  assert.match(
    shortsPageSource,
    /const detailPath = `\/video\/\$\{encodeURIComponent\(item\.id\)\}`;/
  );
  assert.match(
    shortsPageSource,
    /<Link\s+to=\{detailPath\}\s+className="shorts-drive-badge"[\s\S]*?aria-label=\{`查看视频详情，来源：\$\{item\.sourceLabel \|\| "本地"\}`\}[\s\S]*?onClick=\{\(event\) => onRouteClick\(event, detailPath\)\}/
  );
  assert.doesNotMatch(shortsPageSource, /shorts-slide__detail|<Info\b|>查看详情</);
  assert.doesNotMatch(shortsCssSource, /\.shorts-slide__detail/);
  assert.match(
    shortsCssSource,
    /\.shorts-drive-badge\s*\{[^}]*cursor:\s*pointer;[^}]*-webkit-tap-highlight-color:\s*transparent;/s
  );
});

test("low-height landscape shorts keep actions below the header", () => {
  assert.match(
    shortsCssSource,
    /\.shorts-slide__actions \{[\s\S]*?right: calc\(14px \+ env\(safe-area-inset-right\)\);/
  );
  assert.match(
    shortsCssSource,
    /@media \(orientation: landscape\) and \(max-height: 520px\) \{\s*\.shorts-slide__actions \{\s*top: calc\(env\(safe-area-inset-top\) \+ 68px\);\s*bottom: auto;\s*gap: 12px;\s*\}\s*\}/
  );
  assert.doesNotMatch(
    shortsCssSource,
    /\.shorts-drive-badge\s*\{[^}]*display:\s*none;/s
  );
  assert.match(
    shortsCssSource,
    /padding: env\(safe-area-inset-top\)\s*calc\(16px \+ env\(safe-area-inset-right\)\) 0\s*calc\(16px \+ env\(safe-area-inset-left\)\);/
  );
});

test("shorts horizontal video swipe seeks relative to the current playback time", () => {
  assert.match(slideGesturesSource, /const SHORTS_SEEK_ACTIVATION_PX = 12;/);
  assert.match(slideGesturesSource, /const SHORTS_SEEK_DIRECTION_LOCK_RATIO = 1\.2;/);
  assert.match(slideGesturesSource, /type ShortsTouchSeekState = \{/);
  assert.match(slideGesturesSource, /startTime: video\.currentTime \|\| 0/);
  assert.match(
    slideGesturesSource,
    /const passiveTouchMove = shouldUsePassiveShortsTouchMove\(\);/
  );
  assert.match(
    slideGesturesSource,
    /video\.addEventListener\("touchmove", handleTouchMove, \{\s*passive: passiveTouchMove,\s*\}\);/
  );
  assert.match(
    slideGesturesSource,
    /if \(!passiveTouchMove\) event\.preventDefault\(\);/
  );
  assert.match(
    slideGesturesSource,
    /videoWidth: Math\.max\(1, video\.getBoundingClientRect\(\)\.width\)/
  );
  assert.match(
    slideGesturesSource,
    /export const SHORTS_MEDIA_SEEK_INTERVAL_MS = 80;/
  );
  assert.match(slideGesturesSource, /video\.fastSeek\(time\);/);
  assert.match(
    slideGesturesSource,
    /flushMediaSeek\(video, targetTime\);/
  );
  // 相对快进的换算与方向锁的行为用例见 shortsGestures.test.ts
  assert.match(
    slideGesturesSource,
    /input\.startTime \+ \(input\.dx \/ Math\.max\(1, input\.width\)\) \* input\.duration/
  );
  assert.match(slideGesturesSource, /suppressNextClickRef\.current = true;/);
  assert.match(slideGesturesSource, /if \(suppressNextClickRef\.current\) \{/);
  assert.doesNotMatch(
    shortsPageSource + slideGesturesSource,
    /touch\.clientX - rect\.left\) \/ Math\.max\(1,\s*rect\.width\)/
  );
});

test("shorts long-press release does not become a click that pauses playback", () => {
  assert.match(
    slideGesturesSource,
    /const handleTouchEnd = \(event: TouchEvent\) => \{[\s\S]*?const wasFastPress = active;[\s\S]*?if \(wasSeeking \|\| wasFastPress\) \{\s*suppressNextSyntheticClick\(\);/
  );
  assert.match(
    slideGesturesSource,
    /function suppressNextSyntheticClick\(\) \{[\s\S]*?suppressNextClickRef\.current = true;[\s\S]*?SHORTS_SYNTHETIC_CLICK_RESET_MS/
  );
  assert.match(
    slideGesturesSource,
    /if \(suppressNextClickRef\.current\) \{\s*suppressNextClickRef\.current = false;\s*clearSuppressNextClickResetTimer\(\);[\s\S]*?return;/
  );
});

test("shorts progress listeners rebind when deferred videos mount", () => {
  assert.match(
    shortsPageSource,
    /VIDEO_WINDOW_SIZE 会让窗口外的 slide 先以海报占位/
  );
  assert.match(
    shortsPageSource,
    /if \(!shouldMount\) \{\s*updateDuration\(0\);\s*updateCurrentTime\(0\);/
  );
  assert.match(
    shortsPageSource,
    /getVideoElement,[\s\S]*?shouldLoad,[\s\S]*?shouldMount,[\s\S]*?usesSharedVideo,[\s\S]*?\]\);/
  );
});

test("shorts paused overlay follows native video playback events", () => {
  assert.match(
    shortsPageSource,
    /const handlePlay = \(\) => \{[\s\S]*?if \(isVideoPausedByUser\(index\)\) \{[\s\S]*?video\.pause\(\);[\s\S]*?setPaused\(true\);[\s\S]*?return;[\s\S]*?setPaused\(false\);/
  );
  assert.match(
    shortsPageSource,
    /const handlePause = \(\) => \{[\s\S]*?if \(!isActive \|\| video\.ended\) return;[\s\S]*?setPaused\(true\);[\s\S]*?setIsBuffering\(false\);/
  );
  assert.match(shortsPageSource, /video\.addEventListener\("play", handlePlay\);/);
  assert.match(shortsPageSource, /video\.addEventListener\("pause", handlePause\);/);
  assert.match(shortsPageSource, /video\.removeEventListener\("play", handlePlay\);/);
  assert.match(shortsPageSource, /video\.removeEventListener\("pause", handlePause\);/);
});

test("shorts preserves a user pause while the active video is still loading", () => {
  assert.match(shortsPageSource, /const userPausedIndexRef = useRef<number \| null>\(null\);/);
  assert.match(shortsPageSource, /const \[, setUserPausedIndexState\] = useState<number \| null>\(null\);/);
  assert.match(shortsPageSource, /const setUserPausedForIndex = useCallback/);
  assert.match(
    shortsPageSource,
    /const canContinue = \(\) =>[\s\S]*?!isVideoPausedByUser\(index\);/
  );
  assert.match(
    useShortsKeyboardSource,
    /isVideoPausedByUser\(target\.videoIndex\) \|\|\s*\(target\.video\.paused && target\.video\.readyState >= 3\)/
  );
  assert.match(
    useShortsKeyboardSource,
    /setUserPausedForIndex\(target\.videoIndex, false\);\s*target\.video\.play\(\)\.catch/
  );
  assert.match(
    useShortsKeyboardSource,
    /setUserPausedForIndex\(target\.videoIndex, true\);\s*target\.video\.pause\(\);/
  );
  assert.match(
    shortsPageSource,
    /const shouldResume =\s*isVideoPausedByUser\(index\) \|\| \(video\.paused && !isBuffering\);/
  );
  assert.match(
    shortsPageSource,
    /onUserPausedChange\(index, true\);\s*video\.pause\(\);\s*setPaused\(true\);\s*setIsBuffering\(false\);/
  );
  assert.match(
    shortsPageSource,
    /const handleCanPlay = \(\) => \{[\s\S]*?if \(isActive && isVideoPausedByUser\(index\)\) \{[\s\S]*?video\.pause\(\);[\s\S]*?setPaused\(true\);[\s\S]*?return;/
  );
});

test("shorts retries interrupted active playback and exposes rejected autoplay", () => {
  assert.match(shortsPageSource, /const attemptPlay = \(\) => \{/);
  assert.match(shortsPageSource, /request = video\.play\(\);/);
  assert.match(
    shortsPageSource,
    /errorName === "AbortError" && retryCount < 2[\s\S]*?window\.setTimeout\(attemptPlay, retryCount \* 120\)/
  );
  assert.match(shortsPageSource, /video\.addEventListener\("loadeddata", retryWhenReady\);/);
  assert.match(shortsPageSource, /video\.addEventListener\("canplay", retryWhenReady\);/);
  assert.match(
    shortsPageSource,
    /const markPlayBlocked = \(\) => \{[\s\S]*?setIsBuffering\(false\);[\s\S]*?setPaused\(true\);/
  );
  assert.match(shortsPageSource, /autoPlay=\{isActive\}/);
  // \u81ea\u52a8\u64ad\u653e\u88ab\u62d2\u540e\u7684\u9996\u6b21\u70b9\u51fb\uff1a\u5224\u5b9a\u4e0e\u6062\u590d\u5728 slide\uff0c\u5206\u53d1\u65f6\u5e8f\u5728\u624b\u52bf hook
  assert.match(
    shortsPageSource,
    /function shouldResumeImmediatelyOnClick\(\) \{[\s\S]*?video\?\.paused && !isBuffering/
  );
  assert.match(
    shortsPageSource,
    /function handleImmediateResume\(\) \{[\s\S]*?video\.play\(\)\.catch/
  );
  assert.match(
    slideGesturesSource,
    /if \(options\.shouldResumeImmediately\(\)\) \{[\s\S]*?options\.onImmediateResume\(\);[\s\S]*?return;[\s\S]*?\/\/ \u5355\u51fb\u6302\u8d77/
  );
});

test("shorts exposes media failures separately from pause and retries in place", () => {
  assert.match(
    shortsPageSource,
    /type ShortsPlaybackFailure =\s*\| "media-error"\s*\| "play-rejected"\s*\| "loop-restart";/
  );
  assert.match(
    shortsPageSource,
    /const \[playbackFailure, setPlaybackFailure\] =\s*useState<ShortsPlaybackFailure \| null>\(null\);/
  );

  const mediaErrorStart = shortsPageSource.indexOf("const handleError = () => {");
  const mediaErrorEnd = shortsPageSource.indexOf(
    "function syncActivePreloadReadiness",
    mediaErrorStart
  );
  assert.ok(mediaErrorStart >= 0 && mediaErrorEnd > mediaErrorStart);
  const mediaErrorBlock = shortsPageSource.slice(mediaErrorStart, mediaErrorEnd);
  assert.match(mediaErrorBlock, /exposePlaybackFailure\("media-error"\);/);
  assert.doesNotMatch(mediaErrorBlock, /setPaused\(true\)/);

  assert.match(shortsPageSource, /exposePlaybackFailure\("play-rejected"\);/);
  assert.match(shortsPageSource, /exposePlaybackFailure\("loop-restart"\);/);
  assert.match(
    shortsPageSource,
    /\{paused &&\s*!playbackFailure &&\s*isActive &&\s*!scrubbing &&\s*!isMarkedHidden && \(/
  );
  assert.match(
    shortsPageSource,
    /className="shorts-slide__playback-error"\s*role="alert"[\s\S]*?<div className="shorts-slide__playback-error-title">播放失败<\/div>[\s\S]*?className="shorts-slide__playback-retry"[\s\S]*?>\s*重试播放\s*<\/button>/
  );
  assert.doesNotMatch(shortsPageSource, /视频暂时无法播放|>重新播放</);
  const retryButtonMarkup =
    /<button\s+[^>]*className="shorts-slide__playback-retry"[^>]*>[\s\S]*?<\/button>/.exec(
      shortsPageSource
    );
  assert.ok(retryButtonMarkup, "playback retry button should be present");
  assert.doesNotMatch(
    retryButtonMarkup[0],
    /<Play/,
    "playback retry button should not contain an icon"
  );
  assert.doesNotMatch(
    shortsCssSource,
    /shorts-slide__playback-error-copy|shorts-slide__playback-error-message/
  );

  const retryStart = shortsPageSource.indexOf("function handlePlaybackRetry(");
  const retryEnd = shortsPageSource.indexOf("// 手势输入", retryStart);
  assert.ok(retryStart >= 0 && retryEnd > retryStart);
  const retryBlock = shortsPageSource.slice(retryStart, retryEnd);
  assert.match(retryBlock, /resetLoopRestartState\(\);/);
  assert.match(retryBlock, /video\.load\(\);/);
  assert.match(retryBlock, /\.play\(\)/);
  assert.match(
    shortsPageSource,
    /disabled: isMarkedHidden \|\| playbackFailure !== null/
  );
  assert.match(
    slideGesturesSource,
    /const start = \(\) => \{\s*if \(optionsRef\.current\.disabled\) return;/
  );
  assert.match(
    shortsCssSource,
    /\.shorts-slide__playback-error \{[\s\S]*?width:\s*min\(240px, calc\(100% - 48px\)\);[\s\S]*?z-index: 18;[\s\S]*?backdrop-filter: blur\(14px\);/
  );
});

test("shorts keyboard play pause does not show a toast", () => {
  const keyboardBlock = /else if \(e\.key === " "\) \{[\s\S]*?\} else if \(e\.key === "m"/.exec(useShortsKeyboardSource);
  assert.ok(keyboardBlock, "space key handler should be present");
  assert.doesNotMatch(keyboardBlock[0], /showHud\("播放"|showHud\("暂停"/);
});

test("shorts double space likes without toggling playback", () => {
  assert.match(useShortsKeyboardSource, /const SHORTS_KEYBOARD_DOUBLE_SPACE_MS = 280;/);
  assert.match(
    useShortsKeyboardSource,
    /let pendingSpaceTimer: number \| null = null;[\s\S]*?let pendingSpaceTarget:/
  );
  assert.match(
    useShortsKeyboardSource,
    /pendingSpaceTimer = window\.setTimeout\(\(\) => \{[\s\S]*?toggleKeyboardPlayback\(target\);[\s\S]*?SHORTS_KEYBOARD_DOUBLE_SPACE_MS/
  );
  assert.match(
    useShortsKeyboardSource,
    /pendingSpaceTimer !== null &&[\s\S]*?pendingSpaceTarget\.video === activeVideo[\s\S]*?clearKeyboardSpaceTimer\(\);[\s\S]*?likeActiveVideo\(videoIndex\);[\s\S]*?return;/
  );
  assert.match(
    useShortsKeyboardSource,
    /const keyboardLikeHandlersRef = useRef<Map<number, \(\) => void>>\(new Map\(\)\);/
  );
  assert.match(
    useShortsKeyboardSource,
    /const likeActiveVideo = \(videoIndex: number\) => \{\s*keyboardLikeHandlersRef\.current\.get\(videoIndex\)\?\.\(\);/
  );
  assert.match(
    shortsPageSource,
    /registerKeyboardLikeHandler=\{registerKeyboardLikeHandler\}/
  );
  assert.match(
    shortsPageSource,
    /keyboardLikeHandlerRef\.current = \(\) => \{[\s\S]*?slideRef\.current\?\.getBoundingClientRect\(\);[\s\S]*?handleDoubleClickLike\(slideRect\.width \/ 2, slideRect\.height \/ 2\);/
  );
  assert.match(
    shortsPageSource,
    /registerKeyboardLikeHandler\(index, handleKeyboardLike\);[\s\S]*?registerKeyboardLikeHandler\(index, null\);/
  );
  assert.doesNotMatch(shortsPageSource + useShortsKeyboardSource, /new MouseEvent\("click"/);
  assert.match(shortsPageSource, /data-shorts-like=""/);
  assert.match(
    useShortsKeyboardSource,
    /window\.removeEventListener\("blur", handleWindowBlur\);[\s\S]*?clearKeyboardSpaceTimer\(\);/
  );
});

test("shorts keeps the full heart animation when reduced motion is enabled", () => {
  assert.match(
    shortsCssSource,
    /@media \(prefers-reduced-motion: reduce\)\s*\{\s*\.shorts-slide__heart-burst\s*\{\s*animation:\s*shorts-heart-pop 650ms cubic-bezier\(0\.175,\s*0\.885,\s*0\.32,\s*1\.275\) forwards !important;/
  );
});

test("desktop left-key seeking and held right-key playback keep distinct semantics", () => {
  assert.match(
    useShortsKeyboardSource,
    /type ShortsKeyboardSeekPreview = \{[\s\S]*?videoIndex: number;[\s\S]*?currentTime: number;[\s\S]*?duration: number;/
  );
  assert.match(
    useShortsKeyboardSource,
    /const baseTime = canContinuePendingTarget[\s\S]*?pendingTarget\.currentTime[\s\S]*?: activeVideo\.currentTime;[\s\S]*?const currentTime = clamp\(baseTime \+ delta, 0, duration\);/
  );
  assert.match(
    useShortsKeyboardSource,
    /setKeyboardSeekPreview\(\{ videoIndex, currentTime, duration \}\);/
  );
  assert.match(
    useShortsKeyboardSource,
    /if \(e\.key !== "ArrowLeft"\) return;[\s\S]*?keyboardSeekHeldKeysRef\.current\.delete\(e\.key\);[\s\S]*?size === 0\) finishKeyboardSeek\(\)/
  );
  const previewStart = useShortsKeyboardSource.indexOf("const previewKeyboardSeek");
  const keydownStart = useShortsKeyboardSource.indexOf("const handleKeyDown", previewStart);
  assert.ok(previewStart >= 0 && keydownStart > previewStart);
  assert.doesNotMatch(
    useShortsKeyboardSource.slice(previewStart, keydownStart),
    /\.currentTime\s*=/
  );
  assert.match(
    useShortsKeyboardSource,
    /const commitKeyboardSeek = \(\) => \{[\s\S]*?target\.video\.currentTime = nextTime;/
  );
  assert.match(
    useShortsKeyboardSource,
    /SHORTS_KEYBOARD_SEEK_IDLE_COMMIT_MS[\s\S]*?scheduleKeyboardSeekIdleCommit/
  );
  assert.match(
    useShortsKeyboardSource,
    /const SHORTS_KEYBOARD_FAST_PLAYBACK_DELAY_MS = 400;/
  );
  assert.match(
    useShortsKeyboardSource,
    /else if \(e\.key === "ArrowRight"\) \{\s*e\.preventDefault\(\);\s*if \(e\.repeat\) return;\s*startKeyboardRightPress\(\);/
  );
  assert.match(
    useShortsKeyboardSource,
    /keyboardRightPressTimer = window\.setTimeout\([\s\S]*?target\.video\.playbackRate = 2;[\s\S]*?setKeyboardFastPlaybackIndex\(target\.videoIndex\);[\s\S]*?SHORTS_KEYBOARD_FAST_PLAYBACK_DELAY_MS/
  );
  assert.match(
    useShortsKeyboardSource,
    /if \(target\.fastPlaybackActive\) \{[\s\S]*?target\.video\.playbackRate = 1;[\s\S]*?setKeyboardFastPlaybackIndex\(null\);/
  );
  assert.match(
    useShortsKeyboardSource,
    /if \(e\.key === "ArrowRight"\) \{[\s\S]*?finishKeyboardRightPress\(true\);/
  );
  assert.match(
    useShortsKeyboardSource,
    /seekOnShortPress[\s\S]*?previewKeyboardSeek\(\s*SHORTS_KEYBOARD_SEEK_SECONDS,\s*"ArrowRight"\s*\);/
  );
  assert.match(
    useShortsKeyboardSource,
    /const handleWindowBlur = \(\) => \{\s*finishKeyboardRightPress\(false\);/
  );
  assert.match(useShortsKeyboardSource, /window\.addEventListener\("keyup", handleKeyUp\);/);
  assert.match(
    shortsPageSource,
    /className="shorts-keyboard-seek-time" aria-live="polite"[\s\S]*?formatClock\(keyboardSeekPreview\.currentTime\)[\s\S]*?formatClock\(keyboardSeekPreview\.duration\)/
  );
  assert.match(
    shortsCssSource,
    /\.shorts-keyboard-seek-time \{\s*top: calc\(env\(safe-area-inset-top\) \+ 76px\);\s*z-index: 40;/
  );
  assert.match(
    shortsPageSource,
    /keyboardSeekPreview=\{[\s\S]*?keyboardSeekPreview\?\.videoIndex === index[\s\S]*?\? keyboardSeekPreview/
  );
  assert.match(
    shortsPageSource,
    /keyboardFastPlayback=\{keyboardFastPlaybackIndex === index\}/
  );
  assert.match(
    shortsPageSource,
    /\{\(fastActive \|\| keyboardFastPlayback\) && \([\s\S]*?2x 速播放中/
  );
  assert.match(
    shortsPageSource,
    /const writeProgressDisplay = useCallback\([\s\S]*?progressTrackRef\.current\?\.style\.setProperty\(\s*"--progress-pct",\s*`\$\{ratio \* 100\}%`\s*\);/
  );
  assert.match(
    shortsPageSource,
    /useLayoutEffect\(\(\) => \{[\s\S]*?lastKeyboardSeekPreviewTimeRef\.current = keyboardSeekPreview\.currentTime;[\s\S]*?writeProgressDisplay\([\s\S]*?updateCurrentTime\(lastPreviewTime, true\);[\s\S]*?\}, \[keyboardSeekPreview, updateCurrentTime, writeProgressDisplay\]\);/
  );
});

test("shorts play pause does not render transient center hud", () => {
  assert.doesNotMatch(shortsPageSource, /function shouldShowPlayPauseHud\(\)/);
  assert.doesNotMatch(shortsPageSource, /setPlayPauseHud/);
  assert.doesNotMatch(shortsPageSource, /playPauseHud/);
  assert.doesNotMatch(shortsPageSource, /shorts-slide__hud-pulse/);
  assert.doesNotMatch(shortsCssSource, /\.shorts-slide__hud-pulse/);
  assert.doesNotMatch(shortsCssSource, /@keyframes shorts-hud-pop/);
  assert.match(
    shortsPageSource,
    /\{paused &&\s*!playbackFailure &&\s*isActive &&\s*!scrubbing &&\s*!isMarkedHidden && \(\s*<div className="shorts-slide__paused"/
  );
  assert.match(
    shortsPageSource,
    /<span className="shorts-slide__paused-icon">\s*<Play size=\{22\} fill="currentColor" strokeWidth=\{1\.75\} \/>/
  );
  assert.match(
    shortsCssSource,
    /\.shorts-slide__paused-icon\s*\{[\s\S]*?width:\s*52px;[\s\S]*?height:\s*52px;[\s\S]*?border-radius:\s*50%;/
  );
  assert.doesNotMatch(shortsPageSource, />\s*▶\s*</);
});

test("shorts screen taps do not show the browser tap highlight", () => {
  assert.match(
    shortsCssSource,
    /\.shorts-slide\s*\{[^}]*-webkit-tap-highlight-color:\s*transparent;/s
  );
});

test("shorts hud toast keeps icon and text close together", () => {
  assert.match(
    shortsCssSource,
    /\.shorts-hud-toast\s*\{[\s\S]*gap:\s*4px;/
  );
});

test("shorts loading spinner covers video buffering and initial feed loading", () => {
  assert.match(shortsPageSource, /function ShortsLoadingSpinner/);
  const spinnerStart = shortsPageSource.indexOf("function ShortsLoadingSpinner");
  const spinnerEnd = shortsPageSource.indexOf(
    "function applyVideoMutedState",
    spinnerStart
  );
  assert.ok(spinnerStart >= 0 && spinnerEnd > spinnerStart);
  const spinnerBlock = shortsPageSource.slice(spinnerStart, spinnerEnd);
  assert.doesNotMatch(spinnerBlock, /requestAnimationFrame|style\.transform/);
  assert.match(shortsPageSource, /"--shorts-spinner-size": `\$\{size\}px`/);
  assert.match(shortsPageSource, /<ShortsLoadingSpinner size=\{30\} \/>/);
  assert.doesNotMatch(shortsPageSource, /<ShortsLoadingSpinner size=\{16\} \/>/);
  assert.doesNotMatch(shortsPageSource, /加载中…/);
  assert.match(shortsPageSource, /className="shorts-empty shorts-loading" aria-live="polite"/);
  assert.match(shortsPageSource, /正在加载短视频/);
  assert.match(
    shortsCssSource,
    /\.shorts-slide__loading-spinner\s*\{[\s\S]*width:\s*var\(--shorts-spinner-size,\s*30px\);[\s\S]*height:\s*var\(--shorts-spinner-size,\s*30px\);[\s\S]*border:\s*3px solid rgba\(255,\s*255,\s*255,\s*0\.24\);[\s\S]*border-top-color:\s*rgba\(255,\s*255,\s*255,\s*0\.98\);[\s\S]*border-radius:\s*50%;/
  );
  assert.match(
    shortsCssSource,
    /animation:\s*shorts-spinner-rotate 0\.8s linear infinite;/
  );
  assert.match(
    shortsCssSource,
    /@keyframes shorts-spinner-rotate\s*\{[\s\S]*to\s*\{\s*transform:\s*rotate\(360deg\);/
  );
  assert.match(shortsCssSource, /\.shorts-loading \.shorts-slide__loading-spinner\s*\{/);
  const bufferingRule =
    shortsCssSource.match(/\.shorts-slide__buffering\s*\{[^}]*\}/s)?.[0] ?? "";
  assert.doesNotMatch(
    bufferingRule,
    /background:|backdrop-filter:|box-shadow:|border:|border-radius:|width:|height:/
  );
  const mobileBufferingRule =
    shortsCssSource.match(
      /@media \(max-width:\s*640px\)\s*\{[\s\S]*?\.shorts-slide__buffering\s*\{[^}]*\}/
    )?.[0] ?? "";
  assert.match(mobileBufferingRule, /--shorts-spinner-size:\s*24px;/);
  assert.doesNotMatch(mobileBufferingRule, /width:\s*56px|height:\s*56px/);
});

test("shorts prepares the next video lightly until the active buffer is healthy", () => {
  assert.match(shortsPageSource, /const \[activeReadyForPreload, setActiveReadyForPreload\] = useState\(false\);/);
  assert.match(mediaBufferSource, /const ACTIVE_PRELOAD_BUFFER_SECONDS = 12;/);
  assert.match(mediaBufferSource, /export const PRELOAD_AHEAD_COUNT = 2;/);
  // 下一条始终保留 src，但只有授权后才能和更后面的条目一起用 auto。
  assert.match(
    shortsPageSource,
    /const shouldPrepareNext =\s*!useIOSSharedVideo && preloadOffset === 1;/
  );
  assert.match(
    shortsPageSource,
    /const preloadOffset = index - activeIndex;[\s\S]*?preloadOffset > 0 &&[\s\S]*?preloadOffset <= getPreloadAheadCount\(activeReadyForPreload\);/
  );
  assert.doesNotMatch(
    shortsPageSource,
    /const shouldPreload =\s*!useIOSSharedVideo &&\s*activeReadyForPreload/
  );
  assert.match(
    shortsPageSource,
    /const shouldLoad =\s*isActiveSlide \|\|\s*shouldPrepareNext \|\|\s*shouldPreload \|\|\s*shouldRetainCached;/
  );
  assert.match(
    shortsPageSource,
    /const shouldEagerLoad = isActiveSlide \|\| shouldPreload;/
  );
  assert.match(
    shortsPageSource,
    /preload=\{shouldLoad \? \(shouldEagerLoad \? "auto" : "metadata"\) : "none"\}/
  );
  assert.match(shortsPageSource, /shouldLoad=\{shouldLoad\}/);
  assert.match(shortsPageSource, /setActiveReadyForPreload\(false\);\s*setActiveIndex\(bestIndex\);/);
  assert.match(shortsPageSource, /function syncActivePreloadReadiness\(currentVideo: HTMLVideoElement\)/);
  // 水位现在按码率逐条换算，这里只钉住"授权由 comfortable 判定驱动"；
  // 换算规则见 "shorts sizes the preload gate by bitrate" 那条。
  assert.match(
    shortsPageSource,
    /if \(videoHasComfortableBuffer\(currentVideo, preloadBufferSeconds\)\) \{\s*onActiveReadyForPreload\(index\);/
  );
  assert.match(shortsPageSource, /if \(isActive\) onActiveNeedsPriority\(index\);/);
  assert.match(shortsPageSource, /video\.addEventListener\("progress", handleProgress\);/);
  assert.match(shortsPageSource, /src=\{shouldLoad \? item\.videoSrc : undefined\}/);
  assert.match(shortsPageSource, /video\.removeAttribute\("src"\)/);
  assert.doesNotMatch(shortsPageSource, /src=\{shouldLoad \? item\.previewSrc/);
});

test("shorts preload grant uses high/low watermark hysteresis", () => {
  // 高水位授权、低水位收回，之间维持现状，避免阈值附近抖动。两个水位现在
  // 按码率逐条换算，缺省参数仍是原来的 12s / 4s，低码率视频行为不变。
  // 判定本身的行为矩阵见 shortsMediaBuffer.test.ts；这里钉住页面接线。
  assert.match(mediaBufferSource, /const ACTIVE_PRELOAD_KEEP_SECONDS = 4;/);
  assert.match(
    shortsPageSource,
    /\} else if \(videoBufferIsCritical\(currentVideo, preloadKeepSeconds\)\) \{[\s\S]*?onActiveNeedsPriority\(index\);/
  );
  assert.match(
    mediaBufferSource,
    /function videoBufferIsCritical\(\s*video: BufferedMediaProbe,\s*keepSeconds = ACTIVE_PRELOAD_KEEP_SECONDS\s*\)/
  );
  assert.match(
    mediaBufferSource,
    /function videoHasComfortableBuffer\(\s*video: BufferedMediaProbe,\s*bufferSeconds = ACTIVE_PRELOAD_BUFFER_SECONDS\s*\)/
  );
  // 已缓冲到片尾时既视为健康也不视为告急，避免临近结尾误收回授权
  assert.match(mediaBufferSource, /function videoBufferedToEnd\(video: BufferedMediaProbe\)/);
  assert.match(
    mediaBufferSource,
    /if \(videoBufferedToEnd\(video\)\) return true;[\s\S]*?>= bufferSeconds;/
  );
  assert.match(
    mediaBufferSource,
    /if \(videoBufferedToEnd\(video\)\) return false;[\s\S]*?< keepSeconds;/
  );
});

test("shorts only advances viewed cursors and waits for queue end before starting a new feed", () => {
  assert.doesNotMatch(shortsPageSource + shortsFeedSource + useShortsFeedSource, /seenIdsRef|saveSeenIds/);
  assert.match(
    useShortsFeedSource,
    /const persistedFeedHighPositionRef = useRef\(-1\);\s*const queueStartOffsetRef = useRef\(0\);\s*const queueStartOffset = queueStartOffsetRef\.current;/
  );
  assert.match(
    useShortsFeedSource,
    /const activeQueuePosition = queueStartOffset \+ activeIndex;\s*if \(activeQueuePosition > persistedFeedHighPositionRef\.current\) \{\s*persistedFeedHighPositionRef\.current = activeQueuePosition;\s*schedulePersistedFeed\(\{\s*feedToken: active\.feedToken,\s*cursor: active\.feedCursor,/
  );
  assert.match(
    useShortsFeedSource,
    /const SHORTS_FEED_SAVE_DELAY_MS = 500;/
  );
  assert.match(
    useShortsFeedSource,
    /window\.addEventListener\("pagehide", flushPersistedFeed\);[\s\S]*?flushPersistedFeed\(\);/
  );
  // 续播书签只归 feed hook 管；页面的缓存窗口推进不得写书签
  assert.doesNotMatch(shortsPageSource, /saveShortsFeedState/);
  // 换轮时机的行为矩阵见 shortsFeedLogic.test.ts 的 planShortsPrefetch 用例
  assert.match(
    shortsFeedSource,
    /if \(input\.roundComplete\) \{\s*return input\.remainingAfterActive > 0 \? "none" : "new-round";/
  );
  assert.match(
    useShortsFeedSource,
    /if \(plan === "new-round"\) \{\s*requestFeedRef\.current = EMPTY_SHORTS_FEED;\s*setRoundComplete\(false\);\s*\}\s*void loadMore\(\);/
  );
});

test("shorts returns the first playable pair before fetching normal batches", () => {
  assert.match(shortsFeedSource, /export const INITIAL_BATCH_SIZE = 2;/);
  assert.match(shortsFeedSource, /export const BATCH_SIZE = 5;/);
  assert.match(
    useShortsFeedSource,
    /const hasLoadedBatchRef = useRef\(false\);/
  );
  assert.match(
    useShortsFeedSource,
    /count: hasLoadedBatchRef\.current \? BATCH_SIZE : INITIAL_BATCH_SIZE,/
  );
  assert.match(
    useShortsFeedSource,
    /const outcome = await requestShortsBatch\([\s\S]*?hasLoadedBatchRef\.current = true;/
  );
});

test("shorts distinguishes feed failures from a genuinely empty library", () => {
  // 恢复流程（令牌失效 / 快照耗尽 / 空库）的行为用例见 shortsFeedLogic.test.ts
  assert.match(
    shortsFeedSource,
    /if \(resp\.total === 0\) \{\s*return \{ kind: "empty" \};/
  );
  assert.match(
    useShortsFeedSource,
    /if \(outcome\.kind === "empty"\) \{\s*setEmpty\(true\);[\s\S]*?setItems\(\[\]\);\s*onQueueResetRef\.current\(\);[\s\S]*?setRoundComplete\(false\);\s*requestFeedRef\.current = EMPTY_SHORTS_FEED;\s*cancelPendingPersistedFeed\(\);\s*clearShortsFeedState\(\);\s*return;/
  );
  assert.match(
    shortsPageSource,
    /const handleQueueReset = useCallback\(\(\) => setActiveIndex\(0\), \[\]\);/
  );
  assert.match(
    useShortsFeedSource,
    /useEffect\(\(\) => \{\s*if \(empty\) return;\s*const active = items\[activeIndex\];/
  );
  assert.match(useShortsFeedSource, /catch \{\s*setLoadError\(true\);/);
  assert.match(shortsPageSource, /短视频加载失败，请检查网络后重试/);
  assert.match(shortsPageSource, /onClick=\{\(\) => void loadMore\(\)\}/);
  assert.doesNotMatch(
    videosDataSource,
    /\.catch\(\(\) => \(\{ items: \[\], total: 0/
  );
});

test("shorts empty library reuses the homepage empty visual", () => {
  assert.match(
    shortsPageSource,
    /import \{ AdminEmptyVisual \} from "@\/admin\/AdminEmptyVisual";/
  );
  assert.match(
    shortsPageSource,
    /\{empty && items\.length === 0 && \([\s\S]*?<AdminEmptyVisual[\s\S]*?variant="empty"[\s\S]*?text="当前库中没有视频"[\s\S]*?className="shorts-empty__visual"/
  );
  assert.match(
    shortsPageSource,
    /className="shorts-header__actions">\s*\{items\.length > 0 && \(/
  );
  assert.doesNotMatch(shortsPageSource, /当前没有可播放的视频/);
  assert.match(
    shortsCssSource,
    /\.shorts-empty__visual \.admin-empty-visual__text\s*\{[^}]*color:\s*rgba\(255,\s*255,\s*255,\s*0\.72\);/
  );
});

test("shorts hidden overlay keeps only the concise confirmation", () => {
  assert.match(shortsPageSource, /shorts-slide__hidden-title">已隐藏该视频/);
  assert.match(
    shortsCssSource,
    /\.shorts-slide__hidden-overlay\s*\{[\s\S]*?gap:\s*8px;/
  );
  assert.match(
    shortsPageSource,
    /\{paused &&\s*!playbackFailure &&\s*isActive &&\s*!scrubbing &&\s*!isMarkedHidden && \(/
  );
  assert.doesNotMatch(
    shortsPageSource,
    /系统将不会再次在任何地方向您展示此视频|shorts-slide__hidden-desc/
  );
  assert.doesNotMatch(shortsCssSource, /\.shorts-slide__hidden-desc/);
});

test("shorts hide action is icon-only and advances by stable feed key", () => {
  assert.match(
    shortsPageSource,
    /aria-label="不再展示"[\s\S]*?<EyeOff size=\{22\} \/>/
  );
  assert.doesNotMatch(
    shortsPageSource,
    /<span className="shorts-slide__action-count">隐藏<\/span>/
  );
  assert.doesNotMatch(
    shortsPageSource,
    /已选择不再展示，正在滑至下一首/
  );
  // 稳定 key 不受长会话队列裁剪后的 index 平移影响；依赖仍须是空数组，
  // 避免每次取批都把 ShortsSlide 的 memo 整片击穿。
  assert.match(
    shortsPageSource,
    /const handleHideSuccess = useCallback\(\(itemKey: string\) => \{[\s\S]*?element\.dataset\.feedKey === itemKey[\s\S]*?current\?\.nextElementSibling[\s\S]*?nextSlide\.scrollIntoView\(\{ behavior: "smooth" \}\);[\s\S]*?\}, \[\]\);/
  );
});

test("shorts creates and copies the existing one-time video share", () => {
  assert.match(
    shortsPageSource,
    /copyExistingVideoShareURL,[\s\S]*?createAndCopyVideoShare/
  );
  assert.match(
    shortsPageSource,
    /async function handleShareClick[\s\S]*?createAndCopyVideoShare\(item\.id\)[\s\S]*?pendingShareURLRef\.current = result\.url[\s\S]*?showHud\("请再次点击分享按钮"\)[\s\S]*?showHud\("一次性分享链接已复制"\)/
  );
  assert.match(
    shortsPageSource,
    /aria-label="生成并复制一次性分享链接"[\s\S]*?disabled=\{isSharing\}[\s\S]*?onClick=\{handleShareClick\}[\s\S]*?<Share2 size=\{22\} \/>/
  );
  assert.match(
    shortsPageSource,
    /copyExistingVideoShareURL\(pendingShareURLRef\.current\)/
  );
  assert.match(shareClipboardSource, /navigator\.clipboard\?\.writeText/);
  assert.match(shareClipboardSource, /document\.execCommand\("copy"\)/);
  assert.match(
    videosDataSource,
    /`\/api\/video\/\$\{encodeURIComponent\(id\)\}\/share`/
  );
  assert.doesNotMatch(shortsPageSource, /\/shorts\/share|ShortsSharePage/);
});

test("shorts like action does not display a count", () => {
  assert.doesNotMatch(shortsPageSource, /shorts-slide__action-count/);
  assert.doesNotMatch(shortsPageSource, /function formatCount\(/);
  assert.doesNotMatch(shortsCssSource, /\.shorts-slide__action-count/);
});

test("shorts keeps buffered sources inside a four video window", () => {
  assert.match(shortsPageSource, /const \[cacheableSourceIds, setCacheableSourceIds\] = useState<Set<string>>/);
  assert.match(shortsPageSource, /setCacheableSourceIds\(\(prev\) => \{/);
  assert.match(mediaBufferSource, /const VIDEO_WINDOW_SIZE = 4;/);
  assert.doesNotMatch(shortsPageSource + mediaBufferSource, /VIDEO_WINDOW_BACKWARD_BIAS/);
  assert.match(shortsPageSource, /const \[cacheWindowHighIndex, setCacheWindowHighIndex\] = useState\(-1\);/);
  assert.match(shortsPageSource, /setCacheWindowHighIndex\(\(prev\) => Math\.max\(prev, activeIndex\)\);/);
  // 窗口边界计算的行为用例见 shortsMediaBuffer.test.ts
  assert.match(mediaBufferSource, /function getVideoWindowBounds\(highestViewedIndex: number, itemCount: number\)/);
  assert.match(
    shortsPageSource,
    /const videoWindow = getVideoWindowBounds\(cacheWindowHighIndex, items\.length\);/
  );
  assert.match(
    shortsPageSource,
    /const isInCacheWindow =\s*index >= videoWindow\.start && index <= videoWindow\.end;/
  );
  assert.match(
    shortsPageSource,
    /const shouldMount =\s*isActiveSlide \|\|\s*\(!useIOSSharedVideo &&\s*\(isInCacheWindow \|\| shouldPrepareNext \|\| shouldPreload\)\);/
  );
  // 视频窗口内已缓冲过的视频都保留 src，来回切换均复用缓存
  assert.match(
    shortsPageSource,
    /const shouldRetainCached =\s*!useIOSSharedVideo &&\s*isInCacheWindow &&\s*!isActiveSlide &&\s*cacheableSourceIds\.has\(item\.id\);/
  );
  // 窗口内视频一旦 canplay 就标记可复用，快速划走的视频回滑也有缓存
  assert.match(
    shortsPageSource,
    /if \(shouldLoad\) onSourceCached\(item\.id\);/
  );
  // 窗口内视频只要已经产生缓冲就同样标记，授权收回时不丢弃其数据
  assert.match(
    shortsPageSource,
    /if \(shouldLoad && videoHasBufferedData\(video\)\) \{\s*onSourceCached\(item\.id\);/
  );
  const playbackBlock = /\/\/ 先停掉所有非当前屏[\s\S]*?\}, \[activeIndex, items\.length\]\);/.exec(shortsPageSource);
  assert.ok(playbackBlock, "parent inactive-video pause effect should be present");
  assert.doesNotMatch(playbackBlock[0], /video\.play\(\)/);
  assert.doesNotMatch(playbackBlock[0], /currentTime\s*=\s*0/);
  assert.match(shortsPageSource, /shouldEagerLoad=\{shouldEagerLoad\}/);
  assert.match(shortsPageSource, /preload=\{shouldLoad \? \(shouldEagerLoad \? "auto" : "metadata"\) : "none"\}/);
});

test("shorts caps how many slides keep real content in the DOM", () => {
  // 队列批量裁剪之间仍会保留几十条回看历史；内容窗口必须是常量半径，
  // 避免每条 slide 的背景和海报位图一起常驻。
  assert.match(shortsPageSource, /const SLIDE_CONTENT_WINDOW_RADIUS = 3;/);
  assert.match(
    shortsPageSource,
    /const shouldRenderContent =\s*Math\.abs\(preloadOffset\) <= SLIDE_CONTENT_WINDOW_RADIUS \|\|\s*shouldMount \|\|\s*shouldRetainCached;/
  );
  assert.match(shortsPageSource, /shouldRenderContent=\{shouldRenderContent\}/);

  // 空壳仍然是一个等高的 [data-shorts-slide]：滚动高度和吸附点不变，
  // IntersectionObserver 也照样能观测到它，滑回来才重新长出内容。
  const shellReturn =
    /if \(!shouldRenderContent\) \{\s*return \(\s*<article([\s\S]*?)\/>\s*\);\s*\}/.exec(
      shortsPageSource
    );
  assert.ok(shellReturn, "out-of-window slides should render a bare shell");
  assert.match(shellReturn[1], /className="shorts-slide"/);
  assert.match(shellReturn[1], /data-shorts-slide=""/);
  assert.match(shellReturn[1], /data-index=\{index\}/);
  assert.doesNotMatch(shellReturn[1], /shorts-slide__bg/);
  assert.doesNotMatch(shellReturn[1], /shorts-slide__poster/);
  assert.doesNotMatch(shellReturn[1], /<video/);

  // 空壳是条件 return，后面不能再有 hook，否则 hook 调用顺序会随窗口变化。
  const slideTail = shortsPageSource.slice(
    shortsPageSource.indexOf("if (!shouldRenderContent) {"),
    shortsPageSource.indexOf("function ShortsLoadingSpinner")
  );
  assert.ok(slideTail.length > 0, "slide tail slice should be non-empty");
  assert.doesNotMatch(slideTail, /\buse[A-Z]\w*\(/);
});

test("long shorts sessions trim old shells without changing logical identity", () => {
  assert.match(
    shortsFeedSource,
    /export const MAX_SHORTS_QUEUE_ITEMS = 60;/
  );
  assert.match(
    shortsFeedSource,
    /export const SHORTS_QUEUE_KEEP_BEHIND = 20;/
  );
  assert.match(
    shortsPageSource,
    /const removeCount = getShortsQueueTrimCount\(activeIndex, items\.length\);[\s\S]*?pendingQueueTrimRef\.current = \{\s*anchorKey: shortsQueueItemKey\(activeItem\),\s*activeIndex: nextActiveIndex,/
  );
  assert.match(shortsPageSource, /observer\.unobserve\(slides\[index\]\);/);
  assert.match(
    shortsPageSource,
    /trimQueueBefore\(removeCount\);\s*setActiveIndex\(nextActiveIndex\);/
  );
  assert.match(shortsPageSource, /data-feed-key=\{itemKey\}/);
  assert.match(
    useShortsFeedSource,
    /queueStartOffsetRef\.current \+= removeCount;/
  );
});

test("shorts reuses one persistent media element across iOS slides", () => {
  assert.match(shortsPageSource, /const useIOSSharedVideo = shouldUseIOSSharedVideo\(\);/);
  assert.match(shortsPlatformSource, /function shouldUseIOSSharedVideo\(\)/);
  assert.match(shortsPlatformSource, /\\biPhone\\b\|\\biPad\\b\|\\biPod\\b/);
  assert.match(shortsPlatformSource, /navigator\.platform === "MacIntel" && navigator\.maxTouchPoints > 1/);
  // iOS 走共享元素分支，完全不参与上面那套 <video> 预载
  assert.match(
    shortsPageSource,
    /const shouldPreload =\s*!useIOSSharedVideo &&/
  );
  assert.match(shortsPageSource, /const iosSharedVideoRef = useRef<HTMLVideoElement \| null>\(null\);/);
  assert.match(shortsPageSource, /if \(!video\) \{\s*video = document\.createElement\("video"\);/);
  assert.match(shortsPageSource, /slot\.appendChild\(video\);/);
  assert.match(
    shortsPageSource,
    /video\.dataset\.shortsVideoId = item\.id;[\s\S]*?video\.src = item\.videoSrc;[\s\S]*?video\.load\(\);/
  );
  assert.match(shortsPageSource, /className="shorts-slide__ios-video-slot"/);
  assert.match(shortsPageSource, /sharedVideoRef=\{\s*useIOSSharedVideo \? iosSharedVideoRef : undefined/);
  assert.match(
    shortsCssSource,
    /\.shorts-slide__video--ios-shared\s*\{[\s\S]*?z-index:\s*2;/
  );
  assert.doesNotMatch(shortsPageSource, /key=\{item\.id\}[\s\S]{0,300}document\.createElement\("video"\)/);
});

// The iOS branch used to have no preloading at all: the single media element
// only got the next `src` after the swipe settled, so drive link resolution,
// the moov atom fetch and the first-frame decode all ran serially in front of
// the user. A second persistent element now warms the next video up front and
// the two swap roles on swipe, which keeps the "never recreate the node"
// constraint that WebKit's per-element audio grant depends on.
test("iOS preloads the next video on a standby element and promotes it on swipe", () => {
  assert.match(
    shortsPageSource,
    /const iosStandbyVideoRef = useRef<HTMLVideoElement \| null>\(null\);/
  );
  // The standby keeps the next src but switches between metadata and auto on
  // the same high/low watermark as the desktop branch.
  assert.match(
    shortsPageSource,
    /if \(!useIOSSharedVideo \|\| iosStandbyPreloadDisabled\) return;[\s\S]*?const nextIndex = activeIndex \+ browseDirectionRef\.current;/
  );
  assert.match(
    shortsPageSource,
    /applyIOSVideoRole\(standby, "standby"\);\s*standby\.preload = activeReadyForPreload \? "auto" : "metadata";/
  );
  assert.match(
    shortsPageSource,
    /standby\.dataset\.shortsVideoId = nextItem\.id;\s*standby\.poster = nextItem\.poster;\s*standby\.src = nextItem\.videoSrc;\s*standby\.load\(\);/
  );
  // The standby parks inside the next slide's slot, so landing there is a pure
  // ref swap with no DOM move and no src reset.
  assert.match(
    shortsPageSource,
    /if \(standby\.parentElement !== nextSlot\) nextSlot\.appendChild\(standby\);/
  );
  // The queue may legitimately repeat a video id, so the swap is anchored to
  // the index the standby was prepared for. Matching on id alone would promote
  // the standby while still on the same slide and restart playback from 0.
  assert.match(
    shortsPageSource,
    /iosStandbyVideoIndexRef\.current === activeIndex &&\s*preloaded\.dataset\.shortsVideoId === item\.id\s*\) \{[\s\S]*?iosSharedVideoRef\.current = preloaded;\s*iosStandbyVideoRef\.current = demoted;\s*iosStandbyVideoIndexRef\.current = iosSharedVideoIndexRef\.current;/
  );
  // Re-appending an element that already sits in the slot is a remove+insert
  // that needlessly disturbs the WebKit playback pipeline.
  assert.match(
    shortsPageSource,
    /if \(video\.parentElement !== slot\) slot\.appendChild\(video\);/
  );
});

test("the iOS standby element stays silent and never autoplays off-screen", () => {
  assert.match(
    shortsPageSource,
    /function applyIOSVideoRole\([\s\S]*?video\.autoplay = false;\s*video\.removeAttribute\("autoplay"\);\s*applyVideoMutedState\(video, true\);/
  );
  assert.match(
    shortsPageSource,
    /const standbyVideo = iosStandbyVideoRef\.current;\s*if \(standbyVideo\) applyVideoMutedState\(standbyVideo, true\);/
  );
});

// `?iosPreload=0` must disable the standby completely — not merely skip its
// src — so a device can A/B whether the second media element is what starves
// the active video's loop restart of buffer.
test("iOS standby preload has a kill switch for on-device A/B", () => {
  assert.match(
    shortsPlatformSource,
    /export function isIOSStandbyPreloadDisabled\(\)[\s\S]*?get\("iosPreload"\) === "0"/
  );
  assert.match(
    shortsPageSource,
    /const \[iosStandbyPreloadDisabled\] = useState\(isIOSStandbyPreloadDisabled\);/
  );
  // The element itself must not even be created when the switch is off.
  assert.match(
    shortsPageSource,
    /if \(!iosStandbyPreloadDisabled\) \{\s*applyIOSVideoRole\(acquireIOSVideoElement\(iosStandbyVideoRef\), "standby"\);/
  );
});

// WebKit accounts the unmuted-playback grant per media element, so the element
// that takes over playback later must collect it inside the same user gesture.
test("the sound toggle also grants unmuted playback to the iOS standby element", () => {
  assert.match(
    shortsPageSource,
    /if \(useIOSSharedVideo\) \{\s*const standbyVideo = iosStandbyVideoRef\.current;\s*if \(standbyVideo && standbyVideo !== activeVideo\) \{\s*unlockVideoAudioPlayback\(standbyVideo\);/
  );
  assert.match(
    shortsPageSource,
    /function unlockVideoAudioPlayback\([\s\S]*?request = video\.play\(\);[\s\S]*?video\.pause\(\);/
  );
});

test("stale iOS play work cannot control a later shared source", () => {
  assert.match(shortsPageSource, /let disposed = false;/);
  assert.match(shortsPageSource, /disposed = true;/);
  assert.match(shortsPageSource, /if \(retryTimer !== null\) window\.clearTimeout\(retryTimer\);/);
  assert.match(
    shortsPageSource,
    /getVideoElement\(\) === video[\s\S]*?video\.dataset\.shortsVideoId === item\.id/
  );
  assert.match(
    shortsPageSource,
    /const belongsToSlide = \(\) =>[\s\S]*?video\.dataset\.shortsVideoId === item\.id/
  );
});

test("iOS loops restart under app control and progress follows presented frames", () => {
  assert.match(shortsPageSource, /video\.loop = false;/);
  assert.match(shortsPageSource, /const loopRestartPendingRef = useRef\(false\);/);
  assert.match(shortsPageSource, /const handleIOSLoopEnded = \(\) => \{/);
  assert.match(shortsPageSource, /video\.addEventListener\("ended", handleIOSLoopEnded\);/);
  // Same restart sequence as before — mark the round pending, reset the
  // progress readout, arm the spinner, then seek back to 0. The spinner is now
  // armed through the deferred helper so a fast restart shows nothing at all;
  // see "iOS loop restart defers the buffering spinner instead of blinking".
  assert.match(
    shortsPageSource,
    /loopRestartPendingRef\.current = true;[\s\S]*?updateCurrentTime\(0\);[\s\S]*?scheduleBufferingIndicator\([\s\S]*?video\.currentTime = 0;/
  );
  assert.match(
    shortsPageSource,
    /const handleIOSLoopEnded = \(\) => \{[\s\S]*?if \(loopRestartPendingRef\.current\) \{\s*failRestart\(loopRestartAttemptRef\.current\);\s*return;/
  );
  assert.match(shortsPageSource, /const IOS_LOOP_FRAME_WATCHDOG_MS = \d+;/);
  assert.match(shortsPageSource, /const IOS_LOOP_RELOAD_TIMEOUT_MS = \d+;/);
  assert.match(shortsPageSource, /\}, timeoutMs\);/);
  assert.match(
    shortsPageSource,
    /if \(loopRestartReloadedRef\.current\) \{[\s\S]*?failRestart\(attempt\);[\s\S]*?loopRestartReloadedRef\.current = true;[\s\S]*?loopFrameBarrierRef\.current = null;[\s\S]*?video\.load\(\);/
  );
  assert.match(shortsPageSource, /video\.requestVideoFrameCallback\(handlePresentedFrame\)/);
  assert.match(shortsPageSource, /const mediaTime = metadata\.mediaTime;/);
  assert.match(
    shortsPageSource,
    /loopRestartPendingRef\.current[\s\S]*?metadata\.presentationTime >= frameBarrier/
  );
  assert.match(shortsPageSource, /loopFrameBarrierRef\.current = performance\.now\(\);/);
  assert.match(
    shortsPageSource,
    /if \(canObservePresentedFrames\) \{[\s\S]*?video\.currentTime = 0;[\s\S]*?\} else \{[\s\S]*?loopRestartReloadedRef\.current = true;[\s\S]*?video\.load\(\);/
  );
  assert.match(shortsPageSource, /confirmPresentedPlayback\(mediaTime\);/);
  assert.match(shortsPageSource, /video\.cancelVideoFrameCallback\(frameCallbackId\);/);

  const playingStart = shortsPageSource.indexOf("const handlePlaying = () => {");
  const playingEnd = shortsPageSource.indexOf("const handleProgress = () => {", playingStart);
  assert.ok(playingStart >= 0 && playingEnd > playingStart);
  const playingBlock = shortsPageSource.slice(playingStart, playingEnd);
  const waitingForFrameBranch =
    /if \(waitForIOSPlaybackMotion\) \{([\s\S]*?)\} else \{([\s\S]*?)\}/.exec(
      playingBlock
    );
  assert.ok(waitingForFrameBranch);
  assert.doesNotMatch(waitingForFrameBranch[1], /confirmPresentedPlayback|setIsBuffering\(false\)/);
  assert.match(waitingForFrameBranch[2], /confirmPresentedPlayback\(\);/);

  const confirmationStart = shortsPageSource.indexOf(
    "const confirmPresentedPlayback = useCallback("
  );
  const confirmationEnd = shortsPageSource.indexOf(
    "// 是否已点过赞。真正的防重",
    confirmationStart
  );
  assert.ok(confirmationStart >= 0 && confirmationEnd > confirmationStart);
  const confirmationBlock = shortsPageSource.slice(
    confirmationStart,
    confirmationEnd
  );
  assert.match(confirmationBlock, /clearLoopRestartWatchdog\(\);/);
  assert.match(confirmationBlock, /loopRestartPendingRef\.current = false;/);
  assert.match(confirmationBlock, /loopRestartAwaitingFrameRef\.current = false;/);
  assert.match(confirmationBlock, /hasStartedPlayingRef\.current = true;/);
  assert.match(confirmationBlock, /setIsBuffering\(false\);/);
  assert.match(confirmationBlock, /updateCurrentTime\(mediaTime\);/);
  assert.match(shortsPageSource, /const presentedFrameAdvanced =/);
  assert.match(
    shortsPageSource,
    /if \(playbackNeedsMotionConfirmation\) \{[\s\S]*?playbackMotionFrameCountRef\.current \+= 1;[\s\S]*?playbackMotionFrameCountRef\.current >= 2[\s\S]*?confirmPresentedPlayback\(mediaTime\);/
  );

  // The ordinary per-slide videos keep native looping; only the iOS shared
  // element uses the controlled restart path.
  assert.match(
    shortsPageSource,
    /<video[\s\S]*?autoPlay=\{isActive\}[\s\S]*?playsInline[\s\S]*?loop[\s\S]*?muted=\{muted\}/
  );
});

test("shorts buffering state survives stalled and self-heals on real progress", () => {
  const stalledStart = shortsPageSource.indexOf("const handleStalled = () => {");
  const stalledEnd = shortsPageSource.indexOf("const handleError = () => {", stalledStart);
  assert.ok(stalledStart >= 0 && stalledEnd > stalledStart);
  const stalledBlock = shortsPageSource.slice(stalledStart, stalledEnd);
  assert.doesNotMatch(stalledBlock, /setIsBuffering\(true\)/);
  assert.doesNotMatch(stalledBlock, /hasStartedPlayingRef\.current = false/);
  assert.match(stalledBlock, /onActiveNeedsPriority\(index\);/);

  const timeStart = shortsPageSource.indexOf("const handleTime = () => {");
  const timeEnd = shortsPageSource.indexOf("const handleWaiting = () => {", timeStart);
  assert.ok(timeStart >= 0 && timeEnd > timeStart);
  const timeBlock = shortsPageSource.slice(timeStart, timeEnd);
  assert.match(timeBlock, /const mediaTimeAdvanced =/);
  assert.match(
    timeBlock,
    /if \(\s*!usesPresentedFrameProgress &&\s*!loopRestartPendingRef\.current &&\s*!video\.seeking &&\s*!scrubbingRef\.current\s*\) \{\s*updateCurrentTime\(mediaTime\);/
  );
  assert.match(timeBlock, /!video\.paused/);
  assert.match(timeBlock, /!video\.seeking/);
  assert.match(timeBlock, /confirmPresentedPlayback\(mediaTime\);/);

  const waitingStart = shortsPageSource.indexOf("const handleWaiting = () => {");
  const waitingEnd = shortsPageSource.indexOf("const cacheAvailableSource", waitingStart);
  assert.ok(waitingStart >= 0 && waitingEnd > waitingStart);
  const waitingBlock = shortsPageSource.slice(waitingStart, waitingEnd);
  // The deferred-indicator timer now lives in a shared helper so the iOS loop
  // restart can reuse it; handleWaiting must still go through it rather than
  // lighting the spinner up synchronously.
  assert.match(waitingBlock, /scheduleBufferingIndicator\(/);
  assert.doesNotMatch(waitingBlock, /setIsBuffering\(true\)/);
  assert.match(waitingBlock, /hasStartedPlayingRef\.current/);
  assert.match(
    shortsPageSource,
    /const scheduleBufferingIndicator = useCallback\([\s\S]*?SHORTS_BUFFERING_INDICATOR_DELAY_MS\)/
  );
});

// A short video loops often, and iOS drives every loop through a manual
// seek-and-await-first-frame restart. Lighting the spinner up the moment
// `ended` fires made it blink on every single lap even when the restart landed
// in a few dozen milliseconds; Android's native loop is seamless.
test("iOS loop restart defers the buffering spinner instead of blinking every lap", () => {
  const endedStart = shortsPageSource.indexOf("const handleIOSLoopEnded = () => {");
  const endedEnd = shortsPageSource.indexOf(
    "const retryRestartWhenReady",
    endedStart
  );
  assert.ok(endedStart >= 0 && endedEnd > endedStart);
  const endedBlock = shortsPageSource.slice(endedStart, endedEnd);
  assert.doesNotMatch(endedBlock, /setIsBuffering\(true\)/);
  assert.match(
    endedBlock,
    /scheduleBufferingIndicator\(\s*\(\) => canContinueRestart\(attempt\) && loopRestartAwaitingFrameRef\.current\s*\)/
  );

  // The `play` event fired by the restart would otherwise re-light it
  // synchronously and undo the deferral.
  const playStart = shortsPageSource.indexOf("const handleIOSLoopPlay = () => {");
  const playEnd = shortsPageSource.indexOf("video.addEventListener(\"ended\"", playStart);
  assert.ok(playStart >= 0 && playEnd > playStart);
  const playBlock = shortsPageSource.slice(playStart, playEnd);
  assert.doesNotMatch(playBlock, /setIsBuffering\(true\)/);
  assert.match(playBlock, /const attempt = loopRestartAttemptRef\.current;/);
  assert.match(playBlock, /scheduleBufferingIndicator\(/);
  assert.match(playBlock, /startFrameWatchdog\(attempt\);/);
});

// Applying a CSS filter to a <video> drops WebKit out of the direct video
// compositing path: every frame has to be rendered into a layer and filtered.
// The shadow stays on the static poster, where it costs one pass.
test("the active slide never puts a CSS filter on the playing video", () => {
  const videoRule = /\.shorts-slide__video\s*\{([\s\S]*?)\}/.exec(
    shortsCssSource
  );
  assert.ok(videoRule, "video compositing rule should exist");
  assert.match(videoRule[1], /opacity:\s*1;/);
  assert.doesNotMatch(videoRule[1], /filter:|transition:/);
  assert.match(
    shortsCssSource,
    /\.shorts-page\.has-video-transition[\s\S]*?\.shorts-slide\[data-active="true"\][\s\S]*?\.shorts-slide__video/
  );
  assert.match(
    shortsPlatformSource,
    /isLegacyShortsVideoTransitionEnabled\(\)[\s\S]*?get\("shortsVideoTransition"\) ===\s*"1"/
  );
  assert.match(
    shortsCssSource,
    /\.shorts-slide\[data-active="true"\] \.shorts-slide__poster \{\s*filter: drop-shadow/
  );
});

test("shorts uses a server-preblurred texture instead of a viewport blur layer", () => {
  assert.match(videosDataSource, /backgroundPoster\?: string;/);
  assert.match(
    shortsPageSource,
    /backgroundImage: `url\(\$\{item\.backgroundPoster \|\| item\.poster\}\)`/
  );
  const backgroundRule = /\.shorts-slide__bg\s*\{([\s\S]*?)\}/.exec(
    shortsCssSource
  );
  assert.ok(backgroundRule, "shorts background rule should exist");
  assert.doesNotMatch(backgroundRule[1], /filter:\s*blur/);
  assert.match(
    shortsCssSource,
    /\.shorts-slide__bg::after\s*\{[\s\S]*?background:\s*rgb\(0 0 0 \/ 55%\);/
  );
  assert.match(
    shortsFeedGoSource,
    /BackgroundPoster\s+string\s+`json:"backgroundPoster,omitempty"`/
  );
  assert.match(
    shortsFeedGoSource,
    /BackgroundPoster:\s+shortsBackgroundPosterURL\(poster\),/
  );
});

test("shorts grants preload only after the active video really started", () => {
  assert.match(shortsPageSource, /const hasStartedPlayingRef = useRef\(false\);/);
  assert.match(
    shortsPageSource,
    /const confirmPresentedPlayback = useCallback\([\s\S]*?hasStartedPlayingRef\.current = true;/
  );
  assert.match(
    shortsPageSource,
    /const handlePlaying = \(\) => \{[\s\S]*?confirmPresentedPlayback\(\);/
  );
  assert.match(
    shortsPageSource,
    /currentVideo\.paused \|\|[\s\S]*?!hasStartedPlayingRef\.current \|\|[\s\S]*?onActiveNeedsPriority\(index\);/
  );
});

test("shorts sound toggle limits playback recovery to the iOS media path", () => {
  assert.match(shortsPageSource, /function applyVideoMutedState/);
  assert.doesNotMatch(shortsPageSource, /onFirstPointer/);
  assert.doesNotMatch(shortsPageSource, /currentPage\.addEventListener\("pointerdown"/);
  assert.match(
    shortsPageSource,
    /const stopHeaderControlPropagation = useCallback\(\(e: React\.SyntheticEvent\) => \{\s*e\.stopPropagation\(\);/
  );
  assert.match(shortsPageSource, /onPointerDownCapture=\{stopHeaderControlPropagation\}/);
  assert.match(shortsPageSource, /onTouchStartCapture=\{stopHeaderControlPropagation\}/);
  assert.match(shortsPageSource, /onPointerDown=\{stopHeaderControlPropagation\}/);
  assert.match(shortsPageSource, /onTouchStart=\{stopHeaderControlPropagation\}/);
  assert.match(shortsPageSource, /function normalizeVideoPlaybackRate/);
  assert.match(shortsPageSource, /function stabilizeVideoAfterAudioToggle/);
  assert.match(
    shortsPageSource,
    /if \(!useIOSSharedVideo\) \{\s*applyVideoMutedState\(activeVideo, next\);\s*\} else \{[\s\S]*?normalizeVideoPlaybackRate\(activeVideo\);\s*applyVideoMutedState\(activeVideo, next\);[\s\S]*?activeVideo\.play\(\)\.catch[\s\S]*?stabilizeVideoAfterAudioToggle/
  );
  assert.match(shortsPageSource, /getVideoAtIndex\(activeIndexRef\.current\) === activeVideo/);
  assert.match(shortsPageSource, /stabilizeVideoAfterAudioToggle\(\s*activeVideo,\s*canResumeActiveVideo\s*\);/);
  assert.match(shortsPageSource, /if \(shouldResume\(\) && video\.paused && !video\.ended\) \{/);
  assert.match(shortsPageSource, /for \(const delay of \[80, 240, 600\]\)/);
  assert.match(
    shortsPageSource,
    /const sharedVideo = iosSharedVideoRef\.current;\s*if \(sharedVideo\) applyVideoMutedState\(sharedVideo, muted\);/
  );
  assert.match(shortsPageSource, /\}, \[muted, items\.length, useIOSSharedVideo\]\);/);
});

test("shorts leaves loudness to the system and only exposes mute", () => {
  assert.match(
    shortsPageSource,
    /<button[\s\S]*?className="shorts-header__icon-btn"[\s\S]*?aria-label=\{muted \? "取消静音" : "静音"\}[\s\S]*?handleMuteButtonClick\(\);/
  );
  assert.doesNotMatch(shortsPageSource, /type="range"/);
  assert.doesNotMatch(shortsPageSource, /handleVolumeSliderChange|setVolume|volumeRef/);
  assert.doesNotMatch(shortsPageSource, /video\.volume\s*=/);
  assert.doesNotMatch(shortsCssSource, /shorts-header__volume-slider|shorts-header__volume-group/);
  assert.match(
    shortsPageSource,
    /function applyVideoMutedState\(video: HTMLVideoElement, nextMuted: boolean\) \{[\s\S]*?video\.muted = nextMuted;/
  );
});

test("Windows viewport resize keeps the current short aligned", () => {
  assert.match(
    shortsPageSource,
    /const isWindowsShortsPlatform = isWindowsPlatform\(\);/
  );
  assert.match(
    shortsPlatformSource,
    /function isWindowsPlatform\(\) \{[\s\S]*?\/\^Win\/i\.test\(platform\) \|\| \/\\bWindows\\b\/i\.test\(ua\);/
  );
  assert.match(
    shortsPageSource,
    /const viewportResizeAnchorIndexRef = useRef<number \| null>\(null\);/
  );
  assert.match(
    shortsPageSource,
    /const handleViewportResize = \(\) => \{[\s\S]*?viewportResizeAnchorIndexRef\.current = activeIndexRef\.current;[\s\S]*?alignAnchoredSlide\(\);[\s\S]*?window\.requestAnimationFrame/
  );
  assert.match(
    shortsPageSource,
    /root\.scrollTop = activeSlide\.offsetTop;/
  );
  assert.match(
    shortsPageSource,
    /window\.addEventListener\("resize", handleViewportResize\);/
  );
  assert.match(
    shortsPageSource,
    /document\.addEventListener\("fullscreenchange", handleViewportResize\);/
  );
  assert.match(
    shortsPageSource,
    /const observer = new IntersectionObserver\(\s*\(entries\) => \{\s*if \(\s*viewportResizeAnchorIndexRef\.current !== null \|\|\s*queueTrimInProgressRef\.current \|\|\s*pagerGestureActiveRef\.current\s*\) \{\s*return;\s*\}/
  );
});

test("shorts sizes the preload gate by bitrate, not by a fixed second count", () => {
  // 门槛的真实预算是字节数；秒数只是它在某个码率下的换算结果。
  // 行为用例见 tests/shortsMediaBuffer.test.ts。
  assert.match(mediaBufferSource, /const ACTIVE_PRELOAD_BUFFER_BYTES = 4 \* 1024 \* 1024;/);
  assert.match(mediaBufferSource, /const ACTIVE_PRELOAD_MIN_BUFFER_SECONDS = 4;/);
  assert.match(mediaBufferSource, /const ACTIVE_PRELOAD_BUFFER_SECONDS = 12;/);
  assert.match(
    mediaBufferSource,
    /return clamp\(\s*ACTIVE_PRELOAD_BUFFER_BYTES \/ bytesPerSecond,\s*ACTIVE_PRELOAD_MIN_BUFFER_SECONDS,\s*ACTIVE_PRELOAD_BUFFER_SECONDS\s*\);/
  );

  // 每条 slide 用自己的水位，而不是模块级常量
  assert.match(
    shortsPageSource,
    /const preloadBufferSeconds = preloadBufferSecondsFor\(\s*averageBytesPerSecond\(item\)\s*\);\s*const preloadKeepSeconds = preloadKeepSecondsFor\(preloadBufferSeconds\);/
  );
  assert.match(
    shortsPageSource,
    /if \(videoHasComfortableBuffer\(currentVideo, preloadBufferSeconds\)\) \{\s*onActiveReadyForPreload\(index\);\s*\} else if \(videoBufferIsCritical\(currentVideo, preloadKeepSeconds\)\) \{/
  );
  // 水位变化必须重挂媒体 effect，否则闭包里还是旧阈值
  assert.match(
    shortsPageSource,
    /onSourceCached,\s*preloadBufferSeconds,\s*preloadKeepSeconds,\s*resetLoopRestartState,/
  );

  // 码率来自后端，元数据缺失时字段被省略，前端按未知码率兜底
  assert.match(
    shortsFeedGoSource,
    /SizeBytes\s+int64\s+`json:"sizeBytes,omitempty"`/
  );
  assert.match(
    shortsFeedGoSource,
    /DurationSeconds\s+int\s+`json:"durationSeconds,omitempty"`/
  );
  assert.match(
    shortsFeedGoSource,
    /videoSrc, sizeBytes := s\.videoSourceAndSize\(video\)\s*durationSeconds := video\.DurationSeconds/
  );
  assert.match(
    shortsFeedGoSource,
    /if sizeBytes <= 0 \|\| durationSeconds <= 0 \{\s*sizeBytes = 0\s*durationSeconds = 0\s*\}/
  );
  assert.match(
    shortsFeedGoSource,
    /VideoSrc:\s+videoSrc,\s*Poster:[\s\S]*?SizeBytes:\s+sizeBytes,\s*DurationSeconds:\s+durationSeconds,/
  );
  assert.match(videosDataSource, /sizeBytes\?: number;\s*durationSeconds\?: number;/);

  // ?debug=1 面板要能看到实际生效的门槛，否则真机上无法确认
  assert.match(shortsHudSource, /gate=\$\{preloadBufferSeconds\.toFixed\(1\)\}s rate=/);
});

test("shorts keeps per-swipe work off the queue length", () => {
  // ① 观察器只建一次，新批次增量补充；不再每 5 条 disconnect + 整队重挂。
  assert.match(
    shortsPageSource,
    /const slideObserverRef = useRef<IntersectionObserver \| null>\(null\);/
  );
  assert.match(
    shortsPageSource,
    /const observedSlidesRef = useRef<WeakSet<Element>>\(new WeakSet\(\)\);/
  );
  assert.match(
    shortsPageSource,
    /slideObserverRef\.current = observer;\s*return \(\) => \{\s*slideObserverRef\.current = null;\s*observedSlidesRef\.current = new WeakSet\(\);\s*observer\.disconnect\(\);\s*\};\s*\}, \[\]\);/
  );
  assert.match(
    shortsPageSource,
    /slides\.forEach\(\(el\) => \{\s*if \(observedSlidesRef\.current\.has\(el\)\) return;\s*observedSlidesRef\.current\.add\(el\);\s*observer\.observe\(el\);\s*\}\);\s*\}, \[items\.length\]\);/
  );
  assert.doesNotMatch(
    shortsPageSource,
    /slides\.forEach\(\(el\) => observer\.observe\(el\)\);/
  );

  // ② slide 上 memo，切屏只重渲染 props 真正变了的那几条。
  assert.match(shortsPageSource, /^import \{\s*memo,/m);
  assert.match(shortsPageSource, /function ShortsSlideImpl\(\{/);
  assert.match(shortsPageSource, /const ShortsSlide = memo\(ShortsSlideImpl\);/);
  // memo 只有在传下去的回调保持稳定时才有意义，逐个钉住依赖为空。
  for (const callback of [
    "handleActiveReadyForPreload",
    "handleActiveNeedsPriority",
    "handleSourceCached",
    "isVideoPausedByUser",
    "hasLiked",
    "showHud",
  ]) {
    // `(?!\n  const )` 把匹配夹在本条声明内部。少了它，非贪婪的 [\s\S]*?
    // 会一路扫到后面某个不相干的 `[]);`，让断言恒真。
    const declared = new RegExp(
      `const ${callback} = useCallback\\((?:(?!\\n  const )[\\s\\S])*?\\[\\]\\s*\\);`
    );
    assert.match(shortsPageSource, declared, `${callback} should be [] stable`);
  }

  // ③ 备用元素预载不占绘制前的同步块；提升那一条仍然必须是 layout effect。
  assert.match(
    shortsPageSource,
    /useEffect\(\(\) => \{\s*if \(!useIOSSharedVideo \|\| iosStandbyPreloadDisabled\) return;/
  );
  // 备用元素始终保留下一条的源，但全速预载受当前视频健康状态控制。
  assert.doesNotMatch(
    shortsPageSource,
    /iosStandbyPreloadDisabled \|\| !activeReadyForPreload/
  );
  assert.match(
    shortsPageSource,
    /standby\.preload = activeReadyForPreload \? "auto" : "metadata";/
  );
  // 拉下字节还不够：video 在真正播放前一直画 poster，要 seek 一次逼出首帧
  assert.match(shortsPageSource, /warmStandbyFirstFrame\(standby\);/);
  // 备用元素跟着浏览方向放：只盯 activeIndex+1 的话，往回看每一条都是冷启动
  assert.match(shortsPageSource, /const browseDirectionRef = useRef\(1\);/);
  assert.match(
    shortsPageSource,
    /browseDirectionRef\.current = activeIndex > previous \? 1 : -1;/
  );
  // 队列裁剪的索引重排不代表浏览方向
  assert.match(
    shortsPageSource,
    /if \(!queueTrimInProgressRef\.current && activeIndex !== previous\)/
  );
  assert.match(
    shortsPageSource,
    /function warmStandbyFirstFrame\(video: HTMLVideoElement\)/
  );
  assert.match(
    shortsPageSource,
    /video\.addEventListener\("loadedmetadata", nudge, \{ once: true \}\)/
  );
  assert.match(
    shortsPageSource,
    /useLayoutEffect\(\(\) => \{\s*if \(!useIOSSharedVideo\) return;\s*const item = items\[activeIndex\];/
  );
});

test("shorts links exit document fullscreen before leaving the immersive page", () => {
  assert.match(shortsPageSource, /import \{ Link, useNavigate \} from "react-router";/);
  assert.match(shortsPageSource, /const navigate = useNavigate\(\);/);
  assert.match(
    shortsPageSource,
    /const handleShortsRouteClick = useCallback\([\s\S]*?const exitRequest = exitDocumentFullscreen\(\);[\s\S]*?if \(!exitRequest\) return;[\s\S]*?event\.preventDefault\(\);[\s\S]*?const completeNavigation = \(\) => navigate\(destination\);[\s\S]*?exitRequest\.then\(completeNavigation, completeNavigation\)/
  );
  assert.match(
    shortsPageSource,
    /function exitDocumentFullscreen\(\): Promise<void> \| null \{[\s\S]*?fullscreenDocument\.fullscreenElement \?\?[\s\S]*?fullscreenDocument\.webkitFullscreenElement[\s\S]*?fullscreenDocument\.exitFullscreen\?\.bind[\s\S]*?fullscreenDocument\.webkitExitFullscreen\?\.bind[\s\S]*?Promise\.resolve\(exitFullscreen\(\)\)/
  );
  assert.match(
    shortsPageSource,
    /<Link\s*to="\/"[\s\S]*?className="shorts-header__back"[\s\S]*?onClick=\{handleBackToHomeClick\}/
  );
  assert.match(
    shortsPageSource,
    /onRouteClick=\{handleShortsRouteClick\}/
  );
  assert.doesNotMatch(shortsPageSource, /shorts-slide__title-link/);
  assert.match(
    shortsPageSource,
    /className="shorts-drive-badge"[\s\S]*?onClick=\{\(event\) => onRouteClick\(event, detailPath\)\}/
  );
});

test("shorts page defaults to immersive playback without fullscreen controls", () => {
  assert.match(shortsPageSource, /const activeIndexRef = useRef\(0\)/);
  assert.match(shortsCssSource, /\.shorts-page \{[\s\S]*height:\s*100svh/);
  assert.match(shortsPageSource, /html\.style\.overflow = "hidden"/);
  assert.match(shortsPageSource, /body\.style\.overflow = "hidden"/);
  assert.match(shortsPageSource, /body\.style\.background = "#000"/);
  assert.doesNotMatch(shortsPageSource, /Maximize/);
  assert.doesNotMatch(shortsPageSource, /Minimize/);
  assert.doesNotMatch(shortsPageSource, /aria-label=\{isFullscreen \? "退出全屏" : "进入全屏"\}/);
  assert.doesNotMatch(shortsPageSource, /e\.key === "f"/);
  assert.doesNotMatch(shortsPageSource, /requestFullscreen/);
});
