# yt-uploader

Tự động upload video lên YouTube bằng một AI agent điều khiển trình duyệt qua [Browser MCP](https://browsermcp.io).
Thả file video vào một thư mục, hệ thống sẽ lần lượt upload từng video, xoay vòng giữa nhiều kênh (Chrome profile), và nghỉ một khoảng cooldown sau mỗi vòng.

👉 Hướng dẫn từng bước: [HUONG_DAN_SU_DUNG.md](HUONG_DAN_SU_DUNG.md)

Code không gắn với một agent cụ thể nào: lệnh agent (Claude Code, Hermes, …) được cấu hình qua `HARNESS_BIN` / `HARNESS_ARGS`.

## Kiến trúc

```
videos-to-upload/ ──► watcher ──► Redis (job "pending")
                                     │
                                     ▼
                        worker: scheduler/dispatcher
                        (round-robin profile + cooldown)
                                     │  enqueue asynq
                                     ▼
                        worker: asynq server (concurrency = 1)
                                     │
                     mở Chrome đúng profile + nạp Browser MCP (CDP)
                                     │
                                     ▼
                        HARNESS_BIN <prompt> ──► JSON kết quả
```

| Thành phần | Mô tả |
|---|---|
| `cmd/watcher` | Quét `WATCH_DIR` theo chu kỳ. File video (`.mp4 .mov .mkv .avi .webm`) có kích thước không đổi `STABLE_CHECKS` lần liên tiếp thì được coi là đã ghi xong và tạo job `pending`. Dedupe theo đường dẫn tuyệt đối (`seen_files`). |
| `cmd/worker` | Chạy dispatcher và asynq server. Dispatcher chỉ thả **một** job mỗi lúc, gán profile theo cursor round-robin; hết một vòng profile mà còn job thì nghỉ `ROUND_COOLDOWN_SECONDS`. Handler mở Chrome, gọi harness, lưu URL video hoặc lỗi. |
| `cmd/statuscli` | CLI tra cứu trạng thái job trong Redis. |
| `internal/chrome` | Mở Chrome với `--user-data-dir`/`--profile-directory` của profile, bật remote debugging, nạp extension Browser MCP qua CDP (`Extensions.loadUnpacked`) và tự chọn tab (thay cho nút *Connect*). |
| `internal/harness` | Sinh prompt chuẩn, exec lệnh agent, parse dòng JSON kết quả cuối stdout. |
| `internal/scheduler` | Logic round-robin, cooldown, khôi phục job bị kẹt khi worker chết giữa chừng. |
| `internal/store` | Lưu job, cursor, cooldown, lock trong Redis. |

### Vòng đời job

`pending` → `queued` (đã gán profile, nằm trong asynq, kể cả lúc chờ retry) → `processing` → `done` | `failed`

- Retry do asynq quản lý (tối đa `MAX_ATTEMPTS` lần chạy), **giữ nguyên profile** — video không chuyển sang kênh khác.
- Cursor chỉ tăng khi một slot kết thúc (`done` hoặc `failed`), không tăng khi retry.
- Job `processing` không còn task trong asynq (worker crash) được đưa về `pending`; job `queued` bị mất task được enqueue lại với cùng TaskID.

## Yêu cầu

- Windows hoặc Linux (xem [Chạy trên Linux](#chạy-trên-linux))
- Go 1.24+
- Redis 7 (có sẵn `docker-compose.yml`)
- Google Chrome
- Bản unpacked của extension Browser MCP (thư mục có `manifest.json`)
- Một CLI agent có cấu hình Browser MCP làm MCP server

## Cài đặt

```bash
git clone git@github.com:ngxuanth/ytb-uploader.git
cd ytb-uploader

# Redis
docker compose up -d

# Build
go build -o watcher.exe   ./cmd/watcher
go build -o worker.exe    ./cmd/worker
go build -o statuscli.exe ./cmd/statuscli
```

### Chuẩn bị Chrome profile (mỗi profile = một kênh)

Mỗi thư mục con của `CHROME_PROFILES_DIR` (mặc định `./chrome-profile`) có file `Preferences` được coi là một kênh, sắp theo tên. Các profile `Default`, `Guest Profile`, `System Profile` bị bỏ qua.

Tạo profile và đăng nhập YouTube một lần bằng tay:

```powershell
& "C:\Program Files\Google\Chrome\Application\chrome.exe" `
  --user-data-dir="$PWD\chrome-profile" --profile-directory="kenh-a" https://www.youtube.com/
```

Kết quả:

```
chrome-profile/
├── kenh-a/Preferences
└── kenh-b/Preferences
```

## Cấu hình

Toàn bộ cấu hình đọc từ biến môi trường.

| Biến | Mặc định | Ý nghĩa |
|---|---|---|
| `REDIS_ADDR` | `127.0.0.1:6379` | Địa chỉ Redis |
| `REDIS_PASSWORD` | _(rỗng)_ | |
| `REDIS_DB` | `0` | |
| `WATCH_DIR` | `./videos-to-upload` | Thư mục watcher theo dõi |
| `POLL_INTERVAL_SECONDS` | `5` | Chu kỳ quét thư mục |
| `STABLE_CHECKS` | `3` | Số lần kích thước file không đổi trước khi tạo job |
| `HARNESS_BIN` | _(bắt buộc)_ | Lệnh agent sẽ được gọi |
| `HARNESS_ARGS` | _(rỗng)_ | Tham số cho agent, **tách bằng `\|`** (không qua shell). Không có `{prompt}` thì prompt được ghi vào stdin |
| `MCP_CONFIG_PATH` | `./browsermcp.json` | Thay cho `{mcp_config}` |
| `ALLOWED_TOOLS` | `mcp__browsermcp__*` | Thay cho `{allowed_tools}`, và được ghi vào prompt |
| `PERMISSION_MODE` | `acceptEdits` | Thay cho `{permission_mode}` |
| `AGENT_TIMEOUT_SECONDS` | `600` | Timeout của mỗi lần chạy |
| `MAX_ATTEMPTS` | `3` | Tổng số lần chạy tối đa cho một video |
| `VISIBILITY_DEFAULT` | `unlisted` | Chế độ hiển thị video |
| `CHROME_PROFILES_DIR` | `./chrome-profile` | Thư mục chứa các profile/kênh |
| `CHROME_BIN` | `C:\Program Files\Google\Chrome\Application\chrome.exe` nếu có, không thì `chrome` | |
| `BROWSERMCP_EXTENSION` | `C:\Users\Admin\Workspace\browsermcp-extension` | Thư mục extension unpacked |
| `CHROME_DEBUG_PORT` | `9222` | Cổng remote debugging |
| `ROUND_COOLDOWN_SECONDS` | `3600` | Thời gian nghỉ sau mỗi vòng profile |
| `LOG_DIR` | `./logs` | Thư mục log |

### Placeholder trong `HARNESS_ARGS`

`{prompt}`, `{file}`, `{profile}`, `{profile_dir}`, `{profile_directory}`, `{user_data_dir}`, `{mcp_config}`, `{allowed_tools}`, `{permission_mode}`, `{visibility}`

Process agent còn nhận thêm các biến môi trường: `HARNESS_FILE`, `HARNESS_PROFILE`, `HARNESS_PROFILE_DIR`, `HARNESS_PROFILE_DIRECTORY`, `HARNESS_USER_DATA_DIR`.

Ví dụ với Claude Code (MCP server khai báo trong `browsermcp.json`, xem `browsermcp.example.json`):

```powershell
$env:HARNESS_BIN  = "claude"
$env:HARNESS_ARGS = "-p|{prompt}|--mcp-config|{mcp_config}|--allowedTools|{allowed_tools}|--permission-mode|{permission_mode}"
```

Ví dụ với Hermes Agent (MCP server khai báo trong `~/.hermes/config.yaml`, prompt đi qua stdin):

```bash
export HARNESS_BIN=hermes
export HARNESS_ARGS='chat|--query-file|-|--oneshot|-Q|--yolo|-t|mcp-browsermcp'
```

Dù dùng agent nào, MCP server Browser MCP cũng phải có `BMCP_UPLOAD_DIR` trỏ vào `WATCH_DIR`, nếu không sẽ không upload được file.

### Hợp đồng với agent

Prompt yêu cầu agent upload file bằng Browser MCP trên đúng profile đã mở sẵn, dùng tên file làm tiêu đề, và in **dòng JSON cuối cùng** trên stdout:

```json
{"status":"done","url":"https://youtu.be/..."}
{"status":"error","reason":"mô tả ngắn gọn lỗi"}
```

Worker đọc dòng JSON hợp lệ cuối cùng. Không có dòng JSON, exit code lỗi, hoặc quá timeout đều được tính là một lần thất bại.

## Chạy

Mở hai terminal:

```powershell
.\watcher.exe
.\worker.exe
```

Rồi copy video vào `videos-to-upload/`.

Log dạng JSON được ghi ra stdout và `logs/watcher.log`, `logs/worker.log`.

Nhấn Ctrl+C để dừng worker; nhấn lần nữa để thoát ngay. Trên Windows, Chrome và agent do worker mở sẽ bị kill theo worker (Windows Job Object).

## Chạy trên Linux

Phần xử lý process Chrome được tách theo OS: `internal/chrome/proc_windows.go` (PowerShell, `taskkill`) và `internal/chrome/proc_other.go` (đọc `/proc`, `kill`). Phần còn lại dùng chung.

```bash
# Redis. Nếu cổng 6379 đã bị Redis khác chiếm:
#   REDIS_PORT=6380 docker compose up -d   và đặt REDIS_ADDR=127.0.0.1:6380
docker compose up -d

go build -o bin/watcher   ./cmd/watcher
go build -o bin/worker    ./cmd/worker
go build -o bin/statuscli ./cmd/statuscli
```

Tạo profile cho từng kênh và đăng nhập YouTube (đóng Chrome này trước khi chạy worker):

```bash
google-chrome --user-data-dir="$PWD/chrome-profile" --profile-directory=kenh-a https://www.youtube.com/
```

Biến môi trường, ví dụ file `.env.linux`:

```bash
export CHROME_BIN=google-chrome
export CHROME_PROFILES_DIR="$PWD/chrome-profile"
export BROWSERMCP_EXTENSION=/duong/dan/toi/browsermcp-extension   # mặc định là path Windows, bắt buộc đặt
export WATCH_DIR="$PWD/videos-to-upload"
export HARNESS_BIN=claude
export HARNESS_ARGS='-p|{prompt}|--mcp-config|{mcp_config}|--allowedTools|{allowed_tools}|--permission-mode|{permission_mode}'
```

Chạy:

```bash
source .env.linux
./bin/watcher &
./bin/worker
./bin/statuscli list
```

Khác biệt so với Windows:

- Linux không có Windows Job Object, nên Chrome **không** bị tắt khi worker thoát (Chrome chạy trong process group riêng để Ctrl+C không giết nó giữa lúc upload). Lần chạy sau, worker dùng lại Chrome nếu đúng profile, còn không thì tắt và mở lại. Agent (`HARNESS_BIN`) vẫn bị kill khi quá timeout.
- Worker sẽ tắt mọi Chrome đang dùng `--user-data-dir` là `CHROME_PROFILES_DIR`, nên đừng mở tay Chrome với thư mục đó trong lúc worker chạy. Chrome hằng ngày (`~/.config/google-chrome`) thì không bị động tới.

## Xem trạng thái

```bash
statuscli list          # 20 job gần nhất
statuscli list 100      # 100 job gần nhất
statuscli get <job_id>  # chi tiết một job (JSON)
```

## Test

```bash
go test ./...
```

## Dữ liệu trong Redis

| Key | Kiểu | Nội dung |
|---|---|---|
| `job:<id>` | Hash (field `data`) | JSON của job |
| `jobs:index` | Sorted set | ID job, score = thời điểm tạo |
| `seen_files` | Set | Đường dẫn file đã tạo job |
| `scheduler:cursor` | String | Số slot đã hoàn tất (dùng cho round-robin) |
| `scheduler:cooldown_until` | String | Unix timestamp hết cooldown |
| `scheduler:lock` | String | Lock của dispatcher |

Muốn upload lại một file đã xử lý thì xóa đường dẫn đó khỏi `seen_files`:

```bash
redis-cli SREM seen_files "C:\\path\\to\\video.mp4"
```
