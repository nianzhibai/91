import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, copyFileSync, existsSync, lstatSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, renameSync, rmSync, statSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test, { type TestContext } from "node:test";
import { fileURLToPath } from "node:url";
import { parse } from "yaml";

const repository = fileURLToPath(new URL("../", import.meta.url));
const script = join(repository, "91-docker.sh");
const service = "91";
const oldImage = `sha256:${"a".repeat(64)}`;
const newImage = `sha256:${"b".repeat(64)}`;
const oldTelegramImage = `sha256:${"c".repeat(64)}`;
const vaapiDevice = "/dev/dri/renderD128";
const nvidiaDevice = "GPU-11111111-1111-1111-1111-111111111111";
type GPUChoice = { mode: "vaapi" | "nvidia" | "rockchip"; device: string; label: string; devices?: string[] };
const defaultGPUs: Record<string, GPUChoice> = {
  rockchip: { mode: "rockchip", device: "/dev/mpp_service", label: "Rockchip MPP / RGA", devices: ["/dev/mpp_service", "/dev/dri", "/dev/dma_heap", "/dev/rga"] },
  vaapi: { mode: "vaapi", device: vaapiDevice, label: "Intel GPU" },
  nvidia: { mode: "nvidia", device: nvidiaDevice, label: "NVIDIA RTX 4060" },
};
const dockerPath = spawnSync("bash", ["-c", "command -v docker"], { encoding: "utf8" }).stdout?.trim();
const composeHelp = dockerPath && spawnSync(dockerPath, ["compose", "config", "--help"], { encoding: "utf8" });
const hasCompose = composeHelp && composeHelp.status === 0 && composeHelp.stdout.includes("--no-env-resolution");
const hasJq = spawnSync("jq", ["--version"]).status === 0;
const requiresCompose = { skip: !hasJq || !hasCompose ? "jq and Docker Compose with raw config support are required" : false };

type EngineState = {
  pulled: boolean;
  calls: string[][];
  starts: any[];
  failPull?: boolean;
  failStart?: boolean;
  failRollback?: boolean;
  changeDuringPull?: boolean;
  existingContainer?: boolean;
  localImageChanged?: boolean;
  createConfigDuringPull?: boolean;
  telegramContainer?: boolean;
  telegramRunning?: boolean;
  failTelegramPull?: boolean;
  resetExit?: number;
  resetInput?: string;
  uninstalls?: any[];
  uninstallExit?: number;
  missingCurrentTemplates?: boolean;
  oldDatabaseFilename?: string;
  newDatabaseFilename?: string;
  info: { Runtimes: Record<string, unknown>; OperatingSystem: string; SecurityOptions?: string[] };
};

// Simulate Engine operations; use the actual Compose CLI for YAML, variables,
// dotenv files, overlays and validation. No containers or images are changed.
const dockerProbe = `#!${process.execPath}
const { readFileSync, readSync, writeFileSync, copyFileSync } = require("node:fs");
const { basename, join } = require("node:path");
const { spawnSync } = require("node:child_process");
const args = process.argv.slice(2);
const root = process.env.TEST_DEPLOYMENT;
const path = join(root, "engine.json");
const state = JSON.parse(readFileSync(path, "utf8"));
state.calls.push(args);
const save = () => writeFileSync(path, JSON.stringify(state));
const output = (value) => process.stdout.write(value + "\\n");
const exit = (code = 0) => { save(); process.exit(code); };
if (args[0] === "compose") {
  let commandIndex = 1;
  while (["--project-directory", "--project-name", "-p", "-f", "--file"].includes(args[commandIndex])) commandIndex += 2;
  const command = args[commandIndex];
  if (command === "config") {
    save();
    const result = spawnSync(process.env.TEST_REAL_DOCKER, args, { stdio: "inherit" });
    process.exit(result.status ?? 1);
  }
  if (command === "exec") {
    if (state.existingContainer === false) { process.stderr.write("service 91 is not running\\n"); exit(1); }
    const values = args.slice(commandIndex + 1).filter((value) => value !== "-T");
    if (["91", "video-site-91"].includes(values[0]) && values[1] === "docker-entrypoint.sh" && values[2] === "./server" && values[3] === "frontend") { output(JSON.stringify({active: "owner-web@1.2.0"})); exit(); }
    if (values.length !== 4 || !["91", "video-site-91"].includes(values[0]) || values[1] !== "docker-entrypoint.sh" || values[2] !== "./server" || values[3] !== "reset-password") exit(2);
    output("ID | Username | Role | Account banned\\n7 | test-user | admin | false");
    process.stdout.write("User ID to reset: ");
    const buffer = Buffer.alloc(1);
    let input = "";
    while (readSync(0, buffer, 0, 1, null)) { if (buffer[0] === 10) break; input += buffer.toString(); }
    state.resetInput = input;
    if (!input.trim()) { process.stderr.write("no user ID supplied; password was not changed\\n"); exit(1); }
    if (state.resetExit) { process.stderr.write("server rejected the reset request\\n"); exit(state.resetExit); }
    output("New password: Generated123");
    exit();
  }
  if (command === "ps") {
    if (args.at(-1) === "telegram-bot-api") {
      if (state.telegramContainer && (args.includes("-a") || state.telegramRunning !== false)) output("existing-telegram");
    } else if (state.existingContainer !== false) output("existing-container");
    exit();
  }
  if (command === "rm" || command === "stop") exit();
  if (command === "up" || command === "down") {
    const config = spawnSync(process.env.TEST_REAL_DOCKER,
      [...args.slice(0, commandIndex), "config", "--format", "json"], { encoding: "utf8" });
    if (config.status) { process.stderr.write(config.stderr); exit(config.status); }
    if (command === "down") {
      (state.uninstalls ??= []).push(JSON.parse(config.stdout));
      if (state.uninstallExit) process.stderr.write("Docker cleanup failed\\n");
      exit(state.uninstallExit ?? 0);
    }
    state.starts.push(JSON.parse(config.stdout));
    exit((state.starts.length === 1 && state.failStart) || (state.starts.length > 1 && state.failRollback) ? 1 : 0);
  }
} else if (args[0] === "info") { output(args.includes("{{.OperatingSystem}}") ? state.info.OperatingSystem : JSON.stringify(state.info)); exit();
} else if (args[0] === "context") { output("unix:///var/run/docker.sock"); exit();
} else if (args[0] === "inspect") { output(args.at(-1) === "existing-telegram" ? process.env.TEST_OLD_TELEGRAM_IMAGE : process.env.TEST_OLD_IMAGE); exit();
} else if (args[0] === "pull") {
  state.pulled = true;
  if (state.changeDuringPull) writeFileSync(join(root, "docker-compose.yml"), "# concurrent edit\\n" + readFileSync(join(root, "docker-compose.yml"), "utf8"));
  if (state.createConfigDuringPull) writeFileSync(join(root, "docker-compose.yml"), "# created by another process\\nservices: {}\\n");
  exit(state.failPull || (state.failTelegramPull && args[1].includes("telegram")) ? 1 : 0);
} else if (args[0] === "image" && args.includes('{{ index .Config.Labels "io.91.database-filename" }}')) {
  output((args.at(-1) === process.env.TEST_OLD_IMAGE ? state.oldDatabaseFilename : state.newDatabaseFilename) ?? "91.db"); exit();
} else if (args[0] === "image" && args.includes('{{ index .Config.Labels "io.91.media-variant" }}')) { output(process.env.TEST_MEDIA_VARIANT ?? "standard"); exit();
} else if (args[0] === "image") { output(state.pulled || state.localImageChanged ? process.env.TEST_NEW_IMAGE : process.env.TEST_OLD_IMAGE); exit();
} else if (args[0] === "create") { output("template-container"); exit();
} else if (args[0] === "cp") {
  if (state.missingCurrentTemplates && args[1].includes("/usr/local/share/91/deploy/")) exit(1);
  try { copyFileSync(join(root, "templates", basename(args[1])), args[2]); exit(); }
  catch { exit(1); }
} else if (args[0] === "rm") { output("template-container"); exit(); }
else if (args[0] === "run" && args.includes("/usr/local/share/91/migrate-compose.py")) {
  save();
  const result = spawnSync("python3", [process.env.TEST_COMPOSE_MIGRATOR, ...args.slice(args.indexOf("/usr/local/share/91/migrate-compose.py") + 1)], {stdio: "inherit"});
  process.exit(result.status ?? 1);
}
process.stderr.write("Unexpected Docker command: " + JSON.stringify(args));
exit(1);
`;

