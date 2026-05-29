#!/bin/bash
set -e

CONFIG="/opt/video-site-91/config.yaml"
EXAMPLE="/opt/video-site-91/config.example.yaml"

# Auto-generate config from example if missing
if [ ! -f "$CONFIG" ] && [ -f "$EXAMPLE" ]; then
    cp "$EXAMPLE" "$CONFIG"
    # Generate random session secret
    SECRET=$(openssl rand -hex 32)
    sed -i "s/session_secret: \"change-me-to-a-random-string\"/session_secret: \"$SECRET\"/" "$CONFIG"
    # Bind to 0.0.0.0 for Docker (use FRONTEND_PORT or default 9191)
    PORT="${FRONTEND_PORT:-9191}"
    sed -i -E "s#listen: \".*\"#listen: \"0.0.0.0:${PORT}\"#" "$CONFIG"
    echo "[entrypoint] Generated config.yaml with random session_secret"
fi

# Write version file if missing
if [ ! -f /opt/video-site-91/.version ]; then
    echo "docker-${VERSION:-latest}" > /opt/video-site-91/.version
fi

exec "$@"
