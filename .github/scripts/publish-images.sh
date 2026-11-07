#!/usr/bin/env bash
set -euo pipefail

: "${IMAGE:?Image name is required}"
: "${TAGS:?Image tags are required}"

sources=()
if [[ -n "${SOURCE_TAG:-}" ]]; then
  sources=("$IMAGE:$SOURCE_TAG")
  rockchip_source="$IMAGE:$SOURCE_TAG-rockchip"
else
  : "${DIGEST_DIR:?Digest directory is required}"
  for arch in amd64 arm64 rockchip-arm64; do
    digest=$(cat "$DIGEST_DIR/docker-digests-$arch/digest.txt")
    if [[ ! "$digest" =~ ^sha256:[a-f0-9]{64}$ ]]; then
      echo "Invalid image digest for $arch" >&2
      exit 1
    fi
    if [[ "$arch" == rockchip-arm64 ]]; then
      rockchip_source="$IMAGE@$digest"
    else
      sources+=("$IMAGE@$digest")
    fi
  done
fi

tags=()
rockchip_tags=()
while IFS= read -r tag; do
  if [[ -n "$tag" ]]; then
    tags+=(--tag "$tag")
    rockchip_tags+=(--tag "${tag}-rockchip")
  fi
done <<< "$TAGS"
if (( ${#tags[@]} == 0 )); then
  echo "No image tags to publish" >&2
  exit 1
fi
docker buildx imagetools create "${tags[@]}" "${sources[@]}"
docker buildx imagetools create "${rockchip_tags[@]}" "$rockchip_source"
