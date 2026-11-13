import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { copyFileSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test, { type TestContext } from "node:test";
import { fileURLToPath } from "node:url";

const installer = fileURLToPath(new URL("../install.sh", import.meta.url));

function fixture(t: TestContext) {
  const root = mkdtempSync(join(tmpdir(), "91-local-package-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const installation = join(root, "site");
  const bin = join(root, "bin");
  const calls = join(root, "calls");
  mkdirSync(bin);
  for (const name of ["systemctl", "curl"]) {
    writeFileSync(join(bin, name), `#!/bin/sh
printf '%s\\n' '${name}' >> "$CLI_CALLS"
${name === "curl" ? "exit 93" : "exit 0"}
`, { mode: 0o755 });
  }
  return {
    root, installation, bin,
    package(contents: string, label: string) {
      const source = join(root, `source-${label}`);
      const packageRoot = join(source, "91-linux-amd64");
      mkdirSync(join(packageRoot, "dist"), { recursive: true });
      writeFileSync(join(packageRoot, "server"), contents, { mode: 0o755 });
      writeFileSync(join(packageRoot, "dist", "index.html"), contents);
      writeFileSync(join(packageRoot, "config.example.yaml"), 'server:\n  listen: "0.0.0.0:9191"\n');
      const path = join(root, `${label} release.tar.gz`);
      const result = spawnSync("tar", ["-C", source, "-czf", path, "91-linux-amd64"], { encoding: "utf8" });
      assert.equal(result.status, 0, result.stderr);
      return { path, version: `local-${createHash("sha256").update(readFileSync(path)).digest("hex").slice(0, 12)}` };
    },
    calls() {
      return existsSync(calls) ? readFileSync(calls, "utf8").trim().split("\n").filter(Boolean) : [];
    },
    run(args: string[], overrides: Record<string, string> = {}) {
      return spawnSync("bash", ["-c", `
        source "$1" help >/dev/null
        shift
        need_root() {
          printf 'elevation: %s\\n' "$*" >> "$CLI_CALLS"
          if [[ -n "\${CLI_REPLACE_PACKAGE:-}" ]]; then cp "$CLI_REPLACE_PACKAGE" "$PACKAGE_FILE"; fi
        }
        check_system() { ARCH=amd64; }
        install_deps() { printf 'dependencies\\n' >> "$CLI_CALLS"; }
        write_service() { printf 'service\\n' >> "$CLI_CALLS"; [[ "\${CLI_FAIL_PREPARE:-}" != 1 ]]; }
        open_firewall_port() { :; }
        restart_service_ready() { printf 'restart\\n' >> "$CLI_CALLS"; [[ "\${CLI_FAIL_READY:-}" != 1 ]]; }
        show_success() { :; }
        self_update_manager() { printf 'self-update\\n' >> "$CLI_CALLS"; return 1; }
        download_file() { printf 'download\\n' >> "$CLI_CALLS"; return 1; }
        MANAGER_PATH="$CLI_BIN/manager"
        COMMAND_LINK="$CLI_BIN/91"
        APP_COMMAND_LINK="$CLI_BIN/91"
        main "$@"
      `, "bash", installer, ...args], {
        encoding: "utf8", timeout: 10_000,
        env: { ...process.env, PATH: `${bin}:${process.env.PATH}`, INSTALL_PATH: installation,
          CLI_CALLS: calls, CLI_BIN: bin, SELF_UPDATE: "1", VERSION: "latest",
          LEGACY_MANAGER_PATH: join(bin, "video-site-91-manager"), LEGACY_COMMAND_LINK: join(bin, "video-site-91"), ...overrides },
      });
    },
  };
}

test("local installation uses the archive for the complete file and manager installation flow", (t) => {
  const f = fixture(t);
  const archive = f.package("local binary one", "one");
  const result = f.run(["install", "--package", archive.path]);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(readFileSync(join(f.installation, "server"), "utf8"), "local binary one");
  assert.equal(readFileSync(join(f.installation, "dist", "index.html"), "utf8"), "local binary one");
  assert.ok(existsSync(join(f.installation, "config.yaml")));
  assert.equal(existsSync(join(f.installation, "telegram.yml")), false);
  assert.equal(readFileSync(join(f.installation, ".version"), "utf8").split("\n")[0], archive.version);
  const help = spawnSync(join(f.bin, "91"), ["--help"], { encoding: "utf8" });
  assert.equal(help.status, 0, help.stderr);
  assert.match(help.stdout, /Uninstall 91 and related services/);
  assert.ok(!f.calls().some(call => ["curl", "download", "self-update"].includes(call)));
});

test("bootstrap accepts --package without an explicit install action", (t) => {
  const f = fixture(t);
  const archive = f.package("bootstrap binary", "bootstrap");
  const result = f.run(["--package", archive.path]);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(readFileSync(join(f.installation, "server"), "utf8"), "bootstrap binary");
});

test("local updates preserve user configuration and data without checking GitHub", (t) => {
  const f = fixture(t);
  const first = f.package("first binary", "first");
  const second = f.package("second binary", "second");
  assert.equal(f.run(["install", "--package", first.path]).status, 0);
  const config = '# custom configuration\nserver:\n  listen: "0.0.0.0:19191"\n';
  writeFileSync(join(f.installation, "config.yaml"), config);
  writeFileSync(join(f.installation, "data", "keep.txt"), "existing data");
  const result = f.run(["update", "--package", second.path]);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(readFileSync(join(f.installation, "server"), "utf8"), "second binary");
  assert.equal(readFileSync(join(f.installation, "config.yaml"), "utf8"), config);
  assert.equal(readFileSync(join(f.installation, "data", "keep.txt"), "utf8"), "existing data");
  assert.equal(readFileSync(join(f.installation, ".version"), "utf8").split("\n")[0], second.version);
  assert.ok(!f.calls().some(call => ["curl", "download", "self-update"].includes(call)));
});

test("updating from the same local package skips service changes unless forced", (t) => {
  const f = fixture(t);
  const archive = f.package("same binary", "same");
  assert.equal(f.run(["install", "--package", archive.path]).status, 0);
  const before = f.calls().filter(call => ["systemctl", "restart"].includes(call));
  assert.equal(f.run(["update", "--package", archive.path]).status, 0);
  assert.deepEqual(f.calls().filter(call => ["systemctl", "restart"].includes(call)), before);
  assert.equal(f.run(["update", "--package", archive.path], { FORCE_UPDATE: "1" }).status, 0);
  assert.equal(f.calls().filter(call => call === "restart").length, 2);
});

test("failed local startup keeps the new binary and allows the same package to be retried", (t) => {
  const f = fixture(t);
  const first = f.package("working binary", "working");
  const second = f.package("broken binary", "broken");
  assert.equal(f.run(["install", "--package", first.path]).status, 0);
  const result = f.run(["update", "--package", second.path], { CLI_FAIL_READY: "1" });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /new files and data kept, service stopped/);
  assert.equal(readFileSync(join(f.installation, "server"), "utf8"), "broken binary");
  assert.equal(readFileSync(join(f.installation, "dist", "index.html"), "utf8"), "broken binary");
  assert.equal(existsSync(join(f.installation, ".version")), false);
  assert.equal(f.run(["update", "--package", second.path]).status, 0);
  assert.equal(readFileSync(join(f.installation, ".version"), "utf8").split("\n")[0], second.version);
});

test("failed update preparation restores the previous files before any new startup", (t) => {
  const f = fixture(t);
  const first = f.package("working binary", "working");
  const second = f.package("replacement binary", "replacement");
  assert.equal(f.run(["install", "--package", first.path]).status, 0);
  const result = f.run(["update", "--package", second.path], { CLI_FAIL_PREPARE: "1" });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /restoring previous files/);
  assert.equal(readFileSync(join(f.installation, "server"), "utf8"), "working binary");
  assert.equal(readFileSync(join(f.installation, "dist", "index.html"), "utf8"), "working binary");
  assert.equal(readFileSync(join(f.installation, ".version"), "utf8").split("\n")[0], first.version);
  assert.equal(f.calls().filter(call => call === "restart").length, 1);
});

test("invalid local package options are rejected before elevation or service changes", (t) => {
  const f = fixture(t);
  const archive = f.package("binary", "options");
  for (const args of [
    ["install", "--package"], ["update", "--package", "--help"],
    ["install", "--package", join(f.root, "missing.tar.gz")],
    ["install", "--package", archive.path, "--package", archive.path],
    ["status", "--package", archive.path], ["telegram", "--package", archive.path],
  ]) {
    const result = f.run(args);
    assert.equal(result.status, 2, result.stderr);
  }
  const help = f.run(["install", "--package", archive.path, "--help"]);
  assert.equal(help.status, 0, help.stderr);
  assert.match(help.stdout, /--package FILE/);
  assert.deepEqual(f.calls(), []);
});

test("local package replacement during installation is detected before files are copied", (t) => {
  const f = fixture(t);
  const first = f.package("first", "first");
  const second = f.package("replacement", "replacement");
  const result = f.run(["install", "--package", first.path], { CLI_REPLACE_PACKAGE: second.path });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /local package changed during installation/);
  assert.equal(existsSync(join(f.installation, "server")), false);
});
