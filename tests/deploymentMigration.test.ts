import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { copyFileSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test, { type TestContext } from "node:test";
import { fileURLToPath } from "node:url";

const repository = fileURLToPath(new URL("../", import.meta.url));
const installer = join(repository, "install.sh");
const migrator = join(repository, "scripts/migrate-compose.py");
const hasYaml = spawnSync("python3", ["-c", "import yaml"]).status === 0;

function nativeFixture(t: TestContext) {
  const root = mkdtempSync(join(tmpdir(), "91-legacy-native-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const installation = join(root, "legacy installation");
  const bin = join(root, "bin");
  const units = join(root, "units");
  const manager = join(bin, "91-manager");
  const calls = join(root, "calls");
  for (const directory of [bin, units, join(installation, "data"), join(installation, "dist")]) mkdirSync(directory, { recursive: true });
  writeFileSync(join(installation, "server"), "previous binary");
  writeFileSync(join(installation, "dist/index.html"), "previous frontend");
  writeFileSync(join(installation, "config.yaml"), `storage: {data_dir: "${installation}/data"}\n`);
  writeFileSync(join(installation, "data/video-site.db"), "existing database");
  writeFileSync(join(units, "video-site-91.service"), `[Service]\nWorkingDirectory=${installation}\nExecStart=${installation}/server\n`);
  const command = `#!/bin/sh
printf '%s %s\\n' "$(basename "$0")" "$*" >> "$TEST_CALLS"
if [ "$1" = restart ] && [ "$2" = 91.service ] && [ "\${TEST_FAIL_READY:-}" = 1 ]; then exit 1; fi
exit 0
`;
  for (const name of ["systemctl", "curl", "journalctl"]) writeFileSync(join(bin, name), command, { mode: 0o755 });
  const source = join(root, "package/91-linux-amd64");
  mkdirSync(join(source, "dist"), { recursive: true });
  writeFileSync(join(source, "server"), "new binary");
  writeFileSync(join(source, "dist/index.html"), "new frontend");
  writeFileSync(join(source, "config.example.yaml"), "server: {listen: 0.0.0.0:9191}\n");
  const archive = join(root, "release.tar.gz");
  assert.equal(spawnSync("tar", ["-C", join(root, "package"), "-czf", archive, "91-linux-amd64"]).status, 0);
  const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !["INSTALL_PATH", "SERVICE_NAME", "APP_NAME", "VIDEO_SKIP_SELF_UPDATE"].includes(key)));
  const environment = { ...env, PATH: `${bin}:${process.env.PATH}`, MANAGER_PATH: manager, SYSTEMD_UNIT_DIR: units, LEGACY_INSTALL_PATH: installation,
    TEST_CALLS: calls, INSTALL_DEPS: "0", CONFIGURE_UFW: "0", SERVICE_READY_TIMEOUT: "0" };
  return {
    root, installation, bin, units, manager, archive, environment,
    calls: () => existsSync(calls) ? readFileSync(calls, "utf8").trim().split("\n") : [],
    run(overrides: Record<string, string> = {}) {
      return spawnSync("bash", ["-c", `
        source "$1" help >/dev/null
        need_root() { :; }
        COMMAND_LINK="$TEST_BIN/91"
        APP_COMMAND_LINK="$COMMAND_LINK"
        main update --package "$2"
      `, "bash", installer, archive], { encoding: "utf8", env: { ...environment, TEST_BIN: bin, ...overrides }, timeout: 10_000 });
    },
  };
}

test("native migration switches the default service and remembers the existing data path", (t) => {
  const f = nativeFixture(t);
  const config = readFileSync(join(f.installation, "config.yaml"), "utf8");
  const result = f.run({ APP_NAME: "video-site-91", INSTALL_PATH: f.installation, SERVICE_NAME: "video-site-91" });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.equal(existsSync(join(f.units, "video-site-91.service")), false);
  assert.match(readFileSync(join(f.units, "91.service"), "utf8"), /Description=91\n/);
  assert.equal(readFileSync(join(f.installation, "config.yaml"), "utf8"), config);
  assert.equal(readFileSync(join(f.installation, "data/video-site.db"), "utf8"), "existing database");
  assert.equal(readFileSync(`${f.manager}.conf`, "utf8"), `${f.installation}\n91\n`);
  const status = spawnSync(join(f.bin, "91"), ["status"], { encoding: "utf8", env: f.environment });
  assert.equal(status.status, 0, status.stderr);
  assert.equal(f.calls().at(-1), "systemctl --no-pager --full status 91.service");
  assert.ok(f.calls().indexOf("systemctl stop video-site-91.service") < f.calls().indexOf("systemctl restart 91.service"));
  // A later update starts directly from the persisted installation settings.
  const second = f.run();
  assert.equal(second.status, 0, second.stdout + second.stderr);
  assert.match(second.stdout, /already up to date/);
});

test("native migration discovers the legacy installation without inherited overrides", (t) => {
  const f = nativeFixture(t);
  const result = f.run();
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.equal(readFileSync(`${f.manager}.conf`, "utf8"), `${f.installation}\n91\n`);
});

test("failed native startup keeps the new deployment and stops the service", (t) => {
  const f = nativeFixture(t);
  const result = f.run({ TEST_FAIL_READY: "1" });
  assert.notEqual(result.status, 0);
  assert.equal(readFileSync(join(f.installation, "server"), "utf8"), "new binary");
  assert.equal(readFileSync(join(f.installation, "dist/index.html"), "utf8"), "new frontend");
  assert.equal(existsSync(join(f.units, "91.service")), true);
  assert.equal(existsSync(join(f.units, "video-site-91.service")), false);
  assert.equal(readFileSync(`${f.manager}.conf`, "utf8"), `${f.installation}\n91\n`);
  assert.equal(existsSync(join(f.installation, ".version")), false);
  assert.ok(f.calls().includes("systemctl stop 91.service"));
  assert.ok(!f.calls().includes("systemctl start video-site-91.service"));
});

test("repeated native startup failures never restore an old executable after database renaming", (t) => {
  const f = nativeFixture(t);
  writeFileSync(join(f.installation, ".version"), "previous-version\n");
  writeFileSync(join(f.bin, "systemctl"), `#!/bin/sh
printf 'systemctl %s\\n' "$*" >> "$TEST_CALLS"
if [ "$1" = restart ] && [ "$2" = 91.service ]; then
  database="$LEGACY_INSTALL_PATH/data/video-site.db"
  if [ -f "$database" ]; then mv "$database" "$LEGACY_INSTALL_PATH/data/91.db"; fi
  if [ "\${TEST_FAIL_READY:-}" = 1 ]; then exit 1; fi
fi
exit 0
`, { mode: 0o755 });
  for (let attempt = 0; attempt < 2; attempt++) {
    const result = f.run({ TEST_FAIL_READY: "1" });
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /new files and data kept, service stopped/);
    assert.equal(readFileSync(join(f.installation, "server"), "utf8"), "new binary");
    assert.equal(readFileSync(join(f.installation, "data/91.db"), "utf8"), "existing database");
    assert.equal(existsSync(join(f.installation, "data/video-site.db")), false);
    assert.equal(existsSync(join(f.installation, ".version")), false);
  }
  assert.ok(!f.calls().includes("systemctl start video-site-91.service"));
  assert.ok(f.calls().includes("systemctl disable video-site-91.service"));
  const retry = f.run();
  assert.equal(retry.status, 0, retry.stdout + retry.stderr);
  assert.equal(existsSync(join(f.installation, ".version")), true);
  assert.equal(readFileSync(join(f.installation, "data/91.db"), "utf8"), "existing database");
});