function fixture(t: TestContext) {
  const root = mkdtempSync(join(tmpdir(), "91-shell-docker-update-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const bin = join(root, "bin");
  mkdirSync(bin);
  mkdirSync(join(root, "templates"));
  const target = join(root, "docker-compose.yml");
  copyFileSync(join(repository, "docker-compose.yml"), target);
  for (const name of ["docker-compose.yml", "docker-compose.gpu-nvidia.yml", "docker-compose.gpu-vaapi.yml", "docker-compose.gpu-rockchip.yml"]) {
    copyFileSync(join(repository, name), join(root, "templates", name));
  }
  writeFileSync(join(bin, "docker"), dockerProbe, { mode: 0o755 });
  // Even a broken uninstall must never delete files outside this fixture.
  writeFileSync(join(bin, "rm"), `#!/bin/bash
for path in "$@"; do
  case "$path" in
    -*) ;;
    "$TEST_DEPLOYMENT"/*) ;;
    *) printf 'Unexpected deletion outside fixture: %s\\n' "$path" >&2; exit 97 ;;
  esac
done
exec /bin/rm "$@"
`, { mode: 0o755 });
  writeFileSync(join(bin, "nvidia-smi"), `#!/bin/sh
if [ "$TEST_NVIDIA_MODE" = ready ]; then
  [ "$1" = --query-gpu=uuid,name ] || exit 2
  printf '%s\\n' "\${TEST_NVIDIA_DEVICES:-${nvidiaDevice}, NVIDIA RTX 4060}"
else exit 1
fi
`, { mode: 0o755 });
  writeFileSync(join(bin, "timeout"), '#!/bin/sh\nif [ "$TEST_NVIDIA_MODE" = timeout ]; then exit 124; fi\nexec /usr/bin/timeout "$@"\n', { mode: 0o755 });
  // An inherited COMPOSE_FILE must never redirect a test to a real deployment.
  const environment = Object.fromEntries(Object.entries(process.env).filter(([key]) =>
    !key.startsWith("COMPOSE_") && !["DOCKER_HOST", "DOCKER_CONTEXT", "STACK_NAME", "VIDEO_IMAGE", "WEB_PORT", "LOCAL_VALUE", "CUSTOM_VALUE", "PRIVATE_FROM_HOST", "TELEGRAM_API_ID", "TELEGRAM_API_HASH"].includes(key)
  ));
  const env = {
    ...environment,
    SELF_UPDATE: "0",
    PATH: `${bin}:${process.env.PATH ?? ""}`,
    TEST_DEPLOYMENT: root,
    TEST_COMPOSE_MIGRATOR: join(repository, "scripts/migrate-compose.py"),
    TEST_REAL_DOCKER: dockerPath!,
    TEST_OLD_IMAGE: oldImage,
    TEST_NEW_IMAGE: newImage,
    TEST_OLD_TELEGRAM_IMAGE: oldTelegramImage,
    TEST_GPU_CHOICES: "[]",
    TEST_NVIDIA_MODE: "none",
  };
  const stateFile = join(root, "engine.json");
  const initial: EngineState = { pulled: false, calls: [], starts: [], info: { Runtimes: { nvidia: {} }, OperatingSystem: "Debian" } };
  writeFileSync(stateFile, JSON.stringify(initial));
  return {
    root, target, env,
    state: () => JSON.parse(readFileSync(stateFile, "utf8")) as EngineState,
    changeState(update: Partial<EngineState>) { writeFileSync(stateFile, JSON.stringify({ ...this.state(), ...update })); },
    backups: () => readdirSync(root).filter((name) => name.includes(".bak-")),
    raw() {
      const result = spawnSync(dockerPath!, ["compose", "--project-directory", root, "-f", target, "config", "--format", "json", "--no-interpolate", "--no-path-resolution", "--no-normalize", "--no-env-resolution"], { cwd: root, env, encoding: "utf8" });
      assert.equal(result.status, 0, result.stderr);
      return JSON.parse(result.stdout);
    },
    run(args: string[] = [], overrides: Record<string, string> = {}, input?: string) {
      const choices = overrides.TEST_GPU_CHOICES ?? JSON.stringify(
        (overrides.TEST_READY_GPU ?? "").split(/\s+/).filter(Boolean).map((mode) => defaultGPUs[mode])
      );
      return spawnSync("bash", ["-c", 'source "$1"; detect_host_gpu() { GPU_CHOICES="$TEST_GPU_CHOICES"; }; main "${@:2}"', "bash", script, ...args], {
        cwd: root, env: { ...env, ...overrides, TEST_GPU_CHOICES: choices }, encoding: "utf8", timeout: 30_000, input,
      });
    },
    functions(program: string, overrides: Record<string, string> = {}) {
      return spawnSync("bash", ["-c", `source "$1"\n${program}`, "bash", script], {
        cwd: root, env: { ...env, ...overrides }, encoding: "utf8", timeout: 15_000,
      });
    },
  };
}

function succeeded(result: ReturnType<typeof spawnSync>) {
  assert.ifError(result.error);
  assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
}

function selfUpdatingManager(t: TestContext, f = fixture(t)) {
  const manager = join(f.root, "91-docker.sh");
  const download = join(f.root, "downloaded-script.sh");
  const original = readFileSync(script, "utf8");
  const updated = original + "\n# Updated management script fixture\n";
  writeFileSync(manager, original, { mode: 0o640 });
  writeFileSync(download, updated);
  writeFileSync(join(f.root, "bin/curl"), `#!${process.execPath}
const { readFileSync, writeFileSync, copyFileSync } = require("node:fs");
const { join } = require("node:path");
const root = process.env.TEST_DEPLOYMENT;
const args = process.argv.slice(2);
const path = join(root, "engine.json");
const state = JSON.parse(readFileSync(path, "utf8"));
state.calls.push(["script-download", ...args]);
writeFileSync(path, JSON.stringify(state));
const output = args[args.indexOf("-o") + 1];
if (process.env.TEST_DOWNLOAD_FAIL === "1") {
  writeFileSync(output, "partial download");
  process.exit(22);
}
copyFileSync(join(root, "downloaded-script.sh"), output);
`, { mode: 0o755 });
  return {
    ...f, manager, download, original, updated,
    runManager(args: string[], overrides: Record<string, string> = {}, path = manager) {
      return spawnSync("bash", [path, ...args], {
        cwd: f.root, encoding: "utf8", timeout: 15_000, input: "7\n",
        env: { ...f.env, SELF_UPDATE: "1", VIDEO_SKIP_SELF_UPDATE: "0", DOCKER_SCRIPT_URL: "", ...overrides },
      });
    },
  };
}

test("update replaces its script once and preserves arguments, environment, directory and permissions", requiresCompose, (t) => {
  const f = selfUpdatingManager(t);
  const composeFile = join(f.root, "production config.yml");
  renameSync(f.target, composeFile);
  const config = parse(readFileSync(composeFile, "utf8"));
  config.services[service].environment.LOCAL_VALUE = "${LOCAL_VALUE}";
  writeFileSync(composeFile, JSON.stringify(config));
  const before = statSync(f.manager);
  const image = "ghcr.io/example/91:v-next";
  succeeded(f.runManager(["update", "--image", image], {
    COMPOSE_FILE: "production config.yml", COMPOSE_PROJECT_NAME: "self-update-project", LOCAL_VALUE: "kept value",
  }));
  assert.equal(readFileSync(f.manager, "utf8"), f.updated);
  const after = statSync(f.manager);
  for (const key of ["mode", "uid", "gid"] as const) assert.equal(after[key], before[key]);
  const state = f.state();
  const downloads = state.calls.filter(args => args[0] === "script-download");
  assert.equal(downloads.length, 1);
  assert.ok(downloads[0].includes("https://raw.githubusercontent.com/nianzhibai/91/main/91-docker.sh"));
  assert.equal(state.calls[0][0], "script-download");
  assert.equal(state.starts.length, 1);
  assert.equal(state.starts[0].name, "self-update-project");
  assert.equal(state.starts[0].services[service].image, image);
  assert.equal(state.starts[0].services[service].environment.LOCAL_VALUE, "kept value");
  assert.equal(readdirSync(f.root).some(name => name.startsWith("91-docker.sh.tmp.")), false);
});

test("script self-update preserves symlink entry points and accepts a custom download URL", requiresCompose, (t) => {
  const f = selfUpdatingManager(t);
  const link = join(f.root, "manager link.sh");
  symlinkSync(f.manager, link);
  const url = "https://mirror.example/91-docker.sh";
  succeeded(f.runManager(["update"], { DOCKER_SCRIPT_URL: url }, link));
  assert.equal(lstatSync(link).isSymbolicLink(), true);
  assert.equal(readFileSync(f.manager, "utf8"), f.updated);
  assert.ok(f.state().calls.find(args => args[0] === "script-download")!.includes(url));
});

for (const scenario of ["unchanged", "download failure", "empty", "invalid syntax", "not a script", "replace failure"] as const) {
  test(`script self-update ${scenario} keeps the current script and continues deployment`, requiresCompose, (t) => {
    const f = selfUpdatingManager(t);
    const candidates = {
      unchanged: f.original, empty: "", "invalid syntax": "#!/bin/bash\nif then\n", "not a script": "Not Found\n",
    };
    if (scenario in candidates) writeFileSync(f.download, candidates[scenario as keyof typeof candidates]);
    if (scenario === "replace failure") {
      writeFileSync(join(f.root, "bin/mv"), `#!/bin/bash
if [[ "\${@: -1}" == "$TEST_DEPLOYMENT/91-docker.sh" ]]; then exit 1; fi
exec /bin/mv "$@"
`, { mode: 0o755 });
    }
    const result = f.runManager(["update"], { TEST_DOWNLOAD_FAIL: scenario === "download failure" ? "1" : "0" });
    succeeded(result);
    assert.equal(readFileSync(f.manager, "utf8"), f.original);
    assert.equal(f.state().calls.filter(args => args[0] === "script-download").length, 1);
    assert.equal(f.state().starts.length, 1);
    assert.equal(readdirSync(f.root).some(name => name.startsWith("91-docker.sh.tmp.")), false);
    assert.doesNotMatch(result.stdout, /restarting the requested update/);
    if (scenario !== "unchanged") assert.match(result.stdout, /self-update skipped/);
  });
}

test("script self-update can be disabled and does not run for other commands or rejected arguments", requiresCompose, (t) => {
  const f = selfUpdatingManager(t);
  succeeded(f.runManager(["update"], { SELF_UPDATE: "0" }));
  succeeded(f.runManager(["update"], { VIDEO_SKIP_SELF_UPDATE: "1" }));
  succeeded(f.runManager(["reset-password"]));
  succeeded(f.runManager(["update", "--help"]));
  assert.notEqual(f.runManager(["update", "--unknown"]).status, 0);
  assert.equal(f.state().calls.some(args => args[0] === "script-download"), false);
  assert.equal(readFileSync(f.manager, "utf8"), f.original);
});

function freshDeployment(t: TestContext) {
  const f = fixture(t);
  rmSync(f.target);
  f.changeState({ existingContainer: false });
  return f;
}

const telegramCredentials = {
  TELEGRAM_API_ID: "12345",
  TELEGRAM_API_HASH: "0123456789abcdef0123456789abcdef",
};

test("uninstall deletes deployment resources, application data and configuration", requiresCompose, (t) => {
  const f = fixture(t);
  const config = parse(readFileSync(f.target, "utf8"));
  config.services[service].volumes.push("./library:/videos:ro");
  writeFileSync(f.target, JSON.stringify(config));
  for (const directory of ["data", "data/uploads", "data/previews", "library"]) mkdirSync(join(f.root, directory), { recursive: true });
  writeFileSync(join(f.root, "data/config.yaml"), "storage: {data_dir: /opt/91/data}\n");
  writeFileSync(join(f.root, "data/uploads/clip.mp4"), "uploaded video");
  writeFileSync(join(f.root, "library/original.mp4"), "external original");
  writeFileSync(join(f.root, ".env"), "WEB_PORT=9191\n");
  writeFileSync(`${f.target}.bak-20261018.123456`, "old deployment");
  writeFileSync(join(f.root, "notes.txt"), "unrelated file");

  const result = f.run(["uninstall"]);
  succeeded(result);
  const state = f.state();
  const down = state.calls.find((args) => args.includes("down"))!;
  assert.deepEqual(down.slice(down.indexOf("down")), ["down", "--remove-orphans", "--volumes", "--rmi", "all"]);
  assert.equal(state.uninstalls?.length, 1);
  for (const path of [f.target, join(f.root, "data"), join(f.root, ".env")]) assert.equal(existsSync(path), false, path);
  assert.deepEqual(f.backups(), []);
  assert.equal(readdirSync(f.root).some((name) => name.startsWith(".docker-update.")), false);
  assert.equal(existsSync(join(f.root, ".docker-compose.yml.update.lock")), false);
  assert.equal(readFileSync(join(f.root, "library/original.mp4"), "utf8"), "external original");
  assert.equal(readFileSync(join(f.root, "notes.txt"), "utf8"), "unrelated file");
  assert.deepEqual(state.starts, []);
  assert.equal(state.pulled, false);
  assert.equal(state.calls.some((args) => ["image", "create", "cp"].includes(args[0])), false);
  assert.match(result.stdout, /Uninstall complete/);
});

test("uninstall honors custom Compose files, overlays and the dotenv project name", requiresCompose, (t) => {
  const f = fixture(t);
  const alternative = join(f.root, "production.yml");
  const overlay = join(f.root, "storage.yml");
  renameSync(f.target, alternative);
  writeFileSync(overlay, JSON.stringify({ services: { [service]: { volumes: ["./private data:/opt/91/data"] } } }));
  writeFileSync(join(f.root, ".env"), "COMPOSE_FILE=production.yml:storage.yml\nCOMPOSE_PROJECT_NAME=existing-project\n");
  mkdirSync(join(f.root, "private data"));
  writeFileSync(join(f.root, "private data/video-site.db"), "database");
  writeFileSync(`${overlay}.bak-20261018.123456`, "overlay backup");

  succeeded(f.run(["uninstall"]));
  assert.equal(f.state().uninstalls?.[0].name, "existing-project");
  const down = f.state().calls.find((args) => args.includes("down"))!;
  assert.deepEqual(down.slice(1, down.indexOf("down")), ["--project-directory", f.root, "-f", alternative, "-f", overlay]);
  for (const path of [alternative, overlay, f.target, join(f.root, ".env"), join(f.root, "private data")]) assert.equal(existsSync(path), false, path);
  assert.deepEqual(f.backups(), []);
});

test("uninstall removes separate Telegram storage and custom 91 data/config mounts", requiresCompose, (t) => {
  const f = fixture(t);
  const config = parse(readFileSync(join(repository, "docker-compose.yml"), "utf8"));
  config.services["telegram-bot-api"] = {image: "legacy-bot-api", volumes: []};
  Object.assign(config.services[service].environment, { VIDEO_DATA_DIR: "/app-data", VIDEO_CONFIG: "/app-config.yaml" });
  config.services[service].volumes = ["./site:/app-data", "./settings.yaml:/app-config.yaml:ro", "./bot:/var/lib/telegram-bot-api"];
  config.services["telegram-bot-api"].volumes = ["./bot:/var/lib/telegram-bot-api"];
  writeFileSync(f.target, JSON.stringify(config));
  for (const directory of ["site", "bot"]) {
    mkdirSync(join(f.root, directory));
    writeFileSync(join(f.root, directory, "data.bin"), "owned data");
  }
  writeFileSync(join(f.root, "settings.yaml"), "storage: {}\n");

  succeeded(f.run(["uninstall"]));
  assert.ok(f.state().uninstalls?.[0].services["telegram-bot-api"]);
  for (const path of ["site", "bot", "settings.yaml"]) assert.equal(existsSync(join(f.root, path)), false, path);
});

test("uninstall removes named volumes without requiring local host bind cleanup", requiresCompose, (t) => {
  const f = fixture(t);
  const config = parse(readFileSync(f.target, "utf8"));
  config.services[service].volumes = ["app-data:/opt/91/data"];
  config.volumes = { "app-data": {} };
  writeFileSync(f.target, JSON.stringify(config));

  succeeded(f.run(["uninstall"], { DOCKER_HOST: "ssh://docker-host" }));
  assert.ok(f.state().uninstalls?.[0].volumes["app-data"]);
  assert.equal(f.state().calls.some((args) => ["context", "info"].includes(args[0])), false);
  assert.equal(existsSync(f.target), false);
});

test("uninstall removes a data directory and its configured symlink", requiresCompose, (t) => {
  const f = fixture(t);
  mkdirSync(join(f.root, "storage"));
  writeFileSync(join(f.root, "storage/video-site.db"), "database");
  symlinkSync(join(f.root, "storage"), join(f.root, "data"), "dir");

  succeeded(f.run(["uninstall"]));
  assert.equal(existsSync(join(f.root, "storage")), false);
  assert.equal(readdirSync(f.root).includes("data"), false);
});

for (const storage of ["/", ".", "./unsafe-link"]) {
  test(`uninstall refuses unsafe data path ${storage} before Docker cleanup`, requiresCompose, (t) => {
    const f = fixture(t);
    const config = parse(readFileSync(f.target, "utf8"));
    config.services[service].volumes = [`${storage}:/opt/91/data`];
    writeFileSync(f.target, JSON.stringify(config));
    if (storage === "./unsafe-link") symlinkSync(f.root, join(f.root, "unsafe-link"), "dir");
    const original = readFileSync(f.target, "utf8");

    const result = f.run(["uninstall"]);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /Refusing to delete unsafe application data path/);
    assert.equal(readFileSync(f.target, "utf8"), original);
    assert.equal(f.state().uninstalls, undefined);
  });
}

test("uninstall refuses a remote bind-mount deployment before changing anything", requiresCompose, (t) => {
  const f = fixture(t);
  const original = readFileSync(f.target, "utf8");
  const result = f.run(["uninstall"], { DOCKER_HOST: "ssh://docker-host" });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /run uninstall on the Docker host/);
  assert.equal(f.state().uninstalls, undefined);
  assert.equal(readFileSync(f.target, "utf8"), original);
});

