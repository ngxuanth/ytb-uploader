package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"
)

// MCPServer is one MCP server the session must see. Either Command (stdio)
// or URL (streamable HTTP) is set.
type MCPServer struct {
	Name    string            `json:"name"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// MCP modes: how the servers are handed to the CLI.
const (
	MCPJSON      = "json"       // {"mcpServers":{...}} file (Claude Code, Cursor)
	MCPCodexArgs = "codex-args" // -c mcp_servers.<name>.<key>=<toml> args, expanded at {mcp_args}
	MCPTemplate  = "template"   // text/template rendered to a file (any other CLI)
	MCPNone      = "none"       // the CLI is configured out of band; only env vars are set
)

// Spec configures one CLI. A preset fills the defaults; every field can be
// overridden from config (launcher.harness.*).
type Spec struct {
	Preset    string   `mapstructure:"preset" json:"preset"`
	Bin       string   `mapstructure:"bin" json:"bin"`
	Args      []string `mapstructure:"args" json:"args"`
	Env       []string `mapstructure:"env" json:"env"`
	PromptVia string   `mapstructure:"prompt_via" json:"prompt_via"` // stdin | arg ({prompt}) | file ({prompt_file})
	WorkDir   string   `mapstructure:"work_dir" json:"work_dir"`     // default {session_dir}
	Parser    string   `mapstructure:"parser" json:"parser"`         // stream-json | codex-json | ""
	Model     string   `mapstructure:"model" json:"model"`
	MCP       struct {
		Mode     string `mapstructure:"mode" json:"mode"`
		Path     string `mapstructure:"path" json:"path"`         // file to write, placeholders allowed
		Template string `mapstructure:"template" json:"template"` // for mode=template
	} `mapstructure:"mcp" json:"mcp"`
}

// Presets are starting points; flags drift between CLI versions, so check
// them against `<cli> --help` and override in config where needed.
var Presets = map[string]Spec{
	"claude": func() Spec {
		s := Spec{
			Bin: "claude",
			Args: []string{
				"-p", "--output-format", "stream-json", "--verbose",
				"--mcp-config", "{mcp_config}", "--strict-mcp-config",
				"--tools", "", "--allowedTools", "mcp__bmcp,mcp__task_mcp,mcp__report_mcp,mcp__chrome_mcp",
				"--no-session-persistence", "--model", "{model}",
			},
			PromptVia: "stdin", Parser: "stream-json", Model: "sonnet",
		}
		s.MCP.Mode, s.MCP.Path = MCPJSON, "{session_dir}/mcp.json"
		return s
	}(),
	"cursor": func() Spec {
		s := Spec{
			Bin:       "cursor-agent",
			Args:      []string{"-p", "--output-format", "stream-json", "--approve-mcps", "--model", "{model}", "{prompt}"},
			PromptVia: "arg", Parser: "stream-json", Model: "auto",
		}
		// Cursor reads project MCP servers from <cwd>/.cursor/mcp.json.
		s.MCP.Mode, s.MCP.Path = MCPJSON, "{session_dir}/.cursor/mcp.json"
		return s
	}(),
	"codex": func() Spec {
		s := Spec{
			Bin:       "codex",
			Args:      []string{"exec", "--json", "--skip-git-repo-check", "--model", "{model}", "{mcp_args}", "-"},
			PromptVia: "stdin", Parser: "codex-json", Model: "gpt-5-codex",
		}
		s.MCP.Mode = MCPCodexArgs
		return s
	}(),
	"hermes": func() Spec {
		// Hermes only reads MCP servers from $HERMES_HOME/config.yaml, where
		// ${VAR} refs are expanded from the environment. The servers are
		// declared there once (bmcp, task_mcp, report_mcp, chrome_mcp; "browser"
		// would collide with the built-in toolset of that name) against the
		// env vars the launcher exports per session; -t keeps the session to them.
		s := Spec{
			Bin: "hermes",
			Args: []string{
				"chat", "--query-file", "-", "--oneshot", "--format", "stream-json",
				"--yolo", "--ignore-rules", "--source", "tool",
				"-t", "bmcp,task_mcp,report_mcp,chrome_mcp", "-m", "{model}",
			},
			PromptVia: "stdin", Parser: "hermes-json",
		}
		s.MCP.Mode = MCPNone
		return s
	}(),
}

// Resolve merges spec over its preset.
func Resolve(spec Spec) (Spec, error) {
	base := Spec{PromptVia: "stdin"}
	base.MCP.Mode = MCPNone
	if spec.Preset != "" && spec.Preset != "custom" {
		p, ok := Presets[spec.Preset]
		if !ok {
			return Spec{}, fmt.Errorf("harness: unknown preset %q", spec.Preset)
		}
		base = p
	}
	if spec.Bin != "" {
		base.Bin = spec.Bin
	}
	if spec.Args != nil {
		base.Args = spec.Args
	}
	base.Env = append(append([]string{}, base.Env...), spec.Env...)
	if spec.PromptVia != "" {
		base.PromptVia = spec.PromptVia
	}
	if spec.WorkDir != "" {
		base.WorkDir = spec.WorkDir
	}
	if spec.Parser != "" {
		base.Parser = spec.Parser
	}
	if spec.Model != "" {
		base.Model = spec.Model
	}
	if spec.MCP.Mode != "" {
		base.MCP.Mode = spec.MCP.Mode
	}
	if spec.MCP.Path != "" {
		base.MCP.Path = spec.MCP.Path
	}
	if spec.MCP.Template != "" {
		base.MCP.Template = spec.MCP.Template
	}
	if base.Bin == "" {
		return Spec{}, fmt.Errorf("harness: bin is required")
	}
	if base.MCP.Mode == MCPTemplate && (base.MCP.Template == "" || base.MCP.Path == "") {
		return Spec{}, fmt.Errorf("harness: mcp mode %q needs mcp.template and mcp.path", MCPTemplate)
	}
	return base, nil
}

// Session is what one run needs.
type Session struct {
	Dir     string // private directory for this session's files
	Prompt  string
	Servers []MCPServer
	Env     map[string]string // exported to the CLI (and usable from templates)
}

// Build writes the session's files and returns the command to run.
func (s Spec) Build(sess Session) (Command, error) {
	if err := os.MkdirAll(sess.Dir, 0o700); err != nil {
		return Command{}, err
	}
	vars := map[string]string{
		"session_dir": sess.Dir,
		"model":       s.Model,
		"prompt":      "",
		"prompt_file": filepath.Join(sess.Dir, "prompt.txt"),
		"mcp_config":  "",
	}
	sub := func(v string) string {
		for k, val := range vars {
			v = strings.ReplaceAll(v, "{"+k+"}", val)
		}
		return v
	}

	if err := os.WriteFile(vars["prompt_file"], []byte(sess.Prompt), 0o600); err != nil {
		return Command{}, err
	}
	switch s.PromptVia {
	case "arg":
		vars["prompt"] = sess.Prompt
	case "file", "stdin", "":
	default:
		return Command{}, fmt.Errorf("harness: prompt_via %q", s.PromptVia)
	}

	var extraArgs []string
	switch s.MCP.Mode {
	case MCPJSON, MCPTemplate:
		path := sub(s.MCP.Path)
		if path == "" {
			return Command{}, fmt.Errorf("harness: mcp.path is required for mode %q", s.MCP.Mode)
		}
		var body []byte
		var err error
		if s.MCP.Mode == MCPJSON {
			body, err = mcpJSON(sess.Servers)
		} else {
			body, err = renderTemplate(s.MCP.Template, sess)
		}
		if err != nil {
			return Command{}, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return Command{}, err
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return Command{}, err
		}
		vars["mcp_config"] = path
	case MCPCodexArgs:
		extraArgs = codexArgs(sess.Servers)
	case MCPNone, "":
	default:
		return Command{}, fmt.Errorf("harness: mcp mode %q", s.MCP.Mode)
	}

	var args []string
	for _, a := range s.Args {
		if a == "{mcp_args}" {
			args = append(args, extraArgs...)
			continue
		}
		// No model: drop "<flag> {model}" so the CLI uses its own default.
		if a == "{model}" && s.Model == "" {
			if len(args) > 0 {
				args = args[:len(args)-1]
			}
			continue
		}
		args = append(args, sub(a))
	}
	env := make([]string, 0, len(s.Env)+len(sess.Env))
	for _, e := range s.Env {
		env = append(env, sub(e))
	}
	keys := make([]string, 0, len(sess.Env))
	for k := range sess.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+sess.Env[k])
	}
	cmd := Command{Bin: s.Bin, Args: args, Env: env, Dir: sub(s.WorkDir), Parser: s.Parser}
	if cmd.Dir == "" {
		cmd.Dir = sess.Dir
	}
	if s.PromptVia == "stdin" || s.PromptVia == "" {
		cmd.Stdin = sess.Prompt
	}
	return cmd, nil
}

func mcpJSON(servers []MCPServer) ([]byte, error) {
	m := map[string]any{}
	for _, s := range servers {
		if s.URL != "" {
			m[s.Name] = map[string]any{"type": "http", "url": s.URL, "headers": s.Headers}
		} else {
			m[s.Name] = map[string]any{"type": "stdio", "command": s.Command, "args": s.Args, "env": s.Env}
		}
	}
	return json.MarshalIndent(map[string]any{"mcpServers": m}, "", "  ")
}

// codexArgs renders servers as `-c mcp_servers.<name>.<key>=<toml value>`.
func codexArgs(servers []MCPServer) []string {
	var out []string
	add := func(name, key, val string) {
		out = append(out, "-c", fmt.Sprintf("mcp_servers.%s.%s=%s", name, key, val))
	}
	for _, s := range servers {
		if s.URL != "" {
			add(s.Name, "url", tomlString(s.URL))
			if len(s.Headers) > 0 {
				add(s.Name, "http_headers", tomlTable(s.Headers))
			}
			continue
		}
		add(s.Name, "command", tomlString(s.Command))
		parts := make([]string, len(s.Args))
		for i, a := range s.Args {
			parts[i] = tomlString(a)
		}
		add(s.Name, "args", "["+strings.Join(parts, ",")+"]")
		if len(s.Env) > 0 {
			add(s.Name, "env", tomlTable(s.Env))
		}
	}
	return out
}

func tomlString(s string) string { return strconv.Quote(s) }

func tomlTable(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = strconv.Quote(k) + "=" + tomlString(m[k])
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func renderTemplate(src string, sess Session) ([]byte, error) {
	t, err := template.New("mcp").Funcs(template.FuncMap{
		"json": func(v any) (string, error) { b, err := json.Marshal(v); return string(b), err },
	}).Parse(src)
	if err != nil {
		return nil, fmt.Errorf("harness: mcp template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, sess); err != nil {
		return nil, fmt.Errorf("harness: mcp template: %w", err)
	}
	return buf.Bytes(), nil
}
