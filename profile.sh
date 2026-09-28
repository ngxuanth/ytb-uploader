#!/usr/bin/env bash
# Tạo hoặc mở một Chrome profile trong profile/ để đăng nhập YouTube một lần.
#   ./profile.sh <tên-profile>
# Đăng nhập tài khoản Google của kênh, mở studio.youtube.com cho chắc, rồi đóng
# Chrome. Sau đó dùng <tên-profile> làm "profile" khi gọi POST /uploads.
set -euo pipefail
cd "$(dirname "$0")"
name="${1:?cách dùng: ./profile.sh <tên-profile>}"
case "$name" in */*|.*) echo "tên profile không hợp lệ: $name" >&2; exit 1;; esac
if pgrep -f -- "--user-data-dir=$PWD/profile" > /dev/null; then
  echo "Chrome upload đang chạy trên profile/; đóng nó (hoặc ./stop.sh) trước." >&2
  exit 1
fi
mkdir -p profile
exec "${CHROME_BIN:-google-chrome}" --user-data-dir="$PWD/profile" --profile-directory="$name" \
  --no-first-run --no-default-browser-check https://studio.youtube.com
