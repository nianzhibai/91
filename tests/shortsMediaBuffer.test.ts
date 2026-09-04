import assert from "node:assert/strict";
import test from "node:test";
import {
  ACTIVE_PRELOAD_BUFFER_BYTES,
  ACTIVE_PRELOAD_BUFFER_SECONDS,
  ACTIVE_PRELOAD_KEEP_SECONDS,
  ACTIVE_PRELOAD_MIN_BUFFER_SECONDS,
  ACTIVE_PRELOAD_MIN_KEEP_SECONDS,
  VIDEO_WINDOW_SIZE,
  averageBytesPerSecond,
  bufferedAheadSeconds,
  FIRST_FRAME_WARM_TIME,
  getPreloadAheadCount,
  getVideoWindowBounds,
  PRELOAD_AHEAD_COUNT,
  shouldWarmFirstFrame,
  preloadBufferSecondsFor,
  preloadKeepSecondsFor,
  videoBufferIsCritical,
  videoBufferedToEnd,
  videoHasBufferedData,
  videoHasComfortableBuffer,
  type BufferedMediaProbe,
} from "../src/shorts/mediaBuffer";

function probe(input: {
  currentTime?: number;
  duration?: number;
  readyState?: number;
  ranges?: Array<[number, number]>;
}): BufferedMediaProbe {
  const ranges = input.ranges ?? [];
  return {
    currentTime: input.currentTime ?? 0,
    duration: input.duration ?? 600,
    readyState: input.readyState ?? 4,
    buffered: {
      length: ranges.length,
      start: (index) => ranges[index][0],
      end: (index) => ranges[index][1],
    },
  };
}

test("bufferedAheadSeconds measures the range containing the playhead", () => {
  assert.equal(
    bufferedAheadSeconds(probe({ currentTime: 10, ranges: [[0, 25]] })),
    15
  );
  // 播放点落在缓冲空洞里时没有可用前向缓冲
  assert.equal(
    bufferedAheadSeconds(
      probe({ currentTime: 10, ranges: [[0, 5], [20, 30]] })
    ),
    0
  );
  // 起点在播放点 0.25s 容差内的区间仍然算数
  assert.equal(
    bufferedAheadSeconds(probe({ currentTime: 10, ranges: [[10.2, 22]] })),
    12
  );
  assert.equal(bufferedAheadSeconds(probe({ ranges: [] })), 0);
});

test("comfortable buffer needs the high watermark or buffered-to-end", () => {
  // readyState 不足时无论缓冲多少都不健康
  assert.equal(
    videoHasComfortableBuffer(
      probe({ currentTime: 0, readyState: 2, ranges: [[0, 60]] })
    ),
    false
  );
  assert.equal(
    videoHasComfortableBuffer(
      probe({ currentTime: 10, ranges: [[0, 10 + ACTIVE_PRELOAD_BUFFER_SECONDS]] })
    ),
    true
  );
  assert.equal(
    videoHasComfortableBuffer(probe({ currentTime: 10, ranges: [[0, 21]] })),
    false
  );
  // 临近片尾：不足高水位但已缓冲到结尾，同样视为健康
  const nearEnd = probe({ currentTime: 25, duration: 30, ranges: [[0, 30]] });
  assert.equal(videoBufferedToEnd(nearEnd), true);
  assert.equal(videoHasComfortableBuffer(nearEnd), true);
});

test("critical buffer needs dropping below the low watermark", () => {
  assert.equal(
    videoBufferIsCritical(
      probe({ currentTime: 0, readyState: 2, ranges: [[0, 60]] })
    ),
    true
  );
  assert.equal(
    videoBufferIsCritical(probe({ currentTime: 10, ranges: [[0, 13]] })),
    true
  );
  assert.equal(
    videoBufferIsCritical(
      probe({ currentTime: 10, ranges: [[0, 10 + ACTIVE_PRELOAD_KEEP_SECONDS]] })
    ),
    false
  );
  // 片尾只剩 3s 可播时不算告急：不会再因网络卡顿
  assert.equal(
    videoBufferIsCritical(
      probe({ currentTime: 17, duration: 20, ranges: [[0, 20]] })
    ),
    false
  );
});

test("watermarks form a hysteresis band that holds the current grant", () => {
  // 高低水位之间（例如 8s）既不授权也不收回，避免阈值附近抖动
  const between = probe({ currentTime: 10, ranges: [[0, 18]] });
  assert.equal(videoHasComfortableBuffer(between), false);
  assert.equal(videoBufferIsCritical(between), false);
});

