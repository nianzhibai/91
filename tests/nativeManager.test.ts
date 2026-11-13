import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test, { type TestContext } from "node:test";
import { fileURLToPath } from "node:url";

const installer = fileURLToPath(new URL("../install.sh", import.meta.url));
type Call = { command: string; args: string[] };

// Never run host service or cleanup commands. Record their arguments instead.
const commandProbe = `#!${process.execPath}
const { appendFileSync } = require("node:fs");
const { basename } = require("node:path");
const command = basename(process.argv[1]);
const args = process.argv.slice(2);
appendFileSync(process.env.CLI_CALLS, JSON.stringify({ command, args }) + "\\n");
if (command === "systemctl" && args.includes(process.env.CLI_FAIL_ACTION)) {
  process.exit(Number(process.env.CLI_FAIL_CODE || 1));
}
if (command === "docker" && process.env.CLI_FAIL_DOCKER === "1") process.exit(127);
if (command === "docker" && args[0] === "context") process.stdout.write("unix:///var/run/docker.sock\\n");
if (command === "docker" && args[0] === "info") process.stdout.write("Ubuntu 24.04 LTS\\n");
if (command === "docker" && args[0] === "ps") process.stdout.write(process.env.CLI_DOCKER_PS || "");
`;

function fixture(t: TestContext) {
  const root = mkdtempSync(join(tmpdir(), "91-native-manager-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const bin = join(root, "bin");
  const data = join(root, "data");
  const callsFile = join(root, "calls.jsonl");
  mkdirSync(bin);
  mkdirSync(data);
  writeFileSync(join(data, "video-site.db"), "existing database");
  for (const name of ["systemctl", "journalctl", "docker", "ss", "curl", "rm"]) {
    writeFileSync(join(bin, name), commandProbe, { mode: 0o755 });
  }
  symlinkSync(installer, join(bin, "91"));
  const env = {
    ...process.env,
    PATH: `${bin}:${process.env.PATH}`,
    CLI_CALLS: callsFile,
    CLI_INSTALLER: installer,
    TMPDIR: root,
    INSTALL_PATH: root,
    SERVICE_NAME: "91-native-test",
    FRONTEND_PORT: "19191",
    SERVICE_READY_TIMEOUT: "1",
    UNINSTALL_DELETE_FILES: "0",
    TERM: "dumb",
    DOCKER_HOST: "",
    DOCKER_CONTEXT: "",
  };
  return {
    root,
    env,
    calls(): Call[] {
      return existsSync(callsFile)
        ? readFileSync(callsFile, "utf8").trim().split("\n").filter(Boolean).map(line => JSON.parse(line))
        : [];
    },
    run(args: string[], overrides: Record<string, string> = {}) {
      return spawnSync("bash", ["-c", `
        source "$1" help >/dev/null
        shift
        need_root() { :; }
        COMMAND_LINK="$INSTALL_PATH/91"
        APP_COMMAND_LINK="$INSTALL_PATH/91-test"
        MANAGER_PATH="$INSTALL_PATH/manager"
        if [[ "\${CLI_HOLD_LOCK:-}" == 1 ]]; then
          exec {test_lock}< "$INSTALL_PATH"
          flock -n "$test_lock"
        fi
        main "$@"
      `, "bash", installer, ...args], { encoding: "utf8", env: { ...env, ...overrides } });
    },
  };
}

test("native uninstall deletes installed files and data before removing management commands", (t) => {
  const f = fixture(t);
  const result = f.run(["uninstall"]);
  assert.equal(result.status, 0, result.stderr);
  const calls = f.calls();
  assert.deepEqual(calls.slice(0, 5), [
    { command: "systemctl", args: ["disable", "--now", "91-native-test.service"] },
    { command: "rm", args: ["-rf", "--one-file-system", "--", f.root] },
    { command: "rm", args: ["-f", "/etc/systemd/system/91-native-test.service"] },
    { command: "systemctl", args: ["daemon-reload"] },
    { command: "rm", args: ["-f", join(f.root, "91"), join(f.root, "91-test"), join(f.root, "manager"), join(f.root, "manager.conf")] },
  ]);
  assert.ok(calls.at(-1)?.args.at(-1)?.startsWith(join(f.root, "tmp.")));
});

test("native uninstall always deletes application data even when the old preserve flag is set", (t) => {
  const f = fixture(t);
  const result = f.run(["uninstall"], { UNINSTALL_DELETE_FILES: "0" });
  assert.equal(result.status, 0, result.stderr);
  assert.ok(f.calls().some(call => call.command === "rm" && call.args.includes("--one-file-system") && call.args.at(-1) === f.root));
});

test("native uninstall preserves files and commands if systemd cleanup fails", (t) => {
  const f = fixture(t);
  const result = f.run(["uninstall"], { CLI_FAIL_ACTION: "disable", UNINSTALL_DELETE_FILES: "1" });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /installation has been kept/);
  assert.deepEqual(f.calls().slice(0, 1), [
    { command: "systemctl", args: ["disable", "--now", "91-native-test.service"] },
  ]);
  assert.ok(!f.calls().some(call => call.command === "rm" && call.args.includes("--one-file-system")));
});

test("native uninstall removes a legacy Telegram Bot API container before deleting data", (t) => {
  const f = fixture(t);
  writeFileSync(join(f.root, "telegram.yml"), "configured credentials\n");
  const remove = { command: "docker", args: ["rm", "-f", "-v", "owned"] };
  const failed = f.run(["uninstall"], { CLI_FAIL_DOCKER: "1" });
  assert.notEqual(failed.status, 0);
  assert.match(failed.stderr, /installation kept/);
  assert.ok(!f.calls().some(call => call.command === "systemctl" || call.args.includes("--one-file-system")));
  const result = f.run(["uninstall"], { CLI_DOCKER_PS: `owned\t${f.root}\t${join(f.root, "telegram.yml")}\tvideo-site-telegram\n` });
  assert.equal(result.status, 0, result.stderr);
  const calls = f.calls().map(call => JSON.stringify(call));
  const disable = calls.indexOf(JSON.stringify({ command: "systemctl", args: ["disable", "--now", "91-native-test.service"] }));
  assert.ok(calls.indexOf(JSON.stringify(remove)) >= 0 && calls.indexOf(JSON.stringify(remove)) < disable);
});

test("native uninstall refuses to run while another deployment holds the directory lock", (t) => {
  const f = fixture(t);
  const result = f.run(["uninstall"], { CLI_HOLD_LOCK: "1" });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /Another deployment operation is running/);
  assert.deepEqual(f.calls(), []);
});

