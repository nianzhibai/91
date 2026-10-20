import assert from "node:assert/strict";
import test from "node:test";
import {
  SHORTS_COMPLETE_RATIO,
  SHORTS_EVENT_BATCH_SIZE,
  SHORTS_SKIP_FALLBACK_MS,
  SHORTS_SKIP_RATIO,
  classifyShortsWatch,
  createShortsWatchTracker,
  enqueueShortsWatchEvent,
  flushShortsWatchEvents,
  resetShortsWatchEventsForTests,
  shortsWatchEventBacklog,
  type ShortsWatchEvent,
} from "../src/shorts/shortsTelemetry.ts";

function sampleEvent(index: number): ShortsWatchEvent {
  return {
    videoId: `video-${index}`,
    kind: "partial",
    watchMs: 1000,
    loopCount: 0,
    position: index,
  };
}

test("a looped video counts as finished even when the ratio is low", () => {
  assert.equal(
    classifyShortsWatch({ watchMs: 2000, durationMs: 20000, loopCount: 1 }),
    "complete"
  );
});

test("watch ratio decides between finished, partial and swiped past", () => {
  assert.equal(
    classifyShortsWatch({
      watchMs: 20000 * SHORTS_COMPLETE_RATIO,
      durationMs: 20000,
      loopCount: 0,
    }),
    "complete"
  );
  assert.equal(
    classifyShortsWatch({
      watchMs: 20000 * SHORTS_SKIP_RATIO,
      durationMs: 20000,
      loopCount: 0,
    }),
    "skip"
  );
  assert.equal(
    classifyShortsWatch({ watchMs: 10000, durationMs: 20000, loopCount: 0 }),
    "partial"
  );
});

test("an unknown duration falls back to absolute watch time", () => {
  assert.equal(
    classifyShortsWatch({ watchMs: SHORTS_SKIP_FALLBACK_MS, durationMs: 0, loopCount: 0 }),
    "partial"
  );
  assert.equal(
    classifyShortsWatch({ watchMs: 500, durationMs: 0, loopCount: 0 }),
    "skip"
  );
});

test("the tracker only accumulates media time while playing", () => {
  const tracker = createShortsWatchTracker();
  tracker.observe(0, true);
  tracker.observe(1000, true);
  assert.deepEqual(tracker.snapshot(), { watchMs: 1000, loopCount: 0 });

  // 暂停期间 timeupdate 不再推进，恢复时也不该把这段时间算成观看。
  tracker.observe(1000, false);
  tracker.observe(1000, true);
  assert.deepEqual(tracker.snapshot(), { watchMs: 1000, loopCount: 0 });

  tracker.observe(2000, true);
  assert.equal(tracker.snapshot().watchMs, 2000);
});

test("a rewind to the start counts as one loop", () => {
  const tracker = createShortsWatchTracker();
  tracker.observe(0, true);
  tracker.observe(3000, true);
  tracker.observe(0, true);
  assert.deepEqual(tracker.snapshot(), { watchMs: 3000, loopCount: 1 });
});

test("a seek backwards inside the video is not a loop", () => {
  const tracker = createShortsWatchTracker();
  tracker.observe(0, true);
  tracker.observe(30000, true);
  tracker.observe(20000, true);
  assert.equal(tracker.snapshot().loopCount, 0);
});

test("reset clears accumulated watch time", () => {
  const tracker = createShortsWatchTracker();
  tracker.observe(0, true);
  tracker.observe(5000, true);
  tracker.reset();
  assert.deepEqual(tracker.snapshot(), { watchMs: 0, loopCount: 0 });
});

test("events below the batch size stay queued until an explicit flush", (t) => {
  t.after(() => resetShortsWatchEventsForTests());
  resetShortsWatchEventsForTests();

  enqueueShortsWatchEvent(sampleEvent(0));
  enqueueShortsWatchEvent(sampleEvent(1));
  assert.equal(shortsWatchEventBacklog(), 2);

  const sent: ShortsWatchEvent[][] = [];
  flushShortsWatchEvents((events) => sent.push(events));

  assert.equal(sent.length, 1);
  assert.equal(sent[0].length, 2);
  assert.equal(sent[0][1].videoId, "video-1");
  assert.equal(shortsWatchEventBacklog(), 0);
});

test("a full batch is sent immediately", (t) => {
  t.after(() => resetShortsWatchEventsForTests());
  resetShortsWatchEventsForTests();

  const originalFetch = globalThis.fetch;
  let requests = 0;
  globalThis.fetch = (async () => {
    requests += 1;
    return new Response("{}", { status: 200 });
  }) as typeof fetch;
  t.after(() => {
    globalThis.fetch = originalFetch;
  });

  for (let index = 0; index < SHORTS_EVENT_BATCH_SIZE; index += 1) {
    enqueueShortsWatchEvent(sampleEvent(index));
  }

  assert.equal(shortsWatchEventBacklog(), 0);
  assert.equal(requests, 1);
});

test("flushing an empty queue does not call the reporter", (t) => {
  t.after(() => resetShortsWatchEventsForTests());
  resetShortsWatchEventsForTests();

  let calls = 0;
  flushShortsWatchEvents(() => {
    calls += 1;
  });
  assert.equal(calls, 0);
});