test("averageBytesPerSecond needs both size and duration", () => {
  assert.equal(averageBytesPerSecond({ sizeBytes: 1000, durationSeconds: 10 }), 100);
  // 任一缺失 / 为 0 / 为负都视为码率未知
  assert.equal(averageBytesPerSecond({ sizeBytes: 1000 }), 0);
  assert.equal(averageBytesPerSecond({ durationSeconds: 10 }), 0);
  assert.equal(averageBytesPerSecond({}), 0);
  assert.equal(averageBytesPerSecond({ sizeBytes: 1000, durationSeconds: 0 }), 0);
  assert.equal(averageBytesPerSecond({ sizeBytes: -5, durationSeconds: 10 }), 0);
});

test("preload gate converts a byte budget into per-video seconds", () => {
  // 码率未知 / 非法时退回改动前的固定门槛
  assert.equal(preloadBufferSecondsFor(0), ACTIVE_PRELOAD_BUFFER_SECONDS);
  assert.equal(preloadBufferSecondsFor(-1), ACTIVE_PRELOAD_BUFFER_SECONDS);
  assert.equal(preloadBufferSecondsFor(NaN), ACTIVE_PRELOAD_BUFFER_SECONDS);
  assert.equal(preloadBufferSecondsFor(Infinity), ACTIVE_PRELOAD_BUFFER_SECONDS);

  // 低码率：预算换算出的秒数超过上限，被压回原值 —— 普通网络视频行为不变
  const lowRate = ACTIVE_PRELOAD_BUFFER_BYTES / 100; // 换算 = 100s
  assert.equal(preloadBufferSecondsFor(lowRate), ACTIVE_PRELOAD_BUFFER_SECONDS);

  // 分界点恰好落在上限上
  const boundary = ACTIVE_PRELOAD_BUFFER_BYTES / ACTIVE_PRELOAD_BUFFER_SECONDS;
  assert.equal(preloadBufferSecondsFor(boundary), ACTIVE_PRELOAD_BUFFER_SECONDS);

  // 高码率：门槛真正放松，且不会低于下限
  const highRate = boundary * 2;
  assert.equal(preloadBufferSecondsFor(highRate), ACTIVE_PRELOAD_BUFFER_SECONDS / 2);
  assert.equal(
    preloadBufferSecondsFor(boundary * 1000),
    ACTIVE_PRELOAD_MIN_BUFFER_SECONDS
  );

  // 本库实测平均 ~9.81 Mbps 时，门槛应显著低于 12s 但守住下限
  const libraryRate = (9.81 * 1e6) / 8;
  const libraryGate = preloadBufferSecondsFor(libraryRate);
  assert.ok(
    libraryGate < ACTIVE_PRELOAD_BUFFER_SECONDS &&
      libraryGate >= ACTIVE_PRELOAD_MIN_BUFFER_SECONDS,
    `library gate ${libraryGate}s should relax but stay above the floor`
  );
});

test("keep watermark scales with the gate and never closes the band", () => {
  // 原有的 12s / 4s 组合保持不变
  assert.equal(
    preloadKeepSecondsFor(ACTIVE_PRELOAD_BUFFER_SECONDS),
    ACTIVE_PRELOAD_KEEP_SECONDS
  );
  // 门槛收紧时低水位跟着降，但有地板；两者必须始终拉开距离，
  // 否则滞回退化成单一阈值，会在阈值附近反复授权/收回。
  for (const rate of [0, 1e5, 5e5, 1e6, 5e6, 1e9]) {
    const gate = preloadBufferSecondsFor(rate);
    const keep = preloadKeepSecondsFor(gate);
    assert.ok(keep >= ACTIVE_PRELOAD_MIN_KEEP_SECONDS, `keep floor at rate=${rate}`);
    assert.ok(keep < gate, `keep ${keep} must stay below gate ${gate} at rate=${rate}`);
  }
});

