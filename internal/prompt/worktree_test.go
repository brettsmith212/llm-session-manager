package prompt

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"llm-session-manager/internal/sessions"
	"llm-session-manager/internal/worktree"
)

func TestSubmitWorktreeCreatesLabeledManagedAgent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	t.Setenv("TMUX", "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	socketDir, err := os.MkdirTemp("/tmp", "llm-worktree-prompt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	t.Setenv("TMUX_TMPDIR", socketDir)
	tmuxTest(t, "new-session", "-d", "-s", "origin", "sleep 1000")
	t.Cleanup(func() { _, _ = tmuxTestOutput("kill-server") })
	tmuxTest(t, "set-option", "-g", "@llm_command", "sleep 1000")

	repository := t.TempDir()
	gitTest(t, repository, "init", "-q", "-b", "main")
	gitTest(t, repository, "config", "user.name", "llmux test")
	gitTest(t, repository, "config", "user.email", "llmux@example.com")
	if err := os.WriteFile(filepath.Join(repository, "flake.nix"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repository, "add", "flake.nix")
	gitTest(t, repository, "commit", "-qm", "initial")
	repoInfo, err := worktree.Inspect(repository, "")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := worktree.NewPlan(repoInfo, "Test Nix Idea")
	if err != nil {
		t.Fatal(err)
	}
	if err := submitWorktree(plan, "origin"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = tmuxTestOutput("kill-session", "-t", sessions.SessionNameForPath(plan.Path, "llm-"))
		_, _ = worktree.Remove(plan.Path)
	})

	sessionName := sessions.SessionNameForPath(plan.Path, "llm-")
	windowID := tmuxTest(t, "display-message", "-p", "-t", sessionName+":0", "#{window_id}")
	if got := tmuxTest(t, "show-options", "-wqv", "-t", windowID, "@llm_label"); got != plan.Label {
		t.Fatalf("task label = %q, want %q", got, plan.Label)
	}
	if got := tmuxTest(t, "show-options", "-qv", "-t", sessionName, "@llm_worktree_branch"); got != plan.Branch {
		t.Fatalf("worktree branch metadata = %q, want %q", got, plan.Branch)
	}
	if manifest, err := worktree.Load(plan.Path); err != nil || manifest.Branch != plan.Branch {
		t.Fatalf("durable worktree manifest = %#v, %v", manifest, err)
	}
}

func tmuxTest(t *testing.T, args ...string) string {
	t.Helper()
	output, err := tmuxTestOutput(args...)
	if err != nil {
		t.Fatalf("tmux %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(output)
}

func tmuxTestOutput(args ...string) (string, error) {
	output, err := exec.Command("tmux", args...).CombinedOutput()
	return string(output), err
}

func gitTest(t *testing.T, directory string, args ...string) {
	t.Helper()
	commandArgs := append([]string{"-C", directory}, args...)
	if output, err := exec.Command("git", commandArgs...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
