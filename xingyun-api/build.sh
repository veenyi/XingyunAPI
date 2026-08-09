#!/bin/bash
# 行云API 一键打包脚本
# 用法：在 chat-1 根目录运行；产物 → pkg/xingyun-api_v<版本>.fpk
set -u
CHAT1="C:/Users/veenyi/Documents/QoderCN/2026-08-09/chat-1"
SRC="$CHAT1/xingyun-api"
DST="$CHAT1/xingyun-api-slim"
cd "$CHAT1" || { echo "chat-1 dir missing"; exit 1; }

echo "===== [1] robocopy 镜像复制（排除 .git/pkg/*.fpk） ====="
MSYS2_ARG_CONV_EXCL='*' robocopy "$SRC" "$DST" /MIR \
  /XD "$SRC/.git" "$SRC/pkg" \
  /XF *.fpk \
  /MT:16 /NFL /NDL /NJH /NJS /NP
RC=$?
if [ "$RC" -ge 8 ]; then echo "robocopy failed rc=$RC"; exit 1; fi
echo "copy done (rc=$RC)"

echo ""
echo "===== [2] manifest 版本确认 ====="
grep '^version' "$DST/manifest"

echo ""
echo "===== [3] 断言运行必需资源 ====="
for req in "$DST/app/bin/JoyCode2Api" "$DST/ICON.PNG" "$DST/cmd/main" "$DST/cmd/install_callback" "$DST/app/ui/config"; do
  if [ ! -e "$req" ]; then echo "FATAL: 运行资源缺失 $req"; exit 1; fi
done
echo "runtime resources verified"

echo ""
echo "===== [4] fnpack build ====="
./fnpack.exe build --directory "$DST" 2>&1 | tail -2

echo ""
echo "===== [5] 产物检查 ====="
ls -la xingyun-api.fpk 2>/dev/null

echo ""
echo "===== [6] 归档到 pkg/ ====="
VER=$(grep '^version' "$DST/manifest" | awk '{print $3}')
mkdir -p "$SRC/pkg"
if [ -f xingyun-api.fpk ]; then
  mv -f xingyun-api.fpk "$SRC/pkg/xingyun-api_v$VER.fpk"
  echo "归档: $SRC/pkg/xingyun-api_v$VER.fpk"
else
  echo "WARN: 未找到 xingyun-api.fpk"
  ls -la *.fpk 2>/dev/null
fi