test("buffer predicates honour the per-video watermarks", () => {
  // 10s 前向缓冲：按固定 12s 门槛不够，按放松后的 4s 门槛够了
  const tenAhead = probe({ currentTime: 10, ranges: [[0, 20]] });
  assert.equal(videoHasComfortableBuffer(tenAhead), false);
  assert.equal(videoHasComfortableBuffer(tenAhead, 4), true);
  // 缺省参数必须等价于改动前的常量，老调用点行为不变
  assert.equal(
    videoHasComfortableBuffer(tenAhead),
    videoHasComfortableBuffer(tenAhead, ACTIVE_PRELOAD_BUFFER_SECONDS)
  );

  const twoAhead = probe({ currentTime: 10, ranges: [[0, 12]] });
  assert.equal(videoBufferIsCritical(twoAhead), true);
  assert.equal(videoBufferIsCritical(twoAhead, 1.5), false);
  assert.equal(
    videoBufferIsCritical(twoAhead),
    videoBufferIsCritical(twoAhead, ACTIVE_PRELOAD_KEEP_SECONDS)
  );
});

test("videoHasBufferedData requires a non-empty range", () => {
  assert.equal(videoHasBufferedData(probe({ ranges: [] })), false);
  assert.equal(videoHasBufferedData(probe({ ranges: [[5, 5]] })), false);
  assert.equal(videoHasBufferedData(probe({ ranges: [[0, 0.5]] })), true);
});

test("video window slides forward with the highest viewed index", () => {
  // 尚未看过任何视频时窗口为空
  assert.deepEqual(getVideoWindowBounds(-1, 10), { start: 0, end: -1 });
  assert.deepEqual(getVideoWindowBounds(0, 10), { start: 0, end: 0 });
  // 窗口固定为 VIDEO_WINDOW_SIZE 条，尾部对齐最远到达的索引
  assert.deepEqual(getVideoWindowBounds(7, 10), {
    start: 7 - VIDEO_WINDOW_SIZE + 1,
    end: 7,
  });
  // 队列比窗口小则覆盖整个队列
  assert.deepEqual(getVideoWindowBounds(2, 3), { start: 0, end: 2 });
  // 索引越界时收敛到队列末尾（空库后残留的高位索引）
  assert.deepEqual(getVideoWindowBounds(9, 4), { start: 0, end: 3 });
  assert.deepEqual(getVideoWindowBounds(3, 0), { start: 0, end: -1 });
});

// ---------------------------------------------------------------------------
// 预载深度：当前屏优先，健康后才开放后台带宽
// ---------------------------------------------------------------------------

test("aggressive preloading waits for the active video buffer", () => {
  // 授权拿到手：向后囤两条
  assert.equal(getPreloadAheadCount(true), PRELOAD_AHEAD_COUNT);
  // 授权没拿到：不允许任何后续视频用 auto 抢当前视频的带宽。
  // 下一条仍由页面挂源并用 metadata 轻量准备，不属于这个深度计算。
  assert.equal(getPreloadAheadCount(false), 0);
  assert.ok(getPreloadAheadCount(true) > getPreloadAheadCount(false));
});

// ---------------------------------------------------------------------------
// 首帧预热：把字节下下来不等于有画面
// ---------------------------------------------------------------------------

function warmProbe(overrides: Partial<Parameters<typeof shouldWarmFirstFrame>[0]> = {}) {
  return shouldWarmFirstFrame({
    isActive: false,
    shouldLoad: true,
    isPlaybackElement: false,
    readyState: 1,
    currentTime: 0,
    ...overrides,
  });
}

test("preloaded videos are nudged into decoding their first frame", () => {
  // 已绑源、已拿到元数据、还停在起点的非活跃条：正是要预热的对象
  assert.equal(warmProbe(), true);
  assert.equal(warmProbe({ readyState: 4 }), true);
});

test("first-frame warming leaves every other video alone", () => {
  // 当前屏由 play() 负责，不需要也不该被 seek 打断
  assert.equal(warmProbe({ isActive: true }), false);
  // 没绑 src 的空壳，seek 无处可去
  assert.equal(warmProbe({ shouldLoad: false }), false);
  // 播放位由 play() 负责清掉 poster 标志，seek 它只会打断起播
  assert.equal(warmProbe({ isPlaybackElement: true }), false);
  // 元数据都还没到，seek 会被丢弃；等 loadedmetadata 再试
  assert.equal(warmProbe({ readyState: 0 }), false);
  // 已经被推进过（预热过、或用户拖过进度）：不要把位置冲掉
  assert.equal(warmProbe({ currentTime: 0.001 }), false);
  assert.equal(warmProbe({ currentTime: 12.5 }), false);
});

test("the warm target is small enough to still read as the first frame", () => {
  assert.ok(FIRST_FRAME_WARM_TIME > 0, "必须真的产生一次 seek");
  assert.ok(FIRST_FRAME_WARM_TIME < 0.04, "小于一帧的时长，视觉上仍是首帧");
});
