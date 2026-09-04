import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const playerSource = readFileSync(
  new URL("../src/components/VideoPlayer.tsx", import.meta.url),
  "utf8"
);
const detailCss = readFileSync(
  new URL("../src/styles/video-detail.css", import.meta.url),
  "utf8"
);

test("控制栏常驻倍速按钮，档位与设置面板共用一份", () => {
  assert.match(
    playerSource,
    /const controls: PlayerControl\[\] = \[createPlaybackRateControl\(\)\]/
  );
  assert.match(playerSource, /Artplayer\.PLAYBACK_RATE = PLAYBACK_RATE_OPTIONS/);
  assert.match(
    playerSource,
    /createPlaybackRateControl\(\)[\s\S]*selector: PLAYBACK_RATE_OPTIONS\.map/
  );
});

test("选中的倍速一起写入 defaultPlaybackRate，重连后不被重置", () => {
  assert.match(
    playerSource,
    /onSelect\([\s\S]*this\.playbackRate = rate;[\s\S]*this\.video\.defaultPlaybackRate = rate;/
  );
});

test("长按 2 倍速期间不改控制栏文案", () => {
  assert.match(
    playerSource,
    /function handleRateChange\(\)\s*\{[\s\S]*classList\.contains\(FAST_RATE_CLASS\)\) return;[\s\S]*updatePlaybackRateControl\(art\)/
  );
});

test("倍速下拉在触摸端靠 data-rate-open 展开，并在失焦/隐藏时收起", () => {
  assert.match(playerSource, /element\.dataset\.rateOpen = open \? "true" : "false"/);
  assert.match(playerSource, /art\.on\("blur", closeSelector\)/);
  assert.match(playerSource, /art\.off\("blur", closeSelector\)/);
  assert.match(
    playerSource,
    /function handleControlChange\(show: boolean\)\s*\{[\s\S]*if \(!show\) closeSelector\(\)/
  );
  assert.match(
    detailCss,
    /\.art-control-playbackRateToggle\[data-rate-open="true"\][\s\S]*\.art-selector-list\s*\{[^}]*opacity:\s*1[^}]*pointer-events:\s*auto/s
  );
  assert.match(
    detailCss,
    /@media \(hover: none\) and \(pointer: coarse\)\s*\{[\s\S]*\.art-control-playbackRateToggle:hover \.art-selector-list\s*\{[^}]*opacity:\s*0/s
  );
});

test("双击语义由播放器自己实现，关掉 ArtPlayer 内置的两个开关", () => {
  assert.match(playerSource, /Artplayer\.MOBILE_DBCLICK_PLAY = false/);
  assert.match(playerSource, /Artplayer\.DBCLICK_FULLSCREEN = false/);
  // 鼠标设备仍保持双击全屏
  assert.match(
    playerSource,
    /if \(!seekEnabled\) \{[\s\S]*if \(kind === "dblclick"\) art\.fullscreen = !art\.fullscreen;/
  );
});

test("双击侧边快进按连击累计，窗口结束只提交一次 seek", () => {
  assert.match(
    playerSource,
    /const unbindDoubleClickActions = bindPlayerDoubleClickActions\(art\)[\s\S]*unbindDoubleClickActions\(\)/s
  );
  assert.match(
    playerSource,
    /function commitChainSeek\(\)\s*\{[\s\S]*art\.seek = target;/
  );
  assert.match(
    playerSource,
    /function scheduleChainCommit\(\)\s*\{[\s\S]*DOUBLE_TAP_CHAIN_WINDOW_MS\)/
  );
  // 连击未提交时，timeupdate 不能把进度条写回真实播放位置
  assert.match(
    playerSource,
    /function handleTimeUpdate\(\)\s*\{[\s\S]*if \(seekTarget !== null\) renderSeekTarget\(\)/
  );
  assert.match(playerSource, /art\.on\("dblclick", handleDoubleClick\)/);
  assert.match(playerSource, /art\.off\("dblclick", handleDoubleClick\)/);
  assert.match(playerSource, /art\.on\("click", handleClick\)/);
  assert.match(playerSource, /art\.off\("click", handleClick\)/);
});

test("双击快进的反馈是被点一侧的半圆水波纹，随连击刷新、提交时淡出", () => {
  assert.match(
    playerSource,
    /showPlayerSeekRipple\([\s\S]*?action\.side,[\s\S]*?formatDoubleTapSeekLabel\(action\.totalSeconds\),[\s\S]*?event[\s\S]*?\)/
  );
  assert.match(
    playerSource,
    /function commitChainSeek\(\)\s*\{[\s\S]*?hidePlayerSeekRipple\(art\)/
  );
  // 卸载时不放淡出动画，直接清掉
  assert.match(playerSource, /clearPlayerSeekRipple\(art\);\s*\n\s*art\.off\("click"/);
  assert.match(
    detailCss,
    /\.video-player__art-seek-ripple\s*\{[^}]*z-index:\s*40[^}]*width:\s*52%/s
  );
  assert.match(
    detailCss,
    /\.video-player__art-seek-ripple--left\s*\{[^}]*left:\s*0[^}]*border-radius/s
  );
  assert.match(
    detailCss,
    /\.video-player__art-seek-ripple--right\s*\{[^}]*right:\s*0[^}]*border-radius/s
  );
  // 水波纹从指尖扩散，左侧箭头靠水平翻转复用同一段路径
  assert.match(playerSource, /const origin = seekRippleOrigin\(ripple, event\)/);
  assert.match(
    detailCss,
    /\.video-player__art-seek-ripple--left \.video-player__art-seek-ripple-icon\s*\{[^}]*scaleX\(-1\)/s
  );
  assert.match(detailCss, /@keyframes video-player-seek-wave/);
  // 动画敏感用户不播水波纹
  assert.match(
    detailCss,
    /@media \(prefers-reduced-motion: reduce\)[\s\S]*?\.video-player__art-seek-ripple-wave[\s\S]*?animation:\s*none/s
  );
});

test("窄屏上手势浮层文案不再被压成两行", () => {
  // 绝对定位元素按 left 收缩：left: 75% 时可用宽度只剩另一半，中文会被折行
  assert.match(
    detailCss,
    /\.video-player__art-gesture-hud\s*\{[^}]*white-space:\s*nowrap/s
  );
  assert.match(
    detailCss,
    /\.video-player__art-seek-ripple-value\s*\{[^}]*white-space:\s*nowrap/s
  );
});

test("锁屏状态下不响应双击快进", () => {
  assert.match(
    playerSource,
    /function handleTap\(kind: TapKind, event: Event\)\s*\{[\s\S]*if \(art\.isLock\) return;/
  );
});

test("倍速按钮不用 ArtPlayer 的 hint 提示，避免压住展开的最后一档", () => {
  const control = playerSource.slice(
    playerSource.indexOf("function createPlaybackRateControl"),
    playerSource.indexOf("function getPlaybackRateControl")
  );
  assert.doesNotMatch(control, /tooltip:/);
  assert.match(control, /element\.setAttribute\("title", "播放速度"\)/);
});
