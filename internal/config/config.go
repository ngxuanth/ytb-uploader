package config

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Config gom toàn bộ tham số runtime, đọc từ biến môi trường để dễ chỉnh
// khi chạy bằng systemd/docker mà không cần sửa code.
type Config struct {
	// Redis
	RedisAddr     string
	RedisPassword string
	RedisDB       int

	// Watcher
	WatchDir           string
	PollInterval       time.Duration
	StableChecksNeeded int // số lần check size không đổi liên tiếp trước khi coi là file ghi xong

	// Worker / agent harness. Bin + Args do người chạy đặt, không gắn với một agent cụ thể.
	// Code chỉ sinh prompt rồi gán vào chỗ {prompt}, hoặc stdin nếu args không có {prompt}.
	HarnessBin        string
	HarnessArgs       []string
	MCPConfigPath     string
	AllowedTools      string
	PermissionMode    string
	AgentTimeout      time.Duration
	MaxAttempts       int
	VisibilityDefault string

	// Scheduler: mỗi thư mục con của ChromeProfilesDir (có file Preferences) là một kênh.
	// Hết một vòng thì nghỉ RoundCooldown trước video của vòng sau, nếu vẫn còn job pending.
	ChromeProfilesDir   string
	ChromeBin           string
	BrowserMCPExtension string
	ChromeDebugPort     int
	Profiles            []Profile
	RoundCooldown       time.Duration

	// Logging
	LogDir string
}

func Load() Config {
	profilesDir := getEnv("CHROME_PROFILES_DIR", "./chrome-profile")
	return Config{
		RedisAddr:     getEnv("REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword: getEnv("REDIS_PASSWORD", ""),
		RedisDB:       getEnvInt("REDIS_DB", 0),

		WatchDir:           getEnv("WATCH_DIR", "./videos-to-upload"),
		PollInterval:       getEnvDuration("POLL_INTERVAL_SECONDS", 5) * time.Second,
		StableChecksNeeded: getEnvInt("STABLE_CHECKS", 3),

		HarnessBin:        harnessBin(),
		HarnessArgs:       harnessArgs(),
		MCPConfigPath:     getEnv("MCP_CONFIG_PATH", "./browsermcp.json"),
		AllowedTools:      getEnv("ALLOWED_TOOLS", "mcp__browsermcp__*"),
		PermissionMode:    getEnv("PERMISSION_MODE", "acceptEdits"),
		AgentTimeout:      getEnvDuration("AGENT_TIMEOUT_SECONDS", 600) * time.Second,
		MaxAttempts:       getEnvInt("MAX_ATTEMPTS", 3),
		VisibilityDefault: getEnv("VISIBILITY_DEFAULT", "unlisted"),

		ChromeProfilesDir:   profilesDir,
		ChromeBin:           getEnv("CHROME_BIN", defaultChromeBin()),
		BrowserMCPExtension: getEnv("BROWSERMCP_EXTENSION", `C:\Users\Admin\Workspace\browsermcp-extension`),
		ChromeDebugPort:     getEnvInt("CHROME_DEBUG_PORT", 9222),
		Profiles:            discoverProfiles(profilesDir),
		RoundCooldown:       getEnvDuration("ROUND_COOLDOWN_SECONDS", 3600) * time.Second,

		LogDir: getEnv("LOG_DIR", "./logs"),
	}
}

// Profile là một kênh. Name là tên thư mục, Dir là đường dẫn tuyệt đối tới profile Chrome.
type Profile struct {
	Name string
	Dir  string
}

func defaultChromeBin() string {
	p := `C:\Program Files\Google\Chrome\Application\chrome.exe`
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return "chrome"
}

// discoverProfiles lấy mọi thư mục con có file Preferences, sắp theo tên.
// Bỏ profile Chrome tự tạo (Default, Guest, System) vì đó không phải kênh đã seed.
func discoverProfiles(dir string) []Profile {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil
	}
	out := make([]Profile, 0)
	for _, e := range entries {
		if !e.IsDir() || isChromeBuiltinProfile(e.Name()) {
			continue
		}
		profileDir := filepath.Join(abs, e.Name())
		if _, err := os.Stat(filepath.Join(profileDir, "Preferences")); err != nil {
			continue
		}
		out = append(out, Profile{Name: e.Name(), Dir: profileDir})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func isChromeBuiltinProfile(name string) bool {
	switch strings.ToLower(name) {
	case "default", "guest profile", "system profile":
		return true
	default:
		return false
	}
}

func harnessBin() string {
	return os.Getenv("HARNESS_BIN")
}

// harnessArgs đọc HARNESS_ARGS. Không đặt biến thì args rỗng và prompt đi vào stdin.
// Dấu | tách từng argv, không đi qua shell.
func harnessArgs() []string {
	raw, ok := os.LookupEnv("HARNESS_ARGS")
	if !ok {
		return nil
	}
	return splitHarnessArgs(raw)
}

func splitHarnessArgs(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getEnvDuration(key string, defSeconds int) time.Duration {
	n := getEnvInt(key, defSeconds)
	return time.Duration(n)
}
