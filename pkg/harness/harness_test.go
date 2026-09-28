package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var servers = []MCPServer{
	{Name: "browser", Command: "node", Args: []string{"/b/dist/index.js"}, Env: map[string]string{"BMCP_WS_PORT": "9011"}},
	{Name: "uploader", URL: "http://127.0.0.1:3001/mcp", Headers: map[string]string{"Authorization": "Bearer tok"}},
}

func TestClaudePresetWritesMCPJSONAndUsesStdin(t *testing.T) {
	spec, err := Resolve(Spec{Preset: "claude", Model: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cmd, err := spec.Build(Session{Dir: dir, Prompt: "do it", Servers: servers, Env: map[string]string{"X": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Bin != "claude" || cmd.Stdin != "do it" || cmd.Dir != dir || cmd.Parser != "stream-json" {
		t.Fatalf("unexpected command %+v", cmd)
	}
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "--mcp-config "+filepath.Join(dir, "mcp.json")) || !strings.Contains(joined, "--model opus") {
		t.Fatalf("args: %s", joined)
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	b, _ := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.MCPServers["uploader"]["url"] != "http://127.0.0.1:3001/mcp" || cfg.MCPServers["browser"]["command"] != "node" {
		t.Fatalf("mcp.json: %s", b)
	}
	if cmd.Env[len(cmd.Env)-1] != "X=1" {
		t.Fatalf("env: %v", cmd.Env)
	}
}

func TestCursorPresetPutsPromptInArgsAndConfigInProjectDir(t *testing.T) {
	spec, _ := Resolve(Spec{Preset: "cursor"})
	dir := t.TempDir()
	cmd, err := spec.Build(Session{Dir: dir, Prompt: "do it", Servers: servers})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Args[len(cmd.Args)-1] != "do it" || cmd.Stdin != "" {
		t.Fatalf("prompt not passed as arg: %+v", cmd)
	}
	if _, err := os.Stat(filepath.Join(dir, ".cursor", "mcp.json")); err != nil {
		t.Fatal(err)
	}
}

func TestCodexPresetExpandsMCPArgs(t *testing.T) {
	spec, _ := Resolve(Spec{Preset: "codex"})
	cmd, err := spec.Build(Session{Dir: t.TempDir(), Prompt: "do it", Servers: servers})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		`mcp_servers.browser.command="node"`,
		`mcp_servers.browser.args=["/b/dist/index.js"]`,
		`mcp_servers.browser.env={"BMCP_WS_PORT"="9011"}`,
		`mcp_servers.uploader.url="http://127.0.0.1:3001/mcp"`,
		`mcp_servers.uploader.http_headers={"Authorization"="Bearer tok"}`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in %s", want, joined)
		}
	}
	if cmd.Args[len(cmd.Args)-1] != "-" || cmd.Stdin != "do it" {
		t.Fatalf("stdin prompt not wired: %+v", cmd)
	}
}

func TestTemplateModeForOtherCLIs(t *testing.T) {
	bad := Spec{Preset: "custom", Bin: "x"}
	bad.MCP.Mode = MCPTemplate
	if _, err := Resolve(bad); err == nil {
		t.Fatal("template mode without a template should be rejected")
	}
	s := Spec{Preset: "hermes"}
	s.MCP.Mode = MCPTemplate
	s.MCP.Path = "{session_dir}/hermes.yaml"
	s.MCP.Template = `{{range .Servers}}{{.Name}}: {{if .URL}}{{.URL}}{{else}}{{.Command}}{{end}}
{{end}}`
	spec, err := Resolve(s)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := spec.Build(Session{Dir: dir, Prompt: "p", Servers: servers}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "hermes.yaml"))
	if string(b) != "browser: node\nuploader: http://127.0.0.1:3001/mcp\n" {
		t.Fatalf("rendered: %q", b)
	}
}

func TestHermesModelFlag(t *testing.T) {
	for _, tc := range []struct{ model, want string }{{"", "-t bmcp,task_mcp,report_mcp,chrome_mcp"}, {"x/y", "-m x/y"}} {
		spec, err := Resolve(Spec{Preset: "hermes", Model: tc.model})
		if err != nil {
			t.Fatal(err)
		}
		cmd, err := spec.Build(Session{Dir: t.TempDir(), Prompt: "p"})
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Join(cmd.Args, " ")
		if !strings.HasSuffix(args, tc.want) {
			t.Fatalf("model %q: args %q", tc.model, args)
		}
	}
}
