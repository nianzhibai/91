import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import { mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createInterface } from "node:readline";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { parse, parseDocument } from "yaml";

const root = fileURLToPath(new URL("..", import.meta.url));
const prepare = join(root, "scripts/prepare-dev-config.mjs");
const template = join(root, "backend/config.example.yaml");
const launcher = join(root, "start.sh");

test("development configuration isolates defaults and preserves edits when the port changes", (t) => {
  const directory = mkdtempSync(join(tmpdir(), "91-dev-config-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const path = join(directory, "config.dev.yaml");
  const productionPath = join(directory, "config.yaml");
  const production = 'server: {listen: "0.0.0.0:9191"}\n';
  writeFileSync(productionPath, production);
  const prepareConfig = (port: string) => {
    const result = spawnSync(process.execPath, [prepare, template, path, port], { encoding: "utf8" });
    assert.equal(result.status, 0, result.stderr);
    return parse(readFileSync(path, "utf8"));
  };
  const first = prepareConfig("9192");
  assert.equal(first.server.listen, "127.0.0.1:9192");
  assert.deepEqual(first.storage, { data_dir: "./data-dev", db_dir: "" });
  assert.equal(first.logging.directory, undefined);
  assert.equal(first.telegram.enabled, false);
  assert.equal(statSync(path).mode & 0o777, 0o600);

  const edited = parseDocument(readFileSync(path, "utf8"));
  edited.commentBefore = " Keep development preferences";
  edited.setIn(["preview", "enabled"], false);
  edited.setIn(["storage", "data_dir"], "./custom-dev");
  edited.setIn(["storage", "db_dir"], "./fast-dev-db");
  writeFileSync(path, edited.toString());
  const second = prepareConfig("19292");
  assert.equal(second.server.listen, "127.0.0.1:19292");
  assert.equal(second.preview.enabled, false);
  assert.equal(second.storage.data_dir, "./custom-dev");
  assert.equal(second.storage.db_dir, "./fast-dev-db");
  assert.match(readFileSync(path, "utf8"), /Keep development preferences/);
  assert.equal(readFileSync(productionPath, "utf8"), production);
});

test("invalid development configuration and ports fail without overwriting settings", (t) => {
  const directory = mkdtempSync(join(tmpdir(), "91-dev-invalid-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const path = join(directory, "config.dev.yaml");
  const original = "server: [\n";
  writeFileSync(path, original);
  for (const port of ["9192", "0", "65536", "09", "abc"]) {
    const result = spawnSync(process.execPath, [prepare, template, path, port], { encoding: "utf8" });
    assert.notEqual(result.status, 0);
    assert.equal(readFileSync(path, "utf8"), original);
  }
});


test("development launcher refuses to restart or stop another service on its ports", async (t) => {
  const child = spawn(process.execPath, ["-e", `
    const server = require('node:http').createServer((req, res) => res.end('still running'));
    server.listen(0, '127.0.0.1', () => console.log(server.address().port));
  `], { stdio: ["ignore", "pipe", "inherit"] });
  t.after(() => child.kill());
  const lines = createInterface({ input: child.stdout! });
  const [port] = await once(lines, "line");
  lines.close();
  for (const action of ["start", "--restart", "--stop"]) {
    const result = spawnSync("bash", [launcher, action], {
      env: { ...process.env, FRONTEND_PORT: String(port), BACKEND_PORT: port === "9192" ? "9193" : "9192" },
      encoding: "utf8",
      timeout: 5000,
    });
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /occupied by another service/);
    const response = await fetch(`http://127.0.0.1:${port}/`);
    assert.equal(await response.text(), "still running");
  }
});