test("uninstall preserves data and deployment files when Docker cleanup fails", requiresCompose, (t) => {
  const f = fixture(t);
  const original = readFileSync(f.target, "utf8");
  mkdirSync(join(f.root, "data"));
  writeFileSync(join(f.root, "data/video-site.db"), "database");
  f.changeState({ uninstallExit: 23 });

  const result = f.run(["uninstall"]);
  assert.equal(result.status, 23);
  assert.match(result.stderr, /Docker cleanup failed/);
  assert.doesNotMatch(result.stdout, /Uninstall complete/);
  assert.equal(readFileSync(f.target, "utf8"), original);
  assert.equal(readFileSync(join(f.root, "data/video-site.db"), "utf8"), "database");
});

test("uninstall preserves deployment files when host data cleanup fails", requiresCompose, (t) => {
  const f = fixture(t);
  const original = readFileSync(f.target, "utf8");
  mkdirSync(join(f.root, "data"));
  const result = f.functions(`
rm() {
  local path
  for path in "$@"; do
    if [[ "$path" == "$TEST_DEPLOYMENT/data" ]]; then return 27; fi
  done
  command rm "$@"
}
main uninstall
`);
  assert.equal(result.status, 27);
  assert.doesNotMatch(result.stdout, /Uninstall complete/);
  assert.equal(f.state().uninstalls?.length, 1);
  assert.equal(readFileSync(f.target, "utf8"), original);
});

test("uninstall shares the deployment lock with updates", requiresCompose, (t) => {
  const f = fixture(t);
  const result = f.functions(`
exec {TEST_LOCK}> "$TEST_DEPLOYMENT/.docker-compose.yml.update.lock"
flock -n "$TEST_LOCK"
bash "$1" uninstall
`);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /Another deployment operation is running/);
  assert.equal(f.state().uninstalls, undefined);
  assert.equal(existsSync(f.target), true);
});

test("deployment directory stays locked when uninstall removes the lock file", requiresCompose, (t) => {
  const f = fixture(t);
  const result = f.functions(`
exec {TEST_DIRECTORY_LOCK}< "$TEST_DEPLOYMENT"
flock -n "$TEST_DIRECTORY_LOCK"
bash "$1" uninstall
`);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /Another deployment operation is running/);
  assert.equal(f.state().uninstalls, undefined);
  assert.equal(existsSync(f.target), true);
});

test("uninstall requires an existing deployment", requiresCompose, (t) => {
  const f = freshDeployment(t);
  const result = f.run(["uninstall"]);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /uninstall requires an existing Compose configuration/);
  assert.equal(f.state().uninstalls, undefined);
});

test("frontend management forwards a fixed version without replacing the deployment", requiresCompose, t => {
  const f = fixture(t);
  const original = readFileSync(f.target, "utf8");
  const result = f.run(["frontend", "install", "--repository", "owner/web", "--version", "v1.2.0"]);
  succeeded(result);
  const call = f.state().calls.find(args => args.includes("exec"));
  assert.deepEqual(call?.slice(call.indexOf("exec")), ["exec", "-T", service, "docker-entrypoint.sh", "./server", "frontend", "install", "--repository", "owner/web", "--version", "v1.2.0"]);
  assert.equal(readFileSync(f.target, "utf8"), original);
  assert.deepEqual(f.state().starts, []);
  assert.deepEqual(f.backups(), []);
  assert.equal(f.state().pulled, false);
});

test("password reset invokes the running server and leaves deployment files untouched", requiresCompose, (t) => {
  const f = fixture(t);
  const original = readFileSync(f.target, "utf8");
  const result = f.run(["reset-password"], {}, "7\n");
  succeeded(result);
  const exec = f.state().calls.find((args) => args.includes("exec"));
  assert.deepEqual(exec?.slice(exec.indexOf("exec")), ["exec", "-T", service, "docker-entrypoint.sh", "./server", "reset-password"]);
  assert.equal(f.state().resetInput, "7");
  assert.equal(result.stdout.match(/New password:/g)?.length, 1);
  assert.equal(readFileSync(f.target, "utf8"), original);
  assert.deepEqual(f.backups(), []);
  assert.deepEqual(f.state().starts, []);
  assert.equal(f.state().pulled, false);
  assert.equal(f.state().calls.some((args) => args.includes("--help") || args[0] === "info" || args[0] === "create"), false);
});

test("password reset preserves custom Compose files", requiresCompose, (t) => {
  const f = fixture(t);
  const renamed = join(f.root, "production.yml");
  renameSync(f.target, renamed);
  writeFileSync(join(f.root, ".env"), "COMPOSE_FILE=production.yml\n");
  const result = f.run(["reset-password"], {}, "7\n");
  succeeded(result);
  const exec = f.state().calls.find((args) => args.includes("exec"))!;
  assert.equal(exec[exec.indexOf("-f") + 1], renamed);
  assert.deepEqual(exec.slice(-4), [service, "docker-entrypoint.sh", "./server", "reset-password"]);
  assert.equal(existsSync(f.target), false);
});

test("password reset skips jq installation, GPU detection and config rendering", requiresCompose, (t) => {
  const f = fixture(t);
  const result = f.functions(`
command() {
  if [[ "$1" == -v && "$2" == jq ]]; then return 1; fi
  builtin command "$@"
}
ensure_jq() { exit 91; }
detect_host_gpu() { exit 92; }
raw_json() { exit 93; }
main reset-password <<< '7'
`);
  succeeded(result);
  assert.ok(f.state().calls.some((args) => args.includes("exec")));
  assert.equal(f.state().calls.some((args) => args.includes("--no-env-resolution")), false);
});

test("password reset forwards piped user selection to the server", requiresCompose, (t) => {
  const f = fixture(t);
  const result = f.functions('printf "7\\n" | bash "$1" reset-password');
  succeeded(result);
  assert.equal(f.state().resetInput, "7");
  assert.match(result.stdout, /User ID to reset/);
  assert.match(result.stdout, /New password: Generated123/);
});

test("password reset keeps interactive TTY selection available", requiresCompose, (t) => {
  if (spawnSync("script", ["--version"]).status !== 0) return t.skip("PTY utility script is unavailable");
  const f = fixture(t);
  const entry = join(f.root, "interactive-reset.sh");
  writeFileSync(entry, 'bash "$TEST_SCRIPT" reset-password\n');
  const result = spawnSync("script", ["-q", "-e", "-c", `bash '${entry}'`, "/dev/null"], {
    cwd: f.root, env: { ...f.env, TEST_SCRIPT: script }, encoding: "utf8", timeout: 15_000, input: "7\n",
  });
  succeeded(result);
  const exec = f.state().calls.find((args) => args.includes("exec"))!;
  assert.equal(exec.includes("-T"), false);
  assert.equal(f.state().resetInput, "7");
});

test("password reset preserves server failure exit codes", requiresCompose, (t) => {
  const f = fixture(t);
  f.changeState({ resetExit: 23 });
  const result = f.run(["reset-password"], {}, "7\n");
  assert.equal(result.status, 23);
  assert.match(result.stderr, /server rejected/);
  assert.deepEqual(f.state().starts, []);
});

test("password reset does not start a stopped website container", requiresCompose, (t) => {
  const f = fixture(t);
  f.changeState({ existingContainer: false });
  const result = f.run(["reset-password"]);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /not running/);
  assert.deepEqual(f.state().starts, []);
  assert.equal(f.state().pulled, false);
});

test("password reset requires an existing deployment", requiresCompose, (t) => {
  const f = freshDeployment(t);
  const result = f.run(["reset-password"]);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /requires an existing Compose configuration/);
  assert.equal(existsSync(f.target), false);
  assert.equal(f.state().calls.some((args) => args.includes("exec")), false);
});

for (const options of [["--image", "example:latest"], ["gpu", "nvidia"], ["telegram"], ["--port", "8181"]]) {
  test(`password reset rejects unrelated deployment option ${options[0]}`, requiresCompose, (t) => {
    const f = fixture(t);
    const result = f.run(["reset-password", ...options]);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /Unknown argument|only supported/);
    assert.deepEqual(f.state().calls, []);
  });
}

test("removed script options are rejected before invoking Docker or installing", requiresCompose, (t) => {
  for (const action of [[], ["install"], ["update"], ["uninstall"], ["reset-password"], ["gpu"], ["telegram"]]) {
    for (const options of [["--user-id", "7"], ["--user-id=7"], ["--config", "/opt/config files/custom.yaml"], ["--config=/opt/config files/custom.yaml"], ["-p", "custom-project"], ["--project-name", "custom-project"], ["--port", "8181"], ["--data-dir", "/new/data"], ["-f", "custom.yml"], ["--wait-timeout", "10"], ["--gpu", "auto"], ["--telegram", "enable"], ["configure"]]) {
      const f = freshDeployment(t);
      const result = f.run([...action, ...options]);
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, /Unknown/);
      assert.equal(existsSync(f.target), false);
      assert.deepEqual(f.state().calls, []);
    }
  }
});

test("Telegram is absent from the default installation", requiresCompose, (t) => {
  const f = freshDeployment(t);
  succeeded(f.run(["install"]));
  const services = f.raw().services;
  assert.equal(services["telegram-bot-api"], undefined);
  assert.equal(services[service].environment.VIDEO_TELEGRAM_COMPOSE, undefined);
});

test("the retired Telegram setup command cannot create containers", requiresCompose, (t) => {
  const f = fixture(t);
  const result = f.run(["telegram"]);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /Unknown command/);
  assert.deepEqual(f.state().calls, []);
});

test("Shell updater has valid Bash syntax and working help", () => {
  succeeded(spawnSync("bash", ["-n", script], { encoding: "utf8" }));
  const help = spawnSync("bash", [script, "--help"], { encoding: "utf8" });
  succeeded(help);
  assert.match(help.stdout, /^Usage: bash 91-docker\.sh /);
  assert.doesNotMatch(readFileSync(script, "utf8"), /command -v python|python3 -/);
});

test("no arguments show help without installing or updating", requiresCompose, (t) => {
  const f = freshDeployment(t);
  const result = f.run();
  succeeded(result);
  assert.match(result.stdout, /^Usage:/);
  assert.equal(existsSync(f.target), false);
  assert.deepEqual(f.state().calls, []);
});

test("each command provides help without contacting Docker", requiresCompose, (t) => {
  for (const action of ["install", "update", "uninstall", "reset-password", "gpu"]) {
    const f = fixture(t);
    const result = f.run([action, "--help"]);
    succeeded(result);
    assert.ok(result.stdout.startsWith(`Usage: bash 91-docker.sh ${action}`));
    assert.deepEqual(f.state().calls, []);
  }
});

test("GPU mode arguments are rejected before contacting Docker", requiresCompose, (t) => {
  for (const mode of ["auto", "cpu", "nvidia", "vaapi", "keep"]) {
    const f = fixture(t);
    const result = f.run(["gpu", mode]);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /Unknown argument for gpu/);
    assert.deepEqual(f.state().calls, []);
  }
});

test("invalid command arguments are rejected before contacting Docker", requiresCompose, (t) => {
  for (const args of [["install", "--image"], ["update", "--image", "--help"], ["gpu", "invalid"], ["gpu", "cpu", "nvidia"], ["telegram", "update"]]) {
    const f = fixture(t);
    const result = f.run(args);
    assert.notEqual(result.status, 0);
    assert.deepEqual(f.state().calls, []);
  }
});

test("GPU configuration requires an existing deployment without triggering installation", requiresCompose, (t) => {
  const f = freshDeployment(t);
  const result = f.run(["gpu"]);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /gpu requires an existing Compose configuration/);
  assert.equal(existsSync(f.target), false);
  assert.equal(f.state().pulled, false);
  assert.deepEqual(f.state().starts, []);
});

