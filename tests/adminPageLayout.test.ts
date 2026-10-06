import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const adminCss = readFileSync(new URL("../src/styles/admin.css", import.meta.url), "utf8");

const pageSources = {
  drives: readFileSync(new URL("../src/admin/DrivesPage.tsx", import.meta.url), "utf8"),
  driveLoading: readFileSync(
    new URL("../src/admin/DrivesPageLoading.tsx", import.meta.url),
    "utf8"
  ),
  crawlers: readFileSync(new URL("../src/admin/CrawlersPage.tsx", import.meta.url), "utf8"),
  crawlerLoading: readFileSync(
    new URL("../src/admin/CrawlersPageLoading.tsx", import.meta.url),
    "utf8"
  ),
  videos: readFileSync(new URL("../src/admin/VideosPage.tsx", import.meta.url), "utf8"),
  tags: readFileSync(new URL("../src/admin/TagsPage.tsx", import.meta.url), "utf8"),
  users: readFileSync(new URL("../src/admin/UsersPage.tsx", import.meta.url), "utf8"),
  backup: readFileSync(new URL("../src/admin/BackupPage.tsx", import.meta.url), "utf8"),
  logs: readFileSync(new URL("../src/admin/LogsPage.tsx", import.meta.url), "utf8"),
  settings: readFileSync(new URL("../src/admin/SettingsPage.tsx", import.meta.url), "utf8"),
};

test("all admin routes use the shared flex page contract", () => {
  assert.match(
    adminCss,
    /\.admin-page\s*\{[^}]*display\s*:\s*flex;[^}]*flex\s*:\s*1 1 auto;[^}]*flex-direction\s*:\s*column;[^}]*min-width\s*:\s*0;[^}]*min-height\s*:\s*0;/s
  );

  assert.match(pageSources.drives, /className="admin-page admin-page--with-floating-actions admin-drives-page/);
  assert.match(pageSources.driveLoading, /className="admin-page admin-drives-page/);
  assert.match(pageSources.crawlers, /className="admin-page admin-page--with-floating-actions admin-crawlers-page"/);
  assert.match(pageSources.crawlerLoading, /className="admin-page admin-page--with-floating-actions admin-crawlers-page"/);
  assert.match(pageSources.videos, /className="admin-page admin-page--with-floating-actions admin-videos-page"/);
  assert.match(pageSources.tags, /className="admin-page admin-page--with-floating-actions admin-tags-page/);
  assert.match(
    pageSources.users,
    /className="admin-page admin-page--with-floating-actions"/
  );
  assert.match(
    pageSources.backup,
    /className="admin-page admin-page--with-floating-actions backup-page"/
  );
  assert.match(pageSources.logs, /className="admin-page admin-logs-page"/);
  assert.match(pageSources.settings, /className="admin-page admin-page--with-floating-actions admin-config-page/);
});

test("admin route pages do not recalculate viewport height", () => {
  assert.doesNotMatch(
    adminCss,
    /min-height\s*:\s*calc\(100vh - \(var\(--space-7\) \* 2\)\)/
  );
  assert.doesNotMatch(
    adminCss,
    /min-height\s*:\s*calc\(100dvh - var\(--admin-mobile-header-offset\)/
  );
  assert.doesNotMatch(adminCss, /min-height\s*:\s*calc\(100dvh - 64px\)/);

  assert.match(
    adminCss,
    /\.admin-videos-current,[\s\S]*?\.admin-videos-blacklist\s*\{[^}]*flex\s*:\s*1 1 auto;[^}]*min-height\s*:\s*0/s
  );
  assert.match(
    adminCss,
    /\.admin-loading\s*\{[^}]*flex\s*:\s*1 1 auto;[^}]*min-height\s*:\s*0/s
  );
});

test("drive lists do not add spacing on top of the storage summary margin", () => {
  const cardRule = adminCss.match(/\.admin-card\s*\{([^}]*)\}/)?.[1];
  const gridRule = adminCss.match(/\.admin-drives-grid\s*\{([^}]*)\}/)?.[1];
  assert.ok(cardRule);
  assert.ok(gridRule);
  assert.match(cardRule, /margin-bottom\s*:\s*var\(--space-5\)/);
  assert.match(gridRule, /gap\s*:\s*var\(--space-4\)/);
  assert.doesNotMatch(gridRule, /\bmargin(?:-top|-block(?:-start)?)?\s*:/);
  assert.match(pageSources.drives, /className="admin-drives-grid"/);
  assert.match(pageSources.driveLoading, /className="admin-drives-grid admin-drives-grid--skeleton"/);
});
