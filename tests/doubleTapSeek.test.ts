import assert from "node:assert/strict";
import test from "node:test";
import {
  classifyTapZone,
  computeDoubleTapSeekTime,
  DOUBLE_TAP_CHAIN_WINDOW_MS,
  DOUBLE_TAP_SEEK_SECONDS,
  formatDoubleTapSeekLabel,
  isSeekChainActive,
  reduceDoubleTap,
  type SeekChain,
} from "../src/lib/doubleTapSeek";

const WIDTH = 1000;

test("左右热区各占三分之一强，中间留给双击播放/暂停", () => {
  assert.equal(classifyTapZone(0, WIDTH), "left");
  assert.equal(classifyTapZone(349, WIDTH), "left");
  // 边界值落在中间，不会误触发快进
  assert.equal(classifyTapZone(350, WIDTH), "center");
  assert.equal(classifyTapZone(500, WIDTH), "center");
  assert.equal(classifyTapZone(650, WIDTH), "center");
  assert.equal(classifyTapZone(651, WIDTH), "right");
  assert.equal(classifyTapZone(1000, WIDTH), "right");
});

test("拿不到宽度或坐标时当作中间，不做快进", () => {
  assert.equal(classifyTapZone(10, 0), "center");
  assert.equal(classifyTapZone(10, Number.NaN), "center");
  assert.equal(classifyTapZone(Number.NaN, WIDTH), "center");
});

test("连击窗口按最后一次点击时间计算", () => {
  const chain: SeekChain = { steps: 1, side: "right", lastTapAt: 1_000 };
  assert.equal(isSeekChainActive(null, 1_000), false);
  assert.equal(isSeekChainActive(chain, 1_000 + DOUBLE_TAP_CHAIN_WINDOW_MS), true);
  assert.equal(
    isSeekChainActive(chain, 1_001 + DOUBLE_TAP_CHAIN_WINDOW_MS),
    false
  );
});

test("单击不启动连击，双击侧边才开始快进/快退", () => {
  const ignored = reduceDoubleTap(null, { kind: "click", zone: "right", at: 0 });
  assert.equal(ignored.action.type, "ignore");
  assert.equal(ignored.chain, null);

  const forward = reduceDoubleTap(null, { kind: "dblclick", zone: "right", at: 0 });
  assert.deepEqual(forward.action, {
    type: "seek",
    side: "right",
    stepSeconds: DOUBLE_TAP_SEEK_SECONDS,
    totalSeconds: DOUBLE_TAP_SEEK_SECONDS,
  });
  assert.deepEqual(forward.chain, { steps: 1, side: "right", lastTapAt: 0 });

  const backward = reduceDoubleTap(null, { kind: "dblclick", zone: "left", at: 0 });
  assert.deepEqual(backward.action, {
    type: "seek",
    side: "left",
    stepSeconds: -DOUBLE_TAP_SEEK_SECONDS,
    totalSeconds: -DOUBLE_TAP_SEEK_SECONDS,
  });
});

test("窗口内的单击和双击都继续累计，不必每次重新双击", () => {
  // ArtPlayer 的点击派发让一串连点交替派发 click / dblclick
  let state = reduceDoubleTap(null, { kind: "dblclick", zone: "right", at: 0 });
  state = reduceDoubleTap(state.chain, { kind: "click", zone: "right", at: 200 });
  assert.equal(
    (state.action as { totalSeconds: number }).totalSeconds,
    DOUBLE_TAP_SEEK_SECONDS * 2
  );

  state = reduceDoubleTap(state.chain, { kind: "dblclick", zone: "right", at: 400 });
  assert.equal(
    (state.action as { totalSeconds: number }).totalSeconds,
    DOUBLE_TAP_SEEK_SECONDS * 3
  );
  assert.deepEqual(state.chain, { steps: 3, side: "right", lastTapAt: 400 });
});

test("窗口过期后单击不再快进，双击重新从一档开始", () => {
  const chain: SeekChain = { steps: 3, side: "right", lastTapAt: 0 };
  const late = DOUBLE_TAP_CHAIN_WINDOW_MS + 1;

  const ignored = reduceDoubleTap(chain, { kind: "click", zone: "right", at: late });
  assert.equal(ignored.action.type, "ignore");
  assert.equal(ignored.chain, null);

  const restarted = reduceDoubleTap(chain, {
    kind: "dblclick",
    zone: "right",
    at: late,
  });
  assert.deepEqual(restarted.chain, { steps: 1, side: "right", lastTapAt: late });
});

test("连击中换边把累计值往回减，提示也跟到另一侧", () => {
  let state = reduceDoubleTap(null, { kind: "dblclick", zone: "right", at: 0 });
  state = reduceDoubleTap(state.chain, { kind: "click", zone: "right", at: 100 });
  state = reduceDoubleTap(state.chain, { kind: "click", zone: "left", at: 200 });

  assert.deepEqual(state.action, {
    type: "seek",
    side: "left",
    stepSeconds: -DOUBLE_TAP_SEEK_SECONDS,
    totalSeconds: DOUBLE_TAP_SEEK_SECONDS,
  });
  assert.deepEqual(state.chain, { steps: 1, side: "left", lastTapAt: 200 });
});

test("点中间结束连击：单击只提交，双击还要切换播放/暂停", () => {
  const chain: SeekChain = { steps: 2, side: "right", lastTapAt: 0 };

  const committed = reduceDoubleTap(chain, { kind: "click", zone: "center", at: 100 });
  assert.deepEqual(committed.action, { type: "end", toggle: false });
  assert.equal(committed.chain, null);

  const toggled = reduceDoubleTap(chain, { kind: "dblclick", zone: "center", at: 100 });
  assert.deepEqual(toggled.action, { type: "end", toggle: true });

  // 没有连击时，中间单击交回 ArtPlayer 默认行为（显示/隐藏控制栏）
  const idle = reduceDoubleTap(null, { kind: "click", zone: "center", at: 0 });
  assert.deepEqual(idle.action, { type: "ignore" });

  // 没有连击时，中间双击仍是播放/暂停
  const idleToggle = reduceDoubleTap(null, { kind: "dblclick", zone: "center", at: 0 });
  assert.deepEqual(idleToggle.action, { type: "end", toggle: true });
});

test("目标时间在片头片尾夹住边界", () => {
  assert.equal(
    computeDoubleTapSeekTime({ baseTime: 30, stepSeconds: 10, duration: 100 }),
    40
  );
  assert.equal(
    computeDoubleTapSeekTime({ baseTime: 5, stepSeconds: -10, duration: 100 }),
    0
  );
  assert.equal(
    computeDoubleTapSeekTime({ baseTime: 95, stepSeconds: 10, duration: 100 }),
    100
  );
});

test("提示文案区分快进、快退和抵消回原位", () => {
  assert.equal(formatDoubleTapSeekLabel(30), "快进 30 秒");
  assert.equal(formatDoubleTapSeekLabel(-10), "快退 10 秒");
  assert.equal(formatDoubleTapSeekLabel(0), "回到原位");
});