test("options without a command cannot trigger deployment changes", requiresCompose, (t) => {
  for (const options of [["--wait-timeout", "10"], ["-f", "docker-compose.yml"], ["--image", "main"], ["configure"]]) {
    const f = fixture(t);
    const result = f.run(options);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /Unknown command/);
    assert.deepEqual(f.state().calls, []);
  }
});

test("deployment commands reject image changes before invoking Docker", requiresCompose, (t) => {
  for (const action of ["gpu", "reset-password", "uninstall"]) {
    const f = fixture(t);
    const result = f.run([action, "--image", "main"]);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /only supported by install and update/);
    assert.deepEqual(f.state().calls, []);
  }
});

test("no detected GPU removes stale GPU access and preserves the running image", requiresCompose, (t) => {
  const f = fixture(t);
  f.changeState({ failPull: true, localImageChanged: true });
  rmSync(join(f.root, "templates/docker-compose.yml"));
  const config = parse(readFileSync(f.target, "utf8"));
  config.services[service].runtime = "nvidia";
  writeFileSync(f.target, JSON.stringify(config));
  const before = f.raw().services[service];
  succeeded(f.run(["gpu"]));
  const after = f.raw().services[service];
  assert.equal(after.runtime, undefined);
  for (const key of ["image", "ports", "volumes", "environment", "networks"]) {
    assert.deepEqual(after[key], before[key]);
  }
  assert.equal(f.state().starts[0].services[service].image, oldImage);
  assert.equal(f.state().calls.some((args) => ["pull", "create", "cp"].includes(args[0])), false);
  assert.ok(f.state().calls.some((args) => args[0] === "info"));
  const up = f.state().calls.find((args) => args.includes("up"))!;
  assert.ok(up.includes("--no-deps"));
  assert.equal(up[up.indexOf("--pull") + 1], "never");
  assert.equal(up.at(-1), service);
});

test("unchanged settings do not rewrite configuration or recreate containers", requiresCompose, (t) => {
  const f = fixture(t);
  const original = readFileSync(f.target, "utf8");
  succeeded(f.run(["gpu"]));
  assert.equal(readFileSync(f.target, "utf8"), original);
  assert.deepEqual(f.backups(), []);
  assert.deepEqual(f.state().starts, []);
  assert.equal(f.state().pulled, false);
});

test("update switches to the requested image and preserves other settings", requiresCompose, (t) => {
  const f = fixture(t);
  const before = f.raw().services[service];
  succeeded(f.run(["update", "--image", "main"]));
  const after = f.raw().services[service];
  assert.equal(after.image, "ghcr.io/nianzhibai/91:main");
  for (const key of ["ports", "volumes", "environment", "deploy", "devices", "networks"]) {
    assert.deepEqual(after[key], before[key]);
  }
  assert.deepEqual(f.state().calls.filter((args) => args[0] === "pull"), [["pull", "ghcr.io/nianzhibai/91:main"]]);
  assert.equal(f.state().calls.some((args) => ["info", "context"].includes(args[0])), false);
});

test("install deploys an empty directory with the image defaults", requiresCompose, (t) => {
  const f = freshDeployment(t);
  succeeded(f.run(["install"]));
  const s = f.raw().services[service];
  assert.equal(s.image, "ghcr.io/nianzhibai/91:stable");
  assert.equal(s.ports[0].published, "9191");
  assert.equal(s.volumes[0].source, "./data");
  assert.equal(s.volumes[0].target, "/opt/91/data");
  assert.equal(s.environment.VIDEO_RESTART_MANAGED, "true");
  assert.equal(s.deploy, undefined);
  assert.deepEqual(f.backups(), []);
  assert.equal(f.state().starts.length, 1);
  assert.equal(statSync(f.target).mode & 0o777, 0o600);
});

for (const tag of ["main", "stable"]) {
  for (const action of ["install", "update"]) {
    test(`${action} expands image tag shorthand ${tag}`, requiresCompose, (t) => {
      const f = action === "install" ? freshDeployment(t) : fixture(t);
      const image = `ghcr.io/nianzhibai/91:${tag}`;
      succeeded(f.run([action, "--image", tag]));
      assert.ok(f.state().calls.some((args) => args[0] === "pull" && args[1] === image));
      assert.equal(f.raw().services[service].image, image);
      assert.equal(f.state().starts[0].services[service].image, image);
    });
  }
}

test("install accepts a full image reference without configuring GPU access", requiresCompose, (t) => {
  const f = freshDeployment(t);
  const data = join(f.root, "data");
  mkdirSync(data);
  const appConfig = join(data, "config.yaml");
  writeFileSync(appConfig, "generation:\n  use_gpu: false\n");
  succeeded(f.run(["install", "--image", "ghcr.io/nianzhibai/91:v9.0.0"], { TEST_READY_GPU: "nvidia" }));
  const s = f.raw().services[service];
  assert.equal(s.image, "ghcr.io/nianzhibai/91:v9.0.0");
  assert.equal(s.ports[0].published, "9191");
  assert.equal(s.volumes[0].source, "./data");
  assert.equal(s.deploy, undefined);
  assert.equal(f.state().calls.some((args) => args[0] === "info"), false);
  assert.equal(readFileSync(appConfig, "utf8"), "generation:\n  use_gpu: false\n");
  assert.deepEqual(f.backups(), []);
});

test("GPU configuration and updates preserve Compose port and storage settings", requiresCompose, (t) => {
  const f = fixture(t);
  const config = parse(readFileSync(f.target, "utf8"));
  config.services[service].ports = ["8182:9191"];
  config.services[service].volumes = ["./private data:/opt/91/data"];
  writeFileSync(f.target, JSON.stringify(config));
  succeeded(f.run(["gpu"], { TEST_READY_GPU: "vaapi" }));
  const before = f.raw().services[service];
  assert.equal(before.devices[0].source, vaapiDevice);
  assert.equal(before.ports[0].published, "8182");
  assert.equal(before.volumes[0].source, "./private data");
  succeeded(f.run(["update"], { TEST_READY_GPU: "vaapi" }));
  const after = f.raw().services[service];
  assert.deepEqual(after.ports, before.ports);
  assert.deepEqual(after.volumes, before.volumes);
  assert.deepEqual(after.devices, before.devices);
  assert.equal(f.backups().length, 2);
});

test("explicit install refuses to overwrite an existing deployment", requiresCompose, (t) => {
  const f = fixture(t);
  const original = readFileSync(f.target, "utf8");
  const result = f.run(["install"]);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /already exists/);
  assert.equal(readFileSync(f.target, "utf8"), original);
  assert.equal(f.state().pulled, false);
});

test("explicit update requires an existing Compose file", requiresCompose, (t) => {
  const f = freshDeployment(t);
  const result = f.run(["update"]);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /No Compose deployment found/);
  assert.equal(existsSync(f.target), false);
  assert.equal(f.state().pulled, false);
});

test("first deployment honors COMPOSE_FILE and project name from dotenv", requiresCompose, (t) => {
  const f = freshDeployment(t);
  writeFileSync(join(f.root, ".env"), "COMPOSE_FILE=production.yml\nCOMPOSE_PROJECT_NAME=new-project\n");
  succeeded(f.run(["install"]));
  assert.equal(existsSync(f.target), false);
  assert.equal(existsSync(join(f.root, "production.yml")), true);
  assert.equal(f.state().starts[0].name, "new-project");
});

test("first deployment supports a custom filename and preserves existing overlays", requiresCompose, (t) => {
  const f = freshDeployment(t);
  const target = join(f.root, "application.yml");
  const overlay = join(f.root, "custom.yml");
  const original = "name: new-project\nservices:\n  \"91\":\n    ports: !override [\"8183:9191\"]\n";
  writeFileSync(overlay, original);
  writeFileSync(join(f.root, ".env"), "COMPOSE_FILE=application.yml:custom.yml\n");
  succeeded(f.run(["install"]));
  assert.equal(existsSync(target), true);
  assert.equal(existsSync(f.target), false);
  assert.equal(readFileSync(overlay, "utf8"), original);
  assert.equal(f.state().starts[0].services[service].ports[0].published, "8183");
  assert.equal(f.state().starts[0].name, "new-project");
});

test("first deployment rejects service containers whose original configuration is missing", requiresCompose, (t) => {
  const f = freshDeployment(t);
  f.changeState({ existingContainer: true });
  const result = f.run(["install"]);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /restore their original Compose configuration/);
  assert.equal(existsSync(f.target), false);
  assert.deepEqual(f.state().starts, []);
});

test("a configuration created during first deployment is never overwritten", requiresCompose, (t) => {
  const f = freshDeployment(t);
  f.changeState({ createConfigDuringPull: true });
  const result = f.run(["install"]);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /appeared during installation/);
  assert.equal(readFileSync(f.target, "utf8"), "# created by another process\nservices: {}\n");
  assert.deepEqual(f.state().starts, []);
});

test("failed first startup keeps generated configuration and data for a retry", requiresCompose, (t) => {
  const f = freshDeployment(t);
  const data = join(f.root, "data");
  mkdirSync(data);
  writeFileSync(join(data, "keep.txt"), "keep existing data");
  f.changeState({ failStart: true });
  const result = f.run(["install"]);
  assert.notEqual(result.status, 0);
  assert.match(result.stdout, /configuration and application data have been kept/);
  assert.equal(existsSync(f.target), true);
  assert.equal(readFileSync(join(data, "keep.txt"), "utf8"), "keep existing data");
  assert.equal(f.state().starts.length, 1, "A new install has no previous image to restore");
  assert.deepEqual(f.backups(), []);
  f.changeState({ failStart: false, existingContainer: true });
  succeeded(f.run(["update"]));
  assert.equal(f.state().starts.length, 2);
});

for (const failure of ["pull", "template"] as const) {
  test(`first deployment ${failure} failure leaves the target absent`, requiresCompose, (t) => {
    const f = freshDeployment(t);
    if (failure === "pull") f.changeState({ failPull: true });
    else rmSync(join(f.root, "templates/docker-compose.yml"));
    const result = f.run(["install"]);
    assert.notEqual(result.status, 0);
    assert.equal(existsSync(f.target), false);
    assert.deepEqual(f.state().starts, []);
    assert.deepEqual(f.backups(), []);
  });
}

test("failed GPU changes restore the original settings and image without pulling", requiresCompose, (t) => {
  const f = fixture(t);
  const config = parse(readFileSync(f.target, "utf8"));
  config.services[service].runtime = "nvidia";
  writeFileSync(f.target, JSON.stringify(config));
  const original = readFileSync(f.target, "utf8");
  f.changeState({ failStart: true });
  const result = f.run(["gpu"]);
  assert.notEqual(result.status, 0);
  assert.equal(readFileSync(f.target, "utf8"), original);
  assert.equal(f.state().starts.length, 2);
  assert.equal(f.state().starts[0].services[service].runtime, undefined);
  assert.equal(f.state().starts[1].services[service].runtime, "nvidia");
  assert.equal(f.state().starts[1].services[service].image, oldImage);
  assert.equal(f.state().pulled, false);
});

function jqInstallation(t: TestContext) {
  const f = fixture(t);
  const bin = join(f.root, "bin");
  const packageLog = join(f.root, "packages.jsonl");
  const realJq = spawnSync("bash", ["-c", "command -v jq"], { encoding: "utf8" }).stdout.trim();
  const installer = `#!${process.execPath}
const { appendFileSync, writeFileSync } = require("node:fs");
const { basename, join } = require("node:path");
const { spawnSync } = require("node:child_process");
const manager = basename(process.argv[1]);
const args = process.argv.slice(2);
appendFileSync(process.env.TEST_PACKAGE_LOG, JSON.stringify({manager, args, frontend: process.env.DEBIAN_FRONTEND}) + "\\n");
if (manager === "sudo") {
  if (process.env.TEST_SUDO_FAIL === "1") process.exit(1);
  const child = spawnSync(args[0], args.slice(1), { stdio: "inherit" });
  process.exit(child.status ?? 1);
}
const stage = args.includes("update") ? "update" : "install";
if (process.env.TEST_INSTALL_FAIL === stage) process.exit(1);
if (stage === "install" && process.env.TEST_INSTALL_NO_JQ !== "1") {
  const executable = process.env.TEST_INSTALL_BROKEN_JQ === "1" ? "exit 1" : 'exec "$TEST_REAL_JQ" "$@"';
  writeFileSync(join(process.env.TEST_DEPLOYMENT, "bin/jq"), "#!/bin/sh\\n" + executable + "\\n", {mode: 0o755});
}
`;
  // Even an incorrect package-manager choice must never reach a host installer.
  for (const name of ["apt-get", "dnf", "yum", "apk", "zypper", "pacman", "sudo"]) {
    writeFileSync(join(bin, name), installer, { mode: 0o755 });
  }
  const setup = `
command() {
  if [[ "$1" == -v ]]; then
    case "$2" in
      jq) [[ -x "$TEST_DEPLOYMENT/bin/jq" ]] || return 1 ;;
      apt-get|dnf|yum|apk|zypper|pacman) [[ "$2" == "$TEST_PACKAGE_MANAGER" ]]; return ;;
      sudo) [[ "$TEST_HAVE_SUDO" == 1 ]]; return ;;
    esac
  fi
  builtin command "$@"
}
id() { printf '%s\\n' "$TEST_INSTALL_UID"; }
`;
  return {
    ...f,
    calls: () => existsSync(packageLog) ? readFileSync(packageLog, "utf8").trim().split("\n").map((line) => JSON.parse(line)) : [],
    installed: () => existsSync(join(bin, "jq")),
    bootstrap(overrides: Record<string, string> = {}, fullUpdate = false) {
      return f.functions(`${setup}\n${fullUpdate ? 'detect_host_gpu() { GPU_CHOICES="[]"; }; main update' : 'ensure_jq; printf "bootstrap complete\\n"'}`, {
        TEST_PACKAGE_LOG: packageLog,
        TEST_PACKAGE_MANAGER: "apt-get",
        TEST_REAL_JQ: realJq,
        TEST_INSTALL_UID: "0",
        TEST_HAVE_SUDO: "1",
        ...overrides,
      });
    },
  };
}

