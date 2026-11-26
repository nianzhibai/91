import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { createClient } from "../sdk/src/index";

type Parameter = {
  name: string;
  in: string;
  required?: boolean;
  schema: { type: string; enum?: unknown[]; minimum?: number; maximum?: number; maxLength?: number };
};
const contract = JSON.parse(readFileSync(new URL("../backend/internal/api/openapi.json", import.meta.url), "utf8")) as {
  paths: Record<string, Record<string, { parameters?: Parameter[] }>>;
};

test("OpenAPI parameters are unique and every route placeholder is required", () => {
  for (const [path, operations] of Object.entries(contract.paths)) {
    for (const [method, operation] of Object.entries(operations)) {
      const parameters = operation.parameters ?? [];
      const keys = parameters.map(parameter => `${parameter.in}:${parameter.name}`);
      assert.equal(new Set(keys).size, keys.length, `${method} ${path} has duplicate parameters`);
      for (const match of path.matchAll(/\{([^}]+)\}/g)) {
        assert.ok(parameters.some(parameter => parameter.in === "path" && parameter.name === match[1] && parameter.required), `${method} ${path} is missing required ${match[1]}`);
      }
    }
  }
});

test("OpenAPI describes the query parameters sent by SDK library and log requests", async () => {
  const calls: URL[] = [];
  const client = createClient({ baseURL: "https://api.example", fetch: async (input) => {
    const url = new URL(String(input));
    calls.push(url);
    if (url.pathname === "/api/feed") {
      return Response.json({ items: [], total: 3, feedToken: url.searchParams.get("feedToken") || "new-feed", nextCursor: 2, exhausted: false });
    }
    return Response.json({ items: [], total: 0 });
  } });
  await client.videos.list(2, 12, { q: "sample", tag: "demo", sort: "hot", includeTotal: false });
  await client.videos.getFeed({ kind: "listing", cursor: { feedToken: "", position: 0 }, count: 2, q: "sample", tag: "demo", sort: "latest" });
  await client.videos.getFeed({ kind: "listing", cursor: { feedToken: "existing-feed", position: 1 }, count: 2 });
  await client.admin.listLogs({ from: "2026-01-01T00:00:00Z", to: "2026-01-02T00:00:00Z", source: "http", level: "error", method: "GET", query: "sample", limit: 100, before: 10 });

  for (const url of calls) {
    const parameters = contract.paths[url.pathname]?.get?.parameters;
    assert.ok(parameters, `${url.pathname} has no documented parameters`);
    for (const [name, value] of url.searchParams) {
      const parameter = parameters.find(parameter => parameter.in === "query" && parameter.name === name);
      assert.ok(parameter, `${url.pathname} does not document ${name}`);
      const schema = parameter.schema;
      let typed: string | number | boolean = value;
      if (schema.type === "integer") {
        typed = Number(value);
        assert.ok(Number.isInteger(typed), `${name} must be an integer`);
        if (schema.minimum !== undefined) assert.ok(typed >= schema.minimum);
        if (schema.maximum !== undefined) assert.ok(typed <= schema.maximum);
      } else if (schema.type === "boolean") {
        assert.ok(value === "true" || value === "false");
        typed = value === "true";
      } else {
        assert.equal(schema.type, "string");
      }
      if (schema.enum) assert.ok(schema.enum.includes(typed), `${name} rejects ${value}`);
      if (schema.maxLength !== undefined) assert.ok(value.length <= schema.maxLength);
    }
    for (const parameter of parameters) {
      if (parameter.in === "query" && parameter.required) assert.ok(url.searchParams.has(parameter.name));
    }
  }
});

