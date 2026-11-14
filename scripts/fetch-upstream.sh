#!/bin/sh
# Shallow-clone every upstream source tree used by the research into ../upstream
# Usage: scripts/fetch-upstream.sh [dest-dir]
set -eu
here=$(cd "$(dirname "$0")" && pwd)
dest=${1:-"$here/../../upstream"}
mkdir -p "$dest"
while read -r url name; do
  [ -z "$url" ] && continue
  dir=${name:-$(basename "$url" .git)}
  if [ -d "$dest/$dir/.git" ]; then echo "skip $dir"; continue; fi
  git clone --depth 1 --single-branch -q "$url" "$dest/$dir" && echo "ok   $dir" || echo "FAIL $dir"
done < "$here/upstream-repos.txt"
