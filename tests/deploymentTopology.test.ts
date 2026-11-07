import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { loadConfigFromFile } from "vite";

const deploy = readFileSync(new URL("../deploy.sh", import.meta.url), "utf8");

test("source deployment accepts the supported Node LTS range", () => {
  const deployScript = fileURLToPath(new URL("../deploy.sh", import.meta.url));
  const nodeVersion = readFileSync(new URL("../.nvmrc", import.meta.url), "utf8").trim();
  const [major, minor, patch] = nodeVersion.split(".").map(Number);
  const cases: [string, boolean][] = [
    ["v18.20.8", false],
    ["v20.20.2", false],
    ["v22.23.4", false],
    ["v24.20.0", false],
    [`v${major}.${minor - 1}.0`, false],
    [`v${nodeVersion}`, true],
    [`v${major}.${minor}.${patch + 1}`, true],
    [`v${nodeVersion}-rc.1`, false],
    [`v${major + 2}.0.0`, false],
  ];
  for (const [version, accepted] of cases) {
    const result = spawnSync("bash", ["-c", `
      source "$1" help >/dev/null
      node() { printf '%s\\n' "$NODE_TEST_VERSION"; }
      npm() { return 0; }
      node_ok
    `, "bash", deployScript], {
      encoding: "utf8",
      env: { ...process.env, NODE_TEST_VERSION: version },
    });
    assert.equal(result.status, accepted ? 0 : 1, `${version}: ${result.stderr}`);
  }
});

test("source deployment reports the required Node LTS when installation is disabled", () => {
  const deployScript = fileURLToPath(new URL("../deploy.sh", import.meta.url));
  const nodeVersion = readFileSync(new URL("../.nvmrc", import.meta.url), "utf8").trim();
  const result = spawnSync("bash", ["-c", `
    source "$1" help >/dev/null
    node() { printf 'v20.20.2\\n'; }
    npm() { return 0; }
    INSTALL_DEPS=0
    install_node
  `, "bash", deployScript], { encoding: "utf8" });
  assert.equal(result.status, 1);
  assert.ok(result.stderr.includes(`Node.js ${nodeVersion}+`), result.stderr);
});

test("source deployment enforces the pinned npm version when installation is disabled", () => {
  const deployScript = fileURLToPath(new URL("../deploy.sh", import.meta.url));
  const pkg = JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8"));
  const required = pkg.packageManager.replace(/^npm@/, "");
  for (const version of ["11.19.1", required]) {
    const result = spawnSync("bash", ["-c", `
      source "$1" help >/dev/null
      npm() { printf '%s\\n' "$NPM_TEST_VERSION"; }
      INSTALL_DEPS=0
      install_npm
    `, "bash", deployScript], {
      encoding: "utf8",
      env: { ...process.env, NPM_TEST_VERSION: version },
    });
    assert.equal(result.status, version === required ? 0 : 1, result.stderr);
    if (version !== required) assert.ok(result.stderr.includes(`npm ${required} is required`));
  }
});

