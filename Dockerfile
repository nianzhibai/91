# Fixed upstream ARM64 binaries include MPP/RGA and libx264. Download on the
# builder architecture; only the rockchip target depends on this stage.
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS rockchip-media
ARG TARGETARCH
RUN test "$TARGETARCH" = arm64 \
    && apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /media
RUN curl -fL --retry 3 https://github.com/MarcA711/Rockchip-FFmpeg-Builds/releases/download/6.1-11/ffmpeg -o ffmpeg \
    && curl -fL --retry 3 https://github.com/MarcA711/Rockchip-FFmpeg-Builds/releases/download/6.1-11/ffprobe -o ffprobe \
    && printf '%s\n' \
      '2849e8800ed8e7cd2a9ee9c5cff6b9abc325d2400f3fe755d6bd6d0ad4c87a91  ffmpeg' \
      '402b142b0ac1c8e7eeb0f9cbafc18b85fbcb286e4ed0b0be1da50454920ff8b1  ffprobe' \
      | sha256sum -c - \
    && chmod 755 ffmpeg ffprobe

# ---- Build backend ----
FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm AS backend
ARG TARGETOS
ARG TARGETARCH

WORKDIR /app/backend

COPY backend/go.mod backend/go.sum ./
COPY backend/vendor/ vendor/
COPY backend/cmd/ cmd/
COPY backend/internal/ internal/
COPY backend/config_template.go backend/config.example.yaml ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---- Fetch and verify the pinned frontend releases (or offline artifacts) ----
FROM backend AS frontend
COPY frontend.lock.json /app/frontend.lock.json
COPY frontend-bundle/ /bundle/
RUN CGO_ENABLED=0 go build -o /out/frontend-package ./cmd/frontend-package \
    && if ls /bundle/*.tar.gz >/dev/null 2>&1; then \
         /out/frontend-package --lock /app/frontend.lock.json --archives /bundle --output /out/frontend; \
       else \
         /out/frontend-package --lock /app/frontend.lock.json --output /out/frontend; \
       fi

# ---- Shared application runtime ----
FROM debian:bookworm-slim AS application

ENV DEBIAN_FRONTEND=noninteractive

RUN if [ "$(dpkg --print-architecture)" = "amd64" ]; then \
      printf '%s\n' 'deb [signed-by=/usr/share/keyrings/debian-archive-keyring.gpg] http://deb.debian.org/debian bookworm non-free' \
        > /etc/apt/sources.list.d/gpu-drivers.list; \
    fi \
    && apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    ffmpeg \
    mesa-va-drivers \
    python3 \
    python3-bs4 \
    python3-lxml \
    python3-requests \
    python3-socks \
    python3-yaml \
    tar \
    tzdata \
    && if [ "$(dpkg --print-architecture)" = "amd64" ]; then \
      apt-get install -y --no-install-recommends intel-media-va-driver-non-free i965-va-driver; \
    fi \
    && rm -rf /var/lib/apt/lists/*

RUN python3 -c "import requests, bs4, lxml, socks"

WORKDIR /opt/91

COPY --from=backend /out/server ./server
COPY --from=frontend /out/frontend ./dist
COPY backend/config.example.yaml ./config.example.yaml
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
COPY docker-compose.yml docker-compose.gpu-nvidia.yml docker-compose.gpu-vaapi.yml docker-compose.gpu-rockchip.yml /usr/local/share/91/deploy/
COPY scripts/migrate-compose.py /usr/local/share/91/migrate-compose.py

ARG VERSION=dev

LABEL io.91.database-filename="91.db"

ENV VIDEO_FRONTEND_DIR=/opt/91/dist \
    VIDEO_GITHUB_REPO=nianzhibai/91 \
    VIDEO_IMAGE_VERSION=${VERSION} \
    VIDEO_LISTEN_PORT=9191

RUN chmod +x ./server /usr/local/bin/docker-entrypoint.sh

VOLUME ["/opt/91/data"]
EXPOSE 9191

ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["./server"]

# ---- Rockchip runtime (linux/arm64 only) ----
FROM application AS rockchip
LABEL io.91.media-variant="rockchip"
COPY --from=rockchip-media /media/ffmpeg /media/ffprobe /usr/local/bin/
# Verify the bundled tools without requiring media hardware during the build.
RUN python3 - <<'PY'
import subprocess
for flag, names in [('-hwaccels', ['rkmpp']), ('-encoders', ['h264_rkmpp', 'libx264']), ('-filters', ['scale_rkrga'])]:
    output = subprocess.check_output(['ffmpeg', '-hide_banner', flag], text=True)
    for name in names:
        assert name in output, f'Missing FFmpeg capability: {name}'
subprocess.run(['ffprobe', '-version'], check=True)
PY

# Keep ordinary builds on the standard runtime, including generic ARM64 hosts.
FROM application AS runtime
LABEL io.91.media-variant="standard"
