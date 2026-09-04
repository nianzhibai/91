import assert from "node:assert/strict";
import test from "node:test";
import {
  formatPlaybackRateLabel,
  formatPlaybackRateOptionLabel,
  isNormalPlaybackRate,
  matchPlaybackRateOption,
  normalizePlaybackRate,
  NORMAL_PLAYBACK_RATE,
  PLAYBACK_RATE_OPTIONS,
} from "../src/lib/playbackRate";

test("倍速档位与 ArtPlayer 内置设置面板保持同一份", () => {
  // 控制栏按钮、设置面板“播放速度”、桌面端右键菜单共用这份档位
  assert.deepEqual(PLAYBACK_RATE_OPTIONS, [0.5, 0.75, 1, 1.25, 1.5, 2]);
  assert.ok(PLAYBACK_RATE_OPTIONS.includes(NORMAL_PLAYBACK_RATE));
});

test("非法倍速一律回落到正常倍速", () => {
  assert.equal(normalizePlaybackRate(1.5), 1.5);
  assert.equal(normalizePlaybackRate("1.25"), 1.25);
  assert.equal(normalizePlaybackRate(0), NORMAL_PLAYBACK_RATE);
  assert.equal(normalizePlaybackRate(-2), NORMAL_PLAYBACK_RATE);
  assert.equal(normalizePlaybackRate(Number.NaN), NORMAL_PLAYBACK_RATE);
  assert.equal(normalizePlaybackRate(undefined), NORMAL_PLAYBACK_RATE);
  assert.equal(normalizePlaybackRate("abc"), NORMAL_PLAYBACK_RATE);
});

test("正常倍速判定覆盖非法值", () => {
  assert.equal(isNormalPlaybackRate(1), true);
  assert.equal(isNormalPlaybackRate(0), true);
  assert.equal(isNormalPlaybackRate(2), false);
});

test("控制栏按钮在正常倍速下显示“倍速”，其它显示实际倍数", () => {
  assert.equal(formatPlaybackRateLabel(1), "倍速");
  assert.equal(formatPlaybackRateLabel(Number.NaN), "倍速");
  assert.equal(formatPlaybackRateLabel(1.5), "1.5x");
  assert.equal(formatPlaybackRateLabel(2), "2x");
  // 0.75 不能像 ArtPlayer 右键菜单那样被 toFixed(1) 显示成 0.8
  assert.equal(formatPlaybackRateLabel(0.75), "0.75x");
});

test("下拉列表文案与设置面板一致", () => {
  assert.equal(formatPlaybackRateOptionLabel(1), "正常");
  assert.equal(formatPlaybackRateOptionLabel(0.5), "0.5x");
  assert.equal(formatPlaybackRateOptionLabel(1.25), "1.25x");
});

test("不在档位里的临时倍速不标记选中项", () => {
  assert.equal(matchPlaybackRateOption(1.5), 1.5);
  assert.equal(matchPlaybackRateOption("2"), 2);
  // 长按倍速等临时值若不在档位内，返回 null 让调用方清空选中态
  assert.equal(matchPlaybackRateOption(3), null);
  assert.equal(matchPlaybackRateOption(0), NORMAL_PLAYBACK_RATE);
});