test("native migration refuses to replace an existing 91 service", (t) => {
  const f = nativeFixture(t);
  writeFileSync(join(f.units, "91.service"), "another installation");
  const result = f.run();
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /91.service already exists/);
  assert.equal(readFileSync(join(f.units, "91.service"), "utf8"), "another installation");
  assert.equal(readFileSync(join(f.installation, "server"), "utf8"), "previous binary");
  assert.deepEqual(f.calls(), []);
});

for (const savedSettings of [false, true]) {
  test(`failed manager settings publication rolls back migration (existing settings: ${savedSettings})`, (t) => {
    const f = nativeFixture(t);
    const settings = `${f.installation}\nvideo-site-91\n`;
    copyFileSync(installer, f.manager);
    if (savedSettings) writeFileSync(`${f.manager}.conf`, settings);
    writeFileSync(join(f.bin, "mv"), `#!/bin/sh
if [ "$2" = "$MANAGER_PATH.conf" ] && [ ! -e "$TEST_BIN/failed-move" ]; then
  touch "$TEST_BIN/failed-move"
  exit 73
fi
exec /bin/mv "$@"
`, { mode: 0o755 });
    const result = f.run();
    assert.notEqual(result.status, 0, result.stdout + result.stderr);
    assert.equal(readFileSync(join(f.installation, "server"), "utf8"), "previous binary");
    assert.equal(existsSync(join(f.units, "91.service")), false);
    assert.equal(existsSync(join(f.units, "video-site-91.service")), true);
    assert.equal(existsSync(`${f.manager}.conf`), savedSettings);
    if (savedSettings) assert.equal(readFileSync(`${f.manager}.conf`, "utf8"), settings);
    assert.ok(f.calls().includes("systemctl start video-site-91.service"));
    assert.ok(!f.calls().includes("systemctl disable video-site-91.service"));
  });
}

