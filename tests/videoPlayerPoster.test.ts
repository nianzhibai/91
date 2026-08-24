import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const detailCss = readFileSync(
  new URL("../src/styles/video-detail.css", import.meta.url),
  "utf8"
);
const playerSource = readFileSync(
  new URL("../src/components/VideoPlayer.tsx", import.meta.url),
  "utf8"
);
const detailPageSource = readFileSync(
  new URL("../src/pages/VideoDetailPage.tsx", import.meta.url),
  "utf8"
);
const detailLoadingSource = readFileSync(
  new URL("../src/components/VideoDetailLoading.tsx", import.meta.url),
  "utf8"
);
const railSkeletonSource = readFileSync(
  new URL("../src/components/VideoRailSkeleton.tsx", import.meta.url),
  "utf8"
);

test("detail player poster uses full-frame contain scaling", () => {
  assert.match(
    detailCss,
    /\.video-player \.art-poster\s*\{[^}]*background-position:\s*center[^}]*background-repeat:\s*no-repeat[^}]*background-size:\s*contain/s
  );
});

test("fullscreen subtitles follow the contained video frame and scale on desktop", () => {
  assert.match(
    playerSource,
    /const unbindFullscreenSubtitleLayout = bindFullscreenSubtitleLayout\([\s\S]*?unbindFullscreenSubtitleLayout\(\)/s
  );
  assert.match(playerSource, /new ResizeObserver\(scheduleUpdate\)/);
  assert.match(playerSource, /art\.on\("video:loadedmetadata", scheduleUpdate\)/);
  assert.match(playerSource, /art\.on\("fullscreenWeb", scheduleUpdate\)/);
  assert.match(playerSource, /art\.off\("fullscreenWeb", scheduleUpdate\)/);
  assert.match(
    detailCss,
    /\.art-video-player\.art-fullscreen,[\s\S]*\.art-video-player\.art-fullscreen-web,[\s\S]*--art-subtitle-bottom:\s*var\(--video-player-subtitle-bottom, 15px\)/s
  );
  assert.match(
    detailCss,
    /@media \(hover: hover\) and \(pointer: fine\)[\s\S]*--art-subtitle-font-size:\s*clamp\(28px, 1\.8vw, 36px\)/s
  );
});

test("mobile portrait subtitles stay fixed while landscape uses 28px", () => {
  assert.match(
    playerSource,
    /getFullscreenPlayerOrientation\([\s\S]*player\.dataset\[FULLSCREEN_SUBTITLE_ORIENTATION_DATASET\] = orientation/s
  );
  assert.match(
    playerSource,
    /delete player\.dataset\[FULLSCREEN_SUBTITLE_ORIENTATION_DATASET\]/
  );
  assert.match(
    detailCss,
    /@media \(hover: none\) and \(pointer: coarse\)[\s\S]*--art-subtitle-font-size:\s*20px/s
  );
  assert.match(
    detailCss,
    /data-video-player-subtitle-orientation="landscape"\][\s\S]*--art-subtitle-font-size:\s*28px/s
  );
  assert.match(
    detailCss,
    /\.art-video-player\.art-fullscreen\.art-control-show\[data-video-player-subtitle-orientation="portrait"\][\s\S]*\.art-subtitle,[\s\S]*\.art-video-player\.art-fullscreen-web\.art-control-show\[data-video-player-subtitle-orientation="portrait"\][\s\S]*bottom:\s*var\(--art-subtitle-bottom\)/s
  );
  assert.doesNotMatch(
    detailCss,
    /art-control-show\[data-video-player-subtitle-orientation="landscape"\]/
  );
});

test("detail player does not keep playback resume state", () => {
  assert.doesNotMatch(playerSource, /ResumePrompt/);
  assert.doesNotMatch(playerSource, /PlaybackRecord/);
  assert.doesNotMatch(playerSource, /PLAYBACK_KEY_PREFIX/);
  assert.doesNotMatch(playerSource, /maybeOfferResume/);
  assert.doesNotMatch(playerSource, /savePlaybackRecord/);
  assert.doesNotMatch(playerSource, /clearPlaybackRecord/);
  assert.doesNotMatch(playerSource, /video-player__resume/);
  assert.doesNotMatch(detailCss, /video-player__resume/);
});

test("detail player does not persist ArtPlayer user settings", () => {
  assert.doesNotMatch(playerSource, /localStorage/);
  assert.doesNotMatch(playerSource, /SETTINGS_KEY/);
  assert.doesNotMatch(playerSource, /readPlayerSettings/);
  assert.doesNotMatch(playerSource, /writePlayerSettings/);
  assert.doesNotMatch(playerSource, /video-site:player-settings/);
  assert.match(playerSource, /volume:\s*DEFAULT_SETTINGS\.volume/);
  assert.match(playerSource, /muted:\s*DEFAULT_SETTINGS\.muted/);
  assert.match(playerSource, /video\.playbackRate = DEFAULT_SETTINGS\.playbackRate/);
  assert.match(
    playerSource,
    /applyPlayerBrightness\(art,\s*DEFAULT_SETTINGS\.brightness\)/
  );
});

test("detail player uses compact ArtPlayer settings panel on mobile", () => {
  assert.match(playerSource, /const COMPACT_SETTING_LAYOUT = \{[\s\S]*width:\s*172[\s\S]*itemWidth:\s*148[\s\S]*itemHeight:\s*30/s);
  assert.match(
    playerSource,
    /configureArtPlayerSettingLayout\(\s*shouldUseCompactPlayerSettings\(mount,\s*enableOrientationControl\)\s*\)/
  );
  assert.match(playerSource, /Artplayer\.SETTING_WIDTH = layout\.width/);
  assert.match(playerSource, /Artplayer\.SETTING_ITEM_WIDTH = layout\.itemWidth/);
  assert.match(playerSource, /Artplayer\.SETTING_ITEM_HEIGHT = layout\.itemHeight/);
  assert.match(
    detailCss,
    /@media \(max-width:\s*640px\)\s*\{[\s\S]*\.video-player \.art-video-player\s*\{[^}]*--art-settings-icon-size:\s*18px[^}]*--art-settings-max-height:\s*132px[^}]*--art-selector-max-height:\s*132px/s
  );
});

test("detail player exposes a non-persistent loop switch in ArtPlayer settings", () => {
  assert.match(playerSource, /settings:\s*createPlayerSettings\(subtitleState, requestSubtitles\)/);
  assert.match(playerSource, /return \[createLoopSetting\(\), createSubtitleSetting\(state, requestSubtitles\)\]/);
  assert.match(playerSource, /function createLoopSetting\(\)/);
  assert.match(playerSource, /html:\s*"洗脑循环"/);
  assert.match(playerSource, /loop:\s*true/);
  assert.match(playerSource, /tooltip:\s*DEFAULT_SETTINGS\.loop \? "开" : "关"/);
  assert.match(playerSource, /switch:\s*DEFAULT_SETTINGS\.loop/);
  assert.match(playerSource, /video\.loop = DEFAULT_SETTINGS\.loop/);
  assert.match(playerSource, /this\.video\.loop = next/);
  assert.match(playerSource, /item\.tooltip = next \? "开" : "关"/);
});

test("detail player always exposes subtitle selector with default off and no offset setting", () => {
  assert.doesNotMatch(playerSource, /subtitleOffset/);
  assert.match(playerSource, /function createSubtitleSetting\([\s\S]*?state: SubtitleLoadState/);
  assert.match(playerSource, /html:\s*"字幕"/);
  assert.match(playerSource, /state\.status === "idle"[\s\S]*?\?\s*""/);
  assert.doesNotMatch(playerSource, /点击加载/);
  assert.match(playerSource, /art\.notice\.show = "正在加载字幕"/);
  assert.doesNotMatch(playerSource, /正在加载字幕…/);
  assert.match(
    playerSource,
    /name:\s*"online-subtitle-option-off"[\s\S]*?html:\s*"关闭"[\s\S]*?value:\s*"off"[\s\S]*?default:\s*true/
  );
  assert.match(
    playerSource,
    /name:\s*`online-subtitle-option-\$\{index\}`[\s\S]*?default:\s*false/
  );
  assert.match(
    detailCss,
    /\.video-player[\s\S]*?\.art-video-player[\s\S]*?\.art-settings[\s\S]*?\.art-setting-panel[\s\S]*?\.art-setting-item\[data-name\^="online-subtitle-option-"\]\s*\{\s*justify-content:\s*center;/
  );
  assert.match(
    detailCss,
    /\.video-player[\s\S]*?\.art-video-player[\s\S]*?\.art-settings[\s\S]*?\.art-setting-panel[\s\S]*?\.art-setting-item\[data-name\^="online-subtitle-option-"\][\s\S]*?> \.art-setting-item-left[\s\S]*?> \.art-setting-item-left-icon\s*\{\s*display:\s*none;/
  );
  assert.doesNotMatch(
    detailCss,
    /(?:^|\n)\s*\.art-setting-item-left-icon\s*\{\s*display:\s*none;/
  );
  assert.match(playerSource, /default:\s*false/);
  assert.match(playerSource, /mounted\(panel\)[\s\S]*?requestSubtitles\(\)/);
  assert.match(playerSource, /setting\.update\(createSubtitleSetting/);
  assert.doesNotMatch(playerSource, /art\.setting\.show = false/);
  assert.match(playerSource, /setting\.render\(subtitleSetting\.selector\)/);
  assert.match(playerSource, /setting\.show = true/);
  assert.doesNotMatch(playerSource, /src, subtitles, title/);
  assert.doesNotMatch(playerSource, /option\.subtitle = subtitleOption/);
});

test("detail player limits ArtPlayer automatic reconnect attempts", () => {
  assert.match(playerSource, /const ARTPLAYER_RECONNECT_TIME_MAX = 3;/);
  assert.match(
    playerSource,
    /Artplayer\.RECONNECT_TIME_MAX = ARTPLAYER_RECONNECT_TIME_MAX;/
  );
});

test("detail page stays at the document top after video data loads", () => {
  assert.match(
    detailPageSource,
    /window\.scrollTo\(\{ top: 0, behavior: "auto" \}\)/
  );
  assert.doesNotMatch(detailPageSource, /scrollIntoView/);
  assert.doesNotMatch(detailPageSource, /detailTopRef/);
});

test("detail page space hotkey works before player focus without hijacking controls", () => {
  const keyboardStart = playerSource.indexOf("function bindPlayerKeyboardHotkeys");
  const keyboardEnd = playerSource.indexOf(
    "function shouldEnableMobileOrientationControl"
  );
  assert.ok(keyboardStart >= 0 && keyboardEnd > keyboardStart);
  const keyboardBlock = playerSource.slice(keyboardStart, keyboardEnd);

  assert.match(
    keyboardBlock,
    /document\.addEventListener\("keydown", handlePageSpaceKeyDown\)/
  );
  assert.match(
    keyboardBlock,
    /document\.removeEventListener\("keydown", handlePageSpaceKeyDown\)/
  );
  assert.match(
    keyboardBlock,
    /if \(event\.code !== "Space" && event\.key !== " "\) return;[\s\S]*?shouldIgnorePageSpaceHotkey\(event\)[\s\S]*?event\.preventDefault\(\);[\s\S]*?handleSpace\(event\)/
  );
  assert.doesNotMatch(keyboardBlock, /art\.hotkey\.add\("Space"/);
  assert.match(
    playerSource,
    /const PLAYER_SPACE_HOTKEY_EXCLUDED_SELECTOR = \[[\s\S]*?"input"[\s\S]*?"button"[\s\S]*?"\[role='dialog'\]"/
  );
  assert.match(
    keyboardBlock,
    /document\.querySelector\(ACTIVE_MODAL_SELECTOR\)/
  );
  assert.match(
    keyboardBlock,
    /event\.defaultPrevented[\s\S]*?event\.isComposing[\s\S]*?event\.ctrlKey[\s\S]*?event\.metaKey/
  );
});

test("detail player previews held arrow-key seeks and commits once on release", () => {
  const keyboardStart = playerSource.indexOf("function bindPlayerKeyboardHotkeys");
  const keyboardEnd = playerSource.indexOf(
    "function shouldEnableMobileOrientationControl"
  );
  assert.ok(keyboardStart >= 0 && keyboardEnd > keyboardStart);
  const keyboardBlock = playerSource.slice(keyboardStart, keyboardEnd);

  assert.match(playerSource, /hotkey:\s*false/);
  assert.doesNotMatch(playerSource, /Artplayer\.SEEK_STEP\s*=/);
  assert.match(keyboardBlock, /let keyboardSeekTarget: number \| null = null/);
  assert.match(
    keyboardBlock,
    /const baseTime = keyboardSeekTarget \?\? art\.currentTime;[\s\S]*?keyboardSeekTarget = clamp\(baseTime \+ delta, 0, duration\)/
  );
  assert.match(
    keyboardBlock,
    /art\.emit\("setBar", "played", keyboardSeekTarget \/ duration\)/
  );
  assert.match(
    keyboardBlock,
    /art\.on\("video:timeupdate", handleTimeUpdate\)/
  );
  assert.match(
    keyboardBlock,
    /document\.addEventListener\("keyup", handleKeyUp\)/
  );
  assert.match(
    keyboardBlock,
    /scheduleKeyboardSeekIdleCommit\(\)[\s\S]*?KEYBOARD_SEEK_IDLE_COMMIT_MS/
  );
  assert.match(
    keyboardBlock,
    /heldSeekKeys\.size === 0\) commitKeyboardSeek\(\)/
  );

  const previewStart = keyboardBlock.indexOf("function previewKeyboardSeek");
  const commitStart = keyboardBlock.indexOf("function commitKeyboardSeek");
  const escapeStart = keyboardBlock.indexOf("const handleEscape");
  assert.ok(previewStart >= 0 && commitStart > previewStart && escapeStart > commitStart);
  assert.doesNotMatch(
    keyboardBlock.slice(previewStart, commitStart),
    /art\.seek\s*=/
  );
  assert.match(
    keyboardBlock.slice(commitStart, escapeStart),
    /art\.seek = target/
  );
});

test("detail loading skeleton matches current desktop video page layout", () => {
  assert.match(detailPageSource, /<VideoDetailLoading isAdmin=\{isAdmin\} \/>/);
  assert.match(detailLoadingSource, /className="vd-layout vd-skeleton"/);
  assert.match(detailLoadingSource, /className="vd-skeleton__summary"/);
  assert.match(detailLoadingSource, /className="vd-skeleton__info"/);
  assert.match(detailLoadingSource, /<VideoRailSkeleton \/>/);
  assert.match(
    railSkeletonSource,
    /function VideoRailSkeleton[\s\S]*?>\s*推荐视频\s*<[\s\S]*?>相关合集<[\s\S]*?Array\.from\(\{ length: 6 \}\)/
  );
  assert.doesNotMatch(detailLoadingSource, /className="vd-skeleton__meta"/);
  assert.match(
    detailCss,
    /\.vd-skeleton__player\s*\{[^}]*aspect-ratio:\s*16 \/ 9[^}]*border-radius:\s*0/s
  );
  assert.match(
    detailCss,
    /\.vd-skeleton__summary,\s*\.vd-skeleton__info\s*\{[^}]*border:\s*1px solid var\(--border-default\)[^}]*border-radius:\s*var\(--radius-md\)/s
  );
  assert.match(
    detailCss,
    /\.vd-rail__loading-row\s*\{[^}]*grid-template-columns:\s*148px minmax\(0,\s*1fr\)/s
  );
  assert.doesNotMatch(
    detailCss,
    /\.vd-skeleton__player\s*\{[^}]*box-shadow:\s*var\(--shadow-lg\)/s
  );
});

test("detail loading skeleton keeps metadata bars uniform and concise", () => {
  const chipMatches =
    detailLoadingSource.match(
      /<span className="vd-skeleton__chip(?: [^"]+)?" \/>/g
    ) ?? [];
  assert.equal(chipMatches.length, 4);
  assert.match(
    detailLoadingSource,
    /vd-skeleton__chip vd-skeleton__chip--mobile-hidden/
  );
  assert.doesNotMatch(
    detailLoadingSource,
    /vd-skeleton__chip--(?:source|plain)/
  );
  assert.match(
    detailCss,
    /\.vd-skeleton__chip\s*\{[^}]*width:\s*88px;[^}]*height:\s*18px;[^}]*border-radius:\s*var\(--radius-sm\)/s
  );
  assert.match(
    detailCss,
    /@media \(max-width:\s*480px\)\s*\{[\s\S]*?\.vd-skeleton__chip--mobile-hidden\s*\{[^}]*display:\s*none/s
  );
});

test("detail info skeleton omits the two description lines", () => {
  assert.match(
    detailLoadingSource,
    /className="vd-skeleton__info"[\s\S]*?vd-skeleton__section-head[\s\S]*?vd-skeleton__tag-row/
  );
  assert.doesNotMatch(detailLoadingSource, /vd-skeleton__line/);
  assert.doesNotMatch(detailCss, /\.vd-skeleton__line/);
});

test("detail loading skeleton actions stay inside mobile viewport", () => {
  assert.match(
    detailCss,
    /@media \(max-width:\s*480px\)\s*\{[\s\S]*\.vd-skeleton__actions\s*\{[^}]*grid-template-columns:\s*minmax\(0,\s*1fr\) minmax\(0,\s*1fr\) 44px/s
  );
  assert.match(
    detailCss,
    /@media \(max-width:\s*480px\)\s*\{[\s\S]*\.vd-skeleton__actions span:last-child\s*\{[^}]*width:\s*100%/s
  );
});

test("detail loading skeleton mirrors the desktop action toolbar", () => {
  assert.match(detailLoadingSource, /vd-skeleton__action--like/);
  assert.match(detailLoadingSource, /vd-skeleton__action--dislike/);
  assert.match(detailLoadingSource, /vd-skeleton__action--share/);
  assert.match(
    detailLoadingSource,
    /\{isAdmin && \([\s\S]*?vd-skeleton__action--delete/
  );
  assert.match(
    detailCss,
    /\.vd-skeleton__action--share,[\s\S]*?\.vd-skeleton__action--delete\s*\{[^}]*width:\s*84px/s
  );
  assert.match(
    detailCss,
    /\.vd-skeleton__action--delete\s*\{[^}]*margin-left:\s*auto/s
  );
  assert.match(
    detailCss,
    /@media \(min-width:\s*769px\)\s*\{[\s\S]*?\.vd-skeleton__action--dislike\s*\{[^}]*margin-right:\s*calc\(var\(--space-3\) - var\(--space-2\)\)/s
  );
  assert.doesNotMatch(
    detailCss,
    /\.vd-skeleton__actions span:last-child\s*\{[^}]*width:\s*104px/s
  );
});

test("detail video title uses a restrained size", () => {
  assert.match(
    detailCss,
    /\.vd-header__title\s*\{[^}]*font-size:\s*var\(--font-xl\)[^}]*line-height:\s*1\.34/s
  );
  assert.doesNotMatch(
    detailCss,
    /\.vd-header__title\s*\{[^}]*font-size:\s*var\(--font-2xl\)/s
  );
  assert.match(
    detailCss,
    /@media \(max-width:\s*480px\)\s*\{[\s\S]*\.vd-header__title\s*\{[^}]*font-size:\s*var\(--font-base\)/s
  );
});

test("detail player uses custom mobile gestures instead of ArtPlayer native gestures", () => {
  assert.match(playerSource, /gesture:\s*false/);
  assert.match(playerSource, /fastForward:\s*false/);
  assert.match(playerSource, /const KEYBOARD_SEEK_SECONDS = 15;/);
  assert.match(playerSource, /bindPlayerKeyboardHotkeys\(art\)/);
  assert.doesNotMatch(playerSource, /GESTURE_SEEK_MIN_SECONDS/);
  assert.doesNotMatch(playerSource, /GESTURE_SEEK_MAX_SECONDS/);
  assert.doesNotMatch(playerSource, /GESTURE_SEEK_DURATION_RATIO/);
  assert.doesNotMatch(playerSource, /GESTURE_SEEK_SENSITIVITY/);
  assert.match(playerSource, /handleSeekGesture\(event,\s*dx\)/);
  assert.match(playerSource, /state\.startTime \+ \(dx \/ Math\.max\(1,\s*rect\.width\)\) \* duration/);
  assert.doesNotMatch(playerSource, /event\.touches\[0\]\.clientX - rect\.left/);
  assert.match(playerSource, /function bindMobilePlayerGestures/);
  assert.match(playerSource, /let suppressNextClick = false/);
  assert.match(playerSource, /endPress\(true\)/);
  assert.match(playerSource, /event\.stopImmediatePropagation\(\)/);
  assert.match(playerSource, /addEventListener\("click", handleClick, true\)/);
  assert.match(playerSource, /state\.mode = "seek"/);
  assert.match(playerSource, /state\.side === "right" \? "volume" : "brightness"/);
  assert.doesNotMatch(playerSource, /function isPlayerLandscapeExpanded/);
  assert.doesNotMatch(playerSource, /getEffectivePlayerOrientation\(art\) === "landscape"/);
  assert.match(playerSource, /if \(!isPlayerExpanded\(art\)\) \{\s*resetGesture\(\);/);
  assert.match(playerSource, /if \(!isPlayerExpanded\(art\)\) return;\s*onGestureHud\(seekGestureLabel/);
  assert.match(playerSource, /const FAST_RATE_CLASS = "art-fast-rate-active"/);
  assert.match(playerSource, /const FAST_RATE_HINT_CLASS = "video-player__art-rate-hint"/);
  assert.match(playerSource, /const PLAYER_GESTURE_HUD_CLASS = "video-player__art-gesture-hud"/);
  assert.match(playerSource, /setPlayerFastRateHint\(art, active\)/);
  assert.match(playerSource, /player\.appendChild\(hint\)/);
  assert.match(playerSource, /showPlayerGestureHud\(art, "volume", formatPercent\(normalized\)\)/);
  assert.match(playerSource, /showPlayerGestureHud\(art, "brightness", formatBrightnessPercent\(nextBrightness\)\)/);
  assert.match(playerSource, /stroke-width="1\.7"/);
  assert.match(playerSource, /M15\.4 9\.2a4\.2 4\.2 0 0 1 0 5\.6/);
  assert.match(playerSource, /M4\.8 9\.7h3l4\.3-3\.6v11\.8l-4\.3-3\.6h-3/);
  assert.doesNotMatch(playerSource, /stroke-width="2\.2"/);
  assert.doesNotMatch(playerSource, /onGestureHud\(`音量 /);
  assert.doesNotMatch(playerSource, /onGestureHud\(`亮度 /);
  assert.match(playerSource, /fullscreen:\s*true/);
  assert.match(playerSource, /fullscreenWeb:\s*enableWebFullscreen/);
  assert.doesNotMatch(playerSource, /addTextTrack\("captions", "Playback rate"/);
  assert.doesNotMatch(playerSource, /new VTTCue\(/);
  assert.doesNotMatch(playerSource, /onGestureHud\(`\$\{FAST_RATE\}x`/);
  assert.match(playerSource, /addEventListener\("touchmove", handleTouchMove, \{ passive: false \}\)/);
});

test("detail player auto-hides controls during mobile fullscreen playback", () => {
  const helperStart = playerSource.indexOf(
    "function bindMobileFullscreenControlAutoHide"
  );
  const helperEnd = playerSource.indexOf(
    "function setPlayerFastRateHint",
    helperStart
  );
  assert.ok(helperStart >= 0 && helperEnd > helperStart);
  const helperBlock = playerSource.slice(helperStart, helperEnd);

  assert.match(
    playerSource,
    /const ARTPLAYER_CONTROL_HIDE_TIME_MS = 2_000;/
  );
  assert.match(
    playerSource,
    /Artplayer\.CONTROL_HIDE_TIME = ARTPLAYER_CONTROL_HIDE_TIME_MS;/
  );
  assert.match(
    playerSource,
    /const unbindMobileFullscreenControlAutoHide =\s*bindMobileFullscreenControlAutoHide\(art\)/
  );
  assert.match(playerSource, /unbindMobileFullscreenControlAutoHide\(\)/);
  assert.match(helperBlock, /if \(!isMobilePlaybackDevice\(\)\) return noop/);
  assert.match(
    helperBlock,
    /!isPlayerExpanded\(art\) \|\| !art\.playing \|\| !art\.controls\.show/
  );
  assert.match(helperBlock, /Artplayer\.CONTROL_HIDE_TIME/);
  assert.match(helperBlock, /art\.setting\.show/);
  assert.match(helperBlock, /art\.isInput/);
  assert.match(helperBlock, /classList\.contains\(FAST_RATE_CLASS\)/);
  assert.match(helperBlock, /art\.controls\.show = false/);
  assert.match(helperBlock, /art\.on\("fullscreen", handleExpandedChange\)/);
  assert.match(helperBlock, /art\.on\("fullscreenWeb", handleExpandedChange\)/);
  assert.match(helperBlock, /art\.on\("video:playing", scheduleHide\)/);
  assert.match(helperBlock, /art\.on\("video:pause", clearHideTimer\)/);
  assert.match(helperBlock, /art\.on\("control", handleControlChange\)/);
  assert.match(helperBlock, /art\.on\("setting", handleSettingChange\)/);
  assert.match(helperBlock, /art\.off\("fullscreen", handleExpandedChange\)/);
  assert.match(helperBlock, /art\.off\("control", handleControlChange\)/);
});

test("detail player exits body-mounted web fullscreen before route cleanup", () => {
  const mountStart = playerSource.indexOf("function mountArtPlayer");
  const mountEnd = playerSource.indexOf(
    "function bindFullscreenSubtitleLayout",
    mountStart
  );
  assert.ok(mountStart >= 0 && mountEnd > mountStart);
  const mountBlock = playerSource.slice(mountStart, mountEnd);
  const cleanupStart = mountBlock.lastIndexOf("return () => {");
  assert.ok(cleanupStart >= 0);
  const cleanupBlock = mountBlock.slice(cleanupStart);
  const exitFullscreenIndex = cleanupBlock.indexOf(
    "if (art.fullscreenWeb) art.fullscreenWeb = false"
  );
  const destroyIndex = cleanupBlock.indexOf("art.destroy(true)");

  assert.ok(exitFullscreenIndex >= 0);
  assert.ok(destroyIndex > exitFullscreenIndex);
  assert.match(
    cleanupBlock,
    /if \(art\.fullscreenWeb\) art\.fullscreenWeb = false;[\s\S]*art\.destroy\(true\)/
  );
});

test("detail player hides orientation control on iPhone without disabling mobile gestures", () => {
  assert.match(
    playerSource,
    /controls:\s*createPlayerControls\(\s*enableOrientationControl,\s*enableTripleScreenControl\s*\)/
  );
  assert.match(
    playerSource,
    /if \(enableOrientationControl\) \{\s*controls\.push\(createOrientationControl\(\)\);\s*\}/
  );
  assert.match(playerSource, /function shouldEnableMobileOrientationControl\(\)\s*\{\s*return isMobilePlaybackDevice\(\) && !isApplePhoneDevice\(\);/);
  assert.match(playerSource, /function isApplePhoneDevice\(\)\s*\{\s*return \/iPhone\|iPod\/i\.test\(navigator\.userAgent\);/);
  assert.match(playerSource, /function shouldEnableMobileGestures\(\)\s*\{\s*return isMobilePlaybackDevice\(\);/);
});

test("detail player keeps only native fullscreen on Apple devices", () => {
  assert.match(playerSource, /const enableWebFullscreen = shouldEnableWebFullscreen\(enableOrientationControl\)/);
  assert.match(playerSource, /fullscreen:\s*true/);
  assert.match(playerSource, /fullscreenWeb:\s*enableWebFullscreen/);
  assert.match(playerSource, /function shouldEnableWebFullscreen\(enableOrientationControl: boolean\)\s*\{\s*return !enableOrientationControl && !isAppleDevice\(\);/);
  assert.match(playerSource, /function isAppleDevice\(\)/);
  assert.match(playerSource, /\/iPhone\|iPad\|iPod\|Macintosh\/i\.test\(navigator\.userAgent\)/);
});

test("detail player treats backend video routes as native mp4 sources", () => {
  assert.match(playerSource, /if \(isBackendNativeVideoRoute\(cleanPath\)\) return "mp4"/);
  assert.match(playerSource, /pathname\.startsWith\("\/p\/stream\/"\)/);
  assert.match(playerSource, /pathname\.startsWith\("\/p\/upload\/"\)/);
  assert.doesNotMatch(playerSource, /\/p\/spider91\//);
  assert.doesNotMatch(playerSource, /crossOrigin/);
});

test("detail player sets referrer policy before loading media url", () => {
  assert.match(playerSource, /const MEDIA_REFERRER_POLICY = "no-referrer"/);
  assert.match(playerSource, /url:\s*""/);
  assert.match(
    playerSource,
    /video\.setAttribute\("referrerpolicy", MEDIA_REFERRER_POLICY\);[\s\S]*art\.url = src;/
  );
});

test("iOS PikPak MP4 detail uses a typed source element without changing other pages", () => {
  assert.match(
    detailPageSource,
    /preferTypedMp4SourceOnIOS=\{isPikPakMp4Detail\(detail\)\}/
  );
  assert.match(
    detailPageSource,
    /detail\.mediaType\?\.toLowerCase\(\) === "video\/mp4"[\s\S]*detail\.sourceLabel\?\.toLowerCase\(\)\.includes\("pikpak"\)/
  );
  assert.match(
    playerSource,
    /preferTypedMp4SourceOnIOS && isIOSPlaybackDevice\(\)/
  );
  assert.match(
    playerSource,
    /if \(art\.isDestroy \|\| !video\.isConnected\) return;[\s\S]*source\.src = url;\s*source\.type = "video\/mp4";[\s\S]*video\.insertBefore\(source, video\.firstChild\);\s*video\.load\(\);/
  );
  assert.match(
    playerSource,
    /art\.option\.url = src;\s*loadTypedMp4Source\(video, src, art\);/
  );
  assert.match(
    playerSource,
    /if \(useTypedMp4Source\) clearTypedMp4Source\(video\);[\s\S]*art\.destroy\(true\);/
  );
});

test("detail player fullscreen long-press rate hint lives inside ArtPlayer", () => {
  assert.match(
    detailCss,
    /\.video-player__rate-hint,\s*\.video-player__art-rate-hint\s*\{[\s\S]*position:\s*absolute[\s\S]*top:\s*12px/s
  );
  assert.match(
    detailCss,
    /\.video-player__art-rate-hint\s*\{[^}]*z-index:\s*130/s
  );
  assert.match(
    detailCss,
    /\.art-video-player\.art-fullscreen \.video-player__art-rate-hint,[\s\S]*\.art-video-player\.art-fullscreen-web \.video-player__art-rate-hint,[\s\S]*position:\s*fixed/s
  );
});

test("detail player mobile brightness gesture only filters the video surface", () => {
  assert.match(
    detailCss,
    /\.video-player \.art-video,\s*\.video-player \.art-poster\s*\{[^}]*filter:\s*brightness\(var\(--video-player-brightness, 1\)\)/s
  );
  assert.match(
    detailCss,
    /@media \(hover: none\) and \(pointer: coarse\)\s*\{[\s\S]*\.video-player \.art-video-player,[\s\S]*touch-action:\s*pan-y/s
  );
  assert.match(
    detailCss,
    /\.video-player \.art-video-player\.art-fullscreen,[\s\S]*\.video-player \.art-video-player\.art-fullscreen-web,[\s\S]*touch-action:\s*none/s
  );
  assert.match(
    detailCss,
    /\.video-player__art-gesture-hud\s*\{[^}]*top:\s*16%[^}]*background:\s*rgba\(18,\s*18,\s*20,\s*0\.8\)[^}]*font-size:\s*18px/s
  );
  assert.match(
    detailCss,
    /\.video-player__art-gesture-hud-icon\s*\{[^}]*width:\s*18px[^}]*height:\s*18px[^}]*transform:\s*translateY\(-1px\)/s
  );
  assert.match(
    detailCss,
    /\.video-player__art-gesture-hud-icon svg\s*\{[^}]*width:\s*18px[^}]*height:\s*18px/s
  );
  assert.match(
    detailCss,
    /\.art-video-player\.art-fullscreen \.video-player__art-gesture-hud,[\s\S]*\.art-video-player\.art-manual-orientation \.video-player__art-gesture-hud\s*\{[^}]*position:\s*fixed/s
  );
});
