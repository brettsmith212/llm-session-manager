package warm

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"llm-session-manager/internal/sessions"
)

func TestWarmAndClaimWorkWithBaseIndexOne(t *testing.T) {
	startTestServer(t)
	mustTmux(t, "set-option", "-g", "base-index", "1")
	mustTmux(t, "set-option", "-g", "@llm_command", "sleep 1000")
	project := t.TempDir()

	if err := Warm(project); err != nil {
		t.Fatal(err)
	}
	name := sessions.SessionNameForPath(project, "llm-")
	window := mustTmux(t, "display-message", "-p", "-t", name+":", "#{window_id}\t#{window_index}\t#{window_name}\t#{@llm_warm_agent}")
	parts := strings.Split(window, "\t")
	if len(parts) != 4 || parts[1] != "1" || parts[2] != "warm" || parts[3] != "sleep" {
		t.Fatalf("warm window id|index|name|agent = %q", window)
	}

	claimed, ok, err := Claim(name, "sleep")
	if err != nil || !ok || claimed.WindowID != parts[0] || claimed.Agent != "sleep" {
		t.Fatalf("Claim() = %+v, %v, %v; want window %s running sleep", claimed, ok, err, parts[0])
	}
}

func TestClaimDiscardsWarmSessionForAnotherAgent(t *testing.T) {
	startTestServer(t)
	mustTmux(t, "set-option", "-g", "@llm_command", "sleep 1000")
	project := t.TempDir()
	if err := Warm(project); err != nil {
		t.Fatal(err)
	}
	name := sessions.SessionNameForPath(project, "llm-")

	_, ok, err := Claim(name, "claude")
	if err != nil || ok {
		t.Fatalf("Claim() for a different agent = %v, %v; want the warm session discarded", ok, err)
	}
	if err := exec.Command("tmux", "has-session", "-t", name).Run(); err == nil {
		t.Fatal("warm session for the previous agent is still running")
	}
}

func startTestServer(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	t.Setenv("TMUX", "")
	socketDir, err := os.MkdirTemp("/tmp", "llm-warm-tmux-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	t.Setenv("TMUX_TMPDIR", socketDir)
	mustTmux(t, "new-session", "-d", "-s", "origin", "sleep 1000")
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-server").Run() })
}

func mustTmux(t *testing.T, args ...string) string {
	t.Helper()
	output, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
