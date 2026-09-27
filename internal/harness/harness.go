// Package harness chỉ sinh prompt chuẩn rồi đưa vào lệnh agent cấu hình từ bên ngoài.
// Không gắn với claude, cursor, hay một CLI cụ thể nào.
package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"yt-uploader/internal/chrome"
	"yt-uploader/internal/config"
)

type Input struct {
	File    string
	Profile config.Profile
}

type Result struct {
	Status string `json:"status"` // "done" | "error"
	URL    string `json:"url,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Run thay placeholder trong HARNESS_ARGS rồi exec HARNESS_BIN.
// Nếu argv không chứa {prompt}, prompt được ghi vào stdin.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger, in Input) (*Result, error) {
	if err := chrome.Ensure(ctx, cfg, in.Profile, log); err != nil {
		return nil, err
	}
	prompt := BuildPrompt(cfg, in)
	args, viaStdin := expandArgs(cfg, prompt, in)

	log.Info("goi agent harness",
		slog.String("bin", cfg.HarnessBin),
		slog.String("file", in.File),
		slog.String("profile", in.Profile.Name),
		slog.String("profile_dir", in.Profile.Dir),
		slog.Bool("prompt_stdin", viaStdin),
	)

	cmd := exec.CommandContext(ctx, cfg.HarnessBin, args...)
	cmd.Env = append(os.Environ(),
		"HARNESS_FILE="+in.File,
		"HARNESS_PROFILE="+in.Profile.Name,
		"HARNESS_PROFILE_DIR="+in.Profile.Dir,
		"HARNESS_PROFILE_DIRECTORY="+profileDirectory(in.Profile.Dir),
		"HARNESS_USER_DATA_DIR="+userDataDir(in.Profile.Dir),
	)
	if viaStdin {
		cmd.Stdin = strings.NewReader(prompt)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	outText := stdout.String()
	errText := stderr.String()

	if ctx.Err() != nil {
		reason := fmt.Sprintf("agent bi huy do vuot qua thoi gian cho phep: %s", ctx.Err())
		logAgentError(log, reason, outText, errText)
		return nil, fmt.Errorf("%s", reason)
	}

	if err != nil {
		if result, perr := parseOutput(outText); perr == nil {
			if result.Status == "error" {
				logAgentError(log, agentReason(result), outText, errText)
			}
			return result, nil
		}
		reason := fmt.Sprintf("harness thoat voi loi: %s", err)
		logAgentError(log, reason, outText, errText)
		return nil, fmt.Errorf("%s", reason)
	}

	result, perr := parseOutput(outText)
	if perr != nil {
		reason := fmt.Sprintf("khong parse duoc JSON tu agent: %s", perr)
		logAgentError(log, reason, outText, errText)
		return nil, fmt.Errorf("%s", reason)
	}
	if result.Status == "error" {
		logAgentError(log, agentReason(result), outText, errText)
	}
	return result, nil
}

func agentReason(result *Result) string {
	if result.Reason != "" {
		return result.Reason
	}
	return "agent tra status error nhung khong co reason"
}

func logAgentError(log *slog.Logger, reason, stdout, stderr string) {
	attrs := []any{slog.String("reason", reason)}
	if text := strings.TrimSpace(stdout); text != "" {
		attrs = append(attrs, slog.String("stdout", truncate(text, 2000)))
	}
	if text := strings.TrimSpace(stderr); text != "" {
		attrs = append(attrs, slog.String("stderr", truncate(text, 2000)))
	}
	log.Warn("agent harness loi", attrs...)
}

// BuildPrompt là hợp đồng duy nhất với harness: đường dẫn video, profile,
// và một dòng JSON kết quả ở cuối stdout.
func BuildPrompt(cfg config.Config, in Input) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Upload video tại đường dẫn tuyệt đối \"%s\" lên YouTube bằng BrowserMCP.\n", in.File)
	fmt.Fprintf(&b, "Tuyệt đối không sử dụng browser tool built-in (toolset browser). Mọi thao tác trình duyệt chỉ được gọi tool MCP khớp %s.\n", allowedBrowserTools(cfg))
	fmt.Fprintf(&b, "Chỉ dùng đúng profile/kênh \"%s\". Không đăng nhập hay chuyển sang profile khác.\n", in.Profile.Name)
	if dir := in.Profile.Dir; dir != "" {
		fmt.Fprintf(&b, "Chrome đã mở sẵn profile này. user-data-dir=\"%s\" profile-directory=\"%s\".\n", userDataDir(dir), profileDirectory(dir))
		b.WriteString("BrowserMCP đã được load và đã nối tab YouTube đang mở. Không tự mở Chrome khác. Không bấm nút Connect.\n")
	}
	fmt.Fprintf(&b, "Tiêu đề: dùng tên file (không cần đuôi mở rộng) làm tiêu đề tạm thời.\n")
	fmt.Fprintf(&b, "Chế độ hiển thị: %s.\n", cfg.VisibilityDefault)
	b.WriteString("Sau khi publish xong, in RA DUY NHẤT MỘT dòng JSON cuối cùng theo đúng format:\n")
	b.WriteString(`{"status":"done","url":"<link video youtube>"}` + "\n")
	b.WriteString("Nếu có lỗi ở bất kỳ bước nào, in ra dòng JSON cuối cùng theo format:\n")
	b.WriteString(`{"status":"error","reason":"<mô tả ngắn gọn lỗi>"}` + "\n")
	b.WriteString("Không in thêm bất kỳ dòng JSON nào khác sau dòng kết quả này.\n")
	return b.String()
}

func expandArgs(cfg config.Config, prompt string, in Input) (args []string, viaStdin bool) {
	replacer := strings.NewReplacer(
		"{prompt}", prompt,
		"{file}", in.File,
		"{profile}", in.Profile.Name,
		"{profile_dir}", in.Profile.Dir,
		"{profile_directory}", profileDirectory(in.Profile.Dir),
		"{user_data_dir}", userDataDir(in.Profile.Dir),
		"{mcp_config}", cfg.MCPConfigPath,
		"{allowed_tools}", cfg.AllowedTools,
		"{permission_mode}", cfg.PermissionMode,
		"{visibility}", cfg.VisibilityDefault,
	)
	viaStdin = true
	args = make([]string, 0, len(cfg.HarnessArgs))
	for _, raw := range cfg.HarnessArgs {
		if strings.Contains(raw, "{prompt}") {
			viaStdin = false
		}
		args = append(args, replacer.Replace(raw))
	}
	return args, viaStdin
}

func allowedBrowserTools(cfg config.Config) string {
	if cfg.AllowedTools != "" {
		return cfg.AllowedTools
	}
	return "mcp__browsermcp__*"
}

func profileDirectory(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Base(dir)
}

func userDataDir(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Dir(dir)
}

func parseOutput(output string) (*Result, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || line[0] != '{' {
			continue
		}
		var result Result
		if err := json.Unmarshal([]byte(line), &result); err == nil && result.Status != "" {
			return &result, nil
		}
	}
	return nil, fmt.Errorf("khong tim thay dong JSON ket qua hop le trong output cua agent")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
