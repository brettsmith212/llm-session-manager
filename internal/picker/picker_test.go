package picker

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"llm-session-manager/internal/ansi"
	"llm-session-manager/internal/types"
)

func TestFilteredMatchesSessionMeaning(t *testing.T) {
	now := time.Now().Unix()
	p := &picker{sessions: []types.Session{
		{
			Name:        "llm-api",
			WindowID:    "@1",
			WindowIndex: 2,
			WindowName:  "claude",
			State:       types.Working,
			StateAt:     now,
			Path:        "/work/API-Service",
		},
		{
			Name:        "llm-web",
			WindowID:    "@2",
			WindowIndex: 0,
			WindowName:  "amp",
			State:       types.Waiting,
			StateAt:     now,
			Path:        "/work/web-client",
		},
		{
			Name:        "llm-old",
			WindowID:    "@3",
			WindowIndex: 1,
			WindowName:  "opencode",
			State:       types.Working,
			StateAt:     now - 301,
			Path:        "/work/legacy",
		},
	}}

	tests := []struct {
		name    string
		query   string
		wantIDs []string
	}{
		{name: "empty returns every session", wantIDs: []string{"@1", "@2", "@3"}},
		{name: "path is case insensitive", query: "api-service", wantIDs: []string{"@1"}},
		{name: "agent and state terms combine", query: "claude working", wantIDs: []string{"@1"}},
		{name: "human attention label is searchable", query: "needs you", wantIDs: []string{"@2"}},
		{name: "window number is searchable", query: "#2", wantIDs: []string{"@1"}},
		{name: "stale working session is effectively idle", query: "legacy idle", wantIDs: []string{"@3"}},
		{name: "all terms must match", query: "api amp", wantIDs: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p.query = tt.query
			got := p.filtered()
			gotIDs := make([]string, len(got))
			for i, session := range got {
				gotIDs[i] = session.WindowID
			}
			if fmt.Sprint(gotIDs) != fmt.Sprint(tt.wantIDs) {
				t.Fatalf("filtered IDs = %v, want %v", gotIDs, tt.wantIDs)
			}
		})
	}
}

func TestVisibleRangeKeepsSelectionWithinAvailableRows(t *testing.T) {
	list := []types.Session{
		{Name: "project-a", WindowID: "@1"},
		{Name: "project-a", WindowID: "@2"},
		{Name: "project-b", WindowID: "@3"},
		{Name: "project-c", WindowID: "@4"},
		{Name: "project-c", WindowID: "@5"},
	}

	for selected := range list {
		for rows := 2; rows <= 7; rows++ {
			start, end := visibleRange(list, selected, rows)
			if !(start <= selected && selected < end) {
				t.Fatalf("selection %d not in visible range [%d:%d] with %d rows", selected, start, end, rows)
			}
			if used := renderedListRows(list[start:end]); used > rows {
				t.Fatalf("visible range [%d:%d] uses %d rows, only %d available", start, end, used, rows)
			}
		}
	}

	start, end := visibleRange(list, 3, 1)
	if start != 3 || end != 4 {
		t.Fatalf("one-row viewport = [%d:%d], want selected item [3:4]", start, end)
	}
}