test("admin previews source deletion before submitting the chosen scope", async () => {
  const preview = {
    sources: [
      { driveId: "one", fileId: "1", fileName: "clip.mp4" },
      { driveId: "two", fileId: "2", fileName: "copy.mp4" },
    ],
    requiresConfirmation: true,
    groupRevision: "shown",
  };
  const calls: string[] = [];
  const client = createClient({ baseURL: "https://api.example", fetch: async (input, init) => {
    const path = new URL(String(input)).pathname;
    calls.push(`${init?.method ?? "GET"} ${path}`);
    if (path.endsWith("/deletion-preview")) return Response.json(preview);
    assert.deepEqual(JSON.parse(String(init?.body)), {
      deleteSource: true, includeDuplicates: false, groupRevision: "shown",
    });
    return Response.json({ ok: true, deletedSource: true });
  } });
  const shown = await client.admin.previewVideoDeletion("video id");
  assert.deepEqual(shown, preview);
  assert.deepEqual(calls, ["GET /admin/api/videos/video%20id/deletion-preview"]);
  await client.admin.deleteVideo("video id", {
    deleteSource: true, includeDuplicates: false, groupRevision: shown.groupRevision,
  });
  assert.equal(calls[1], "DELETE /admin/api/videos/video%20id");
});

test("admin updates video author through the documented payload", async () => {
  const bodies: unknown[] = [];
  const client = createClient({ baseURL: "https://api.example", fetch: async (input, init) => {
    assert.equal(init?.method, "PUT");
    assert.equal(new URL(String(input)).pathname, "/admin/api/videos/video%20id");
    bodies.push(JSON.parse(String(init?.body)));
    return Response.json({ id: "video id", author: "Alice" });
  } });
  await client.admin.updateVideo("video id", { author: "Alice" });
  await client.admin.updateVideo("video id", { author: "" });
  assert.deepEqual(bodies, [{ author: "Alice" }, { author: "" }]);
});

test("admin manages merged videos and duplicate reviews through documented routes", async () => {
  const requests: Array<{ method: string; url: string; body?: unknown }> = [];
  const client = createClient({ baseURL: "https://api.example", fetch: async (input, init) => {
    const url = new URL(String(input));
    const method = init?.method ?? "GET";
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    requests.push({ method, url: `${url.pathname}${url.search}`, body });
    if (url.pathname === "/admin/api/merged-videos") {
      return Response.json({ items: [], total: 0, page: 1, size: 20 });
    }
    if (url.pathname === "/admin/api/merged-videos/old%20id") {
      return Response.json({ videoId: "old id", canonicalVideoId: "keep", sourceRevision: "rev-1" });
    }
    if (url.pathname === "/admin/api/merged-videos/old%20id/source") {
      return Response.json({ videoId: "old id", sourceState: "gone" });
    }
    if (url.pathname === "/admin/api/duplicate-reviews") {
      return Response.json({ items: [], total: 0, page: 1, size: 20 });
    }
    return Response.json(body ?? {});
  } });

  await client.admin.listMergedVideos({ driveId: "drive-1", sourceState: "recorded", page: 2, size: 20 });
  await client.admin.getMergedVideo("old id");
  await client.admin.deleteMergedVideoSource("old id", "rev-1");
  await client.admin.listDuplicateReviews({ page: 1, size: 10 });
  await client.admin.mergeDuplicateReview("left-1", "right-2");
  await client.admin.dismissDuplicateReview("left-1", "right-2");

  assert.deepEqual(requests, [
    { method: "GET", url: "/admin/api/merged-videos?driveId=drive-1&sourceState=recorded&page=2&size=20", body: undefined },
    { method: "GET", url: "/admin/api/merged-videos/old%20id", body: undefined },
    { method: "DELETE", url: "/admin/api/merged-videos/old%20id/source", body: { sourceRevision: "rev-1" } },
    { method: "GET", url: "/admin/api/duplicate-reviews?page=1&size=10", body: undefined },
    { method: "POST", url: "/admin/api/duplicate-reviews/merge", body: { keepVideoId: "left-1", mergeVideoId: "right-2" } },
    { method: "POST", url: "/admin/api/duplicate-reviews/dismiss", body: { leftVideoId: "left-1", rightVideoId: "right-2" } },
  ]);
});

const HTTP_METHODS = new Set(["get", "post", "put", "patch", "delete"]);

