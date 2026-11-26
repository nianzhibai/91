// Builds `@91/client`, packs a reproducible tarball named with its package
// version, and syncs it into one or more sibling frontend checkouts.
//
// `npm pack` runs the SDK's `prepack` build, honors its `files` list, and
// writes entries in a fixed order with fixed timestamps, so unchanged sources
// always yield the same bytes and the same lockfile integrity.
//
// Usage:
//   node scripts/sync-sdk.mjs ../91-web ../91-web-nostalgic
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const SDK_DIR = join(ROOT, "sdk");

const targets = process.argv.slice(2);
if (targets.length === 0) {
  console.error("Usage: node scripts/sync-sdk.mjs <frontend-dir> [<frontend-dir>...]");
  process.exit(1);
}

const sdkPkg = JSON.parse(readFileSync(join(SDK_DIR, "package.json"), "utf8"));
const tarballName = `91-client-${sdkPkg.version}.tgz`;

// Start from an empty `dist` so modules deleted from `src` cannot linger in the package.
rmSync(join(SDK_DIR, "dist"), { recursive: true, force: true });
const packDir = mkdtempSync(join(tmpdir(), "91-client-"));
let tgz;
try {
  execFileSync("npm", ["pack", "--pack-destination", packDir, "--loglevel=warn"], {
    cwd: SDK_DIR,
    stdio: "inherit",
  });
  tgz = readFileSync(join(packDir, tarballName));
} finally {
  rmSync(packDir, { recursive: true, force: true });
}
const integrity = `sha512-${createHash("sha512").update(tgz).digest("base64")}`;

for (const rawTarget of targets) {
  const repoDir = resolve(process.cwd(), rawTarget);
  const vendorDir = join(repoDir, "vendor");
  mkdirSync(vendorDir, { recursive: true });

  for (const existing of readdirSync(vendorDir)) {
    if (/^91-client-.*\.tgz$/.test(existing) && existing !== tarballName) {
      rmSync(join(vendorDir, existing), { force: true });
    }
  }
  const destTarball = join(vendorDir, tarballName);
  writeFileSync(destTarball, tgz);

  const pkgPath = join(repoDir, "package.json");
  const pkgText = readFileSync(pkgPath, "utf8");
  const updatedPkgText = pkgText.replace(
    /"@91\/client":\s*"file:vendor\/91-client-[^"]+\.tgz"/,
    `"@91/client": "file:vendor/${tarballName}"`
  );
  if (updatedPkgText !== pkgText) {
    writeFileSync(pkgPath, updatedPkgText);
  }

  const lockPath = join(repoDir, "package-lock.json");
  const lockText = readFileSync(lockPath, "utf8");
  const lock = JSON.parse(lockText);
  const pkgEntry = lock.packages?.["node_modules/@91/client"];
  if (!pkgEntry) {
    throw new Error(`Missing node_modules/@91/client entry in ${lockPath}`);
  }
  let updatedLockText = lockText
    .replaceAll(`"file:vendor/${pkgEntry.resolved.replace(/^file:vendor\//, "")}"`, `"file:vendor/${tarballName}"`)
    .replaceAll(`"integrity": "${pkgEntry.integrity}"`, `"integrity": "${integrity}"`);
  if (pkgEntry.version !== sdkPkg.version) {
    updatedLockText = updatedLockText.replace(
      /("node_modules\/@91\/client":\s*\{\s*"version":\s*")[^"]+(")/,
      `$1${sdkPkg.version}$2`
    );
  }
  writeFileSync(lockPath, updatedLockText);

  const installedDir = join(repoDir, "node_modules", "@91", "client");
  rmSync(installedDir, { recursive: true, force: true });
  mkdirSync(installedDir, { recursive: true });
  execFileSync("tar", ["-xzf", destTarball, "--strip-components=1", "-C", installedDir]);

  console.log(`Synced ${tarballName} (${integrity}) -> ${repoDir}`);
}
