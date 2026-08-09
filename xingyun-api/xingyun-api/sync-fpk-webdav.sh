#!/bin/bash
# sync-fpk-webdav.sh — 同步行云API FPK + 更新记录到 alist WebDAV
# 用法：bash sync-fpk-webdav.sh [指定.fpk 路径（默认 pkg 下最新）]；-md 仅更 MD
set -u
CHAT1="C:/Users/veenyi/Documents/QoderCN/2026-08-09/chat-1"
PKG_DIR="$CHAT1/xingyun-api/pkg"
UPDATE_MD="$CHAT1/xingyun-api/UPDATE-LOG.md"
WEBDAV_BASE="http://nas.aio.run:5244/dav/FnosAPP"
WD_USER="tim"
WD_PASS=""
CRED_FILE="$HOME/.qwenworkcn/webdav-credentials"
if [ -f "$CRED_FILE" ]; then
  WD_PASS=$(head -1 "$CRED_FILE" 2>/dev/null | tr -d '\r\n')
fi
if [ -z "$WD_PASS" ]; then
  echo "ERROR: 未找到 WebDAV 密码（请写入 $CRED_FILE 第一行）"
  exit 1
fi

MD_ONLY=0
[ "${1:-}" = "-md" ] && MD_ONLY=1

if [ -f "$UPDATE_MD" ]; then
  echo "===== 同步更新记录 → $WEBDAV_BASE/xingyun-api.md ====="
  curl -s --connect-timeout 20 --max-time 120 -u "$WD_USER:$WD_PASS" -T "$UPDATE_MD" "$WEBDAV_BASE/xingyun-api.md" -o /dev/null -w 'PUT: %{http_code}\n'
fi
[ "$MD_ONLY" = "1" ] && { echo "（-md 模式完成）"; exit 0; }

FPK="${1:-}"
if [ -z "$FPK" ]; then
  FPK=$(ls -t "$PKG_DIR"/xingyun-api_v*.fpk 2>/dev/null | head -1)
fi
[ -z "$FPK" ] || [ ! -f "$FPK" ] && { echo "ERROR: FPK 不存在: $FPK"; exit 1; }

NAME=$(basename "$FPK")
SIZE=$(stat -c '%s' "$FPK")
echo "===== 同步 $NAME ($((SIZE/1048576))MB) → $WEBDAV_BASE/ ====="
curl -s --connect-timeout 20 --max-time 600 -u "$WD_USER:$WD_PASS" -T "$FPK" "$WEBDAV_BASE/$NAME" -o /dev/null -w 'PUT: %{http_code}\n'
RC=$?
if [ "$RC" -ne 0 ]; then echo "ERROR: 上传失败 rc=$RC"; exit 1; fi

REMOTE=$(curl -s --connect-timeout 20 --max-time 30 -u "$WD_USER:$WD_PASS" -I "$WEBDAV_BASE/$NAME" 2>/dev/null | grep -i content-length | tr -d '\r' | awk '{print $2}')
echo "本地大小: $SIZE | 远端大小: ${REMOTE:-?}"
if [ "$REMOTE" = "$SIZE" ]; then
  echo "SYNC OK  $NAME 已同步到 WebDAV"
else
  echo "SYNC MISMATCH 请检查网络后重试"
  exit 1
fi