test("failure after settings publication restores the previous manager selection and version", (t) => {
  const f = nativeFixture(t);
  const settings = `${f.installation}\nvideo-site-91\n`;
  writeFileSync(`${f.manager}.conf`, settings);
  writeFileSync(join(f.installation, ".version"), "previous-version\n");
  writeFileSync(join(f.bin, "systemctl"), `#!/bin/sh
printf 'systemctl %s\\n' "$*" >> "$TEST_CALLS"
if [ "$1" = disable ] && [ "$2" = video-site-91.service ]; then exit 1; fi
exit 0
`, { mode: 0o755 });
  const result = f.run();
  assert.notEqual(result.status, 0, result.stdout + result.stderr);
  assert.equal(readFileSync(`${f.manager}.conf`, "utf8"), settings);
  assert.equal(readFileSync(join(f.installation, ".version"), "utf8"), "previous-version\n");
  assert.equal(existsSync(join(f.units, "91.service")), false);
  assert.equal(existsSync(join(f.units, "video-site-91.service")), true);
  const status = spawnSync("bash", [f.manager, "status"], { encoding: "utf8", env: f.environment });
  assert.equal(status.status, 0, status.stderr);
  assert.equal(f.calls().at(-1), "systemctl --no-pager --full status video-site-91.service");
});

