package add

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"llm-session-manager/internal/sessions"
	"llm-session-manager/internal/warm"
)

func TestAddPromotesWarmSessionWithBaseIndexOne(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	t.Setenv("TMUX", "")
	socketDir, err := os.MkdirTemp("/tmp", "llm-add-tmux-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	t.Setenv("TMUX_TMPDIR", socketDir)
	mustTmux(t, "new-session", "-d", "-s", "origin", "sleep 1000")
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-server").Run() })
	mustTmux(t, "set-option", "-g", "base-index", "1")
	mustTmux(t, "set-option", "-g", "@llm_command", "sleep 1000")

	project := t.TempDir()
	if err := warm.Warm(project); err != nil {
		t.Fatal(err)
	}
	name := sessions.SessionNameForPath(project, "llm-")
	warmWindow := mustTmux(t, "display-message", "-p", "-t", name+":", "#{window_id}")

	windowID, err := Add(project, "origin", false)
	if err != nil {
		t.Fatalf("Add() on a warm session = %v", err)
	}
	if windowID != warmWindow {
		t.Fatalf("Add() created %s instead of promoting warm window %s", windowID, warmWindow)
	}
	got := mustTmux(t, "display-message", "-p", "-t", windowID, "#{@llm_agent}|#{window_name}|#{@llm_ever_attached}|#{@llm_origin}")
	if got != "sleep|sleep|1|origin" {
		t.Fatalf("promoted window agent|name|attached|origin = %q", got)
	}

	// A second Add on the now in-use session opens another agent window.
	second, err := Add(project, "origin", false)
	if err != nil || second == windowID {
		t.Fatalf("second Add() = %q, %v; want a new window", second, err)
	}
}

func mustTmux(t *testing.T, args ...string) string {
	t.Helper()
	output, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