test("Docker frontend inputs can load the complete Vite build configuration", async (t) => {
  const repository = fileURLToPath(new URL("../", import.meta.url));
  const directory = mkdtempSync(join(tmpdir(), "91-docker-frontend-inputs-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const dockerfile = readFileSync(join(repository, "Dockerfile"), "utf8");
  const frontend = dockerfile.split(/^FROM\s+/m).find((stage) => /^[^\n]+\s+AS\s+frontend\s*$/im.test(stage));
  assert.ok(frontend, "Dockerfile contains a frontend build stage");

  // Materialize the same local inputs as the frontend stage. Resolving the
  // actual config catches missing helper imports when build tooling changes.
  for (const line of frontend.split("\n")) {
    const copy = line.match(/^COPY\s+(.+)/);
    if (!copy) continue;
    const paths = copy[1].trim().split(/\s+/);
    const target = join(directory, paths.pop()!);
    mkdirSync(target, { recursive: true });
    for (const source of paths) {
      const origin = join(repository, source);
      const destination = statSync(origin).isDirectory()
        ? target
        : join(target, source);
      cpSync(origin, destination, { recursive: true });
    }
  }
  symlinkSync(join(repository, "node_modules"), join(directory, "node_modules"), "dir");
  const loaded = await loadConfigFromFile(
    { command: "build", mode: "production" },
    join(directory, "vite.config.ts"),
    directory
  );
  assert.ok(loaded, "Vite config loads from Docker's copied inputs");
  await Promise.all((loaded.config.plugins ?? []).flat(Infinity));
});

test("systemd deployment serves built frontend assets from the backend", () => {
  assert.match(
    deploy,
    /BACKEND_LISTEN="\$\{BACKEND_LISTEN:-0\.0\.0\.0:\$\{FRONTEND_PORT\}\}"/
  );
  assert.match(deploy, /npm run build/);
  assert.doesNotMatch(deploy, /npm run preview/);
  assert.match(deploy, /ExecStart=\$\{REPO_DIR\}\/backend\/server/);
  assert.match(deploy, /retire_legacy_frontend_service/);
  assert.match(
    deploy,
    /systemctl disable --now "\$\{FRONTEND_SERVICE\}\.service"[\s\S]*?rm -f "\$frontend_unit"/
  );
  assert.match(deploy, /systemctl enable "\$\{SERVICE_NAME\}\.service"/);
  assert.doesNotMatch(
    deploy,
    /systemctl enable "\$\{SERVICE_NAME\}\.service" "\$\{FRONTEND_SERVICE\}\.service"/
  );
});

test("systemd deployment migrates only the legacy default listen address", () => {
  assert.match(deploy, /BACKEND_LISTEN_WAS_SET/);
  assert.match(deploy, /migrating legacy two-service listen address/);
  assert.match(deploy, /127\\\.0\\\.0\\\.1:9192/);
  assert.match(deploy, /backend\/config\.yaml already exists; keeping it/);
  assert.match(
    deploy,
    /install_frontend\s+build_backend[\s\S]*?prepare_config\s+write_systemd_units/
  );
});

test("source deployment needs no separate Telegram deployment", () => {
  const source = readFileSync(new URL("../deploy.sh", import.meta.url), "utf8");
  assert.doesNotMatch(source, /telegram\.example\.yml|telegram-bot-api/);
});

test("source deployment retires the previous backend before enabling 91", (t) => {
  const directory = mkdtempSync(join(tmpdir(), "91-service-rename-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const units = join(directory, "units");
  mkdirSync(units);
  const script = join(directory, "deploy.sh");
  writeFileSync(script, deploy.replaceAll("/etc/systemd/system/", "$TEST_UNIT_DIR/"));
  writeFileSync(join(directory, ".nvmrc"), "24.22.1\n");
  const oldUnit = join(units, "video-site-backend.service");
  writeFileSync(oldUnit, "previous service");
  const result = spawnSync("bash", ["-c", `
    source "$1" help >/dev/null
    systemctl() {
      printf '%s\\n' "$*" >> "$TEST_CALLS"
      [[ "$1" != is-active ]]
    }
    DEPLOY_USER=root DEPLOY_GROUP=root DEPLOY_HOME=/root
    write_systemd_units
  `, "bash", script], {
    encoding: "utf8",
    env: { ...process.env, SERVICE_NAME: "91", TEST_UNIT_DIR: units, TEST_CALLS: join(directory, "calls") },
  });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(readFileSync(join(units, "91.service"), "utf8").includes("Description=91\n"), true);
  assert.equal(statSync(join(units, "91.service")).isFile(), true);
  assert.throws(() => statSync(oldUnit), { code: "ENOENT" });
  const calls = readFileSync(join(directory, "calls"), "utf8").trim().split("\n");
  const disable = calls.indexOf("disable --now video-site-backend.service");
  assert.ok(disable >= 0, calls.join("\n"));
  assert.ok(calls.indexOf("daemon-reload") > disable);
  assert.ok(calls.indexOf("enable 91.service") > disable);
});
