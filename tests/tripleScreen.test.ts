import "./helpers/dom-environment.ts";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { videoSourceRelayURL } from "../src/lib/videoSource";
import {
  calculateCanvasPixelSize,
  calculateTripleScreenViewport,
  isPortraitVideo,
} from "../src/lib/tripleScreen";

const playerSource = readFileSync(
  new URL("../src/components/VideoPlayer.tsx", import.meta.url),
  "utf8"
);
const rendererSource = readFileSync(
  new URL("../src/lib/tripleScreen.ts", import.meta.url),
  "utf8"
);
const detailCss = readFileSync(
  new URL("../src/styles/video-detail.css", import.meta.url),
  "utf8"
);

function assertClose(actual: number, expected: number) {
  assert.ok(Math.abs(actual - expected) < 0.001, `${actual} != ${expected}`);
}

test("triple screen eligibility only accepts portrait video dimensions", () => {
  assert.equal(isPortraitVideo(1088, 1920), true);
  assert.equal(isPortraitVideo(1920, 1080), false);
  assert.equal(isPortraitVideo(1080, 1080), false);
  assert.equal(isPortraitVideo(0, 1920), false);
  assert.equal(isPortraitVideo(Number.NaN, 1920), false);
});

test("triple screen viewport preserves the three-frame composite ratio", () => {
  const viewport = calculateTripleScreenViewport(1920, 1080, 1088, 1920);
  assert.notEqual(viewport, null);
  assertClose(viewport!.x, 42);
  assertClose(viewport!.y, 0);
  assertClose(viewport!.width, 1836);
  assertClose(viewport!.height, 1080);
  assertClose(viewport!.width / viewport!.height, (1088 * 3) / 1920);
});

test("triple screen viewport letterboxes vertically inside a square player", () => {
  const viewport = calculateTripleScreenViewport(1000, 1000, 1080, 1920);
  assert.notEqual(viewport, null);
  assertClose(viewport!.x, 0);
  assertClose(viewport!.y, 203.7037037037037);
  assertClose(viewport!.width, 1000);
  assertClose(viewport!.height, 592.5925925925926);
  assert.equal(
    calculateTripleScreenViewport(1920, 1080, 1920, 1080),
    null
  );
});

test("triple screen canvas caps device pixel ratio at two", () => {
  assert.deepEqual(calculateCanvasPixelSize(800, 450, 1), {
    width: 800,
    height: 450,
  });
  assert.deepEqual(calculateCanvasPixelSize(800, 450, 3), {
    width: 1600,
    height: 900,
  });
  assert.deepEqual(calculateCanvasPixelSize(800, 450, Number.NaN), {
    width: 800,
    height: 450,
  });
  assert.equal(calculateCanvasPixelSize(0, 450, 2), null);
});

test("detail player exposes triple screen only for desktop portrait videos", () => {
  assert.match(
    playerSource,
    /function shouldEnableTripleScreenControl\(\)\s*\{\s*return !isMobilePlaybackDevice\(\);/
  );
  assert.match(
    playerSource,
    /art\.on\("video:loadedmetadata", updateEligibility\)/
  );
  assert.match(
    playerSource,
    /eligible = isPortraitVideo\(video\.videoWidth, video\.videoHeight\)/
  );
  assert.match(
    playerSource,
    /controls:\s*createPlayerControls\(\s*enableOrientationControl,\s*enableTripleScreenControl\s*\)/
  );
  assert.doesNotMatch(playerSource, /KeyS/);
  assert.doesNotMatch(playerSource, /tripleScreen[\s\S]{0,100}localStorage/);
});

test("triple screen switches backend stream routes to same-origin relay", () => {
  for (const path of ["/p/stream/google/file", "/p/playback/video/cloud-18", "/p/share/share-id/stream", "/p/share/share-id/playback/cloud-18"]) {
    const relay = videoSourceRelayURL(`${path}?playbackId=current#frame`);
    assert.ok(relay);
    const url = new URL(relay, window.location.href);
    assert.equal(url.pathname, path);
    assert.equal(url.searchParams.get("playbackId"), "current");
    assert.equal(url.searchParams.get("tripleScreenRelay"), "1");
    assert.equal(url.hash, "#frame");
  }
  for (const path of ["https://cdn.example/p/playback/video/cloud-18", "/api/video/video", "/p/share/share-id/hls/session/index.m3u8", "/assets/movie.mp4"]) {
    assert.equal(videoSourceRelayURL(path), null);
  }
});

test("triple screen renderer reuses one video texture and follows presented frames", () => {
  assert.match(
    rendererSource,
    /gl\.texImage2D\([\s\S]*?gl\.UNSIGNED_BYTE,\s*this\.video\s*\)/
  );
  assert.match(
    rendererSource,
    /gl\.drawElements\(gl\.TRIANGLES,\s*18,\s*gl\.UNSIGNED_SHORT,\s*0\)/
  );
  assert.match(rendererSource, /requestVideoFrameCallback/);
  assert.match(rendererSource, /preserveDrawingBuffer:\s*true/);
  assert.match(rendererSource, /this\.video\.paused/);
  assert.match(rendererSource, /this\.video\.addEventListener\("seeked"/);
  assert.match(rendererSource, /this\.canvas\.remove\(\)/);
  const drawFrameSource = rendererSource.slice(
    rendererSource.indexOf("private drawFrame()"),
    rendererSource.indexOf("private scheduleFrame()")
  );
  assert.doesNotMatch(drawFrameSource, /gl\.getError\(\)/);
  assert.match(
    playerSource,
    /if \(visible && pendingEnableSuccessNotice\)[\s\S]*?已开启三屏画面/
  );
});

test("triple screen canvas stays between the media and player overlays", () => {
  assert.match(
    detailCss,
    /\.video-player__triple-screen-canvas\s*\{[^}]*position:\s*absolute[^}]*z-index:\s*12[^}]*pointer-events:\s*none/s
  );
  assert.match(
    detailCss,
    /\.art-video-player\.art-triple-screen-active[\s\S]*?\.art-subtitle\s*\{[^}]*display:\s*none/s
  );
  assert.match(
    detailCss,
    /data-triple-screen-eligible="false"\]\s*\{[^}]*display:\s*none !important/s
  );
  assert.match(
    playerSource,
    /当前浏览器或视频源不支持三屏画面/
  );
});