for (const [manager, args] of [
  ["apt-get", ["install", "-y", "--no-install-recommends", "jq"]],
  ["dnf", ["install", "-y", "jq"]],
  ["yum", ["install", "-y", "jq"]],
  ["apk", ["add", "--no-cache", "jq"]],
  ["zypper", ["--non-interactive", "install", "jq"]],
  ["pacman", ["-S", "--needed", "--noconfirm", "jq"]],
] as const) {
  test(`missing jq is installed automatically with ${manager}`, requiresCompose, (t) => {
    const f = jqInstallation(t);
    const result = f.bootstrap({ TEST_PACKAGE_MANAGER: manager });
    succeeded(result);
    assert.equal(f.installed(), true);
    const calls = f.calls();
    assert.deepEqual(calls.at(-1).args, [...args]);
    assert.equal(calls.every((call) => call.manager === manager), true);
    if (manager === "apt-get") {
      assert.deepEqual(calls[0].args, ["update"]);
      assert.equal(calls.every((call) => call.frontend === "noninteractive"), true);
    }
    assert.match(result.stdout, /jq installed successfully/);
  });
}

test("existing jq skips package installation entirely", requiresCompose, (t) => {
  const f = jqInstallation(t);
  succeeded(f.bootstrap());
  const previous = f.calls();
  succeeded(f.bootstrap({ TEST_PACKAGE_MANAGER: "none", TEST_INSTALL_UID: "1000", TEST_HAVE_SUDO: "0" }));
  assert.deepEqual(f.calls(), previous);
});

test("non-root jq installation uses sudo only for package commands", requiresCompose, (t) => {
  const f = jqInstallation(t);
  succeeded(f.bootstrap({ TEST_INSTALL_UID: "1000" }));
  const calls = f.calls();
  assert.equal(calls.filter((call) => call.manager === "sudo").length, 2);
  assert.equal(calls.filter((call) => call.manager === "apt-get").length, 2);
  assert.equal(f.installed(), true);
});

for (const [scenario, settings, expected] of [
  ["package list failure", { TEST_INSTALL_FAIL: "update" }, /refresh package lists/],
  ["install failure", { TEST_INSTALL_FAIL: "install" }, /Automatic jq installation failed/],
  ["no supported manager", { TEST_PACKAGE_MANAGER: "none" }, /no supported package manager/],
  ["no sudo privileges", { TEST_INSTALL_UID: "1000", TEST_HAVE_SUDO: "0" }, /requires root privileges/],
  ["sudo rejection", { TEST_INSTALL_UID: "1000", TEST_SUDO_FAIL: "1" }, /refresh package lists/],
  ["installer produces no jq", { TEST_INSTALL_NO_JQ: "1" }, /jq is still unavailable/],
  ["installed jq cannot run", { TEST_INSTALL_BROKEN_JQ: "1" }, /jq is still unavailable/],
] as const) {
  test(`${scenario} stops before deployment changes`, requiresCompose, (t) => {
    const f = jqInstallation(t);
    const original = readFileSync(f.target, "utf8");
    const result = f.bootstrap(settings, true);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, expected);
    assert.equal(readFileSync(f.target, "utf8"), original);
    assert.deepEqual(f.backups(), []);
    assert.equal(f.state().pulled, false);
    assert.deepEqual(f.state().starts, []);
    if (scenario === "package list failure") {
      assert.deepEqual(f.calls().map((call) => call.args), [["update"]]);
    }
  });
}

test("Docker update continues after automatic jq installation", requiresCompose, (t) => {
  const f = jqInstallation(t);
  succeeded(f.bootstrap({}, true));
  assert.equal(f.installed(), true);
  assert.equal(f.state().starts.length, 1);
  assert.equal(f.backups().length, 1);
});

test("upgrade preserves custom settings, variable references, env files and other services", requiresCompose, (t) => {
  const f = fixture(t);
  writeFileSync(f.target, `name: \${STACK_NAME:-private-stack}
services:
  "91":
    image: \${VIDEO_IMAGE:-ghcr.io/nianzhibai/91:stable}
    container_name: customized-name
    ports: ["127.0.0.1:\${WEB_PORT:-8181}:9191"]
    volumes: ["./private data:/opt/91/data", "cache:/cache"]
    env_file: ./service.env
    environment: {CUSTOM: "\${LOCAL_VALUE:-keep}", ESCAPED: "$$TOKEN"}
    labels: {custom: keep}
    networks: [private]
    restart: always
  sidecar:
    image: busybox:1
    profiles: [maintenance]
    command: [sleep, infinity]
networks:
  private: {}
volumes:
  cache: {}
`);
  writeFileSync(join(f.root, "service.env"), "PRIVATE_SECRET=not-for-compose\n");
  const before = f.raw();
  const template = join(f.root, "templates/docker-compose.yml");
  writeFileSync(template, readFileSync(template, "utf8") + "    labels:\n      new-default: new-value\n");
  succeeded(f.run(["update"]));
  const after = f.raw();
  for (const key of ["image", "ports", "volumes", "env_file", "networks", "container_name", "restart"]) {
    assert.deepEqual(after.services[service][key], before.services[service][key]);
  }
  assert.deepEqual(after.services.sidecar, before.services.sidecar);
  assert.deepEqual(after.services[service].labels, { custom: "keep", "new-default": "new-value" });
  const text = readFileSync(f.target, "utf8");
  assert.match(text, /\$\{WEB_PORT:-8181\}/);
  assert.match(text, /\$\$TOKEN/);
  assert.doesNotMatch(text, /not-for-compose/);
});

test("NVIDIA preparation updates one Compose file and preserves the app GPU switch", requiresCompose, (t) => {
  const f = fixture(t);
  const original = readFileSync(f.target, "utf8");
  mkdirSync(join(f.root, "data"));
  const config = join(f.root, "data/config.yaml");
  writeFileSync(config, "generation:\n  use_gpu: false\n");
  chmodSync(f.target, 0o600);
  succeeded(f.run(["gpu"], { TEST_READY_GPU: "nvidia" }));
  const s = f.raw().services[service];
  assert.equal(s.environment.NVIDIA_DRIVER_CAPABILITIES, "compute,video,utility");
  assert.equal(s.deploy.resources.reservations.devices[0].driver, "nvidia");
  assert.equal(readFileSync(config, "utf8"), "generation:\n  use_gpu: false\n");
  assert.equal(f.backups().length, 1);
  assert.equal(readFileSync(join(f.root, f.backups()[0]), "utf8"), original);
  assert.equal(statSync(f.target).mode & 0o777, 0o600);
  assert.equal(statSync(join(f.root, f.backups()[0])).mode & 0o777, 0o600);
  assert.equal(existsSync(join(f.root, "docker-compose.override.yml")), false);
  assert.ok(f.state().calls.some((args) => args[0] === "rm" && args.includes("-v")));
  assert.equal(f.state().pulled, false);
  assert.equal(f.state().starts[0].services[service].image, oldImage);
  assert.equal(f.state().calls.find((args) => args[0] === "create")?.at(-1), oldImage);
});

const secondVaapiGPU: GPUChoice = { mode: "vaapi", device: "/dev/dri/renderD129", label: "AMD GPU" };
const secondNvidiaGPU: GPUChoice = { mode: "nvidia", device: "GPU-22222222-2222-2222-2222-222222222222", label: "NVIDIA RTX 4090" };

for (const [name, choices] of [
  ["NVIDIA over VAAPI", [defaultGPUs.vaapi, defaultGPUs.nvidia]],
  ["VAAPI over NVIDIA", [defaultGPUs.nvidia, defaultGPUs.vaapi]],
  ["one of multiple VAAPI GPUs", [defaultGPUs.vaapi, secondVaapiGPU]],
  ["one of multiple NVIDIA GPUs", [defaultGPUs.nvidia, secondNvidiaGPU]],
] as const) {
  test(`GPU selection lets users choose ${name} and exposes only that card`, requiresCompose, (t) => {
    const f = fixture(t);
    const config = parse(readFileSync(f.target, "utf8"));
    Object.assign(config.services[service], {
      devices: ["/dev/fuse:/dev/fuse", "/dev/dri:/dev/dri", "/dev/nvidia0:/dev/nvidia0", "/tmp/host-render:/dev/dri/renderD130"],
      runtime: "nvidia",
      gpus: "all",
      environment: { ...config.services[service].environment, NVIDIA_VISIBLE_DEVICES: "all", CUDA_VISIBLE_DEVICES: "2" },
    });
    writeFileSync(f.target, JSON.stringify(config));
    const result = f.run(["gpu"], { TEST_GPU_CHOICES: JSON.stringify(choices) }, "2\n");
    succeeded(result);
    const selected = choices[1];
    for (const choice of choices) assert.ok(result.stderr.includes(choice.device));
    assert.ok(result.stdout.includes(selected.device));
    const s = f.raw().services[service];
    assert.equal(s.runtime, undefined);
    assert.equal(s.gpus, undefined);
    assert.equal(s.environment?.NVIDIA_VISIBLE_DEVICES, undefined);
    assert.equal(s.environment?.CUDA_VISIBLE_DEVICES, undefined);
    assert.deepEqual(s.devices, [
      { source: "/dev/fuse", target: "/dev/fuse", permissions: "rwm" },
      ...(selected.mode === "vaapi" ? [{ source: selected.device, target: selected.device, permissions: "rwm" }] : []),
    ]);
    if (selected.mode === "nvidia") {
      assert.deepEqual(s.deploy.resources.reservations.devices, [{ driver: "nvidia", device_ids: [selected.device], capabilities: ["gpu"] }]);
    } else {
      assert.equal(s.deploy, undefined);
    }
    assert.equal(f.state().starts[0].services[service].image, oldImage);
    assert.equal(f.state().pulled, false);
  });
}

test("GPU selection retries invalid input without silently choosing a default", requiresCompose, (t) => {
  const f = fixture(t);
  const result = f.run(["gpu"], { TEST_READY_GPU: "vaapi nvidia" }, "\n0\ninvalid\n999999999999999999999999999999\n2\n");
  succeeded(result);
  assert.equal(result.stderr.match(/Enter a number/g)?.length, 4);
  assert.deepEqual(f.raw().services[service].deploy.resources.reservations.devices[0].device_ids, [nvidiaDevice]);
});

test("GPU selection works in an interactive terminal", requiresCompose, (t) => {
  if (spawnSync("script", ["--version"]).status !== 0) return t.skip("PTY utility script is unavailable");
  const f = fixture(t);
  const entry = join(f.root, "interactive-gpu.sh");
  writeFileSync(entry, 'source "$TEST_SCRIPT"\ndetect_host_gpu() { GPU_CHOICES="$TEST_GPU_CHOICES"; }\nmain gpu\n');
  const result = spawnSync("script", ["-q", "-e", "-c", `bash '${entry}'`, "/dev/null"], {
    cwd: f.root,
    env: { ...f.env, TEST_SCRIPT: script, TEST_GPU_CHOICES: JSON.stringify([defaultGPUs.vaapi, defaultGPUs.nvidia]) },
    encoding: "utf8", timeout: 15_000, input: "2\n",
  });
  succeeded(result);
  assert.match(result.stdout, /Available GPUs/);
  assert.match(result.stdout, /Select GPU/);
  assert.deepEqual(f.raw().services[service].deploy.resources.reservations.devices[0].device_ids, [nvidiaDevice]);
});

