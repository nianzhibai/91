import assert from "node:assert/strict";
import test from "node:test";
import { createClient, APIResponseError, UnauthorizedError, ConfigConflictError } from "../sdk/src/index";

const original = { id: "original", label: "原画", kind: "original", protocol: "file", status: "ready", url: "/p/upload/demo" };

test("independent clients keep their backend origins and credentials separate", async () => {
  const calls: { url: string; init?: RequestInit }[] = [];
  const fetch: typeof globalThis.fetch = async (url, init) => { calls.push({ url: String(url), init }); return Response.json({ authenticated: true }); };
  const first = createClient({ baseURL: "https://one.example/", fetch });
  const second = createClient({ baseURL: "https://two.example", fetch });
  await first.auth.login("owner", "secret");
  await second.auth.me();
  await first.admin.getSettings();
  assert.deepEqual(calls.map(call => call.url), ["https://one.example/api/auth/login", "https://two.example/api/auth/me", "https://one.example/admin/api/settings"]);
  assert.ok(calls.every(call => call.init?.credentials === "include"));
  assert.deepEqual(JSON.parse(String(calls[0].init?.body)), { username: "owner", password: "secret" });
  assert.equal(first.resourceURL("/p/thumb/a?v=1"), "https://one.example/p/thumb/a?v=1");
  assert.equal(first.resourceURL("https://cdn.example/video.mp4"), "https://cdn.example/video.mp4");
});

test("client URL helpers reject credentials, unsupported base paths and foreign API targets", async () => {
  for (const baseURL of ["file:///tmp", "https://owner:secret@example.com", "https://example.com/prefix", "https://example.com/?token=x"]) {
    assert.throws(() => createClient({ baseURL }));
  }
  let called = false;
  const client = createClient({ baseURL: "https://api.example", fetch: async () => { called = true; return Response.json({}); } });
  for (const path of ["//other.example/api", "/\\other.example/api", "/\n/other.example/api", "https://other.example/api"]) {
    await assert.rejects(client.http.request(path));
  }
  await assert.rejects(client.playback.resolve("https://other.example/api/video/a/playback"));
  await assert.rejects(client.playback.prepare("https://other.example/api/video/a/playback/prepare"));
  assert.throws(() => client.resourceURL("javascript:alert(1)"));
  assert.equal(called, false);
});

test("the shared error type preserves HTTP status and structured server details", async () => {
  const client = createClient({ fetch: async () => Response.json({ code: "forbidden", error: "Administrator required" }, { status: 403 }) });
  await assert.rejects(client.admin.listUsers(), error => {
    assert.ok(error instanceof APIResponseError);
    assert.equal(error.status, 403);
    assert.equal(error.code, "forbidden");
    assert.equal(error.message, "Administrator required");
    return true;
  });
  const guest = createClient({ fetch: async () => new Response("unauthorized", { status: 401 }) });
  await assert.rejects(guest.videos.fetchListing(1, 12), UnauthorizedError);
});

test("configuration editing retains ETags and reports a conflict without retrying", async () => {
  const calls: RequestInit[] = [];
  const client = createClient({ baseURL: "https://api.example", fetch: async (url, init) => {
    assert.equal(String(url), "https://api.example/admin/api/config.yaml");
    calls.push(init!);
    return init?.method === "PUT"
      ? Response.json({ error: "Changed by another administrator" }, { status: 409 })
      : new Response("server:\n  listen: ':9191'\n", { headers: { ETag: '"revision-1"' } });
  } });
  const document = await client.admin.getConfigYAML();
  assert.equal(document.version, "revision-1");
  await assert.rejects(client.admin.updateConfigYAML(document.content, document.version), ConfigConflictError);
  assert.equal(calls.length, 2);
  assert.equal(new Headers(calls[1].headers).get("If-Match"), '"revision-1"');
  assert.equal(calls[1].body, document.content);
});

test("uploads preserve browser multipart boundaries and binary request bodies", async () => {
  const calls: RequestInit[] = [];
  const client = createClient({ fetch: async (_url, init) => { calls.push(init!); return Response.json({}); } });
  const file = new File(["sample"], "sample.mp4", { type: "video/mp4" });
  await client.videos.uploadVideo({ file, title: "Sample", tags: ["one", "two"] });
  assert.ok(calls[0].body instanceof FormData);
  assert.deepEqual(calls[0].body.getAll("tags"), ["one", "two"]);
  assert.equal(new Headers(calls[0].headers).has("Content-Type"), false);
  await client.admin.putBackupUploadChunk("session", 0, file);
  assert.equal(calls[1].body, file);
  assert.equal(new Headers(calls[1].headers).get("Content-Type"), "application/octet-stream");
});

