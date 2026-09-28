# Hướng dẫn sử dụng yt-uploader

Tài liệu này hướng dẫn từng bước để cài đặt và vận hành hàng ngày. Kiến trúc và bảng biến môi trường đầy đủ nằm trong [README.md](README.md).

Tóm tắt: bỏ video vào `videos-to-upload/`, hệ thống upload từng video một lên YouTube, lần lượt qua từng kênh (mỗi kênh là một Chrome profile). Sau mỗi vòng qua hết các kênh thì nghỉ 1 giờ.

---

## 1. Chuẩn bị

| Thứ cần có | Kiểm tra |
|---|---|
| Go 1.24+ | `go version` |
| Docker (để chạy Redis) | `docker compose version` |
| Google Chrome | `google-chrome --version` (Linux) |
| Node.js (để chạy MCP server Browser MCP) | `node --version` |
| Extension Browser MCP (bản unpacked, có `manifest.json`) | thư mục `browsermcp-extension/` |
| MCP server Browser MCP (`bmcp`) đã build | có file `bmcp/dist/index.js` |
| Một agent CLI: Claude Code **hoặc** Hermes Agent | `claude --version` / `hermes --version` |

Ví dụ bố cục thư mục (các đường dẫn dưới đây đều giả định như vậy):

```
~/Workspace/BrowserMCP/
├── browsermcp-extension/   # extension unpacked
├── bmcp/                   # MCP server (npm install && npm run build)
└── ytb-uploader/           # repo này
```

## 2. Cài đặt (làm một lần)

### 2.1. Build

```bash
cd ~/Workspace/BrowserMCP/ytb-uploader
go build -o bin/watcher   ./cmd/watcher
go build -o bin/worker    ./cmd/worker
go build -o bin/statuscli ./cmd/statuscli
```

Trên Windows thì đổi tên output thành `.exe`.

### 2.2. Chạy Redis

```bash
docker compose up -d
```

Nếu cổng 6379 đã bị Redis khác chiếm (lỗi `port is already allocated`), đổi cổng khi chạy:

```bash
REDIS_PORT=6380 docker compose up -d
# và nhớ đặt REDIS_ADDR=127.0.0.1:6380 ở bước 2.5
```

### 2.3. Tạo profile cho từng kênh

Mỗi kênh YouTube là một thư mục con trong `chrome-profile/`. Tạo từng kênh bằng lệnh sau, đăng nhập đúng tài khoản YouTube, rồi **đóng Chrome**:

```bash
google-chrome --user-data-dir="$PWD/chrome-profile" --profile-directory=kenh-a https://www.youtube.com/
google-chrome --user-data-dir="$PWD/chrome-profile" --profile-directory=kenh-b https://www.youtube.com/
```

Windows (PowerShell):

```powershell
& "C:\Program Files\Google\Chrome\Application\chrome.exe" `
  --user-data-dir="$PWD\chrome-profile" --profile-directory="kenh-a" https://www.youtube.com/
```

Lưu ý:

- Đặt tên thư mục đơn giản (`kenh-a`, `kenh-b`, …). Các kênh được upload lần lượt **theo thứ tự tên**.
- Không dùng tên `Default`: hệ thống bỏ qua profile này.
- Kiểm tra: mỗi thư mục kênh phải có file `Preferences`.

### 2.4. Cấu hình agent

#### Cách A: Claude Code

Tạo `browsermcp.json` từ file mẫu, sửa lại hai đường dẫn cho đúng máy của bạn:

```bash
cp browsermcp.example.json browsermcp.json
```

```json
{
  "mcpServers": {
    "browsermcp": {
      "command": "node",
      "args": ["/home/<user>/Workspace/BrowserMCP/bmcp/dist/index.js"],
      "env": {
        "BMCP_UPLOAD_DIR": "/home/<user>/Workspace/BrowserMCP/ytb-uploader/videos-to-upload"
      }
    }
  }
}
```

> `BMCP_UPLOAD_DIR` **bắt buộc** phải trỏ vào đúng thư mục video. Nếu không đặt, Browser MCP tắt tool `browser_upload_file` và agent sẽ không upload được.

#### Cách B: Hermes Agent

Thêm vào `~/.hermes/config.yaml`:

```yaml
mcp_servers:
  browsermcp:
    command: "node"
    args: ["/home/<user>/Workspace/BrowserMCP/bmcp/dist/index.js"]
    env:
      BMCP_UPLOAD_DIR: "/home/<user>/Workspace/BrowserMCP/ytb-uploader/videos-to-upload"
```

Thử trước bằng tay xem Hermes đã thấy tool của Browser MCP chưa:

```bash
hermes chat -q "liệt kê các tool browser bạn có" --oneshot -t mcp-browsermcp
```

### 2.5. Tạo file biến môi trường

Tạo `.env.sh` (file này chỉ để trên máy, không commit):

```bash
# Redis
export REDIS_ADDR=127.0.0.1:6379          # đổi 6380 nếu đã đổi REDIS_PORT

# Chrome
export CHROME_BIN=google-chrome
export CHROME_PROFILES_DIR="$PWD/chrome-profile"
export BROWSERMCP_EXTENSION="$HOME/Workspace/BrowserMCP/browsermcp-extension"   # bắt buộc trên Linux

# Thư mục video
export WATCH_DIR="$PWD/videos-to-upload"

# Upload
export VISIBILITY_DEFAULT=unlisted         # public | unlisted | private
export ROUND_COOLDOWN_SECONDS=3600         # nghỉ bao lâu sau mỗi vòng kênh
export AGENT_TIMEOUT_SECONDS=600           # tối đa bao lâu cho 1 video
export MAX_ATTEMPTS=3                      # số lần thử cho 1 video

