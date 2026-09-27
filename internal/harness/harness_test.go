package harness

import (
	"strings"
	"testing"

	"yt-uploader/internal/config"
)

func TestBuildPromptIncludesChromeProfile(t *testing.T) {
	prompt := BuildPrompt(config.Config{VisibilityDefault: "unlisted"}, Input{
		File: `D:\videos\a.mp4`,
		Profile: config.Profile{
			Name: "Kenh A",
			Dir:  `C:\Chrome\User Data\Profile 1`,
		},
	})
	for _, want := range []string{
		`D:\videos\a.mp4`,
		"Kenh A",
		`user-data-dir="C:\Chrome\User Data"`,
		`profile-directory="Profile 1"`,
		"Không bấm nút Connect",
		"không sử dụng browser tool built-in",
		"mcp__browsermcp__*",
		`"status":"done"`,
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt thieu %q\n%s", want, prompt)
		}
	}
}

func TestExpandArgsPromptStdinWhenMissing(t *testing.T) {
	cfg := config.Config{HarnessArgs: []string{"--print"}}
	args, viaStdin := expandArgs(cfg, "PROMPT", Input{Profile: config.Profile{Name: "A"}})
	if !viaStdin {
		t.Fatal("khong co {prompt} thi phai dua prompt vao stdin")
	}
	if len(args) != 1 || args[0] != "--print" {
		t.Fatalf("args = %#v", args)
	}
}

func TestExpandArgsReplacesPrompt(t *testing.T) {
	cfg := config.Config{
		HarnessArgs:   []string{"-p", "{prompt}", "--profile", "{profile_directory}"},
		MCPConfigPath: "m.json",
	}
	args, viaStdin := expandArgs(cfg, "hello", Input{
		Profile: config.Profile{Name: "Kenh", Dir: `C:\Chrome\User Data\Profile 1`},
	})
	if viaStdin {
		t.Fatal("co {prompt} thi khong dua stdin")
	}
	if args[1] != "hello" || args[3] != "Profile 1" {
		t.Fatalf("args = %#v", args)
	}
}
