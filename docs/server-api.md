# Server API

`cmd/server` nhận yêu cầu upload qua REST, xếp mỗi task vào hàng đợi của Chrome profile của nó, giao lần lượt từng task cho launcher qua WebSocket, và cập nhật trạng thái task mỗi khi agent (hermes) gọi `task_mcp` / `report_mcp`. Tất cả task được lưu trong một file JSON, nên khởi động lại server vẫn còn task.

- [Chạy server](#chạy-server)
- [Hàng đợi theo profile](#hàng-đợi-theo-profile)
- [Vòng đời một task](#vòng-đời-một-task)
- [Đối tượng Task](#đối-tượng-task)
- [REST API](#rest-api)
- [Endpoint cho agent](#endpoint-cho-agent)
- [Ví dụ](#ví-dụ)

## Chạy server

```
./server [-addr 127.0.0.1:8090] [-profiles profile] [-data data/server/tasks.json] [-finish-grace 45s]
./launcher run -server ws://127.0.0.1:8090/ws
```

| Cờ | Mặc định | Ý nghĩa |
|---|---|---|
| `-addr` | `127.0.0.1:8090` | địa chỉ lắng nghe |
| `-profiles` | `profile` | thư mục Chrome user-data-dir; mỗi thư mục con có file `Preferences` là một profile hợp lệ cho `POST /uploads` |
| `-data` | `data/server/tasks.json` | file lưu task; ghi file tạm rồi rename sau mỗi thay đổi |
| `-finish-grace` | `45s` | sau `task_finish`, nếu phiên hermes chưa thoát trong khoảng này thì server gửi `cancel` cho launcher để dừng nó. `0` là không bao giờ |

API không có xác thực và chỉ nên lắng nghe trên `127.0.0.1`. Mọi body đều là JSON. Lỗi luôn có dạng `{"error": "..."}`.

## Hàng đợi theo profile

Mỗi Chrome profile là một hàng đợi (topic). Mọi task được **publish** vào hàng đợi của profile đó; launcher **subscribe** các profile nó khai báo trong `hello`. Trong một profile, mỗi lúc chỉ có **một** task chạy, vì một profile chỉ có một Chrome và một extension. Task sau chỉ được giao khi task trước đã **giải phóng** profile.

```
POST /uploads ──► hàng đợi "kenh1": [t3, t4]   đang chạy: t2 ──assign──► launcher (hello: kenh1, kenh2)
POST /uploads ──► hàng đợi "kenh2": []         đang chạy: t5 ──assign──► launcher
                  hàng đợi "kenh3": [t6]       chưa launcher nào giữ kenh3: t6 chờ
```

- **Thứ tự:** vào trước ra trước (FIFO) trong từng profile. `POST /tasks/:id/retry?front=true` chen task lên đầu hàng.
- **Giao task:** server giao ngay khi profile rảnh và có launcher giữ profile đó. Việc kiểm tra diễn ra khi có task mới, khi launcher gửi `hello`, và mỗi khi một task giải phóng profile. Có nhiều launcher cùng giữ một profile thì server chọn launcher kết nối gần nhất.
- **Giải phóng profile** xảy ra khi:
  - launcher báo `session_ended`, tức phiên hermes đã thoát và Chrome đã rảnh. `task_finish` thôi thì **chưa** giải phóng, vì hermes có thể vẫn đang dùng Chrome. Nếu phiên vẫn chạy sau `-finish-grace` (mặc định 45 giây) kể từ `task_finish`, server tự gửi `cancel` cho launcher để hàng đợi không bị kẹt. Status của task giữ nguyên.
  - launcher từ chối task (`reject`).
  - task bị cancel trong lúc launcher giữ nó không còn kết nối.
- **Launcher mất kết nối:** task đang chạy vẫn giữ profile, vì phiên có thể còn chạy và launcher sẽ kết nối lại. Khi launcher đó gửi `hello` mà không còn báo task này là đang chạy (`running_task_id`), tức nó đã khởi động lại, task chuyển sang `LOST` và profile được giải phóng. Launcher không quay lại thì dùng `cancel` để giải phóng.
- Hàng đợi được suy ra từ các task đang `QUEUED` trong `tasks.json`, nên server khởi động lại vẫn giữ nguyên hàng đợi.

## Vòng đời một task

```
POST /uploads ─► QUEUED ─► ASSIGNED ─► PREPARING ─► ATTACHING ─► FILLING_METADATA ─► UPLOADING ─► PROCESSING ─► PUBLISHING ─► DONE
                               │            (theo step của task_report)
                               ├─► FAILED            task_finish status=failed, launcher từ chối, hoặc giao task thất bại
                               ├─► NEEDS_ATTENTION   task_finish status=needs_attention (cần người: đăng nhập, sai kênh, giới hạn upload...)
                               ├─► LOST              phiên hermes thoát mà chưa gọi task_finish, hoặc launcher khởi động lại (error_code AGENT_LOST)
                               └─► CANCELLED         POST /tasks/:id/cancel (cả khi còn QUEUED)

FAILED / NEEDS_ATTENTION / LOST / CANCELLED ── POST /tasks/:id/retry ──► QUEUED (attempt + 1)
```

Các trạng thái **kết thúc** là `DONE`, `FAILED`, `CANCELLED`: không thể cancel nữa. `NEEDS_ATTENTION` và `LOST` chưa kết thúc: vẫn cancel hoặc retry được.

Server cập nhật task như sau:

| Nguồn | Thay đổi |
|---|---|
| `task_claim` | thêm event `task_claim`. Nếu lần chạy này đã có `video_id`, claim trả thêm `existing_video_id`, để LLM nhận tiếp việc từ script không upload lại |
| `task_report` | `step`, `message`, `progress` (chỉ khi > 0). Nếu `step` là một trong `DOWNLOADING, PREPARING, ATTACHING, FILLING_METADATA, UPLOADING, PROCESSING, PUBLISHING` thì `status` = `step` |
| `task_video_created` | `video_id`, `video_url` (mặc định `https://youtu.be/<video_id>`) |
| `task_finish` | ghi `finish`. `done` → `DONE`, `progress` = 100. `needs_attention` → `NEEDS_ATTENTION`. Giá trị khác → `FAILED`. `error_code`/`error` lấy từ `error_code`/`reason`. Task đã `CANCELLED` thì giữ nguyên |
| launcher từ chối task (`reject`) | `FAILED`, `error` = lý do; giải phóng profile |
| launcher báo phiên kết thúc (`session_ended`) | ghi `session`. Nếu chưa có `finish` và task chưa kết thúc thì `LOST`, `error_code` = `AGENT_LOST`. Giải phóng profile, task kế tiếp được giao |

Sau `task_finish`, `session_ended`, `cancel` hoặc `reject`, mọi lần gọi task_* tiếp theo của lần chạy đó đều nhận `control: "stop"`. Các lần gọi này vẫn được ghi vào `events`.

## Đối tượng Task

`GET /tasks/:id`, `POST /uploads`, `POST /tasks/:id/retry` và `POST /tasks/:id/cancel` trả về một task đầy đủ. `GET /tasks` trả bản tóm tắt: giống hệt nhưng không có `events`.

```json
{
  "task_id": "srv-02d3e23e",
  "attempt": 1,
  "profile": "isophtalic",
  "channel": "UCbHiIpWVK_jqIbinKacp2Ig",
  "title": "Đây là video test",
  "visibility": "private",
  "status": "UPLOADING",
  "step": "UPLOADING",
  "progress": 40,
  "message": "Uploading 40%",
  "video_id": "KUgjqeWrT3Y",
  "video_url": "https://youtu.be/KUgjqeWrT3Y",
  "queue_position": 0,
  "agent_id": "ThangNX",
  "finish_reported": false,
  "session_ended": false,
  "last_event": {"event": "task_report", "attempt": 1, "step": "UPLOADING", "progress": 40, "message": "Uploading 40%", "at": "2026-09-28T17:48:02+07:00"},
  "events": [ ... ],
  "created_at": "2026-09-28T17:45:07+07:00",
  "updated_at": "2026-09-28T17:48:02+07:00"
}
```

Các trường trống bị bỏ khỏi JSON, trừ `attempt`, `progress`, `queue_position`, `finish_reported`, `session_ended` và các mốc thời gian. Ví dụ `finish` chỉ xuất hiện sau khi agent gọi `task_finish`, `session` chỉ xuất hiện sau khi phiên hermes thoát.

| Trường | Ý nghĩa |
|---|---|
| `task_id` | id do server cấp (`srv-xxxxxxxx`) |
| `attempt` | lần chạy hiện tại, bắt đầu từ 1, tăng sau mỗi lần retry |
| `profile`, `channel` | Chrome profile và kênh YouTube (UC…). `channel` trống khi upload không chỉ định kênh: agent dùng kênh mặc định của profile |
| `title`, `visibility` | metadata gửi cho agent |
| `status` | trạng thái, xem [vòng đời](#vòng-đời-một-task) |
| `step`, `progress`, `message` | lần `task_report` gần nhất của lần chạy hiện tại |
| `video_id`, `video_url` | video đã tạo trên YouTube; giữ lại qua các lần retry |
| `existing_video_id` | có khi retry mà lần trước đã tạo video: agent sửa tiếp video này, không upload lại |
| `queue_position` | vị trí trong hàng đợi của profile khi `status` = `QUEUED`: 1 là task được giao tiếp theo. Bằng 0 khi không nằm trong hàng đợi |
| `agent_id` | launcher nhận lần chạy hiện tại |
| `error_code`, `error` | lỗi gần nhất, ví dụ `LOGIN_REQUIRED`, `UPLOAD_LIMIT`, `STEP_FAILED`, `AGENT_LOST` |
| `finish_reported` | `true` khi agent đã gọi `task_finish` trong lần chạy hiện tại |
| `finish` | nội dung `task_finish`: `status`, `error_code`, `reason`, `video_id`, `at` |
| `session_ended` | `true` khi launcher báo phiên hermes của lần chạy hiện tại đã thoát |
| `session` | `exit_code`, `killed_by`, `duration_ms`, `error`, `ended_at`, `runner` (`playbook`: script làm hết; `playbook+llm`: script chuyển một số bước cho LLM; `llm`: phiên LLM làm cả task), `failed_steps` (các bước script đã chuyển cho LLM, theo thứ tự), `handoffs`. `killed_by` trống nếu hermes tự thoát; nếu launcher dừng nó thì là `timeout` (quá hạn task), `stalled` (lâu không có hoạt động) hoặc `interrupted` (task bị cancel, kể cả `stop_after_finish`, hoặc launcher bị tắt) |
| `last_event` | dòng cuối của `events` |
| `events` | dòng thời gian, tối đa 200 dòng cuối, gồm mọi lần chạy |

**Event** có các trường `event`, `attempt`, `step`, `progress`, `status`, `video_id`, `message`, `at`. Các giá trị của `event`:

| `event` | Từ đâu |
|---|---|
| `queued` | task được đưa vào hàng đợi (`POST /uploads`) |
| `assigned` | server đã giao task cho launcher (`message` = `agent_id`) |
| `session_lost` | launcher kết nối lại mà không còn phiên của task này |
| `stop_after_finish` | phiên vẫn chạy sau `-finish-grace` kể từ `task_finish`; server đã gửi `cancel` cho launcher |
| `task_claim` | agent gọi `task_claim` |
| `task_report` | agent hoặc script của launcher gọi `task_report` (`step`, `progress`, `message`; script ghi `message` bắt đầu bằng `[playbook]`) |
| `task_video_created` | agent gọi `task_video_created` (`video_id`, `message` = URL) |
| `task_finish` | agent gọi `task_finish` (`status`, `video_id`, `message` = reason) |
| `rejected` | launcher từ chối task hoặc lỗi trước khi chạy hermes, ví dụ tải file hỏng hay profile đang bận (`message` là lý do) |
| `session_ended` | phiên hermes đã thoát (`message` ví dụ `exit 130, killed by timeout`) |
| `retry` | `POST /tasks/:id/retry`: task vào lại hàng đợi (`video_id` = `existing_video_id`) |
| `cancel` | `POST /tasks/:id/cancel` (`message`: `removed from the queue`, hoặc `agent not told: ...` nếu không báo được cho launcher) |

## REST API

### `POST /uploads`

Tạo task upload và đưa vào cuối hàng đợi của `profile`. Nếu profile đang rảnh và có launcher giữ nó, task được giao ngay trong lần gọi này. Không cần launcher đang kết nối: task sẽ chờ tới khi có launcher.

Body:

| Trường | Bắt buộc | Ý nghĩa |
|---|---|---|
| `profile` | có | tên thư mục profile trong `-profiles` |
| `channel` | không | id kênh `UC…`. Bỏ trống thì agent mở YouTube Studio và dùng kênh mặc định của profile |
| `video` | có | đường dẫn file video **trên máy chạy server** |
| `thumbnail` | không | đường dẫn ảnh thumbnail trên máy chạy server |
| `title` | không | mặc định là tên file video, bỏ đuôi |
| `description` | không | |
| `visibility` | không | `public`, `unlisted` hoặc `private` (mặc định) |

```
curl -XPOST 127.0.0.1:8090/uploads -H 'content-type: application/json' -d '{
  "profile": "isophtalic",
  "video": "/home/me/videos/a.mp4", "title": "Đây là video test", "visibility": "private"
}'
```

| Mã | Khi nào |
|---|---|
| `202` | đã tạo task; body là task. `status` = `ASSIGNED` nếu đã được giao ngay, còn không thì `QUEUED` kèm `queue_position` |
| `400` | thiếu trường, `visibility` sai, tên profile không hợp lệ, file video/thumbnail không đọc được. Profile không tồn tại thì trả `{"error": "unknown profile", "profiles": [...]}` |

### `GET /queues`

Hàng đợi của mọi profile trong `-profiles`, cộng các profile đang có task chạy hoặc chờ.

```json
{"queues": [
  {"profile": "kenh1", "running": "srv-0a1b2c3d", "queued": ["srv-11111111", "srv-22222222"], "subscribed": true, "agent_id": "ThangNX"},
  {"profile": "kenh3", "queued": ["srv-33333333"], "subscribed": false}
]}
```

| Trường | Ý nghĩa |
|---|---|
| `running` | task đang giữ profile (đã giao, phiên chưa kết thúc) |
| `queued` | task đang chờ, theo thứ tự sẽ được giao |
| `subscribed`, `agent_id` | có launcher đang kết nối giữ profile này không, và là launcher nào |

### `GET /tasks`

Danh sách task, mới nhất trước, không kèm `events`.

| Query | Ý nghĩa |
|---|---|
| `status` | lọc theo trạng thái, nhiều giá trị cách nhau bằng dấu phẩy, không phân biệt hoa thường: `?status=failed,lost` |
| `profile` | lọc theo profile: `?profile=isophtalic` |

```
curl '127.0.0.1:8090/tasks?status=FAILED,LOST,NEEDS_ATTENTION'
```

`200`:
```json
{"tasks": [ {"task_id": "srv-02d3e23e", "status": "LOST", "finish_reported": false, "session_ended": true, "...": "..."} ]}
```

### `GET /tasks/:id`

Một task đầy đủ, kèm `events`.

| Mã | Khi nào |
|---|---|
| `200` | task |
| `404` | `{"error": "no such task"}` |

### `POST /tasks/:id/retry`

Chạy lần mới cho task đang `FAILED`, `CANCELLED`, `LOST` hoặc `NEEDS_ATTENTION`. Server sẽ:

1. tăng `attempt` và cấp token mới, nên phiên cũ nếu còn sót lại không ghi được vào lần mới;
2. đặt `existing_video_id` = `video_id` nếu lần trước đã tạo video, để agent mở `https://studio.youtube.com/video/<id>/edit` và làm tiếp chứ không upload lại;
3. xoá `step`, `progress`, `message`, `error_code`, `error`, `finish`, `session` của lần trước (`events` và `video_id` vẫn giữ);
4. đưa task vào lại hàng đợi của profile: mặc định ở cuối, còn với `?front=true` thì ở đầu hàng. Profile rảnh thì task được giao ngay.

Không có body. Server không tự retry: chỉ retry khi gọi API này.

| Mã | Khi nào |
|---|---|
| `202` | body là task, `status` = `ASSIGNED` (đã giao ngay) hoặc `QUEUED` |
| `404` | không có task |
| `409` | task đang ở trạng thái không retry được (đang chạy hoặc `DONE`), hoặc phiên của lần trước chưa thoát (chờ `session_ended`) |

### `POST /tasks/:id/cancel`

Dừng task: đặt `status` = `CANCELLED`. Không có body.

- Task đang `QUEUED`: chỉ bị gỡ khỏi hàng đợi.
- Task đang chạy: mọi lần gọi task_* tiếp theo nhận `control: "stop"`, và launcher nhận thông điệp `cancel` để dừng phiên hermes. Profile được giải phóng khi `session_ended` về, hoặc ngay lập tức nếu launcher đó không còn kết nối.

| Mã | Khi nào |
|---|---|
| `200` | body là task. Nếu launcher không kết nối, task vẫn bị huỷ và event `cancel` ghi `agent not told: ...` |
| `404` | không có task |
| `409` | task đã `DONE`, `FAILED` hoặc `CANCELLED` |

### `GET /agent`

Các launcher đang kết nối. `agent` là launcher kết nối gần nhất, `agents` là tất cả, mới nhất trước.

```json
{
  "connected": true,
  "agents": [ ... ],
  "agent": {
    "agent_id": "ThangNX",
    "version": "…",
    "profiles": [{"directory": "isophtalic", "email": "…", "online": true}],
    "connected_at": "2026-09-28T17:40:00+07:00",
    "last_seen": "2026-09-28T17:48:15+07:00"
  }
}
```

Thông tin lấy từ `hello` của mỗi launcher; `last_seen` cập nhật mỗi khi nhận một thông điệp (heartbeat khoảng 15 giây một lần). Khi `connected` là `false` thì `agent` là `null` và `agents` rỗng.

### `GET /profiles`

Các profile trong `-profiles`, tức giá trị hợp lệ cho `profile` của `POST /uploads`.

```json
{"profiles": ["isophtalic"]}
```

## Endpoint cho agent

Các endpoint này dành cho launcher và hermes, không cần gọi tay.

| Path | Ý nghĩa |
|---|---|
| `GET /ws` | WebSocket của launcher. Launcher → server: `hello`, `heartbeat`, `reject`, `event` (`session_ended`). Server → launcher: `assign`, `cancel` |
| `/task/mcp` | MCP (streamable HTTP) với tool `task_claim`. Header `Authorization: Bearer <token>` |
| `/report/mcp` | MCP với `task_report`, `task_video_created`, `task_finish`, cùng token |
| `GET /files/:id/:name` | launcher tải video (`video.<ext>`) và thumbnail của task |

Token có trong thông điệp `assign` và đổi sau mỗi lần retry.

## Ví dụ

Upload rồi theo dõi đến khi agent gọi `task_finish` hoặc phiên thoát:

```sh
id=$(curl -s -XPOST 127.0.0.1:8090/uploads -H 'content-type: application/json' \
  -d '{"profile":"isophtalic","video":"/path/a.mp4","title":"Đây là video test"}' | jq -r .task_id)

while :; do
  t=$(curl -s 127.0.0.1:8090/tasks/$id)
  echo "$t" | jq -c '{status, step, progress, video_id, finish_reported, session_ended, error}'
  [ "$(echo "$t" | jq .finish_reported)" = true ] || [ "$(echo "$t" | jq .session_ended)" = true ] && break
  sleep 10
done
```

Xem agent đã gọi những gì:

```sh
curl -s 127.0.0.1:8090/tasks/$id | jq -r '.events[] | "\(.at)  #\(.attempt)  \(.event)  \(.step // "") \(.progress // "") \(.status // "") \(.message // "")"'
```

Retry các task bị mất phiên:

```sh
curl -s '127.0.0.1:8090/tasks?status=LOST' | jq -r '.tasks[].task_id' |
  xargs -I{} curl -s -XPOST 127.0.0.1:8090/tasks/{}/retry
```