for (const input of [undefined, "q\n", "invalid\n"]) {
  test(`GPU selection aborts without deployment changes on ${input === undefined ? "EOF" : JSON.stringify(input)}`, requiresCompose, (t) => {
    const f = fixture(t);
    const original = readFileSync(f.target, "utf8");
    const result = f.run(["gpu"], { TEST_READY_GPU: "vaapi nvidia" }, input);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /GPU selection cancelled before deployment changes/);
    assert.equal(readFileSync(f.target, "utf8"), original);
    assert.deepEqual(f.backups(), []);
    assert.deepEqual(f.state().starts, []);
    assert.equal(f.state().calls.some((args) => ["pull", "create", "cp"].includes(args[0])), false);
  });
}

test("Ctrl+C at GPU selection preserves configuration and removes temporary files", requiresCompose, (t) => {
  const f = fixture(t);
  const original = readFileSync(f.target, "utf8");
  const result = f.functions(`
detect_host_gpu() {
  add_gpu_choice vaapi /dev/dri/renderD128 Intel
  add_gpu_choice nvidia ${nvidiaDevice} NVIDIA
}
read() {
  if [[ "$#" == 2 && "$1" == -r && "$2" == choice ]]; then kill -INT "$$"; return 130; fi
  builtin read "$@"
}
main gpu
`);
  assert.equal(result.status, 130, `${result.stdout}\n${result.stderr}`);
  assert.equal(readFileSync(f.target, "utf8"), original);
  assert.deepEqual(f.backups(), []);
  assert.deepEqual(f.state().starts, []);
  assert.equal(readdirSync(f.root).some((name) => name.startsWith(".docker-update.")), false);
});

for (const [previous, available, selected] of [
  ["nvidia", "vaapi nvidia", "vaapi"],
  ["nvidia", "vaapi", "vaapi"],
  ["vaapi", "nvidia", "nvidia"],
] as const) {
  test(`GPU detection replaces existing ${previous} access with ${selected} when ${available} is available`, requiresCompose, (t) => {
    const f = fixture(t);
    const config = parse(readFileSync(f.target, "utf8"));
    const site = config.services[service];
    site.devices = ["/dev/fuse:/dev/fuse"];
    if (previous === "nvidia") {
      site.runtime = "nvidia";
      site.environment.NVIDIA_VISIBLE_DEVICES = "all";
      site.environment.NVIDIA_DRIVER_CAPABILITIES = "utility";
    } else {
      site.devices.push("/dev/dri:/dev/dri");
    }
    writeFileSync(f.target, JSON.stringify(config));
    succeeded(f.run(["gpu"], { TEST_READY_GPU: available }, available.includes(" ") ? "1\n" : undefined));
    const after = f.raw().services[service];
    assert.deepEqual(after.devices.map((device: any) => device.source).sort(),
      selected === "vaapi" ? [vaapiDevice, "/dev/fuse"] : ["/dev/fuse"]);
    assert.equal(after.runtime, undefined);
    assert.equal(after.environment.NVIDIA_VISIBLE_DEVICES, undefined);
    if (selected === "nvidia") {
      assert.equal(after.deploy.resources.reservations.devices[0].driver, "nvidia");
      assert.equal(after.environment.NVIDIA_DRIVER_CAPABILITIES, "compute,video,utility");
    } else {
      assert.equal(after.environment.NVIDIA_DRIVER_CAPABILITIES, undefined);
    }
    assert.equal(f.state().starts[0].services[service].image, oldImage);
    assert.equal(f.state().pulled, false);
  });
}

for (const detected of ["nvidia", "vaapi"]) {
  test(`repeated GPU detection leaves matching ${detected} configuration and containers untouched`, requiresCompose, (t) => {
    const f = fixture(t);
    succeeded(f.run(["gpu"], { TEST_READY_GPU: detected }));
    const original = readFileSync(f.target, "utf8");
    const backups = f.backups();
    const starts = f.state().starts;
    succeeded(f.run(["gpu"], { TEST_READY_GPU: detected }));
    assert.equal(readFileSync(f.target, "utf8"), original);
    assert.deepEqual(f.backups(), backups);
    assert.deepEqual(f.state().starts, starts);
  });
}

test("NVIDIA selection replaces stale device IDs and configures missing capabilities", requiresCompose, (t) => {
  const f = fixture(t);
  const base = parse(readFileSync(f.target, "utf8"));
  base.services[service].deploy = { resources: { reservations: { devices: [{ driver: "nvidia", device_ids: ["2"], capabilities: ["gpu"] }] } } };
  writeFileSync(f.target, JSON.stringify(base));
  succeeded(f.run(["gpu"], { TEST_READY_GPU: "nvidia" }));
  assert.deepEqual(f.raw().services[service].deploy.resources.reservations.devices[0].device_ids, [nvidiaDevice]);
  assert.equal(f.raw().services[service].environment.NVIDIA_DRIVER_CAPABILITIES, "compute,video,utility");
});

test("NVIDIA selection preserves explicitly enabled driver capabilities", requiresCompose, (t) => {
  const f = fixture(t);
  const config = parse(readFileSync(f.target, "utf8"));
  config.services[service].environment.NVIDIA_DRIVER_CAPABILITIES = "all";
  writeFileSync(f.target, JSON.stringify(config));
  succeeded(f.run(["gpu"], { TEST_READY_GPU: "nvidia" }));
  assert.equal(f.raw().services[service].environment.NVIDIA_DRIVER_CAPABILITIES, "all");
});

test("host networking does not gain deliberately omitted ports or mounts", requiresCompose, (t) => {
  const f = fixture(t);
  writeFileSync(f.target, "services:\n  \"91\":\n    image: ghcr.io/nianzhibai/91:stable\n    network_mode: host\n");
  succeeded(f.run(["update"]));
  const s = f.raw().services[service];
  assert.equal(s.network_mode, "host");
  assert.equal(s.ports, undefined);
  assert.equal(s.volumes, undefined);
});

test("null deployment and environment lists retain their meaning during GPU preparation", requiresCompose, (t) => {
  const f = fixture(t);
  writeFileSync(f.target, `services:
  "91":
    image: ghcr.io/nianzhibai/91:stable
    deploy: null
    environment:
      - PRIVATE_FROM_HOST
      - CUSTOM=\${CUSTOM_VALUE:-keep}
      - LITERAL=$$TOKEN
`);
  succeeded(f.run(["gpu"], { TEST_READY_GPU: "nvidia", PRIVATE_FROM_HOST: "do-not-write-this-secret" }));
  assert.equal(f.raw().services[service].deploy.resources.reservations.devices[0].driver, "nvidia");
  const text = readFileSync(f.target, "utf8");
  assert.match(text, /PRIVATE_FROM_HOST/);
  assert.match(text, /\$\{CUSTOM_VALUE:-keep\}/);
  assert.match(text, /\$\$TOKEN/);
  assert.doesNotMatch(text, /do-not-write-this-secret/);
});

for (const scenario of ["pull failure", "missing templates", "invalid template", "concurrent edit"] as const) {
  test(`${scenario} leaves the existing configuration intact`, requiresCompose, (t) => {
    const f = fixture(t);
    const before = readFileSync(f.target, "utf8");
    if (scenario === "pull failure") f.changeState({ failPull: true });
    if (scenario === "missing templates") rmSync(join(f.root, "templates/docker-compose.yml"));
    if (scenario === "invalid template") writeFileSync(join(f.root, "templates/docker-compose.yml"), "services: [bad]\n");
    if (scenario === "concurrent edit") f.changeState({ changeDuringPull: true });
    const result = f.run(["update"]);
    assert.notEqual(result.status, 0);
    assert.equal(readFileSync(f.target, "utf8"), scenario === "concurrent edit" ? `# concurrent edit\n${before}` : before);
    assert.deepEqual(f.backups(), []);
    assert.deepEqual(f.state().starts, []);
    if (scenario === "missing templates" || scenario === "invalid template") {
      assert.ok(f.state().calls.some((args) => args[0] === "rm" && args.includes("-v")));
    }
  });
}

for (const failRollback of [false, true]) {
  test(`startup failure restores original bytes and previous image (rollback fails: ${failRollback})`, requiresCompose, (t) => {
    const f = fixture(t);
    const original = readFileSync(f.target, "utf8");
    f.changeState({ failStart: true, failRollback });
    const result = f.run(["update"], { TEST_READY_GPU: "nvidia" });
    assert.notEqual(result.status, 0);
    assert.equal(readFileSync(f.target, "utf8"), original);
    const starts = f.state().starts;
    assert.equal(starts.length, 2);
    assert.equal(starts[0].services[service].image, "ghcr.io/nianzhibai/91:stable");
    assert.equal(starts[1].services[service].image, oldImage);
    assert.equal(starts[1].services[service].deploy, undefined);
    if (failRollback) assert.match(result.stdout, /Container rollback failed/);
  });
}

test("default overlay discovery preserves the existing overlay", requiresCompose, (t) => {
  const f = fixture(t);
  const overlay = join(f.root, "docker-compose.override.yml");
  writeFileSync(overlay, "services:\n  \"91\":\n    ports: !override [\"8181:9191\"]\n    environment:\n      CUSTOM_FROM_OVERLAY: keep\n");
  const before = readFileSync(overlay, "utf8");
  succeeded(f.run(["update"]));
  assert.equal(f.state().starts[0].services[service].ports[0].published, "8181");
  assert.equal(f.state().starts[0].services[service].environment.CUSTOM_FROM_OVERLAY, "keep");
  assert.equal(readFileSync(overlay, "utf8"), before);
});

const requiresMigration = { skip: requiresCompose.skip || (spawnSync("python3", ["-c", "import yaml"]).status !== 0 && "Python PyYAML is required") };

function legacyDeployment(t: TestContext, container = "video-site-91") {
  const f = fixture(t);
  const config = parse(readFileSync(f.target, "utf8"));
  const app = config.services[service];
  delete config.services[service];
  app.container_name = container;
  app.ports = ["8181:9191"];
  app.volumes = ["./private data:/opt/video-site-91/data"];
  config.services["video-site-91"] = app;
  config.services.sidecar = { image: "busybox", depends_on: { "video-site-91": { condition: "service_started" } } };
  writeFileSync(f.target, JSON.stringify(config));
  mkdirSync(join(f.root, "private data"));
  writeFileSync(join(f.root, "private data/video-site.db"), "original database");
  writeFileSync(join(f.root, "private data/config.yaml"), "storage: {data_dir: /opt/video-site-91/data}\n");
  return f;
}

for (const failStart of [false, true]) {
  test(`script self-update continues into legacy migration (startup failure: ${failStart})`, requiresMigration, (t) => {
    const f = selfUpdatingManager(t, legacyDeployment(t));
    const before = readFileSync(f.target, "utf8");
    f.changeState({ failStart });
    const result = f.runManager(["update"]);
    assert.equal(readFileSync(f.manager, "utf8"), f.updated);
    assert.equal(f.state().calls.filter(args => args[0] === "script-download").length, 1);
    assert.equal(readFileSync(join(f.root, "private data/video-site.db"), "utf8"), "original database");
    if (failStart) {
      assert.notEqual(result.status, 0);
      assert.equal(readFileSync(f.target, "utf8"), before);
      assert.equal(f.state().starts[1].services["video-site-91"].image, oldImage);
    } else {
      succeeded(result);
      assert.equal(f.state().starts[0].services[service].volumes[0].target, "/opt/video-site-91/data");
    }
  });
}

for (const container of ["video-site-91", "custom-container"]) {
  test(`legacy update renames the service and keeps data mounts (container: ${container})`, requiresMigration, (t) => {
    const f = legacyDeployment(t, container);
    succeeded(f.run(["update"]));
    const state = f.state();
    const app = state.starts[0].services[service];
    assert.equal(app.container_name, container === "video-site-91" ? "91" : container);
    assert.equal(state.starts[0].services["video-site-91"], undefined);
    assert.equal(app.volumes[0].target, "/opt/video-site-91/data");
    assert.equal(app.volumes[0].source, join(f.root, "private data"));
    assert.equal(app.ports[0].published, "8181");
    assert.ok(app.networks.default.aliases.includes("video-site-91"));
    assert.equal(state.starts[0].services.sidecar.depends_on[service].condition, "service_started");
    assert.equal(readFileSync(join(f.root, "private data/video-site.db"), "utf8"), "original database");
    const stop = state.calls.findIndex(args => args.includes("stop") && args.at(-1) === "video-site-91");
    const remove = state.calls.findIndex(args => args.includes("rm") && args.at(-1) === "video-site-91");
    const start = state.calls.findIndex(args => args.includes("up"));
    assert.ok(stop >= 0 && remove > stop && start > remove);
    assert.ok(!state.calls[remove].includes("-v"));
    succeeded(f.run(["update"]));
    assert.equal(f.state().starts.at(-1).services[service].volumes[0].target, "/opt/video-site-91/data");
  });
}

