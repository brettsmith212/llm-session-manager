package picker

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInspectGitLeavesTheIndexToAgents(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q", "-b", "main")
	mustGit(t, repo, "config", "user.name", "llmux test")
	mustGit(t, repo, "config", "user.email", "llmux@example.com")
	var files []string
	for i := 0; i < 20; i++ {
		file := filepath.Join(repo, fmt.Sprintf("file-%d.txt", i))
		if err := os.WriteFile(file, []byte("content\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	mustGit(t, repo, "add", ".")
	mustGit(t, repo, "commit", "-qm", "initial")

	// New timestamps leave the index's cached stat data stale, so a status
	// that is allowed to would take .git/index.lock and rewrite the index.
	touchAll := func() {
		later := time.Now().Add(2 * time.Second)
		for _, file := range files {
			if err := os.Chtimes(file, later, later); err != nil {
				t.Fatal(err)
			}
		}
	}
	indexPath := filepath.Join(repo, ".git", "index")
	readIndex := func() []byte {
		data, err := os.ReadFile(indexPath)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	touchAll()
	before := readIndex()
	if info := inspectGit(repo); !info.valid || info.changed != 0 {
		t.Fatalf("inspectGit() = %#v, want a clean checkout", info)
	}
	if !bytes.Equal(before, readIndex()) {
		t.Fatal("inspectGit rewrote .git/index; agents' git commands could hit index.lock")
	}

	// Prove the scenario is real: an ordinary status does rewrite it.
	touchAll()
	before = readIndex()
	mustGit(t, repo, "status", "--porcelain")
	if bytes.Equal(before, readIndex()) {
		t.Skip("this git version does not refresh the index during status")
	}
}

func TestGitRefreshKeepsMetadataForCheckoutsThatTimedOut(t *testing.T) {
	p := &picker{
		previewTimer:  stoppedTimer(t),
		gitRefreshing: true,
		gitByPath: map[string]gitInfo{
			"/work/slow": {valid: true, branch: "main", commonDir: "/work/slow/.git"},
			"/work/gone": {valid: true, branch: "old"},
		},
	}
	captureStdout(t, func() {
		p.applyGitRefresh(gitRefresh{
			info:   map[string]gitInfo{"/work/fast": {valid: true, branch: "dev"}},
			failed: map[string]bool{"/work/slow": true},
		})
	})
	if p.gitByPath["/work/slow"].branch != "main" {
		t.Fatalf("timed-out checkout lost its metadata: %#v", p.gitByPath)
	}
	if p.gitByPath["/work/fast"].branch != "dev" {
		t.Fatalf("fresh metadata was not applied: %#v", p.gitByPath)
	}
	if _, ok := p.gitByPath["/work/gone"]; ok {
		t.Fatalf("checkout that is no longer a repository kept stale metadata: %#v", p.gitByPath)
	}
	if p.gitRefreshing {
		t.Fatal("refresh still marked as running")
	}
}

func TestParentClientPrefersTheClientShowingThisRoom(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	t.Setenv("TMUX", "")
	socketDir, err := os.MkdirTemp("/tmp", "llm-picker-parent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	t.Setenv("TMUX_TMPDIR", socketDir)
	mustTmux(t, "new-session", "-d", "-x", "120", "-y", "30", "-s", "origin", "-n", "shell", "sleep 1000")
	t.Cleanup(func() { _, _ = runTmux("kill-server") })
	roomWindow := mustTmux(t, "new-window", "-dP", "-t", "origin:", "-n", windowName, "-F", "#{window_id}", "sleep 1000")
	mustTmux(t, "select-window", "-t", "origin:"+windowName)
	mustTmux(t, "new-session", "-d", "-s", "other", "sleep 1000")
	// Synthetic terminals: tmux clients running inside panes of helper sessions.
	mustTmux(t, "new-session", "-d", "-x", "120", "-y", "30", "-s", "terminal-a", "env -u TMUX tmux attach-session -t origin")
	mustTmux(t, "new-session", "-d", "-x", "120", "-y", "30", "-s", "terminal-b", "env -u TMUX tmux attach-session -t other")

	viewer, other := "", ""
	waitFor(t, 2*time.Second, "both terminal clients", func() bool {
		clients, _ := runTmux("list-clients", "-F", "#{client_name}|#{session_name}")
		for _, line := range strings.Split(clients, "\n") {
			parts := strings.SplitN(line, "|", 2)
			if len(parts) != 2 {
				continue
			}
			switch parts[1] {
			case "origin":
				viewer = parts[0]
			case "other":
				other = parts[0]
			}
		}
		return viewer != "" && other != ""
	})
	// The shared option names the other terminal, as it would after that
	// terminal last opened its own control room.
	mustTmux(t, "set-option", "-g", "@llm_parent", other)

	p := &picker{ownWindowID: roomWindow}
	if got := p.parentClient(); got != viewer {
		t.Fatalf("parentClient() = %q, want %q showing this room", got, viewer)
	}
	p.ownWindowID = ""
	if got := p.parentClient(); got != other {
		t.Fatalf("parentClient() without a known room window = %q, want @llm_parent %q", got, other)
	}
}

func stoppedTimer(t *testing.T) *time.Timer {
	t.Helper()
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	t.Cleanup(func() { timer.Stop() })
	return timer
}
