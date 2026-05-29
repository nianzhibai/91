# ---- Stage 1: Build frontend ----
FROM node:20-slim AS frontend
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY tsconfig.json vite.config.ts index.html ./
COPY src/ src/
COPY public/ public/
RUN npm run build

# ---- Stage 2: Build backend ----
FROM golang:1.23-bookworm AS backend
WORKDIR /app/backend
COPY backend/ .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o server ./cmd/server

# ---- Stage 3: Runtime ----
FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl tar ffmpeg openssl iproute2 \
    python3 python3-requests python3-bs4 python3-lxml \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /opt/video-site-91

# Copy built artifacts
COPY --from=backend /app/backend/server ./server
COPY --from=frontend /app/dist ./dist
COPY backend/config.example.yaml ./config.example.yaml
COPY 91VideoSpider/ ./91VideoSpider/
COPY install.sh ./install.sh

RUN chmod +x ./server

# Config via env vars (see install.sh)
ENV VIDEO_CONFIG=/opt/video-site-91/config.yaml
ENV VIDEO_FRONTEND_DIR=/opt/video-site-91/dist
ENV VIDEO_VERSION_FILE=/opt/video-site-91/.version
ENV VIDEO_GITHUB_REPO=nianzhibai/91

# Data volume
VOLUME ["/opt/video-site-91/data"]

EXPOSE 9191

# Generate default config on first run if missing
COPY docker-entrypoint.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/docker-entrypoint.sh
ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["./server"]
