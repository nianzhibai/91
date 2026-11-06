import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import test from "node:test";
import { fileURLToPath } from "node:url";

const installer = fileURLToPath(new URL("../install.sh", import.meta.url));
test("native Telegram runs in the backend without Docker setup", () => {
  const source = readFileSync(installer, "utf8");
  assert.doesNotMatch(source, /telegram-bot-api|setup_telegram|require_local_docker/);
  const result = spawnSync("bash", [installer, "telegram"], {encoding: "utf8", env: {...process.env, VIDEO_SITE_SKIP_SELF_UPDATE: "1"}});
  assert.notEqual(result.status, 0);
});
