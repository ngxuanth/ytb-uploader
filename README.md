# YouTube uploader

Upload video lên YouTube Studio bằng một agent LLM (hermes, claude, codex…) điều khiển Chrome thật đã đăng nhập sẵn.

```
POST /uploads ─► server ─(WebSocket)─► launcher ─► hermes ─┬─ task_mcp / report_mcp ─► server  (nhận task, báo tiến độ)
                                                           ├─ chrome_mcp ─► Chrome (mở profile, cài extension)
                                                           └─ bmcp ─(WebSocket)─► extension ─► tab YouTube Studio
```

- **server** (`cmd/server`): nhận yêu cầu upload qua REST, giao task cho launcher, phục vụ `task_mcp` / `report_mcp`, và lưu trạng thái task vào `data/server/tasks.json`. Tài liệu API ở [docs/server-api.md](docs/server-api.md).
- **launcher** (`cmd/launcher`): kết nối tới server, tải file của task về, rồi mở một phiên agent cho mỗi task, kèm 4 MCP server của phiên đó.
- **bmcp** (`resource/bmcp`): MCP server `browser_*` (Browser MCP đã sửa: `browser_upload_file`, `browser_evaluate`, `browser_scroll`…).
- **extension** (`resource/browsermcp-extension`): extension Chrome "Browser MCP Local", nhận lệnh từ bmcp và thao tác trên tab.
- **prompt** (`pkg/prompt`): các bước upload trên YouTube Studio mà agent làm theo.

## Yêu cầu

- Linux x86-64 có màn hình (X11 hoặc Wayland có XWayland). Chrome chạy với `--ozone-platform=x11`, vì trên Wayland cửa sổ bị che thì không vẽ khung hình mới và extension bị treo.
- Google Chrome (`google-chrome`), Node.js ≥ 20.
- Một agent CLI. Mặc định là `hermes`; cũng có preset `claude`, `cursor`, `codex`.
- Chỉ khi build từ source: Go ≥ 1.26 và npm.

## Cài đặt

```sh
./build.sh                 # chỉ khi build từ source; gói đóng sẵn đã có binary
./profile.sh kenh1         # mở Chrome trên profile/kenh1, đăng nhập Google của kênh rồi đóng Chrome
```

`profile/` là thư mục dữ liệu Chrome (user-data-dir) dùng chung cho mọi profile. Mỗi profile là một thư mục con có file `Preferences`. Thư mục này chứa cookie đăng nhập: đừng commit hay chia sẻ nó.

Với hermes, dán khối trong [hermes/mcp_servers.yaml.template](hermes/mcp_servers.yaml.template) vào `mcp_servers:` của `~/.hermes/config.yaml`. Các preset khác nhận MCP server trực tiếp từ launcher nên không cần bước này.

## Chạy

```sh
./start.sh                                   # server trên 127.0.0.1:8090 + launcher (hermes), log trong data/
curl 127.0.0.1:8090/agent                    # launcher đã kết nối chưa
curl -XPOST 127.0.0.1:8090/uploads -H 'content-type: application/json' -d '{
  "profile": "kenh1", "channel": "UC...", "video": "/đường/dẫn/video.mp4",
  "title": "Tiêu đề", "description": "Mô tả", "visibility": "private"
}'
curl 127.0.0.1:8090/tasks/<task_id>          # trạng thái, các lần gọi report_mcp, finish_reported, session_ended
curl -XPOST 127.0.0.1:8090/tasks/<task_id>/retry
./stop.sh
```

Biến cho `start.sh`: `ADDR` (mặc định `127.0.0.1:8090`), `HARNESS` (mặc định `hermes`), `MODEL` (mặc định lấy model trong config của agent).

Chạy thử một lần, không cần server:

```sh
./launcher try -video a.mp4 -channel UC... -profile kenh1 -title "Tiêu đề" [-harness hermes]
./launcher chrome-check -profile kenh1       # chỉ kiểm tra Chrome + extension, không dùng LLM
```

Mỗi phiên có thư mục riêng `data/run/<task_id>/` chứa `prompt.txt`, `transcript.jsonl` (mọi lệnh tool agent đã gọi) và `upload/`.

## Lưu ý

- API không có xác thực: chỉ để server lắng nghe trên `127.0.0.1`.
- Mỗi profile chỉ chạy một task một lúc. Mọi profile dùng chung một Chrome với cổng debug 9222.
- Khi file trong `resource/browsermcp-extension` thay đổi, lần `extension_setup` tiếp theo tự reload extension.
- Khi retry một task đã tạo video, agent được giao `existing_video_id` và sửa tiếp video đó chứ không upload lại. bmcp cũng từ chối attach cùng một file hai lần trong một phiên.
