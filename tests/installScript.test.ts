import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const installSource = readFileSync(
  new URL("../install.sh", import.meta.url),
  "utf8"
);

test("installer bypasses proxy settings for local service health checks", () => {
  assert.match(
    installSource,
    /local_service_curl\(\) \{[\s\S]*?curl --disable --noproxy '\*' "\$@"/
  );
  assert.match(
    installSource,
    /if local_service_curl -fsS --connect-timeout 2 --max-time 5 "\$url"/
  );
  assert.doesNotMatch(
    installSource,
    /if curl -fsS --connect-timeout 2 --max-time 5 "\$url"/
  );
});

test("installer distinguishes readiness failures from process start failures", () => {
  assert.match(
    installSource,
    /service process is active, but its local health endpoint is unreachable/
  );
  assert.match(
    installSource,
    /listener\(s\) found on port \$port/
  );
  assert.match(
    installSource,
    /service readiness check failed; see diagnostics above/
  );
  assert.doesNotMatch(installSource, /die "service failed to start"/);
});

test("installer preserves website Telegram settings without a separate deployment", (t) => {
  const root = mkdtempSync(join(tmpdir(), "91-telegram-settings-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const configuration = 'telegram: {enabled: false, api_id: 12345, api_hash: "0123456789abcdef0123456789abcdef"}\n';
  writeFileSync(join(root, "config.example.yaml"), configuration);
  const installer = fileURLToPath(new URL("../install.sh", import.meta.url));
  const prepare = () => {
    const result = spawnSync("bash", ["-c", 'source "$1" help >/dev/null; prepare_config', "bash", installer], {
      encoding: "utf8", env: { ...process.env, INSTALL_PATH: root },
    });
    assert.equal(result.status, 0, result.stderr);
  };
  prepare();
  assert.equal(existsSync(join(root, "telegram.yml")), false);
  const custom = configuration.replace("12345", "54321");
  writeFileSync(join(root, "config.yaml"), custom);
  prepare();
  assert.equal(readFileSync(join(root, "config.yaml"), "utf8"), custom);
});

test("password reset delegates to the server without prompting for a password", () => {
  const start = installSource.indexOf("reset_password() {");
  const end = installSource.indexOf("\nshow_menu()", start);
  const resetSource = installSource.slice(start, end);
  assert.match(resetSource, /\.\/server reset-password "\$@"/);
  assert.doesNotMatch(resetSource, /python3|hash-password|new_pass|systemctl|read -r/);
});

const serverProbe = `#!/usr/bin/env node
console.log(JSON.stringify({args: process.argv.slice(2), cwd: process.cwd(), config: process.env.VIDEO_CONFIG}));
process.exit(Number(process.env.RESET_PROBE_EXIT ?? 0));
`;

test("installer reset forwards arguments, config, working directory and errors", (t) => {
  const root = mkdtempSync(join(tmpdir(), "91-password-reset-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  writeFileSync(join(root, "server"), serverProbe, { mode: 0o700 });
  const installer = fileURLToPath(new URL("../install.sh", import.meta.url));
  for (const exitCode of [0, 23]) {
    const customConfig = join(root, "custom.yaml");
    const result = spawnSync("bash", ["-c", 'source "$1" help >/dev/null; reset_password --user-id 7', "bash", installer], {
      encoding: "utf8",
      env: { ...process.env, INSTALL_PATH: root, VIDEO_CONFIG: customConfig, RESET_PROBE_EXIT: String(exitCode) },
    });
    assert.equal(result.status, exitCode, result.stderr);
    assert.deepEqual(JSON.parse(result.stdout), { args: ["reset-password", "--user-id", "7"], cwd: root, config: customConfig });
  }
});

test("Docker password reset bypasses configuration and version writes", (t) => {
  const root = mkdtempSync(join(tmpdir(), "91-docker-reset-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  writeFileSync(join(root, "server"), serverProbe, { mode: 0o700 });
  const config = join(root, "data", "config.yaml");
  const entrypoint = join(root, "entrypoint.sh");
  writeFileSync(entrypoint, readFileSync(new URL("../docker-entrypoint.sh", import.meta.url), "utf8")
    .replace('APP_DIR="/opt/91"', `APP_DIR="${root}"`));
  for (const server of ["./server", join(root, "server")]) {
    for (const exitCode of [0, 23]) {
      const result = spawnSync("sh", [entrypoint, server, "reset-password", "--user-id", "4"], {
        cwd: root,
        encoding: "utf8",
        env: {
          ...process.env,
          VIDEO_CONFIG: config,
          VIDEO_DATA_DIR: join(root, "data"),
          VIDEO_VERSION_FILE: join(root, "version"),
          VIDEO_IMAGE_VERSION: "test-version",
          RESET_PROBE_EXIT: String(exitCode),
        },
      });
      assert.equal(result.status, exitCode, result.stderr);
      assert.deepEqual(JSON.parse(result.stdout), { args: ["reset-password", "--user-id", "4"], cwd: root, config });
      assert.equal(existsSync(join(root, "data")), false);
      assert.equal(existsSync(join(root, "version")), false);
    }
  }
});