// Operations intentionally not bound to dedicated SDK methods, documented
// individually. Media proxy paths (/p/*) are served via client.resourceURL().
// Every other OpenAPI (METHOD, path) must be emitted by a real SDK method call.
const INTENTIONALLY_UNWRAPPED_OPERATIONS: Record<string, string> = {
  "GET /api/openapi.json": "OpenAPI schema document itself, not an application API endpoint.",
  "GET /admin/api/drives/{id}": "Alias of GET /admin/api/drives/{id}/config, which is wrapped by admin.getDriveSnapshot(id, 'config').",
};

type OpenAPIOperationEntry = {
  key: string;
  method: string;
  path: string;
  literalSegments: number;
  pattern: RegExp;
};

function buildOpenAPIOperationIndex(): OpenAPIOperationEntry[] {
  const entries: OpenAPIOperationEntry[] = [];
  for (const [path, operations] of Object.entries(contract.paths)) {
    if (path.startsWith("/p/")) continue;
    const segments = path.split("/").filter(Boolean);
    const literalSegments = segments.filter((segment) => !(segment.startsWith("{") && segment.endsWith("}"))).length;
    const regexBody = segments
      .map((segment) =>
        segment.startsWith("{") && segment.endsWith("}")
          ? "[^/]+"
          : segment.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")
      )
      .join("/");
    const pattern = new RegExp(`^/${regexBody}$`);
    for (const method of Object.keys(operations)) {
      if (!HTTP_METHODS.has(method.toLowerCase())) continue;
      const upper = method.toUpperCase();
      entries.push({ key: `${upper} ${path}`, method: upper, path, literalSegments, pattern });
    }
  }
  // Prefer routes with more literal segments so static routes like
  // /admin/api/drives/storage win over parameterized ones like /admin/api/drives/{id}.
  entries.sort((a, b) => b.literalSegments - a.literalSegments || a.key.localeCompare(b.key));
  return entries;
}

function matchOpenAPIOperation(
  index: OpenAPIOperationEntry[],
  method: string,
  pathname: string
): OpenAPIOperationEntry | undefined {
  const upper = method.toUpperCase();
  return index.find((entry) => entry.method === upper && entry.pattern.test(pathname));
}

function trackMethodCalls<T extends Record<string, unknown>>(
  namespace: string,
  target: T,
  called: Set<string>
): T {
  return new Proxy(target, {
    get(obj, prop, receiver) {
      const value = Reflect.get(obj, prop, receiver);
      if (typeof prop === "string" && typeof value === "function") {
        return (...args: unknown[]) => {
          called.add(`${namespace}.${prop}`);
          return (value as (...a: unknown[]) => unknown).apply(obj, args);
        };
      }
      return value;
    },
  });
}