test("Compose migration keeps custom YAML tags, interpolation, mounts and unrelated values", { skip: !hasYaml && "Python PyYAML is required" }, () => {
  const input = `x-service: &common
  environment: {service: video-site-91, SECRET: "\${PRIVATE_SECRET}"}
services:
  video-site-91:
    <<: *common
    image: ghcr.io/nianzhibai/91:stable
    container_name: video-site-91
    ports: !override ["8181:9191"]
    volumes: ["./data:/opt/video-site-91/data"]
  sidecar:
    image: busybox
    depends_on: {video-site-91: {condition: service_started}}
    links: ["video-site-91:app"]
`;
  const result = spawnSync("python3", [migrator], { input, encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /!override/);
  assert.match(result.stdout, /\$\{PRIVATE_SECRET\}/);
  assert.match(result.stdout, /service: video-site-91/);
  assert.match(result.stdout, /\.\/data:\/opt\/video-site-91\/data/);
  assert.match(result.stdout, /container_name: ['"]91['"]/);
  assert.match(result.stdout, /91:app/);
});

test("release packaging produces only the current installation layout", (t) => {
  const root = mkdtempSync(join(tmpdir(), "91-transition-package-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  mkdirSync(join(root, "backend"));
  mkdirSync(join(root, "dist"));
  writeFileSync(join(root, "backend/config.example.yaml"), "server: {}\n");
  writeFileSync(join(root, "dist/index.html"), "frontend");
  copyFileSync(installer, join(root, "install.sh"));
  const releaseScript = join(root, "release-functions.sh");
  writeFileSync(releaseScript, readFileSync(join(repository, "scripts/build-release.sh"), "utf8").replace(/\nmain "\$@"\s*$/, "\n"));
  const result = spawnSync("bash", ["-c", `
    source "$1" help >/dev/null
    ROOT_DIR="$2" OUT_DIR="$2/release" VERSION=test
    mkdir -p "$OUT_DIR/.work"
    go() { while [[ "$1" != -o ]]; do shift; done; printf binary > "$2"; }
    build_package linux amd64
  `, "bash", releaseScript, root], { encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);
  const archive = join(root, "release/91-linux-amd64.tar.gz");
  const contents = spawnSync("tar", ["-tzf", archive], { encoding: "utf8" });
  assert.equal(contents.status, 0, contents.stderr);
  assert.ok(contents.stdout.split("\n").includes("91-linux-amd64/server"), contents.stdout);
  assert.ok(contents.stdout.split("\n").includes("91-linux-amd64/install.sh"), contents.stdout);
  assert.equal(existsSync(join(root, "release/video-site-91-linux-amd64.tar.gz")), false);
});

function entrypointFixture(t: TestContext) {
  const root = mkdtempSync(join(tmpdir(), "91-entrypoint-migration-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const app = join(root, "91");
  const legacy = join(root, "video-site-91");
  mkdirSync(join(app, "data"), { recursive: true });
  mkdirSync(join(legacy, "data"), { recursive: true });
  writeFileSync(join(app, "config.example.yaml"), 'server:\n  listen: "0.0.0.0:9191"\nstorage:\n  data_dir: "./data"\n');
  writeFileSync(join(app, "server"), `#!${process.execPath}
console.log(JSON.stringify({cwd: process.cwd(), config: process.env.VIDEO_CONFIG, data: process.env.VIDEO_DATA_DIR, args: process.argv.slice(2)}));
`, { mode: 0o755 });
  const entrypoint = join(root, "entrypoint.sh");
  writeFileSync(entrypoint, readFileSync(join(repository, "docker-entrypoint.sh"), "utf8")
    .replace('APP_DIR="/opt/91"', `APP_DIR="${app}"`)
    .replace('LEGACY_DIR="/opt/video-site-91"', `LEGACY_DIR="${legacy}"`), { mode: 0o755 });
  const imageEnv = Object.fromEntries(
    [...readFileSync(join(repository, "Dockerfile"), "utf8").matchAll(/\b(VIDEO_\w+)=([^\s\\]+)/g)]
      .map(([, key, value]) => [key, value.replaceAll("/opt/91", app)])
  );
  const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !["VIDEO_CONFIG", "VIDEO_DATA_DIR", "VIDEO_VERSION_FILE", "VIDEO_IMAGE_VERSION"].includes(key)));
  return {
    app, legacy,
    run(overrides: Record<string, string> = {}, args = ["./server"]) {
      const result = spawnSync("sh", [entrypoint, ...args], { cwd: app, encoding: "utf8", env: { ...env,
        ...imageEnv, VIDEO_IMAGE_VERSION: "v-next", ...overrides } });
      return { ...result, probe: result.status === 0 ? JSON.parse(result.stdout.trim().split("\n").at(-1)!) : null };
    },
  };
}

test("container startup uses the existing legacy mount and preserves relative paths", (t) => {
  const f = entrypointFixture(t);
  const config = 'storage: {data_dir: "./data"}\n';
  writeFileSync(join(f.legacy, "data/config.yaml"), config);
  writeFileSync(join(f.legacy, "data/video-site.db"), "original database");
  const result = f.run();
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.probe.cwd, f.legacy);
  assert.equal(result.probe.config, join(f.legacy, "data/config.yaml"));
  assert.equal(result.probe.data, join(f.legacy, "data"));
  assert.equal(readFileSync(join(f.legacy, "data/config.yaml"), "utf8"), config);
  assert.equal(readFileSync(join(f.legacy, "data/video-site.db"), "utf8"), "original database");
  assert.equal(readFileSync(join(f.legacy, "data/.version"), "utf8"), "v-next\n");
  assert.equal(existsSync(join(f.app, "data/config.yaml")), false);
});

for (const legacy of [false, true]) {
  test(`empty data mounts receive config and version in the selected directory (legacy: ${legacy})`, (t) => {
    const f = entrypointFixture(t);
    if (!legacy) rmSync(f.legacy, { recursive: true });
    const directory = legacy ? f.legacy : f.app;
    const result = f.run();
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.probe.cwd, directory);
    assert.equal(result.probe.config, join(directory, "data/config.yaml"));
    assert.match(readFileSync(join(directory, "data/config.yaml"), "utf8"), new RegExp(directory + "/data"));
    assert.equal(readFileSync(join(directory, "data/.version"), "utf8"), "v-next\n");
  });
}

test("container startup rejects ambiguous data and honors an explicit selection", (t) => {
  const f = entrypointFixture(t);
  for (const path of [f.legacy, f.app]) writeFileSync(join(path, "data/config.yaml"), 'storage: {data_dir: "./data"}\n');
  const ambiguous = f.run();
  assert.notEqual(ambiguous.status, 0);
  assert.match(ambiguous.stderr, /both legacy and current data exist/);
  for (const directory of [f.app, f.legacy]) {
    const selected = f.run({ VIDEO_DATA_DIR: join(directory, "data") });
    assert.equal(selected.status, 0, selected.stderr);
    assert.equal(selected.probe.cwd, directory);
    assert.equal(selected.probe.data, join(directory, "data"));
    assert.equal(selected.probe.config, join(directory, "data/config.yaml"));
    assert.equal(readFileSync(join(directory, "data/.version"), "utf8"), "v-next\n");
    const reset = f.run({ VIDEO_DATA_DIR: join(directory, "data") }, ["./server", "reset-password"]);
    assert.equal(reset.status, 0, reset.stderr);
    assert.equal(reset.probe.config, join(directory, "data/config.yaml"));
  }
});

test("container startup detects the renamed database before choosing a legacy mount", (t) => {
  const f = entrypointFixture(t);
  writeFileSync(join(f.app, "data/91.db"), "current database");
  const result = f.run();
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /both legacy and current data exist/);
  assert.equal(existsSync(join(f.legacy, "data/config.yaml")), false);
});

test("explicit container config and version paths are preserved", (t) => {
  const f = entrypointFixture(t);
  const config = join(f.app, "custom.yaml");
  const version = join(f.app, "custom.version");
  writeFileSync(config, 'storage: {data_dir: "./data"}\n');
  const result = f.run({ VIDEO_DATA_DIR: join(f.legacy, "data"), VIDEO_CONFIG: config, VIDEO_VERSION_FILE: version });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.probe.config, config);
  assert.equal(readFileSync(version, "utf8"), "v-next\n");
  assert.equal(existsSync(join(f.legacy, "data/.version")), false);
  const byConfig = f.run({ VIDEO_CONFIG: config });
  assert.equal(byConfig.status, 0, byConfig.stderr);
  assert.equal(byConfig.probe.cwd, f.app);
  assert.equal(byConfig.probe.data, join(f.app, "data"));
});

test("legacy password reset selects the existing config without writing files", (t) => {
  const f = entrypointFixture(t);
  const config = 'storage: {data_dir: "./data"}\n';
  writeFileSync(join(f.legacy, "data/config.yaml"), config);
  const result = f.run({}, ["./server", "reset-password"]);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.probe.config, join(f.legacy, "data/config.yaml"));
  assert.deepEqual(result.probe.args, ["reset-password"]);
  assert.equal(existsSync(join(f.legacy, "data/.version")), false);
  assert.equal(readFileSync(join(f.legacy, "data/config.yaml"), "utf8"), config);
});