# --- Chọn MỘT trong hai agent ---

# Claude Code
export HARNESS_BIN=claude
export HARNESS_ARGS='-p|{prompt}|--mcp-config|{mcp_config}|--allowedTools|{allowed_tools}|--permission-mode|{permission_mode}'

# Hermes Agent
# export HARNESS_BIN=hermes
# export HARNESS_ARGS='chat|--query-file|-|--oneshot|-Q|--yolo|-t|mcp-browsermcp'
```

`HARNESS_ARGS` dùng dấu `|` để tách các tham số. Nếu không có `{prompt}` thì prompt được gửi qua stdin (Hermes đọc bằng `--query-file -`).

## 3. Chạy

Mở hai terminal trong thư mục repo:

```bash
# Terminal 1
source .env.sh
./bin/watcher
```

```bash
# Terminal 2
source .env.sh
./bin/worker
```

Worker in ra danh sách kênh tìm được (`"profiles":[...]`). Nếu thấy lỗi `khong tim thay chrome profile` thì quay lại bước 2.3.

Upload: copy video (`.mp4 .mov .mkv .avi .webm`) vào `videos-to-upload/`. Không cần đợi copy xong, watcher tự đợi đến khi kích thước file không đổi nữa.

- Tiêu đề video là tên file (bỏ đuôi), nên hãy đặt tên file theo tiêu đề mong muốn.
- Chế độ hiển thị lấy theo `VISIBILITY_DEFAULT`.

Trong lúc chạy, **không mở tay Chrome với thư mục `chrome-profile/`**, vì worker sẽ tắt nó. Chrome thường ngày của bạn thì không bị ảnh hưởng.

## 4. Theo dõi

```bash
source .env.sh
./bin/statuscli list          # 20 video gần nhất
./bin/statuscli list 100
./bin/statuscli get <job_id>  # chi tiết một video
```

Trạng thái:

| State | Nghĩa |
|---|---|
| `pending` | Đã phát hiện, đang chờ tới lượt |
| `queued` | Đã gán kênh, đang chờ chạy hoặc chờ retry |
| `processing` | Agent đang upload |
| `done` | Xong, có link video |
| `failed` | Hết số lần thử, xem cột lỗi |

Log chi tiết (JSON) nằm ở `logs/watcher.log` và `logs/worker.log`:

```bash
tail -f logs/worker.log | grep -E 'upload thanh cong|that bai|cooldown'
```

## 5. Dừng

Bấm Ctrl+C ở mỗi terminal. Bấm thêm lần nữa để thoát ngay.

- Video đang upload dở sẽ được chạy lại ở lần khởi động sau.
- Trên Linux, Chrome vẫn mở sau khi worker thoát; lần chạy sau worker sẽ dùng lại hoặc tự tắt nó.

Dừng Redis: `docker compose down` (dữ liệu vẫn giữ trong `redis_data/`).

## 6. Thao tác thường gặp

**Thêm hoặc bớt kênh:** tạo hoặc xoá thư mục trong `chrome-profile/` (như bước 2.3), rồi **restart worker**. Worker chỉ quét profile lúc khởi động. Nếu muốn vòng xoay bắt đầu lại từ kênh đầu tiên:

```bash
docker exec lovediary_redis redis-cli DEL scheduler:cursor
```

**Bỏ qua thời gian nghỉ, upload tiếp ngay:**

```bash
docker exec lovediary_redis redis-cli DEL scheduler:cooldown_until
```

**Upload lại một video đã xử lý:** watcher nhớ file theo đường dẫn tuyệt đối. Xoá đường dẫn đó khỏi danh sách rồi đổi tên hoặc copy lại file:

```bash
docker exec lovediary_redis redis-cli SREM seen_files "/duong/dan/tuyet/doi/video.mp4"
```

**Kênh bị đăng xuất:** dừng worker, mở lại profile đó bằng lệnh ở bước 2.3, đăng nhập, đóng Chrome, rồi chạy lại worker.

## 7. Xử lý lỗi

| Hiện tượng | Nguyên nhân / cách xử lý |
|---|---|
| `khong ket noi duoc redis` | Redis chưa chạy, hoặc `REDIS_ADDR` sai cổng |
| `khong tim thay chrome profile` | `CHROME_PROFILES_DIR` sai, hoặc thư mục kênh chưa có `Preferences` (chưa mở lần nào) |
| `HARNESS_BIN trong` | Chưa `source .env.sh` |
| `khong thay browsermcp extension tai ...` | `BROWSERMCP_EXTENSION` sai, hoặc thư mục không có `manifest.json` |
| `chrome debug port 9222 khong san sang` | Cổng 9222 đang bị chương trình khác dùng. Đổi `CHROME_DEBUG_PORT` |
| `chua thay extension Browser MCP Local` | Extension không lên. Xoá Chrome đang treo của `chrome-profile/` rồi chạy lại |
| `khong parse duoc JSON tu agent` | Agent không in dòng JSON kết quả ở cuối. Xem `stdout` / `stderr` trong `logs/worker.log`; kiểm tra lại `HARNESS_ARGS` |
| Agent báo không upload được file | Kiểm tra `BMCP_UPLOAD_DIR` trỏ đúng `WATCH_DIR` |
| `agent bi huy do vuot qua thoi gian` | Video lớn hoặc mạng chậm. Tăng `AGENT_TIMEOUT_SECONDS` |
| Video không được nhận | Sai đuôi file, hoặc file đã có trong `seen_files` (xem mục 6) |
