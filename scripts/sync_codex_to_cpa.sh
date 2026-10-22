#!/bin/bash
# 阶段7-2：把 codex 凭证同步到 CPA manager 的 auths 目录
set -u
SRC=/www/wwwroot/gptcli-2api/data/cpa_auth_files
DST=/www/wwwroot/cpa-manager-plus/cliproxyapi/auths
MANIFEST=/www/wwwroot/gptcli-2api/data/cpa_sync_manifest.txt
mkdir -p "$DST"
n=0; skip=0
if [ -f "$MANIFEST" ]; then
  while IFS= read -r f; do
    [ -z "$f" ] && continue
    if [ -f "$SRC/$f" ]; then
      if ! cmp -s "$SRC/$f" "$DST/$f"; then
        cp -f "$SRC/$f" "$DST/$f"; n=$((n+1))
      else
        skip=$((skip+1))
      fi
    fi
  done < "$MANIFEST"
else
  for p in "$SRC"/codex-*.json; do
    [ -e "$p" ] || continue
    f=$(basename "$p")
    if ! cmp -s "$p" "$DST/$f"; then cp -f "$p" "$DST/$f"; n=$((n+1)); else skip=$((skip+1)); fi
  done
fi
echo "同步完成: 复制 $n 个 | 已一致 $skip 个 | 目标目录总数: $(ls "$DST"/codex-*.json 2>/dev/null | wc -l)"
