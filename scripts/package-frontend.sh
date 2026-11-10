#!/usr/bin/env bash
# Package a built frontend as a release installable by `91 frontend install`
# and the admin panel. Self-contained so frontend repositories can copy it.
set -euo pipefail

if [[ $# != 2 ]]; then
  echo "Usage: package-frontend.sh BUILD_DIRECTORY OUTPUT_DIRECTORY" >&2
  exit 2
fi
site="$(realpath -- "$1")"
output="$(realpath -m -- "$2")"
if [[ "$output/" == "$site/"* ]]; then
  echo "OUTPUT_DIRECTORY must be outside $site, or the archive would package itself" >&2
  exit 2
fi

for file in index.html frontend.json; do
  if [[ ! -f "$site/$file" ]]; then
    echo "$site/$file is missing; build in release mode first" >&2
    exit 1
  fi
done
if [[ -n "$(find "$site" ! -type f ! -type d -print -quit)" ]]; then
  echo "$site may only contain regular files and directories" >&2
  exit 1
fi

read -r name version < <(node -e '
  const m = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"));
  console.log(m.name, m.version);
' "$site/frontend.json")
if ! grep -qF "/_frontend/$name/$version/" "$site/index.html"; then
  echo "index.html must reference assets under /_frontend/$name/$version/" >&2
  exit 1
fi

mkdir -p "$output"
archive="$output/frontend.tar.gz"
# Installers accept only regular files and directories, so archive independent
# copies: tar would otherwise store hard-linked files as link entries.
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
# `sh -e` stops at the first failed copy; a loop's status is otherwise only its
# last command's, which would silently drop files from the package.
(cd "$site" &&
  find . -mindepth 1 -type d -exec sh -ec 'for d; do mkdir -p "$0/$d"; done' "$stage" {} + &&
  find . -type f -exec sh -ec 'for f; do cp "$f" "$0/$f"; done' "$stage" {} +)
# Entries must be plain relative paths: no "./" prefix and no "." root entry.
# The list is NUL-delimited so tar reads every name literally: a file named
# like an option (`--exclude=*.js`) or containing a newline stays a file.
(cd "$stage" &&
  find . -mindepth 1 -exec sh -c 'for p; do printf "%s\0" "${p#./}"; done' sh {} + |
  LC_ALL=C sort -z | tar --null --no-recursion -T - -cf - | gzip -n -9 > "$archive")

if command -v sha256sum > /dev/null; then
  digest="$(sha256sum "$archive" | cut -d' ' -f1)"
else
  digest="$(shasum -a 256 "$archive" | cut -d' ' -f1)"
fi
printf '%s  frontend.tar.gz\n' "$digest" > "$archive.sha256"

echo "Packaged $name@$version ($digest)"
echo "Publish both files in a GitHub release tagged v$version:"
echo "  $archive"
echo "  $archive.sha256"