func TestANSIContentIsClippedWithoutBreakingUTF8(t *testing.T) {
	value := ansi.Foreground(ansi.Blue) + "a-very-long-πroject-name" + ansi.Reset
	got := fitANSI(value, 12)
	if width := ansiWidth(got); width > 12 {
		t.Fatalf("clipped width = %d, want <= 12", width)
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("clipped value %q does not indicate truncation", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("clipped value is invalid UTF-8: %q", got)
	}

	if got := truncateVisible("claude-α", "", "", 6); got != "claud…" {
		t.Fatalf("Unicode-safe truncation = %q, want %q", got, "claud…")
	}
}

func TestSelectedRowHighlightContinuesBehindWindowNumber(t *testing.T) {
	frame := newScreenFrame(80, 1)
	drawItem(frame, types.Session{
		Name:        "llm-api",
		WindowIndex: 0,
		WindowName:  "claude",
		State:       types.Idle,
		Path:        "/work/api",
	}, 80, true, 1, true)

	output := captureStdout(t, frame.flush)
	marker := strings.LastIndex(output, "#0")
	if marker < 0 {
		t.Fatalf("selected row did not render its window number: %q", output)
	}
	beforeMarker := output[:marker]
	lastHighlight := strings.LastIndex(beforeMarker, ansi.Background(ansi.Surface0))
	lastReset := strings.LastIndex(beforeMarker, ansi.Reset)
	if lastHighlight < lastReset {
		t.Fatal("selected row reset its background before the window number")
	}
}

func TestPreviewQueueCoalescesToTheFinalSelection(t *testing.T) {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	t.Cleanup(func() { timer.Stop() })

	p := &picker{previewTimer: timer, previewWindowID: "@1"}
	p.queuePreview(types.Session{WindowID: "@2"})
	p.queuePreview(types.Session{WindowID: "@3"})
	if !p.hasPendingPreview || p.pendingPreview.WindowID != "@3" {
		t.Fatalf("pending preview = %#v, want final selection @3", p.pendingPreview)
	}

	// Returning to the preview that is already visible must cancel all
	// intermediate work instead of briefly attaching another session.
	p.queuePreview(types.Session{WindowID: "@1"})
	if p.hasPendingPreview {
		t.Fatal("returning to the displayed preview did not cancel pending work")
	}
}

// TestPickerHelperProcess runs the real picker inside a tmux pane for the
// isolated workflow test below. It is a no-op during a normal test process.
func TestPickerHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_PICKER_HELPER_PROCESS") != "1" {
		return
	}
	if err := Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func TestPickerWorkflowInIsolatedTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}

	t.Setenv("TMUX", "")
	socketDir, err := os.MkdirTemp("/tmp", "llm-picker-tmux-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	t.Setenv("TMUX_TMPDIR", socketDir)
	mustTmux(t, "new-session", "-d", "-x", "150", "-y", "36", "-s", "origin", "-n", "shell", "sleep 1000")
	t.Cleanup(func() { _, _ = runTmux("kill-server") })

	addManagedTestSession(t, "llm-zeta", "amp", "/tmp/zeta-project", types.Waiting)
	addManagedTestSession(t, "llm-alpha", "claude", "/tmp/alpha-project", types.Working)
	addManagedTestSession(t, "llm-beta", "opencode", "/tmp/beta-project", types.Idle)
	mustTmux(t, "set-option", "-g", "@llm_session_prefix", "llm-")
	mustTmux(t, "set-option", "-g", "@llm_command", "amp")

	mustTmux(t, "new-window", "-d", "-t", "origin:", "-n", windowName, "sleep 1000")
	mustTmux(t, "split-window", "-d", "-h", "-l", "67%", "-t", "origin:"+windowName+".0", "sleep 1000")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helperCommand := "GO_WANT_PICKER_HELPER_PROCESS=1 " + shellQuote(executable) + " -test.run=^TestPickerHelperProcess$"
	mustTmux(t, "respawn-pane", "-k", "-t", "origin:"+windowName+".0", helperCommand)

	waitFor(t, 5*time.Second, "picker startup", func() bool {
		return strings.Contains(capturePicker(), "Sessions") && previewTitle() == "▶ Preview · alpha-project · claude #0"
	})

	// Pasted command characters outside search mode are inert.
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "-l", "\x1b[200~q\x1b[201~")
	time.Sleep(100 * time.Millisecond)
	if !tmuxSucceeds("has-session", "-t", "origin") || !strings.Contains(mustTmux(t, "list-windows", "-t", "origin", "-F", "#{window_name}"), windowName) {
		t.Fatal("bracketed paste triggered a picker command")
	}

	// Search is semantic, and attention navigation is global rather than
	// being trapped inside a filter.
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "/", "alpha", "Enter")
	waitFor(t, 2*time.Second, "committed filter", func() bool {
		return strings.Contains(capturePicker(), "filter: alpha")
	})
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "n")
	waitFor(t, 2*time.Second, "global attention navigation", func() bool {
		return !strings.Contains(capturePicker(), "filter:") && previewTitle() == "▶ Preview · zeta-project · amp #0"
	})

	// A delayed preview update must not pull focus back from the live pane.
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "k")
	mustTmux(t, "select-pane", "-t", "origin:"+windowName+".1")
	waitFor(t, 2*time.Second, "focus-preserving preview update", func() bool {
		active := mustTmux(t, "display-message", "-p", "-t", "origin:"+windowName+".1", "#{pane_active}")
		return active == "1" && previewTitle() == "▶ Preview · beta-project · opencode #0"
	})
	mustTmux(t, "select-pane", "-t", "origin:"+windowName+".0")

	// This is the add prompt's real handoff contract: remain in the picker,
	// consume the exact new window ID, select it, and preview it.
	newWindowID := addManagedTestSession(t, "llm-delta", "codex", "/tmp/delta-project", types.Idle)
	mustTmux(t, "set-option", "-w", "-t", "origin:"+windowName, pickerSelectionOption, newWindowID)
	waitFor(t, 4*time.Second, "new session handoff", func() bool {
		return tmuxSucceeds("has-session", "-t", "origin") &&
			strings.Contains(capturePicker(), "3/4") &&
			previewTitle() == "▶ Preview · delta-project · codex #0"
	})

	// Waiting/working sessions require confirmation, Escape cancels, and the
	// second Ctrl-X performs the stop. Idle sessions stop immediately.
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "n", "C-x")
	waitFor(t, 2*time.Second, "stop confirmation", func() bool {
		return strings.Contains(capturePicker(), "^x confirm")
	})
	if !tmuxSucceeds("has-session", "-t", "llm-zeta") {
		t.Fatal("first Ctrl-X stopped a session that required confirmation")
	}
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "Escape")
	waitFor(t, 2*time.Second, "confirmation cancellation", func() bool {
		return !strings.Contains(capturePicker(), "^x confirm")
	})
	if !tmuxSucceeds("has-session", "-t", "llm-zeta") {
		t.Fatal("Escape did not cancel the pending stop")
	}
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "C-x")
	waitFor(t, 2*time.Second, "second stop confirmation", func() bool {
		return strings.Contains(capturePicker(), "^x confirm")
	})
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "C-x")
	waitFor(t, 2*time.Second, "confirmed stop", func() bool {
		return !tmuxSucceeds("has-session", "-t", "llm-zeta")
	})

	// The selection falls back to the newly-added idle session after zeta is
	// removed, so one Ctrl-X should stop it without another confirmation.
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "C-x")
	waitFor(t, 2*time.Second, "immediate idle stop", func() bool {
		return !tmuxSucceeds("has-session", "-t", "llm-delta")
	})
	if strings.Contains(capturePicker(), "^x confirm") {
		t.Fatal("idle session unexpectedly required stop confirmation")
	}
}

