package listcmd

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	llmuxtmux "llm-session-manager/internal/tmux"
)

func TestListCommandReturnsFromLivePaneWithoutRebuildingControlRoom(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	socketDir, err := os.MkdirTemp("/tmp", "llmux-list-tmux-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	t.Setenv("TMUX_TMPDIR", socketDir)

	mustTmux(t, "new-session", "-d", "-x", "120", "-y", "30", "-s", "origin", "-n", "shell", "sleep 1000")
	t.Cleanup(func() { _, _ = runTmux("kill-server") })
	mustTmux(t, "new-session", "-d", "-x", "120", "-y", "30", "-s", "llm-agent", "-n", "amp", "sleep 1000")
	mustTmux(t, "new-window", "-d", "-t", "origin:", "-n", windowName, "sleep 1000")
	mustTmux(t, "split-window", "-d", "-h", "-t", "origin:"+windowName+".0", "sleep 1000")
	mustTmux(t, "respawn-pane", "-k", "-t", "origin:"+windowName+".1", "env -u TMUX tmux attach-session -t llm-agent")
	mustTmux(t, "select-window", "-t", "origin:"+windowName)
	mustTmux(t, "select-pane", "-t", "origin:"+windowName+".1")
	// This synthetic terminal pane gives origin an outer client, just as the
	// user's physical terminal does around the nested live agent client.
	mustTmux(t, "new-session", "-d", "-x", "120", "-y", "30", "-s", "terminal", "-n", "display", "env -u TMUX tmux attach-session -t origin")

	var nested, outer *llmuxtmux.ClientInfo
	waitFor(t, 2*time.Second, "nested and outer clients", func() bool {
		nested, outer = nil, nil
		for _, client := range llmuxtmux.ListClients() {
			client := client
			switch client.Session {
			case "llm-agent":
				nested = &client
			case "origin":
				outer = &client
			}
		}
		return nested != nil && outer != nil
	})
	if room := containingControlRoom(nested); room == nil || room.Client != outer.Client {
		t.Fatalf("containingControlRoom(%+v) = %+v, want outer client %+v", nested, room, outer)
	}

	paneID := mustTmux(t, "display-message", "-p", "-t", "origin:"+windowName+".0", "#{pane_id}")
	livePaneID := mustTmux(t, "display-message", "-p", "-t", "origin:"+windowName+".1", "#{pane_id}")
	if err := ListCommand(nested.Client); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 2*time.Second, "control-room focus with live agent still attached", func() bool {
		active := mustTmux(t, "display-message", "-p", "-t", "origin:"+windowName+".0", "#{pane_active}")
		for _, client := range llmuxtmux.ListClients() {
			if active == "1" && client.Session == "llm-agent" && client.Window == nested.Window {
				return true
			}
		}
		return false
	})
	if got := mustTmux(t, "display-message", "-p", "-t", "origin:"+windowName+".0", "#{pane_id}"); got != paneID {
		t.Fatalf("control-room list pane was rebuilt: got %s, want %s", got, paneID)
	}
	if got := mustTmux(t, "display-message", "-p", "-t", "origin:"+windowName+".1", "#{pane_id}"); got != livePaneID {
		t.Fatalf("control-room live pane was rebuilt: got %s, want %s", got, livePaneID)
	}
	if got := mustTmux(t, "show-options", "-wv", "-t", "origin:"+windowName, "pane-border-format"); !strings.Contains(got, "FOCUSED") {
		t.Fatalf("reused control room has no explicit focus indicator: %q", got)
	}
	if got := mustTmux(t, "display-message", "-p", "-t", "origin:"+windowName+".0", "#{pane_title}"); got != "CONTROL ROOM" {
		t.Fatalf("control-room pane title = %q, want CONTROL ROOM", got)
	}
	if got := llmuxtmux.GetGlobalOption("@llm_parent", ""); got != outer.Client {
		t.Fatalf("@llm_parent = %q, want outer client %q", got, outer.Client)
	}

	// The same room is reused when invoked from a project window; its live
	// nested client must not be detached just because the room was backgrounded.
	mustTmux(t, "select-window", "-t", "origin:shell")
	if err := ListCommand(outer.Client); err != nil {
		t.Fatal(err)
	}
	if got := mustTmux(t, "display-message", "-p", "-t", "origin:"+windowName, "#{window_name}:#{pane_index}:#{pane_id}"); got != windowName+":0:"+paneID {
		t.Fatalf("background control room was not reused: got %q", got)
	}
	if got := mustTmux(t, "display-message", "-p", "-t", "origin:"+windowName+".1", "#{pane_id}:#{pane_dead}"); got != livePaneID+":0" {
		t.Fatalf("background live pane was not preserved: got %q", got)
	}
}

func waitFor(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	clients, _ := runTmux("list-clients", "-F", "#{client_name}|#{session_name}|#{window_name}")
	panes, _ := runTmux("list-panes", "-a", "-F", "#{session_name}|#{window_name}|#{pane_index}|#{pane_id}|#{pane_dead}|#{pane_current_command}|#{pane_start_command}")
	t.Fatalf("timed out waiting for %s\nclients:\n%s\npanes:\n%s", description, clients, panes)
}

func mustTmux(t *testing.T, args ...string) string {
	t.Helper()
	output, err := runTmux(args...)
	if err != nil {
		t.Fatalf("tmux %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

func runTmux(args ...string) (string, error) {
	cmd := exec.Command("tmux", args...)
	output, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}
