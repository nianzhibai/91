import assert from "node:assert/strict";
import {
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { brotliDecompressSync, gunzipSync } from "node:zlib";
import viteConfig from "../vite.config";

type TestWriteBundleHook = (
  this: object,
  options: { dir?: string },
  bundle: Record<string, { fileName: string }>
) => void | Promise<void>;

type TestPlugin = {
  name?: string;
  writeBundle?: TestWriteBundleHook | { handler: TestWriteBundleHook };
};

test("build precompression uses the finalized files written by Vite", async (t) => {
  const config = viteConfig as { plugins?: unknown[] };
  const plugins = (config.plugins ?? []).flat(Infinity) as TestPlugin[];
  const plugin = plugins.find((candidate) => candidate?.name === "precompress-build-assets");
  assert.ok(plugin, "precompress-build-assets plugin is configured");

  const hook =
    typeof plugin.writeBundle === "function"
      ? plugin.writeBundle
      : plugin.writeBundle?.handler;
  assert.ok(hook, "precompress-build-assets has a writeBundle hook");

  const outputDir = mkdtempSync(join(tmpdir(), "91-precompression-"));
  t.after(() => rmSync(outputDir, { recursive: true, force: true }));
  mkdirSync(join(outputDir, "assets"));

  const finalSource = Buffer.from(
    `${"const finalized = true;\n".repeat(128)}/* finalized by Vite */\n`
  );
  const assetPath = join(outputDir, "assets", "app.js");
  writeFileSync(assetPath, finalSource);

  // The in-memory bundle deliberately differs from the file. Vite performs
  // final marker/hash substitutions after generateBundle hooks, so sidecars
  // must be based on the output on disk.
  await hook.call(
    {},
    { dir: outputDir },
    {
      "assets/app.js": {
        fileName: "assets/app.js",
      },
    }
  );

  assert.deepEqual(
    brotliDecompressSync(readFileSync(`${assetPath}.br`)),
    finalSource
  );
  assert.deepEqual(gunzipSync(readFileSync(`${assetPath}.gz`)), finalSource);
});