test("native start starts the configured service and checks readiness without restarting", (t) => {
  const f = fixture(t);
  const result = f.run(["start"]);
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(f.calls(), [
    { command: "systemctl", args: ["start", "91-native-test.service"] },
    { command: "curl", args: ["--disable", "--noproxy", "*", "-fsS", "--connect-timeout", "2", "--max-time", "5", "http://127.0.0.1:19191/api/settings/theme"] },
  ]);
});

test("native start does not probe readiness after systemd fails to start", (t) => {
  const f = fixture(t);
  const result = f.run(["start"], { CLI_FAIL_ACTION: "start", CLI_FAIL_CODE: "17" });
  assert.equal(result.status, 17);
  assert.deepEqual(f.calls(), [{ command: "systemctl", args: ["start", "91-native-test.service"] }]);
});

test("native status preserves the systemctl exit code", (t) => {
  const f = fixture(t);
  const result = f.run(["status"], { CLI_FAIL_ACTION: "status", CLI_FAIL_CODE: "3" });
  assert.equal(result.status, 3);
  assert.deepEqual(f.calls(), [{ command: "systemctl", args: ["--no-pager", "--full", "status", "91-native-test.service"] }]);
});

test("native command help and invalid arguments never operate on the deployment", (t) => {
  const f = fixture(t);
  for (const action of ["install", "start", "stop", "restart", "status", "logs", "update", "uninstall", "reset-password"]) {
    const help = f.run([action, "--help"]);
    assert.equal(help.status, 0, help.stderr);
    const invocation = action === "install" ? "sudo bash install.sh [install]" : `91 ${action}`;
    assert.ok(help.stdout.startsWith(`Usage: ${invocation}`), help.stdout);
    assert.doesNotMatch(help.stdout, /INSTALL_SCRIPT_URL|INSTALL_SCRIPT_REF|SELF_UPDATE|GITHUB_REPO/);
    if (action !== "reset-password") {
      const invalid = f.run([action, "unexpected"]);
      assert.equal(invalid.status, 2, invalid.stderr);
      assert.match(invalid.stderr, /unexpected arguments/);
    }
  }
  for (const action of ["menu", "gpu", "unknown"]) {
    const invalid = f.run([action]);
    assert.equal(invalid.status, 2, invalid.stderr);
    assert.match(invalid.stderr, /unknown command/);
  }
  assert.deepEqual(f.calls(), []);
});

