//go:build unix

package harness

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestResultLineIsParsedAndPromptGoesToStdin(t *testing.T) {
	dir := t.TempDir()
	c := Command{Bin: "sh", Parser: "stream-json", Stdin: "hello\n",
		Args: []string{"-c", `read p; echo '{"type":"assistant"}'; echo "{\"type\":\"result\",\"num_turns\":7,\"total_cost_usd\":0.25,\"result\":\"$p\"}"`}}
	var lines int
	p, err := Start(c, filepath.Join(dir, "t.jsonl"), func(map[string]any) { lines++ })
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.NumTurns != 7 || res.CostUSD != 0.25 || res.Text != "hello" || lines != 2 || res.ExitCode != 0 {
		t.Fatalf("unexpected result %+v lines=%d", res, lines)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "t.jsonl")); !strings.Contains(string(b), `"num_turns":7`) {
		t.Fatalf("transcript not written: %s", b)
	}
}

// Kill must take the whole group down, including children (like an MCP
// server spawned by the CLI).
func TestKillTakesDownChildren(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	c := Command{Bin: "sh", Args: []string{"-c", `sleep 300 & echo $! > ` + pidFile + `; wait`}}
	p, err := Start(c, filepath.Join(dir, "t.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var child int
	for i := 0; i < 100 && child == 0; i++ {
		b, _ := os.ReadFile(pidFile)
		child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		time.Sleep(20 * time.Millisecond)
	}
	if child == 0 {
		t.Fatal("child never started")
	}
	p.Kill("test", time.Second)
	res, _ := p.Wait(context.Background())
	if !res.Signaled || p.KilledBy() != "test" {
		t.Fatalf("expected signaled result, got %+v", res)
	}
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(child, 0); err == nil {
		t.Fatalf("child %d still alive after kill", child)
	}
}
