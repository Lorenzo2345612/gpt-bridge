#!/usr/bin/env bash
# Build and atomically install the Go executable; leave user profiles alone.
set -euo pipefail
source_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
prefix="$HOME/.local"
prebuilt=""
while (($#)); do
  case "$1" in
    --prefix) prefix="$2"; shift 2 ;;
    --binary) prebuilt="$2"; shift 2 ;;
    *) echo "Usage: install.sh [--prefix PATH] [--binary PREBUILT_EXECUTABLE]" >&2; exit 2 ;;
  esac
done
mkdir -p -- "$prefix/share/claude-codex-bridge" "$prefix/bin"
target="$(cd -- "$prefix/share/claude-codex-bridge" && pwd)"
link="$prefix/bin/claude-codex"
expected="$target/claude-codex"
if [[ -L "$link" ]]; then
  if [[ "$(readlink -f -- "$link")" != "$expected" ]]; then
    echo "Refusing to replace unrelated symlink: $link" >&2; exit 1
  fi
elif [[ -e "$link" ]]; then
  echo "Refusing to replace existing file: $link" >&2; exit 1
fi
if [[ -z "$prebuilt" ]]; then
  go_binary="${CLAUDE_CODEX_GO:-go}"
  mkdir -p -- "$source_dir/dist"
  CGO_ENABLED=0 "$go_binary" -C "$source_dir" build -trimpath -ldflags='-s -w' -o "$source_dir/dist/claude-codex" .
  prebuilt="$source_dir/dist/claude-codex"
fi
"$prebuilt" --bridge-version
temporary="$(mktemp "$target/.claude-codex-install.XXXXXX")"
trap 'rm -f -- "$temporary"' EXIT
cp -- "$prebuilt" "$temporary"
chmod 755 "$temporary"
mv -f -- "$temporary" "$expected"
cp -- "$source_dir/README.md" "$target/README.md"
if [[ ! -L "$link" ]]; then ln -s -- "$expected" "$link"; fi
echo "Installed Go executable: $link"
echo "Your aliases, sessions and profile settings were preserved."