test("playback parsing, resolving and preparation share one validated protocol", async () => {
  const calls: { url: string; init: RequestInit }[] = [];
  const client = createClient({ baseURL: "https://api.example", fetch: async (url, init) => {
    calls.push({ url: String(url), init: init! });
    return new Response(JSON.stringify({ sources: [original] }));
  } });
  const manifest = await client.playback.get("video/one", { refresh: true });
  assert.deepEqual(manifest.sources, [original]);
  assert.equal(calls[0].url, "https://api.example/api/video/video%2Fone/playback?refresh=1");
  await client.playback.resolve("https://api.example/api/video/demo/playback?source=cloud");
  await client.playback.prepare("/api/video/demo/playback/prepare?source=cloud");
  assert.equal(calls[2].init.method, "POST");
  assert.equal(calls[2].url, "https://api.example/api/video/demo/playback/prepare?source=cloud");
  const malformed = createClient({ fetch: async () => Response.json({ sources: [original, original] }) });
  await assert.rejects(malformed.playback.get("demo"), /无效数据/);
});

test("aborted library reads stop without retrying and writes are sent only once", async () => {
  const controller = new AbortController();
  let reads = 0;
  const client = createClient({ fetch: async (_url, init) => {
    reads++;
    controller.abort();
    throw init!.signal!.reason;
  } });
  await assert.rejects(client.videos.fetchListing(1, 12, {}, { signal: controller.signal }), { name: "AbortError" });
  assert.equal(reads, 1);
  let writes = 0;
  const failing = createClient({ fetch: async () => { writes++; return new Response("unavailable", { status: 503 }); } });
  await assert.rejects(failing.admin.createTag("demo"));
  assert.equal(writes, 1);
});

test("drive event streams use the configured backend and request cookies", (t) => {
  const previous = Object.getOwnPropertyDescriptor(globalThis, "EventSource");
  const calls: { url: string; init?: EventSourceInit }[] = [];
  class FakeEventSource {
    constructor(url: string, init?: EventSourceInit) { calls.push({ url, init }); }
    addEventListener() {}
    close() {}
  }
  Object.defineProperty(globalThis, "EventSource", { configurable: true, value: FakeEventSource });
  t.after(() => { if (previous) Object.defineProperty(globalThis, "EventSource", previous); else Reflect.deleteProperty(globalThis, "EventSource"); });
  const client = createClient({ baseURL: "https://api.example" });
  client.admin.subscribeDriveSnapshots("drive/one", () => {}, () => {}, () => {}).close();
  assert.deepEqual(calls, [{ url: "https://api.example/admin/api/drives/drive%2Fone/events", init: { withCredentials: true } }]);
});

test("frontend management sends one request per explicit action", async () => {
  const calls: { path: string; method: string; body: unknown; type: string | null }[] = [];
  const client = createClient({ baseURL: "https://api.example", fetch: async (input, init) => {
    calls.push({ path: String(input), method: init?.method || "GET", body: init?.body, type: new Headers(init?.headers).get("Content-Type") });
    return Response.json({ frontends: [] });
  } });
  const archive = new Blob(["archive"]);
  await client.frontend.list();
  await client.frontend.updates();
  await client.frontend.activate("");
  await client.frontend.install("https://github.com/owner/web");
  await client.frontend.upload(archive);
  assert.deepEqual(calls, [
    { path: "https://api.example/admin/api/frontend", method: "GET", body: undefined, type: null },
    { path: "https://api.example/admin/api/frontend/updates", method: "GET", body: undefined, type: null },
    { path: "https://api.example/admin/api/frontend/activate", method: "POST", body: '{"id":""}', type: "application/json" },
    { path: "https://api.example/admin/api/frontend/install", method: "POST", body: '{"repository":"https://github.com/owner/web"}', type: "application/json" },
    { path: "https://api.example/admin/api/frontend/upload", method: "POST", body: archive, type: "application/gzip" },
  ]);
  assert.deepEqual(Object.keys(client.frontend).sort(), ["activate", "install", "list", "updates", "upload"]);
});
