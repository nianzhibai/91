import { defineConfig } from "vite";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL(".", import.meta.url));
const target = `http://127.0.0.1:${process.env.BACKEND_PORT || "9192"}`;
const manifest = JSON.parse(readFileSync(new URL("frontend.json", import.meta.url), "utf8"));

// `--mode release` builds an installable frontend package: assets live under the
// versioned prefix and the manifest ships beside index.html. Other builds stay
// root-relative for VIDEO_FRONTEND_DIR or independent hosting.
export default defineConfig(({ mode }) => {
  const release = mode === "release";
  return {
    root,
    base: release ? `/_frontend/${manifest.name}/${manifest.version}/` : "/",
    plugins: release ? [{
      name: "frontend-manifest",
      generateBundle() {
        this.emitFile({ type: "asset", fileName: "frontend.json", source: `${JSON.stringify(manifest, null, 2)}\n` });
      },
    }] : [],
    server: {
      host: "127.0.0.1",
      port: 9193,
      strictPort: true,
      proxy: Object.fromEntries(["/api", "/admin/api", "/p"].map(path => [path, { target, xfwd: true }])),
    },
  };
});
