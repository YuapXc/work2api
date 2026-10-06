#!/bin/sh
# 构建前端并同步到 go:embed 目录。改 WebUI 后跑这个，而不是裸 npm run build。
# （vite 输出在 webui/dist，而 embed 挂在 internal/app/webui——忘记同步会让
# 新功能"构建了却没生效"。）
set -e
cd "$(dirname "$0")"
npm run build:embed
echo "synced to internal/app/webui:"
grep -oE 'assets/index-[^"]+\.js' ../internal/app/webui/index.html
