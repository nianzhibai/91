import assert from "node:assert/strict";
import test from "node:test";
import { createElement } from "react";
import { MemoryRouter } from "react-router";
import { act, render, screen } from "./helpers/dom";
import { BackupPage } from "../src/admin/BackupPage";
import { AuthProvider } from "../src/admin/AuthContext";
import { ToastProvider } from "../src/components/ToastContext";

const resumeKey = "video-site-91-backup-upload-v1";
const hint = { id: "upload-1", fileName: "backup.zip", size: 1024, lastModified: 1 };

for (const scenario of ["writable", "read-only"] as const) {
  test(`backup upload remains resumable with ${scenario} storage`, async (t) => {
    const storage = window.localStorage;
    const previousStorage = Object.getOwnPropertyDescriptor(globalThis, "localStorage");
    Object.defineProperty(globalThis, "localStorage", { configurable: true, value: storage });
    t.after(() => {
      if (previousStorage) Object.defineProperty(globalThis, "localStorage", previousStorage);
      else Reflect.deleteProperty(globalThis, "localStorage");
    });
    storage.setItem(resumeKey, JSON.stringify(hint));
    const prototype = Object.getPrototypeOf(storage);
    const write = t.mock.method(prototype, "setItem", scenario === "read-only"
      ? () => { throw new Error("Storage quota exceeded"); }
      : prototype.setItem);
    if (scenario === "read-only") {
      t.mock.method(prototype, "removeItem", () => { throw new Error("Storage is not writable"); });
    }
    const requests: string[] = [];
    t.mock.method(globalThis, "fetch", async (input: string | URL | Request) => {
      const path = String(input);
      requests.push(path);
      const responses: Record<string, unknown> = {
        "/admin/api/me": { authenticated: true, role: "admin" },
        "/admin/api/backups": { backups: [] },
        "/admin/api/backup-transfers": [],
        "/admin/api/backup-receives": [],
        "/admin/api/backup-uploads/upload-1": { ...hint, state: "uploading", received: [], totalChunks: 1, chunkSize: 1024 },
      };
      assert.ok(path in responses, `Unexpected request: ${path}`);
      return Response.json(responses[path]);
    });
    let view!: ReturnType<typeof render>;
    await act(async () => {
      view = render(createElement(MemoryRouter, null,
        createElement(AuthProvider, { children:
          createElement(ToastProvider, { children: createElement(BackupPage) }),
        }),
      ));
    });
    try {
      assert.match(screen.getByRole("button", { name: "从本地上传" }).title, /backup\.zip/);
      assert.ok(requests.includes("/admin/api/backup-uploads/upload-1"));
      assert.equal(write.mock.callCount(), 0);
      assert.deepEqual(JSON.parse(storage.getItem(resumeKey)!), hint);
    } finally {
      await act(async () => view.unmount());
    }
  });
}
