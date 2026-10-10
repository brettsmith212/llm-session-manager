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
	if got := mustTmux(t, "show-options", "-wqv", "-t", "origin:"+windowName, "@llm_control_room"); got != "1" {
		t.Fatalf("control-room ownership marker = %q, want 1", got)
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

func TestListCommandFromAgentPopupKeepsTheRoomAndItsLivePane(t *testing.T) {
	room := startControlRoom(t)
	// An agent popup is a tmux client attached to the same agent session the
	// room is previewing; here it runs inside a pane of a helper session.
	mustTmux(t, "new-session", "-d", "-x", "120", "-y", "30", "-s", "popup", "env -u TMUX tmux attach-session -t llm-agent")
	popupTTY := mustTmux(t, "display-message", "-p", "-t", "popup:", "#{pane_tty}")
	var popup *llmuxtmux.ClientInfo
	waitFor(t, 2*time.Second, "popup client", func() bool {
		popup = findClient(func(c llmuxtmux.ClientInfo) bool { return c.TTY == popupTTY })
		return popup != nil
	})

	started := time.Now()
	if err := ListCommand(popup.Client); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("ListCommand from a popup took %s", elapsed)
	}
	if findClient(func(c llmuxtmux.ClientInfo) bool { return c.TTY == popupTTY }) != nil {
		t.Fatal("the popup client is still attached")
	}
	if findClient(func(c llmuxtmux.ClientInfo) bool { return c.TTY == room.liveTTY }) == nil {
		t.Fatal("the control room's live pane was detached along with the popup")
	}
	if got := mustTmux(t, "display-message", "-p", "-t", "origin:"+windowName+".0", "#{pane_id}"); got != room.listPaneID {
		t.Fatalf("control room was rebuilt: list pane %s, want %s", got, room.listPaneID)
	}
	if got := mustTmux(t, "display-message", "-p", "-t", "origin:"+windowName+".1", "#{pane_id}:#{pane_dead}"); got != room.livePaneID+":0" {
		t.Fatalf("live pane = %q, want %s alive", got, room.livePaneID)
	}
	if windows := mustTmux(t, "list-windows", "-t", "llm-agent", "-F", "#{window_name}"); strings.Contains(windows, windowName) {
		t.Fatalf("a control room was built inside the agent session: %q", windows)
	}
	if got := llmuxtmux.GetGlobalOption("@llm_parent", ""); got != room.outer.Client {
		t.Fatalf("@llm_parent = %q, want outer client %q", got, room.outer.Client)
	}
	if got := clientWindowName(room.outer.Client); got != windowName {
		t.Fatalf("outer client shows %q, want the control room", got)
	}
}

func TestListCommandReusesRoomWhoseLivePaneClosed(t *testing.T) {
	room := startControlRoom(t)
	mustTmux(t, "kill-pane", "-t", "origin:"+windowName+".1")
	mustTmux(t, "select-window", "-t", "origin:shell")

	if err := ListCommand(room.outer.Client); err != nil {
		t.Fatal(err)
	}
	if got := mustTmux(t, "display-message", "-p", "-t", "origin:"+windowName+".0", "#{pane_id}"); got != room.listPaneID {
		t.Fatalf("room without a live pane was rebuilt: list pane %s, want %s", got, room.listPaneID)
	}
	if got := clientWindowName(room.outer.Client); got != windowName {
		t.Fatalf("outer client shows %q, want the control room", got)
	}
}

type testRoom struct {
	outer      *llmuxtmux.ClientInfo
	listPaneID string
	livePaneID string
	liveTTY    string
}

// startControlRoom builds an isolated tmux server holding a control room in
// session origin, whose live pane previews llm-agent, displayed by an outer
// client standing in for the user's terminal.
func startControlRoom(t *testing.T) testRoom {
	t.Helper()
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
	mustTmux(t, "split-window", "-d", "-h", "-t", "origin:"+windowName+".0", "env -u TMUX tmux attach-session -t llm-agent")
	mustTmux(t, "select-window", "-t", "origin:"+windowName)
	mustTmux(t, "new-session", "-d", "-x", "120", "-y", "30", "-s", "terminal", "-n", "display", "env -u TMUX tmux attach-session -t origin")

	room := testRoom{
		listPaneID: mustTmux(t, "display-message", "-p", "-t", "origin:"+windowName+".0", "#{pane_id}"),
		livePaneID: mustTmux(t, "display-message", "-p", "-t", "origin:"+windowName+".1", "#{pane_id}"),
		liveTTY:    mustTmux(t, "display-message", "-p", "-t", "origin:"+windowName+".1", "#{pane_tty}"),
	}
	waitFor(t, 2*time.Second, "outer and live-pane clients", func() bool {
		room.outer = findClient(func(c llmuxtmux.ClientInfo) bool { return c.Session == "origin" })
		return room.outer != nil && findClient(func(c llmuxtmux.ClientInfo) bool { return c.TTY == room.liveTTY }) != nil
	})
	return room
}

func findClient(match func(llmuxtmux.ClientInfo) bool) *llmuxtmux.ClientInfo {
	for _, client := range llmuxtmux.ListClients() {
		if match(client) {
			client := client
			return &client
		}
	}
	return nil
}

func clientWindowName(name string) string {
	if client := findClient(func(c llmuxtmux.ClientInfo) bool { return c.Client == name }); client != nil {
		return client.Window
	}
	return ""
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