test("legacy migration updates overlays without losing override tags or variable references", requiresMigration, (t) => {
  const f = legacyDeployment(t);
  const overlay = join(f.root, "docker-compose.override.yml");
  writeFileSync(overlay, 'services:\n  video-site-91:\n    ports: !override ["8282:9191"]\n    environment: {CUSTOM: "${LOCAL_VALUE:-keep}"}\n');
  succeeded(f.run(["update"]));
  const text = readFileSync(overlay, "utf8");
  assert.match(text, /!override/);
  assert.match(text, /\$\{LOCAL_VALUE:-keep\}/);
  assert.doesNotMatch(text, /video-site-91:/);
  assert.equal(f.state().starts[0].services[service].ports[0].published, "8282");
  assert.equal(f.state().starts[0].services[service].environment.CUSTOM, "keep");
  assert.equal(f.backups().length, 2);
});

test("failed legacy migration restores every Compose file and the previous service image", requiresMigration, (t) => {
  const f = legacyDeployment(t);
  const overlay = join(f.root, "docker-compose.override.yml");
  writeFileSync(overlay, 'services:\n  video-site-91:\n    ports: !override ["8282:9191"]\n');
  const before = [readFileSync(f.target, "utf8"), readFileSync(overlay, "utf8")];
  f.changeState({ failStart: true });
  const result = f.run(["update"]);
  assert.notEqual(result.status, 0);
  assert.equal(readFileSync(f.target, "utf8"), before[0]);
  assert.equal(readFileSync(overlay, "utf8"), before[1]);
  const starts = f.state().starts;
  assert.equal(starts.length, 2, result.stdout + result.stderr);
  assert.equal(starts[0].services[service].container_name, "91");
  assert.equal(starts[1].services["video-site-91"].image, oldImage);
  assert.equal(starts[1].services["video-site-91"].volumes[0].target, "/opt/video-site-91/data");
});

test("a failed database filename upgrade keeps the new deployment stopped instead of starting an incompatible image", requiresMigration, (t) => {
  const f = legacyDeployment(t);
  f.changeState({ failStart: true, oldDatabaseFilename: "<no value>", newDatabaseFilename: "91.db" });
  const result = f.run(["update"]);
  assert.notEqual(result.status, 0);
  assert.match(result.stdout, /Automatic image rollback skipped/);
  assert.equal(f.state().starts.length, 1);
  assert.ok(f.raw().services[service]);
  assert.equal(f.raw().services["video-site-91"], undefined);
  assert.ok(f.state().calls.some(args => args.includes("stop") && args.at(-1) === service));
  assert.equal(readFileSync(join(f.root, "private data/video-site.db"), "utf8"), "original database");
  f.changeState({ failStart: false, oldDatabaseFilename: "91.db" });
  succeeded(f.run(["update"]));
});

test("legacy migration rejects anonymous data before stopping the old container", requiresCompose, (t) => {
  const f = legacyDeployment(t);
  const config = parse(readFileSync(f.target, "utf8"));
  config.services["video-site-91"].volumes = ["/opt/video-site-91/data"];
  writeFileSync(f.target, JSON.stringify(config));
  const result = f.run(["update"]);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /explicit application data mount/);
  assert.deepEqual(f.state().starts, []);
  assert.equal(f.state().calls.some(args => args.includes("stop")), false);
});

test("legacy migration rejects simultaneous old and new services", requiresCompose, (t) => {
  const f = legacyDeployment(t);
  const config = parse(readFileSync(f.target, "utf8"));
  config.services[service] = { image: "busybox" };
  writeFileSync(f.target, JSON.stringify(config));
  const result = f.run(["update"]);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /Both legacy and current services exist/);
  assert.equal(f.state().pulled, false);
  assert.deepEqual(f.backups(), []);
});

test("legacy migration rejects old target images before changing the deployment", requiresCompose, (t) => {
  const f = legacyDeployment(t);
  const before = readFileSync(f.target, "utf8");
  f.changeState({ missingCurrentTemplates: true });
  const result = f.run(["update"]);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /no 91 deployment templates/);
  assert.equal(readFileSync(f.target, "utf8"), before);
  assert.deepEqual(f.backups(), []);
  assert.deepEqual(f.state().starts, []);
  assert.equal(f.state().calls.some(args => args.includes("stop")), false);
});

test("legacy network migration retains private networks and their existing aliases", requiresMigration, (t) => {
  const f = legacyDeployment(t);
  const config = parse(readFileSync(f.target, "utf8"));
  config.services["video-site-91"].networks = { private: { aliases: ["custom-api"] } };
  config.services.sidecar.networks = ["private"];
  config.networks = { private: {} };
  writeFileSync(f.target, JSON.stringify(config));
  writeFileSync(join(f.root, "docker-compose.override.yml"), "services:\n  video-site-91:\n    environment: {CUSTOM: keep}\n");
  succeeded(f.run(["update"]));
  const networks = f.state().starts[0].services[service].networks;
  assert.deepEqual(Object.keys(networks), ["private"]);
  assert.deepEqual(networks.private.aliases, ["custom-api", "video-site-91"]);
});

test("legacy migration isolates shared YAML anchors when retaining the old DNS name", requiresMigration, (t) => {
  const f = legacyDeployment(t);
  writeFileSync(f.target, `x-common: &common
  networks: {private: {}}
services:
  video-site-91:
    <<: *common
    image: ghcr.io/nianzhibai/91:stable
    container_name: video-site-91
    volumes: ["./private data:/opt/video-site-91/data"]
    environment: {service: video-site-91}
  sidecar:
    <<: *common
    image: busybox
networks:
  private: {}
`);
  succeeded(f.run(["update"]));
  const services = f.state().starts[0].services;
  assert.ok(services[service].networks.private.aliases.includes("video-site-91"));
  assert.equal(services.sidecar.networks.private.aliases?.includes("video-site-91") ?? false, false);
  assert.equal(services[service].environment.service, "video-site-91");
});

test("legacy migration preserves inherited network aliases and dependency mappings", requiresMigration, (t) => {
  const f = legacyDeployment(t);
  writeFileSync(f.target, `x-aliases: &aliases
  aliases: [custom-api]
x-networks: &networks
  private:
    <<: *aliases
x-dependencies: &dependencies
  video-site-91: {condition: service_started}
services:
  video-site-91:
    image: ghcr.io/nianzhibai/91:stable
    volumes: ["./private data:/opt/video-site-91/data"]
    networks:
      <<: *networks
  sidecar:
    image: busybox
    networks: *networks
    depends_on:
      <<: *dependencies
networks:
  private: {}
`);
  succeeded(f.run(["update"]));
  const services = f.state().starts[0].services;
  assert.deepEqual(services[service].networks.private.aliases, ["custom-api", "video-site-91"]);
  assert.deepEqual(services.sidecar.networks.private.aliases, ["custom-api"]);
  assert.equal(services.sidecar.depends_on[service].condition, "service_started");
  assert.equal(services.sidecar.depends_on["video-site-91"], undefined);
});

for (const external of [false, true]) {
  test(`legacy migration resolves extends in its own file namespace (external: ${external})`, requiresMigration, (t) => {
    const f = legacyDeployment(t);
    const shared = join(f.root, "shared.yml");
    const sharedSource = "services:\n  video-site-91:\n    image: busybox\n    environment: {SOURCE: external}\n";
    writeFileSync(shared, sharedSource);
    const config = parse(readFileSync(f.target, "utf8"));
    config.services.sidecar = {
      extends: { ...(external ? { file: "shared.yml" } : {}), service: "video-site-91" },
      container_name: "sidecar",
    };
    writeFileSync(f.target, JSON.stringify(config));
    succeeded(f.run(["update"]));
    assert.equal(readFileSync(shared, "utf8"), sharedSource);
    const sidecar = f.state().starts[0].services.sidecar;
    assert.equal(sidecar.image, external ? "busybox" : "ghcr.io/nianzhibai/91:stable");
    if (external) assert.equal(sidecar.environment.SOURCE, "external");
    assert.ok(f.state().starts[0].services[service]);
  });
}

test("COMPOSE_FILE from dotenv selects the original files and project name", requiresCompose, (t) => {
  const f = fixture(t);
  const alternative = join(f.root, "production.yml");
  renameSync(f.target, alternative);
  writeFileSync(join(f.root, ".env"), "COMPOSE_FILE=production.yml\nCOMPOSE_PROJECT_NAME=existing-project\n");
  succeeded(f.run(["update"]));
  assert.equal(existsSync(f.target), false);
  assert.equal(existsSync(alternative), true);
  assert.equal(f.state().starts[0].name, "existing-project");
});

test("GPU conflicts in overlays abort without changing either file", requiresCompose, (t) => {
  const f = fixture(t);
  const overlay = join(f.root, "gpu.yml");
  copyFileSync(join(repository, "docker-compose.gpu-nvidia.yml"), overlay);
  const original = readFileSync(f.target, "utf8");
  const overlayOriginal = readFileSync(overlay, "utf8");
  writeFileSync(join(f.root, ".env"), `COMPOSE_FILE=${f.target}:${overlay}\n`);
  const result = f.run(["gpu"]);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /overlay conflicts/);
  assert.equal(readFileSync(f.target, "utf8"), original);
  assert.equal(readFileSync(overlay, "utf8"), overlayOriginal);
});

for (const conflict of ["extra VAAPI device", "different NVIDIA device", "NVIDIA visibility", "CUDA visibility", "NVIDIA capabilities", "privileged access", "GPU volume mount"]) {
  test(`an overlay cannot override the selected GPU through ${conflict}`, requiresCompose, (t) => {
    const f = fixture(t);
    const overlay = join(f.root, "gpu-overlay.yml");
    const mode = conflict === "extra VAAPI device" ? "vaapi" : "nvidia";
    const settings = {
      "extra VAAPI device": { devices: ["/dev/dri:/dev/dri"] },
      "different NVIDIA device": { deploy: { resources: { reservations: { devices: [{ driver: "nvidia", device_ids: [secondNvidiaGPU.device], capabilities: ["gpu"] }] } } } },
      "NVIDIA visibility": { environment: { NVIDIA_VISIBLE_DEVICES: "all" } },
      "CUDA visibility": { environment: { CUDA_VISIBLE_DEVICES: "1" } },
      "NVIDIA capabilities": { environment: { NVIDIA_DRIVER_CAPABILITIES: "utility" } },
      "privileged access": { privileged: true },
      "GPU volume mount": { volumes: ["/dev/dri:/dev/dri"] },
    }[conflict];
    writeFileSync(overlay, JSON.stringify({ services: { [service]: settings } }));
    writeFileSync(join(f.root, ".env"), `COMPOSE_FILE=${f.target}:${overlay}\n`);
    const original = readFileSync(f.target, "utf8");
    const other = readFileSync(overlay, "utf8");

    const result = f.run(["gpu"], { TEST_READY_GPU: mode });
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /Compose settings override the selected GPU/);
    assert.equal(readFileSync(f.target, "utf8"), original);
    assert.equal(readFileSync(overlay, "utf8"), other);
    assert.deepEqual(f.state().starts, []);
    assert.deepEqual(f.backups(), []);
  });
}

test("explicit image and project name from dotenv are retained", requiresCompose, (t) => {
  const f = fixture(t);
  writeFileSync(join(f.root, ".env"), "COMPOSE_PROJECT_NAME=existing-project\n");
  succeeded(f.run(["update", "--image", "ghcr.io/nianzhibai/91:v9.0.0"]));
  assert.equal(f.raw().services[service].image, "ghcr.io/nianzhibai/91:v9.0.0");
  assert.equal(f.state().starts[0].name, "existing-project");
});

test("no detected GPU removes GPU access and preserves unrelated resources", requiresCompose, (t) => {
  const f = fixture(t);
  const base = parse(readFileSync(f.target, "utf8"));
  Object.assign(base.services[service], {
    runtime: "nvidia", devices: ["/dev/dri:/dev/dri", "/dev/mpp_service:/dev/mpp_service", "/dev/rga:/dev/rga", "/dev/dma_heap:/dev/dma_heap", "/dev/fuse:/dev/fuse"],
    environment: { CUSTOM: "keep", NVIDIA_DRIVER_CAPABILITIES: "all" },
    deploy: { resources: { limits: { memory: "2G" }, reservations: { devices: [
      { driver: "nvidia", capabilities: ["gpu"] }, { driver: "custom", capabilities: ["custom"] },
    ] } } },
  });
  writeFileSync(f.target, JSON.stringify(base));
  const before = f.raw().services[service];
  succeeded(f.run(["gpu"]));
  const s = f.raw().services[service];
  assert.equal(s.runtime, undefined);
  assert.deepEqual(s.devices.map((device: any) => device.source), ["/dev/fuse"]);
  assert.deepEqual(s.deploy.resources.reservations.devices, before.deploy.resources.reservations.devices.filter((device: any) => device.driver === "custom"));
  assert.equal(s.deploy.resources.limits.memory, before.deploy.resources.limits.memory);
  assert.equal(s.environment.CUSTOM, "keep");
  assert.equal(s.environment.NVIDIA_DRIVER_CAPABILITIES, undefined);
});

