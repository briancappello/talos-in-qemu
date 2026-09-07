#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
recipe="$root/patches/talos/recipe"
while IFS= read -r line; do
  line=${line%%#*}
  line=${line//[[:space:]]/}
  case "$line" in
    patch:*)
      file="${recipe%/*}/${line#patch:}"
      test -f "$file" || { printf 'Missing patch: %s\n' "$file" >&2; exit 1; }
      ;;
  esac
done < "$recipe"
