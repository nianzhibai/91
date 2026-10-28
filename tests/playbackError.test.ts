import assert from "node:assert/strict";
import test from "node:test";
import {
  diagnosePlaybackFailure,
  diagnosePlaybackSource,
  isSameOriginPlaybackURL,
} from "../src/lib/playbackError.ts";

test("playback diagnostics only inspect same-origin backend media routes", () => {
  const page = new URL("https://video.example/videos/1");
  assert.equal(
    isSameOriginPlaybackURL(
      new URL("https://video.example/p/stream/115/file-1"),
      page
    ),
    true
  );
  assert.equal(
    isSameOriginPlaybackURL(
      new URL("https://cdn.example/p/stream/115/file-1"),
      page
    ),
    false
  );
  assert.equal(
    isSameOriginPlaybackURL(
      new URL("https://video.example/p/share/share-1/stream"),
      page
    ),
    true
  );
  assert.equal(
    isSameOriginPlaybackURL(new URL("https://video.example/api/videos/1"), page),
    false
  );
});

test("playback diagnostics identify an expired share session", async () => {
  const message = await diagnosePlaybackSource("/p/share/share-1/stream", {
    baseHref: "https://video.example/share/token",
    fetch: async () => new Response("not found", { status: 404 }),
  });

  assert.equal(
    message,
    "分享播放凭证已失效，请重新打开有效的一次性分享链接"
  );
});

test("HLS relay diagnostics detect an expired login", async () => {
  const src = "/p/hls/video/session/index.m3u8";
  const login = await diagnosePlaybackSource(src, {
    baseHref: "https://video.example/videos/1",
    fetch: async () => new Response("unauthorized", { status: 401 }),
  });
  assert.equal(login, "当前登录状态已失效，请重新登录后播放");
});

test("expired HLS relay sources permit refresh instead of becoming access failures", async () => {
  const failure = await diagnosePlaybackFailure("/p/hls/video/session/index.m3u8", {
    baseHref: "https://video.example/videos/1",
    fetch: async () => new Response(JSON.stringify({
      code: "playback_source_expired", message: "兼容播放地址已过期，请重新获取",
    }), { status: 410, headers: { "Content-Type": "application/json" } }),
  });
  assert.equal(failure?.kind, "network");
});

test("playback diagnostics surface the backend drive error message", async () => {
  let requestURL = "";
  let requestInit: RequestInit | undefined;
  const fetcher: typeof fetch = async (input, init) => {
    requestURL = String(input);
    requestInit = init;
    return new Response(
      JSON.stringify({
        code: "drive_auth_failed",
        message: "115 网盘登录或授权已失效，请联系管理员重新登录",
      }),
      {
        status: 424,
        headers: { "Content-Type": "application/json; charset=utf-8" },
      }
    );
  };

  const failure = await diagnosePlaybackFailure("/p/stream/115/file-1", {
    baseHref: "https://video.example/videos/1",
    fetch: fetcher,
  });

  assert.equal(failure?.kind, "access");
  assert.equal(
    failure?.message,
    "115 网盘登录或授权已失效，请联系管理员重新登录"
  );
  assert.equal(requestURL, "https://video.example/p/stream/115/file-1");
  assert.equal(requestInit?.redirect, "manual");
  assert.equal(requestInit?.credentials, "same-origin");
  assert.equal(requestInit?.cache, "no-store");
  assert.deepEqual(requestInit?.headers, {
    Accept: "application/json",
    Range: "bytes=0-0",
  });
});

test("playback diagnostics do not mislabel an unstructured 502 as unsupported format", async () => {
  const message = await diagnosePlaybackSource("/p/stream/pikpak/file-1", {
    baseHref: "https://video.example/videos/1",
    fetch: async () =>
      new Response("bad gateway", {
        status: 502,
        headers: { "Content-Type": "text/plain" },
      }),
  });

  assert.equal(
    message,
    "视频源服务暂时不可用（HTTP 502），请稍后重试或联系管理员"
  );
  assert.doesNotMatch(message || "", /格式不受支持/);
});

test("playback diagnostics skip external sources without fetching", async () => {
  let called = false;
  const message = await diagnosePlaybackSource("https://cdn.example/video.mp4", {
    baseHref: "https://video.example/videos/1",
    fetch: async () => {
      called = true;
      return new Response(null, { status: 200 });
    },
  });
  assert.equal(message, null);
  assert.equal(called, false);
});
