# AGENTS.md

## Project shape

- Full-stack private video site: Vite/React/TypeScript frontend at repo root, Go backend under `backend/`, bundled 91 crawler at `91VideoSpider/spider_91porn.py`.
- Backend entrypoint is `backend/cmd/server`; real server modules live in `backend/internal/{api,auth,catalog,config,drives,scanner,preview,proxy}`.
- `backend/vendor/` is committed for offline Go builds. Do not edit vendored code manually; update dependencies with `go get ...`, `go mod tidy`, then `go mod vendor` from `backend/`.

## Commands

- Install frontend deps: `npm install` (CI/release scripts use `npm ci` when `package-lock.json` exists).
- Frontend typecheck/lint: `npm run lint` (`tsc --noEmit`).
- Frontend build: `npm run build` (`tsc -b && vite build`).
- Frontend tests: `npm test` (`node --import tsx --test tests/*.test.ts`); focused example: `node --import tsx --test tests/previewIntent.test.ts`.
- Backend tests: from `backend/`, run `go test ./... -count=1`.
- Full local dev from Git Bash/WSL: `npm install`, then `./start.sh`; use `FRONTEND_MODE=dev ./start.sh --restart` for Vite HMR.
- Manual split startup: root `npm run build` + `npm run preview` for frontend 9191, and from `backend/` `go run ./cmd/server` for backend 9192.
- Release packaging entrypoint is `scripts/build-release.sh`; GitHub release workflow runs it on `v*` tags.

## Runtime and config gotchas

- Frontend proxy is hardwired in `vite.config.ts`: `/api`, `/p`, and `/admin/api` target `http://127.0.0.1:9192`. Keep backend `server.listen` aligned when changing dev ports.
- Backend first run creates `backend/config.yaml`, `backend/data/video-site.db`, and `backend/data/previews/` from `backend/config.example.yaml`.
- Local preview/thumbnail generation depends on `ffmpeg` and `ffprobe`; Windows dev docs assume Go 1.23+ and ffmpeg are on `PATH`.
- Docker runtime exposes `9191`, persists `/opt/video-site-91/data`, and sets `VIDEO_CONFIG`, `VIDEO_FRONTEND_DIR`, `VIDEO_LISTEN_PORT`, `VIDEO_IMAGE_VERSION`, and `VIDEO_VERSION_FILE` in the image/entrypoint flow.

## Code conventions worth preserving

- Frontend uses strict TypeScript with `@/*` mapped to `src/*`; tests use Node's built-in test runner plus `tsx`, not Jest/Vitest.
- Admin API client uses `credentials: "include"` and throws `UnauthorizedError` on 401; keep cookie-auth behavior consistent.
- Most user-facing and admin text is Chinese; avoid switching established UI copy to English unless requested.
- Go module requires Go 1.23/toolchain 1.23.4. Keep backend changes inside `backend/internal/` unless adding a command under `backend/cmd/`.
- Drive implementations are under `backend/internal/drives/<kind>/`; add new provider behavior there and wire it through catalog/API/server attach paths rather than special-casing frontend-only state.

## Verification expectations

- Frontend-only changes: run `npm run lint`, `npm run build`, and relevant `npm test` or focused `node --import tsx --test tests/<name>.test.ts`.
- Backend-only changes: run `go test ./... -count=1` from `backend/`.
- Cross-stack changes: run both frontend and backend checks; verify port/proxy assumptions if touching `vite.config.ts`, `start.sh`, or `backend/config.example.yaml`.