test("SDK runtime requests match OpenAPI operations in both directions", async (t) => {
  const operations = buildOpenAPIOperationIndex();
  const operationKeys = new Set(operations.map((entry) => entry.key));
  const recorded: Array<{ method: string; pathname: string }> = [];

  const originalEventSource = globalThis.EventSource;
  class RecordingEventSource {
    onopen: (() => void) | null = null;
    onerror: (() => void) | null = null;
    constructor(url: string) {
      recorded.push({ method: "GET", pathname: new URL(url).pathname });
    }
    addEventListener() {}
    close() {}
  }
  globalThis.EventSource = RecordingEventSource as unknown as typeof EventSource;
  t.after(() => {
    globalThis.EventSource = originalEventSource;
  });

  const rawClient = createClient({
    baseURL: "https://api.example",
    fetch: async (input, init) => {
      const url = new URL(String(input));
      const method = (init?.method ?? "GET").toUpperCase();
      recorded.push({ method, pathname: url.pathname });

      if (url.pathname === "/api/list") {
        return Response.json({ items: [], total: 0 });
      }
      if (url.pathname === "/api/feed") {
        return Response.json({ items: [], total: 0, feedToken: "", nextCursor: 0, exhausted: true });
      }
      if (
        url.pathname === "/api/home" ||
        url.pathname === "/api/home/latest" ||
        url.pathname === "/api/tags" ||
        url.pathname === "/admin/api/tags" ||
        url.pathname === "/admin/api/upload/tags" ||
        url.pathname.endsWith("/recommendations")
      ) {
        return Response.json([]);
      }
      if (url.pathname.endsWith("/collection")) {
        return Response.json({ name: "col", total: 0, currentIndex: 0, items: [] });
      }
      if (url.pathname.endsWith("/collection/summary")) {
        return Response.json({ name: "col", total: 0, currentIndex: 0 });
      }
      if (url.pathname === "/api/share/consume") {
        return Response.json({
          shareId: "s1",
          expiresAt: "2026-01-01T00:00:00Z",
          video: { videoSrc: "/p/video/v1" },
        });
      }
      if (url.pathname.endsWith("/like")) {
        return Response.json({ likes: 1 });
      }
      if (url.pathname.endsWith("/reaction")) {
        return Response.json({ reaction: "like", likes: 1, dislikes: 0 });
      }
      if (url.pathname === "/api/shorts/next") {
        return Response.json({ items: [], total: 0, feedToken: "", nextCursor: 0, roundComplete: true });
      }
      if (url.pathname.endsWith("/playback") || url.pathname.endsWith("/playback/prepare")) {
        return Response.json({
          sources: [
            {
              id: "src-1",
              label: "Original",
              kind: "original",
              protocol: "file",
              status: "ready",
              url: "/p/video/v1",
            },
          ],
        });
      }
      if (method === "GET" && url.pathname === "/admin/api/config.yaml") {
        return new Response("theme: dark\n", { status: 200, headers: { ETag: '"v1"' } });
      }
      if (method === "GET" && url.pathname === "/admin/api/logs" && url.searchParams.get("download") === "1") {
        return new Response("log\n", { status: 200 });
      }
      if (method === "DELETE" && url.pathname.startsWith("/admin/api/videos/")) {
        return Response.json({ ok: true });
      }
      return Response.json({});
    },
  });

  const calledMethods = new Set<string>();
  const auth = trackMethodCalls("auth", rawClient.auth, calledMethods);
  const videos = trackMethodCalls("videos", rawClient.videos, calledMethods);
  const playback = trackMethodCalls("playback", rawClient.playback, calledMethods);
  const frontend = trackMethodCalls("frontend", rawClient.frontend, calledMethods);
  const settings = trackMethodCalls("settings", rawClient.settings, calledMethods);
  const admin = trackMethodCalls("admin", rawClient.admin, calledMethods);

  // Server & public settings
  await rawClient.server();
  await settings.theme();
  await settings.preview();

  // Auth
  await auth.setupStatus();
  await auth.setupAdmin("admin", "secret");
  await auth.login("admin", "secret");
  await auth.logout();
  await auth.me();

  // Videos & shares
  await videos.listHome(8);
  await videos.listLatest(8);
  await videos.list(1, 20);
  await videos.getFeed({ kind: "listing", cursor: { feedToken: "", position: 0 }, count: 10 });
  await videos.get("v1");
  await videos.listRecommendations("v1");
  await videos.getCollection("v1");
  await videos.getCollectionSummary("v1");
  await videos.listSubtitles("v1");
  await videos.createShare("v1");
  await videos.claimShare("tok-1");
  await videos.listSharedSubtitles("s1");
  await videos.recordSharedView("s1");
  await videos.recordView("v1");
  await videos.setLike("v1", true);
  await videos.setLike("v1", false);
  await videos.setReaction("v1", "visit-1", "like");
  await videos.listTags();
  await videos.getShorts("", 0, 5);

  // Playback (video + share, resolve + prepare)
  await playback.get("v1");
  await playback.resolve("/api/share/s1/playback");
  await playback.prepare("/api/video/v1/playback/prepare");
  await playback.prepare("/api/share/s1/playback/prepare");

  // Frontend management
  await frontend.list();
  await frontend.updates();
  await frontend.activate("modern");
  await frontend.install("owner/repo");
  await frontend.upload(new Blob(["tar"]));

  // Admin: system & users
  await admin.checkUpdate();
  await admin.listLogs();
  await admin.downloadLogs();
  await admin.clearLogs();
  await admin.getSettings();
  await admin.updateSettings({ theme: "dark" });
  await admin.getGPUStatus();
  await admin.probeGPU();
  await admin.getConfigYAML();
  await admin.updateConfigYAML("theme: dark\n", "v1");
  await admin.getScanAllJobStatus();
  await admin.runScanAllJob();
  await admin.stopAllTasks();
  await admin.listUsers();
  await admin.createUser({ username: "u1", password: "pw", role: "user" });
  await admin.deleteUser(1);
  await admin.banUser(1);
  await admin.unbanUser(1);
  await admin.resetPassword(1, "pw2");
  await admin.listBannedIPs();
  await admin.unbanIP("1.2.3.4");

  // Admin: backups & transfers
  await admin.listBackups();
  await admin.createBackup();
  await admin.cancelBackup();
  await admin.deleteBackup("b1");
  recorded.push({ method: "GET", pathname: new URL(admin.backupDownloadURL("b1")).pathname });
  await admin.beginBackupUpload({ fileName: "b.tar", size: 10 });
  await admin.getBackupUpload("u1");
  await admin.putBackupUploadChunk("u1", 0, new Blob(["chunk"]));
  await admin.finalizeBackupUpload("u1", "sha256");
  await admin.cancelBackupUpload("u1");
  await admin.restoreBackup("b1", { confirmation: "CONFIRM" });
  await admin.listBackupTransfers();
  await admin.listBackupReceiveTransfers();
  await admin.cancelBackupReceiveTransfer("r1");
  await admin.createBackupTransfer("b1", { targetUrl: "https://peer.example", receiveToken: "tok" });
  await admin.cancelBackupTransfer("t1");
  await admin.retryBackupTransfer("t1");
  await admin.listBackupReceiveTokens();
  await admin.createBackupReceiveToken();
  await admin.revokeBackupReceiveToken("tok-1");

  // Admin: drives, crawlers & drive login
  await admin.listDrives();
  await admin.getDriveCredentials("d1");
  await admin.getDriveStorage();
  await admin.getDriveSnapshot("d1", "config");
  await admin.getDriveSnapshot("d1", "runtime");
  await admin.getDriveSnapshot("d1", "stats");
  await admin.getDriveSnapshot("d1", "storage");
  admin.subscribeDriveSnapshots("d1", () => {}, () => {}, () => {}).close();
  await admin.upsertDrive({ id: "d1", kind: "localstorage", name: "Local", rootId: "/", credentials: {} });
  await admin.deleteDrive("d1", { deleteVideos: true });
  await admin.rescan("d1");
  await admin.stopDriveTasks("d1");
  await admin.listCrawlers();
  await admin.upsertCrawler({ id: "c1", name: "Crawler", scriptPath: "/tmp/c.js", selectedFeedId: "f1" });
  await admin.importCrawlerScriptFile(new File(["js"], "c.js"));
  await admin.importCrawlerScriptURL("https://example.com/c.js");
  await admin.testCrawlerScript({ scriptPath: "/tmp/c.js", selectedFeedId: "f1" });
  await admin.runCrawler("c1");
  await admin.uploadCrawlerVideos("c1");
  await admin.stopCrawlerTasks("c1");
  await admin.listCrawlerTasks("c1");
  await admin.getCrawlerTask("c1", "t1");
  await admin.cancelCrawlerTask("c1", "t1");
  await admin.setCrawlerPaused("c1", true);
  await admin.deleteCrawler("c1");
  await admin.startDriveLogin("quark");
  await admin.pollDriveLogin("l1");
  await admin.cancelDriveLogin("l1");
  await admin.listDriveDirChildren("d1");
  await admin.setDriveSkipDirIds("d1", ["dir-1"]);
  await admin.setDriveGenerationParallelism("d1", 2);
  await admin.generateDrivePreviews("d1");
  await admin.generateDriveThumbnails("d1");
  await admin.generateDriveFingerprints("d1");

  // Admin: videos, blacklist, merged videos, duplicate reviews & uploads
  await admin.listVideos();
  await admin.getVideoStats();
  await admin.listBlacklist();
  await admin.removeBlacklist("v1");
  await admin.getBlacklistSourceDeleteStatus();
  await admin.startBlacklistSourceDelete();
  await admin.updateVideo("v1", { title: "Updated" });
  await admin.previewVideoDeletion("v1");
  await admin.deleteVideo("v1");
  await admin.regenPreview("v1");
  await admin.regenAllPreviews();
  await admin.listMergedVideos();
  await admin.getMergedVideo("v1");
  await admin.deleteMergedVideoSource("v1", "rev-1");
  await admin.listDuplicateReviews();
  await admin.mergeDuplicateReview("v1", "v2");
  await admin.dismissDuplicateReview("v1", "v2");
  await admin.updateVideoTags("v1", ["tag-1"]);
  await admin.hideVideo("v1");
  await admin.listUploadTags();
  await admin.uploadVideo({ file: new File(["mp4"], "clip.mp4"), title: "Clip", tags: [] });
  recorded.push({ method: "POST", pathname: new URL(admin.uploadURL()).pathname });
  await admin.createRemoteUpload({ url: "https://example.com/v.mp4", title: "Clip", tags: [] });
  await admin.listRemoteUploads();
  await admin.cancelRemoteUpload("job-1");

  // Admin: tags
  await admin.listTags();
  await admin.createTag("demo");
  await admin.updateTag(1, { keywords: ["demo"] });
  await admin.deleteTag(1);
  await admin.getTagJobStatus();
  await admin.retagVideos();

  // Admin: telegram & imports
  await admin.getTelegramAvailability();
  await admin.getTelegramStatus();
  await admin.testTelegram();
  await admin.resumeTelegram();
  await admin.listTelegramImports(20);
  await admin.cancelImport("imp-1");
  await admin.retryImport("imp-1");

  // 1. Ensure every SDK namespace method (except pure non-HTTP helpers) was exercised.
  const NON_HTTP_HELPERS = new Set(["admin.logQueryParams"]);
  const unexercisedMethods: string[] = [];
  for (const [namespace, clientObj] of [
    ["auth", rawClient.auth],
    ["videos", rawClient.videos],
    ["playback", rawClient.playback],
    ["frontend", rawClient.frontend],
    ["settings", rawClient.settings],
    ["admin", rawClient.admin],
  ] as const) {
    for (const [name, value] of Object.entries(clientObj)) {
      if (typeof value !== "function") continue;
      const key = `${namespace}.${name}`;
      if (NON_HTTP_HELPERS.has(key)) continue;
      if (!calledMethods.has(key)) unexercisedMethods.push(key);
    }
  }
  assert.deepEqual(
    unexercisedMethods,
    [],
    `SDK methods missing from contract coverage test: ${unexercisedMethods.join(", ")}`
  );

  // 2. Forward check: every request emitted by the SDK must match a documented OpenAPI operation.
  const matchedKeys = new Set<string>();
  const undocumentedRequests: string[] = [];
  for (const { method, pathname } of recorded) {
    const match = matchOpenAPIOperation(operations, method, pathname);
    if (!match) {
      undocumentedRequests.push(`${method} ${pathname}`);
    } else {
      matchedKeys.add(match.key);
    }
  }
  assert.deepEqual(
    undocumentedRequests,
    [],
    `SDK emitted requests not documented in OpenAPI: ${undocumentedRequests.join(", ")}`
  );

  // 3. Reverse check: every non-media OpenAPI operation must be covered by the SDK or explicitly excluded.
  const uncoveredOperations = operations
    .map((entry) => entry.key)
    .filter((key) => !matchedKeys.has(key) && !(key in INTENTIONALLY_UNWRAPPED_OPERATIONS));
  assert.deepEqual(
    uncoveredOperations,
    [],
    `OpenAPI operations missing from SDK: ${uncoveredOperations.join(", ")}`
  );

  // 4. Exclusion hygiene: every excluded operation must exist in OpenAPI and not already be covered.
  for (const key of Object.keys(INTENTIONALLY_UNWRAPPED_OPERATIONS)) {
    assert.ok(operationKeys.has(key), `Stale exclusion not in OpenAPI: ${key}`);
    assert.ok(!matchedKeys.has(key), `Exclusion is actually covered by SDK and should be removed: ${key}`);
  }
});
