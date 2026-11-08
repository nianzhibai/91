import { defineConfig } from "vite";
import { fileURLToPath } from "node:url";

const target = `http://127.0.0.1:${process.env.BACKEND_PORT || "9192"}`;

export default defineConfig({
  root: fileURLToPath(new URL(".", import.meta.url)),
  server: {
    host: "127.0.0.1",
    port: 9193,
    strictPort: true,
    proxy: Object.fromEntries(["/api", "/admin/api", "/p"].map(path => [path, { target, xfwd: true }])),
  },
});
