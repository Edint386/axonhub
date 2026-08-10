#!/usr/bin/env bash
set -euo pipefail

title_file=${1:-}
if [[ -z "$title_file" || ! -f "$title_file" ]]; then
  echo "Usage: commit-patch.sh TITLE_FILE" >&2
  exit 2
fi

git commit -F "$title_file"