const probeHost = `
rockchip_devices() { printf '%s\\n' "\${TEST_ROCKCHIP_DEVICES:-}"; }
list_render_devices() { printf '%s\\n' "\${TEST_RENDER_DEVICES:-\${TEST_RENDER_DEVICE:-}}"; }
render_vendor() { printf '%s\\n' "\${TEST_RENDER_VENDOR:-0x8086}"; }
render_device_name() { printf 'Intel GPU\\n'; }
pci_gpu_vendors() { printf '%s\\n' "\${TEST_PCI_VENDORS:-}"; }
docker info --format '{{json .}}' > "$TEST_DEPLOYMENT/info.json"
detect_host_gpu "$TEST_DEPLOYMENT/info.json"
printf '%s\\n' "$GPU_CHOICES"
`;

test("NVIDIA detection requires a working driver and Docker runtime", requiresCompose, (t) => {
  const f = fixture(t);
  let result = f.functions(probeHost, { TEST_NVIDIA_MODE: "ready" });
  succeeded(result);
  assert.deepEqual(JSON.parse(result.stdout.trim()), [defaultGPUs.nvidia]);
  f.changeState({ info: { Runtimes: {}, OperatingSystem: "Debian" } });
  result = f.functions(probeHost, { TEST_NVIDIA_MODE: "ready" });
  succeeded(result);
  assert.match(result.stdout, /Container Toolkit/);
  assert.match(result.stdout, /\[\]\s*$/);
});

test("driver timeout and missing PCI GPU prerequisites produce actionable notes", requiresCompose, (t) => {
  const f = fixture(t);
  const result = f.functions(probeHost, { TEST_NVIDIA_MODE: "timeout", TEST_PCI_VENDORS: "0x10de\n0x8086" });
  succeeded(result);
  assert.match(result.stdout, /kernel driver/);
  assert.match(result.stdout, /failed or timed out/);
  assert.match(result.stdout, /\[\]\s*$/);
});

test("VAAPI detection checks vendor and rootless device access", requiresCompose, (t) => {
  const f = fixture(t);
  let result = f.functions(probeHost, { TEST_RENDER_DEVICE: "/missing-render-device" });
  succeeded(result);
  assert.deepEqual(JSON.parse(result.stdout.trim()), [{ ...defaultGPUs.vaapi, device: "/missing-render-device" }], "Rootful Docker uses daemon device permissions");
  f.changeState({ info: { Runtimes: {}, OperatingSystem: "Debian", SecurityOptions: ["name=rootless"] } });
  result = f.functions(probeHost, { TEST_RENDER_DEVICE: "/missing-render-device" });
  succeeded(result);
  assert.match(result.stdout, /render\/video group/);
  assert.match(result.stdout, /\[\]\s*$/);
  result = f.functions(probeHost, { TEST_RENDER_DEVICE: "/missing-render-device", TEST_RENDER_VENDOR: "0x10de" });
  succeeded(result);
  assert.deepEqual(JSON.parse(result.stdout.trim()), []);
});

test("GPU detection lists each VAAPI and NVIDIA card separately", requiresCompose, (t) => {
  const f = fixture(t);
  const result = f.functions(probeHost, {
    TEST_RENDER_DEVICES: `${vaapiDevice}\n${secondVaapiGPU.device}`,
    TEST_NVIDIA_MODE: "ready",
    TEST_NVIDIA_DEVICES: `${nvidiaDevice}, NVIDIA RTX 4060\n${secondNvidiaGPU.device}, NVIDIA RTX 4090`,
  });
  succeeded(result);
  assert.deepEqual(JSON.parse(result.stdout.trim()), [
    defaultGPUs.vaapi,
    { ...secondVaapiGPU, label: "Intel GPU" },
    defaultGPUs.nvidia,
    secondNvidiaGPU,
  ]);
});

test("automatic GPU detection rejects remote Docker hosts", requiresCompose, (t) => {
  const f = fixture(t);
  const result = f.functions(probeHost, { DOCKER_HOST: "ssh://example.test" });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /local Linux Docker Engine/);
});

test("GPU detection failure leaves the deployment and containers untouched", requiresCompose, (t) => {
  const f = fixture(t);
  const original = readFileSync(f.target, "utf8");
  const result = f.functions("main gpu", { DOCKER_HOST: "ssh://example.test" });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /local Linux Docker Engine/);
  assert.equal(readFileSync(f.target, "utf8"), original);
  assert.deepEqual(f.backups(), []);
  assert.deepEqual(f.state().starts, []);
  assert.equal(f.state().pulled, false);
});

test("update preserves GPU settings without probing host hardware by default", requiresCompose, (t) => {
  const f = fixture(t);
  const config = parse(readFileSync(f.target, "utf8"));
  config.services[service].runtime = "nvidia";
  Object.assign(config.services[service].environment, {
    NVIDIA_VISIBLE_DEVICES: "GPU-existing",
    NVIDIA_DRIVER_CAPABILITIES: "utility",
  });
  writeFileSync(f.target, JSON.stringify(config));
  const program = `source "$1"
detect_host_gpu() { exit 92; }
main update`;
  const result = spawnSync("bash", ["-c", program, "bash", script], { cwd: f.root, env: f.env, encoding: "utf8", timeout: 30_000 });
  succeeded(result);
  assert.equal(f.state().calls.some((args) => args[0] === "info"), false);
  const after = f.raw().services[service];
  assert.equal(after.runtime, "nvidia");
  assert.equal(after.environment.NVIDIA_VISIBLE_DEVICES, "GPU-existing");
  assert.equal(after.environment.NVIDIA_DRIVER_CAPABILITIES, "utility");
  assert.equal(after.deploy, undefined);
});

function rockchipDeployment(t: TestContext) {
  const f = fixture(t);
  const config = parse(readFileSync(f.target, "utf8"));
  config.services[service].image = "ghcr.io/nianzhibai/91:stable-rockchip";
  config.services[service].devices = ["/dev/fuse:/dev/fuse"];
  writeFileSync(f.target, JSON.stringify(config));
  return f;
}

test("Rockchip selection maps its device set and keeps the image and app setting", requiresCompose, (t) => {
  const f = rockchipDeployment(t);
  mkdirSync(join(f.root, "data"));
  const config = join(f.root, "data/config.yaml");
  writeFileSync(config, "generation: {use_gpu: false}\n");
  succeeded(f.run(["gpu"], { TEST_READY_GPU: "rockchip", TEST_MEDIA_VARIANT: "rockchip" }));
  const after = f.raw().services[service];
  assert.equal(after.image, "ghcr.io/nianzhibai/91:stable-rockchip");
  assert.deepEqual(after.devices.map((device: any) => device.source), ["/dev/fuse", ...defaultGPUs.rockchip.devices!]);
  assert.equal(after.privileged, undefined);
  assert.equal(readFileSync(config, "utf8"), "generation: {use_gpu: false}\n");
  assert.equal(f.state().starts[0].services[service].image, oldImage);
  assert.equal(f.state().pulled, false);
  const original = readFileSync(f.target, "utf8");
  succeeded(f.run(["gpu"], { TEST_READY_GPU: "rockchip", TEST_MEDIA_VARIANT: "rockchip" }));
  assert.equal(readFileSync(f.target, "utf8"), original);
  assert.equal(f.state().starts.length, 1);
});

test("Rockchip accepts a legacy service node without adding absent optional devices", requiresCompose, (t) => {
  const f = rockchipDeployment(t);
  const choice = { ...defaultGPUs.rockchip, device: "/dev/mpp-service", devices: ["/dev/mpp-service", "/dev/dri"] };
  succeeded(f.run(["gpu"], { TEST_GPU_CHOICES: JSON.stringify([choice]), TEST_MEDIA_VARIANT: "rockchip" }));
  assert.deepEqual(f.raw().services[service].devices.map((device: any) => device.source), ["/dev/fuse", ...choice.devices]);
});

test("Rockchip selection rejects a standard image before changing the deployment", requiresCompose, (t) => {
  const f = fixture(t);
  const original = readFileSync(f.target, "utf8");
  const result = f.run(["gpu"], { TEST_READY_GPU: "rockchip" });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /update --image stable-rockchip/);
  assert.equal(readFileSync(f.target, "utf8"), original);
  assert.deepEqual(f.backups(), []);
  assert.deepEqual(f.state().starts, []);
});

test("Rockchip updates preserve the variant and mappings without probing hardware", requiresCompose, (t) => {
  const f = rockchipDeployment(t);
  succeeded(f.run(["gpu"], { TEST_READY_GPU: "rockchip", TEST_MEDIA_VARIANT: "rockchip" }));
  const before = f.raw().services[service];
  succeeded(f.functions('detect_host_gpu() { exit 92; }; main update', { TEST_MEDIA_VARIANT: "rockchip" }));
  const after = f.raw().services[service];
  assert.equal(after.image, before.image);
  assert.deepEqual(after.devices, before.devices);
  assert.ok(f.state().calls.some((args) => args[0] === "pull" && args[1] === before.image));
});

test("an image update cannot leave Rockchip devices attached to a standard image", requiresCompose, (t) => {
  const f = rockchipDeployment(t);
  succeeded(f.run(["gpu"], { TEST_READY_GPU: "rockchip", TEST_MEDIA_VARIANT: "rockchip" }));
  const original = readFileSync(f.target, "utf8");
  const starts = f.state().starts.length;
  const result = f.run(["update", "--image", "stable"]);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /Rockchip devices require a Rockchip image/);
  assert.equal(readFileSync(f.target, "utf8"), original);
  assert.equal(f.state().starts.length, starts);
});

test("switching from Rockchip to VAAPI removes MPP and RGA mappings", requiresCompose, (t) => {
  const f = rockchipDeployment(t);
  succeeded(f.run(["gpu"], { TEST_READY_GPU: "rockchip", TEST_MEDIA_VARIANT: "rockchip" }));
  succeeded(f.run(["gpu"], { TEST_READY_GPU: "vaapi" }));
  assert.deepEqual(f.raw().services[service].devices.map((device: any) => device.source), ["/dev/fuse", vaapiDevice]);
});

test("failed Rockchip startup rolls back the image and device mappings", requiresCompose, (t) => {
  const f = rockchipDeployment(t);
  const original = readFileSync(f.target, "utf8");
  f.changeState({ failStart: true });
  const result = f.run(["gpu"], { TEST_READY_GPU: "rockchip", TEST_MEDIA_VARIANT: "rockchip" });
  assert.notEqual(result.status, 0);
  assert.equal(readFileSync(f.target, "utf8"), original);
  assert.equal(f.state().starts.length, 2);
  assert.equal(f.state().starts[1].services[service].image, oldImage);
});

test("an overlay cannot add devices to a selected Rockchip configuration", requiresCompose, (t) => {
  const f = rockchipDeployment(t);
  const overlay = join(f.root, "extra-media.yml");
  writeFileSync(overlay, JSON.stringify({ services: { [service]: { devices: ["/dev/dma_heap/extra:/dev/dma_heap/extra"] } } }));
  writeFileSync(join(f.root, ".env"), `COMPOSE_FILE=${f.target}:${overlay}\n`);
  const original = readFileSync(f.target, "utf8");
  const result = f.run(["gpu"], { TEST_READY_GPU: "rockchip", TEST_MEDIA_VARIANT: "rockchip" });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /Compose settings override the selected GPU/);
  assert.equal(readFileSync(f.target, "utf8"), original);
  assert.deepEqual(f.state().starts, []);
});

test("Rockchip image aliases work for first installation and explicit updates", requiresCompose, (t) => {
  const f = freshDeployment(t);
  succeeded(f.run(["install", "--image", "stable-rockchip"]));
  assert.equal(f.raw().services[service].image, "ghcr.io/nianzhibai/91:stable-rockchip");
  succeeded(f.run(["update", "--image", "main-rockchip"]));
  assert.equal(f.raw().services[service].image, "ghcr.io/nianzhibai/91:main-rockchip");
});

test("host detection recognizes Rockchip independently from PCI GPUs", requiresCompose, (t) => {
  const f = fixture(t);
  const result = f.functions(probeHost, { TEST_ROCKCHIP_DEVICES: defaultGPUs.rockchip.devices!.join("\n") });
  succeeded(result);
  assert.deepEqual(JSON.parse(result.stdout.trim()), [defaultGPUs.rockchip]);
});

test("rootless detection reports inaccessible Rockchip media devices", requiresCompose, (t) => {
  const f = fixture(t);
  f.changeState({ info: { Runtimes: {}, OperatingSystem: "Debian", SecurityOptions: ["name=rootless"] } });
  const result = f.functions(probeHost, { TEST_ROCKCHIP_DEVICES: "/missing-mpp-device" });
  succeeded(result);
  assert.match(result.stdout, /check Rockchip media device permissions/);
  assert.match(result.stdout, /\[\]\s*$/);
});
