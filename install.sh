#!/usr/bin/env bash
# Build the Go executable and optionally configure Bash/zsh aliases.
set -euo pipefail
source_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
resolve_path() {
  local path="$1" parent destination count=0
  while [[ -L "$path" ]]; do
    ((count+=1))
    if ((count>40)); then echo "Too many symbolic links: $1" >&2; return 1; fi
    parent="$(cd -P -- "$(dirname -- "$path")" && pwd)"
    destination="$(readlink "$path")"
    case "$destination" in /*) path="$destination" ;; *) path="$parent/$destination" ;; esac
  done
  printf '%s/%s\n' "$(cd -P -- "$(dirname -- "$path")" && pwd)" "$(basename -- "$path")"
}
prefix="$HOME/.local"
prebuilt=""
normal_alias=""
yolo_alias=""
normal_set=false
yolo_set=false
no_aliases=false
rc_file=""
while (($#)); do
  case "$1" in
    --prefix) prefix="$2"; shift 2 ;;
    --binary) prebuilt="$2"; shift 2 ;;
    --alias) normal_alias="$2"; normal_set=true; shift 2 ;;
    --yolo-alias) yolo_alias="$2"; yolo_set=true; shift 2 ;;
    --rc-file) rc_file="$2"; shift 2 ;;
    --no-aliases) no_aliases=true; shift ;;
    *) echo "Usage: install.sh [--prefix PATH] [--binary PATH] [--alias NAME] [--yolo-alias NAME] [--rc-file PATH] [--no-aliases]" >&2; exit 2 ;;
  esac
done
if [[ -z "$rc_file" ]]; then
  shell_name="${SHELL:-/bin/bash}"
  case "${shell_name##*/}" in
    zsh) rc_file="$HOME/.zshrc" ;;
    *) rc_file="$HOME/.bashrc" ;;
  esac
fi
if ! $no_aliases; then
  if [[ ! -t 0 ]] && ! $normal_set && ! $yolo_set; then
    echo "Run interactively to choose aliases, or pass --no-aliases / --alias NAME --yolo-alias NAME." >&2
    exit 2
  fi
  choose_alias() {
    local label="$1" value="$2" default_name="$3" supplied="$4"
    while true; do
      if [[ -z "$value" ]]; then
        if [[ -t 0 ]] && [[ "$supplied" == false ]]; then
          read -r -p "$label [$default_name] (- to skip): " value || return 1
          value="${value:-$default_name}"
        else
          value="-"
        fi
      fi
      if [[ "$value" == "-" ]]; then printf '%s' "-"; return; fi
      if [[ ! "$value" =~ ^[a-zA-Z_][a-zA-Z0-9_-]*$ ]]; then
        echo "Use letters, digits, underscores and hyphens; start with a letter or underscore." >&2
      elif [[ -f "$rc_file" ]] && grep -Eq "^[[:space:]]*(alias[[:space:]]+['\"]?${value}=|function[[:space:]]+${value}([[:space:](]|$)|${value}[[:space:]]*\\(\\))" "$rc_file"; then
        echo "'$value' already exists in $rc_file; it will be preserved. Choose another name." >&2
      elif [[ "$value" != "claude-codex" ]] && command -v "$value" >/dev/null 2>&1; then
        echo "'$value' is already a command; choose another name." >&2
      else
        printf '%s' "$value"; return
      fi
      if [[ ! -t 0 ]] || [[ "$supplied" == true ]]; then return 1; fi
      value=""
    done
  }
  normal_alias="$(choose_alias 'Normal alias' "$normal_alias" gpt "$normal_set")"
  yolo_alias="$(choose_alias 'YOLO alias (skips permission prompts)' "$yolo_alias" gpt-yolo "$yolo_set")"
  if [[ "$normal_alias" != "-" && "$normal_alias" == "$yolo_alias" ]]; then
    echo "Normal and YOLO aliases must have different names." >&2; exit 2
  fi
fi
mkdir -p -- "$prefix/share/claude-codex-bridge" "$prefix/bin"
target="$(cd -- "$prefix/share/claude-codex-bridge" && pwd)"
link="$prefix/bin/claude-codex"
expected="$target/claude-codex"
if [[ -L "$link" ]]; then
  if [[ "$(resolve_path "$link")" != "$expected" ]]; then
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
if ! $no_aliases && [[ "$normal_alias" != "-" || "$yolo_alias" != "-" ]]; then
  alias_dir="${XDG_CONFIG_HOME:-$HOME/.config}/gpt-bridge"
  alias_file="$alias_dir/aliases.sh"
  if [[ -e "$alias_file" ]] && [[ "$(head -n 1 -- "$alias_file")" != '# Managed by gpt-bridge installer' ]]; then
    echo "Refusing to replace unrelated file: $alias_file" >&2; exit 1
  fi
  mkdir -p -- "$alias_dir" "$(dirname -- "$rc_file")"
  temporary="$(mktemp "$alias_dir/.aliases.XXXXXX")"
  printf '# Managed by gpt-bridge installer\n' > "$temporary"
  printf -v quoted_binary '%q' "$expected"
  if [[ "$normal_alias" != "-" ]]; then
    printf -v definition '%q' "$quoted_binary"
    printf 'alias %s=%s\n' "$normal_alias" "$definition" >> "$temporary"
  fi
  if [[ "$yolo_alias" != "-" ]]; then
    printf -v definition '%q' "CLAUDE_CODE_AUTO_COMPACT_WINDOW=600000 CLAUDE_YOLO=1 $quoted_binary --dangerously-skip-permissions"
    printf 'alias %s=%s\n' "$yolo_alias" "$definition" >> "$temporary"
  fi
  chmod 600 "$temporary"
  mv -f -- "$temporary" "$alias_file"
  printf -v quoted_alias_file '%q' "$alias_file"
  source_line="[ ! -f $quoted_alias_file ] || . $quoted_alias_file"
  if [[ ! -f "$rc_file" ]] || ! grep -Fqx -- "$source_line" "$rc_file"; then
    printf '\n# gpt-bridge aliases\n%s\n' "$source_line" >> "$rc_file"
  fi
  echo "Aliases saved: normal=$normal_alias, yolo=$yolo_alias"
  printf 'Activate them in your current shell: source %q\n' "$rc_file"
else
  echo "Existing aliases were preserved; no shell configuration was changed."
fi
echo "Claude sessions and profile settings were preserved."