func addManagedTestSession(t *testing.T, name, agentName, path string, state types.State) string {
	t.Helper()
	mustTmux(t, "new-session", "-d", "-x", "150", "-y", "36", "-s", name, "-n", agentName, "-c", "/tmp", "sleep 1000")
	mustTmux(t, "set-option", "-t", name, "@llm_path", path)
	mustTmux(t, "set-option", "-t", name, "@llm_ever_attached", "1")
	target := name + ":0"
	mustTmux(t, "set-option", "-w", "-t", target, "@llm_agent", agentName)
	mustTmux(t, "set-option", "-w", "-t", target, "@llm_path", path)
	mustTmux(t, "set-option", "-w", "-t", target, "@llm_state", string(state))
	mustTmux(t, "set-option", "-w", "-t", target, "@llm_state_at", fmt.Sprint(time.Now().Unix()))
	return mustTmux(t, "display-message", "-p", "-t", target, "#{window_id}")
}

func capturePicker() string {
	output, _ := runTmux("capture-pane", "-p", "-t", "origin:"+windowName+".0")
	return output
}

func previewTitle() string {
	output, _ := runTmux("display-message", "-p", "-t", "origin:"+windowName+".1", "#{pane_title}")
	return output
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
	t.Fatalf("timed out waiting for %s\npicker:\n%s", description, capturePicker())
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
	command := exec.Command("tmux", args...)
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func tmuxSucceeds(args ...string) bool {
	_, err := runTmux(args...)
	return err == nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func captureStdout(t *testing.T, write func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	write()
	os.Stdout = original
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return string(output)
}
