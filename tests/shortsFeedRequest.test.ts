import assert from "node:assert/strict";
import test from "node:test";
import {
  fetchShortsNext,
  ShortsFeedExpiredError,
} from "../src/data/videos";

test("shorts feed requests use a body-free token and cursor GET", async (t) => {
  const originalFetch = globalThis.fetch;
  let requestPath = "";
  let requestInit: RequestInit | undefined;
  globalThis.fetch = (async (input, init) => {
    requestPath = String(input);
    requestInit = init;
    return new Response(
      JSON.stringify({
        items: [{ id: "video-1", feedCursor: 43 }],
        total: 100,
        feedToken: "feed-token",
        nextCursor: 43,
        roundComplete: false,
      }),
      { status: 200, headers: { "Content-Type": "application/json" } }
    );
  }) as typeof fetch;
  t.after(() => {
    globalThis.fetch = originalFetch;
  });

  const response = await fetchShortsNext("feed-token", 42, 5);

  assert.equal(
    requestPath,
    "/api/shorts/next?cursor=42&count=5&mode=recommend&feedToken=feed-token"
  );
  assert.equal(requestInit?.method, undefined);
  assert.equal(requestInit?.body, undefined);
  assert.equal(requestInit?.cache, "no-store");
  assert.equal(response.items[0]?.feedCursor, 43);
});

test("shorts feed requests select server ordering for every mode", async (t) => {
  const paths: string[] = [];
  t.mock.method(globalThis, "fetch", async (input) => {
    paths.push(String(input));
    return Response.json({ items: [], total: 0, feedToken: "", nextCursor: 0, roundComplete: true });
  });
  for (const mode of ["latest", "hot", "recommend"] as const) {
    await fetchShortsNext("", 0, 2, mode);
    assert.equal(new URL(paths.at(-1)!, "http://localhost").searchParams.get("mode"), mode);
  }
});

test("shorts feed requests can be cancelled when the selected mode changes", async (t) => {
  let requestSignal: AbortSignal | undefined;
  t.mock.method(globalThis, "fetch", (_input, init) => new Promise<Response>((_resolve, reject) => {
    requestSignal = init?.signal as AbortSignal;
    requestSignal.addEventListener("abort", () => reject(requestSignal!.reason), { once: true });
  }));
  const controller = new AbortController();
  const pending = fetchShortsNext("", 0, 2, "latest", { signal: controller.signal });
  controller.abort();
  await assert.rejects(pending, { name: "AbortError" });
  assert.equal(requestSignal?.aborted, true);
});

test("shorts feed reports an expired server token without turning it into an empty list", async (t) => {
  const originalFetch = globalThis.fetch;
  let calls = 0;
  globalThis.fetch = (async () => {
    calls += 1;
    return new Response("shorts feed expired", { status: 410 });
  }) as typeof fetch;
  t.after(() => {
    globalThis.fetch = originalFetch;
  });

  await assert.rejects(
    () => fetchShortsNext("expired-token", 10, 5),
    (error) => error instanceof ShortsFeedExpiredError
  );
  assert.equal(calls, 1);
});

test("shorts feed rejects malformed success responses", async (t) => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = (async () =>
    new Response(JSON.stringify({ items: [], total: 5 }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    })) as typeof fetch;
  t.after(() => {
    globalThis.fetch = originalFetch;
  });

  await assert.rejects(
    () => fetchShortsNext("feed-token", 0, 5),
    /Invalid \/api\/shorts\/next response/
  );
});

test("shorts feed accepts an explicit empty-library response", async (t) => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = (async () =>
    new Response(
      JSON.stringify({
        items: [],
        total: 0,
        feedToken: "",
        nextCursor: 0,
        roundComplete: true,
      }),
      { status: 200, headers: { "Content-Type": "application/json" } }
    )) as typeof fetch;
  t.after(() => {
    globalThis.fetch = originalFetch;
  });

  const response = await fetchShortsNext("", 0, 5);
  assert.equal(response.total, 0);
  assert.deepEqual(response.items, []);
  assert.equal(response.roundComplete, true);
});