test("native reset forwards command arguments and elevation arguments without service operations", (t) => {
  const f = fixture(t);
  writeFileSync(join(f.root, "server"), `#!${process.execPath}
console.log(JSON.stringify(process.argv.slice(2)));
`, { mode: 0o755 });
  const result = spawnSync("bash", ["-c", `
    source "$1" help >/dev/null
    need_root() { printf 'elevation: %s\\n' "$*" >&2; }
    main reset-password --user-id 7 --config "$INSTALL_PATH/custom.yaml"
  `, "bash", installer], { encoding: "utf8", env: f.env });
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(JSON.parse(result.stdout), ["reset-password", "--user-id", "7", "--config", join(f.root, "custom.yaml")]);
  assert.match(result.stderr, /elevation: reset-password --user-id 7 --config/);
  assert.deepEqual(f.calls(), []);
});

test("native frontend management forwards the release choice without restarting services", t => {
  const f = fixture(t);
  writeFileSync(join(f.root, "server"), `#!${process.execPath}\nconsole.log(JSON.stringify({args: process.argv.slice(2), config: process.env.VIDEO_CONFIG}));\n`, { mode: 0o755 });
  const result = f.run(["frontend", "install", "--repository", "owner/web", "--version", "v1.2.0"]);
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(JSON.parse(result.stdout), { args: ["frontend", "install", "--repository", "owner/web", "--version", "v1.2.0"], config: join(f.root, "config.yaml") });
  assert.deepEqual(f.calls(), []);
  const help = f.run(["frontend", "--help"]);
  assert.equal(help.status, 0, help.stderr);
  assert.match(help.stdout, /^Usage: 91 frontend/);
});

test("privileged native actions retain their command and arguments when elevated", (t) => {
  const f = fixture(t);
  const bin = f.env.PATH.split(":")[0];
  writeFileSync(join(bin, "id"), "#!/bin/sh\nprintf '1000\\n'\n", { mode: 0o755 });
  writeFileSync(join(bin, "sudo"), commandProbe, { mode: 0o755 });
  const actions = [
    ["install"], ["start"], ["stop"], ["restart"], ["update"], ["uninstall"],
    ["reset-password", "--user-id", "7"],
  ];
  const archive = join(f.root, "local package.tar.gz");
  writeFileSync(archive, "package");
  actions.push(["install", "--package", archive], ["update", "--package", archive]);
  for (const args of actions) {
    const result = spawnSync("bash", [installer, ...args], { encoding: "utf8", env: f.env });
    assert.equal(result.status, 0, result.stderr);
  }
  assert.deepEqual(f.calls(), actions.map(args => ({
    command: "sudo", args: ["-E", "bash", installer, ...args],
  })));
});

test("the installed 91 command shows help on noninteractive stdin instead of installing", (t) => {
  const f = fixture(t);
  const result = spawnSync(join(f.env.PATH.split(":")[0], "91"), [], { encoding: "utf8", env: f.env });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /native \(systemd\) deployment/);
  assert.ok(result.stdout.trim().split("\n").length <= 22, result.stdout);
  assert.doesNotMatch(result.stdout, /Environment:|INSTALL_PATH=|FRONTEND_PORT=|INSTALL_SCRIPT_URL=/);
  assert.doesNotMatch(result.stdout, /^\s+menu\b/m);
  assert.match(result.stdout, /91 COMMAND --help/);
  assert.deepEqual(f.calls(), []);
});

test("the native menu remains open after a failed operation", (t) => {
  const f = fixture(t);
  const result = spawnSync("script", ["-q", "-e", "-c", "91", "/dev/null"], {
    encoding: "utf8",
    input: "1\nx0\n",
    timeout: 10_000,
    env: { ...f.env, CLI_FAIL_ACTION: "status", CLI_FAIL_CODE: "3" },
  });
  assert.equal(result.status, 0, `${result.error ?? ""}\n${result.stdout}\n${result.stderr}`);
  assert.match(result.stdout, /Command 'status' exited with code 3/);
  assert.equal(result.stdout.split("91 native deployment (systemd)").length - 1, 2);
  assert.deepEqual(f.calls(), [{ command: "systemctl", args: ["--no-pager", "--full", "status", "91-native-test.service"] }]);
});
