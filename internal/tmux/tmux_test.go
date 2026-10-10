package tmux

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDisplayFloatingNotificationIsDetachedAndTransient(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	t.Setenv("TMUX", "")
	socketDir, err := os.MkdirTemp("/tmp", "llmux-floating-tmux-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	t.Setenv("TMUX_TMPDIR", socketDir)
	t.Cleanup(func() { _ = RunRaw([]string{"kill-server"}) })

	if _, err := Run([]string{"new-session", "-d", "-x", "120", "-y", "30", "-s", "target", "-n", "editor", "cat"}); err != nil {
		t.Fatal(err)
	}
	if !SupportsFloatingPanes() {
		t.Skip("tmux does not support floating panes")
	}
	originalPane, err := DisplayMessage("#{pane_id}", "target:editor")
	if err != nil {
		t.Fatal(err)
	}

	paneID, err := DisplayFloatingNotification("target:editor", 120, "Obsidian #notes: Needs Review", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if paneID == "" {
		t.Fatal("DisplayFloatingNotification returned no pane")
	}
	geometry, err := DisplayMessage("#{pane_floating_flag}|#{pane_active}|#{pane_left}|#{pane_width}|#{pane_top}|#{pane_height}|#{@llm_notification}", paneID)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(geometry, "|")
	if len(parts) != 7 || parts[0] != "1" || parts[1] != "0" || parts[6] != "1" {
		t.Fatalf("floating pane geometry = %q", geometry)
	}
	// tmux 3.7 reports the requested 3-row box; 3.8 draws its border inside
	// the box and reports the single content row within it.
	left, leftErr := strconv.Atoi(parts[2])
	width, widthErr := strconv.Atoi(parts[3])
	top, topErr := strconv.Atoi(parts[4])
	height, heightErr := strconv.Atoi(parts[5])
	if leftErr != nil || widthErr != nil || topErr != nil || heightErr != nil {
		t.Fatalf("floating pane geometry = %q", geometry)
	}
	if right := left + width; right < 119 || right > 120 || top > 1 || height < 1 || height > 3 {
		t.Fatalf("floating pane is not top-right: geometry=%q", geometry)
	}
	if active, err := DisplayMessage("#{pane_id}", "target:editor"); err != nil || active != originalPane {
		t.Fatalf("active pane = %q, want unchanged %q (err=%v)", active, originalPane, err)
	}
	if _, err := Run([]string{"send-keys", "-t", originalPane, "-l", "typing-continues"}); err != nil {
		t.Fatal(err)
	}
	if output, err := Run([]string{"capture-pane", "-p", "-t", originalPane}); err != nil || !strings.Contains(output, "typing-continues") {
		t.Fatalf("input did not reach original pane: %q (err=%v)", output, err)
	}
	if output, err := Run([]string{"capture-pane", "-p", "-t", paneID}); err != nil || !strings.Contains(output, "Obsidian #notes: Needs Review") {
		t.Fatalf("notification content = %q (err=%v)", output, err)
	}

	replacementID, err := DisplayFloatingNotification("target:editor", 120, "API: Needs Review", 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if replacementID == paneID || hasPane(paneID) {
		panes, _ := Run([]string{"list-panes", "-t", "target:editor", "-F", "#{pane_id}|#{@llm_notification}"})
		t.Fatalf("replacement pane %q did not remove previous pane %q; panes=%q", replacementID, paneID, panes)
	}
	time.Sleep(300 * time.Millisecond)
	if hasPane(replacementID) {
		t.Fatalf("floating notification pane %q remained after command exit", replacementID)
	}
}

func TestAlertClientsWritesOncePerTTY(t *testing.T) {
	first, err := os.CreateTemp("", "llmux-bell-first-")
	if err != nil {
		t.Fatal(err)
	}
	firstPath := first.Name()
	_ = first.Close()
	t.Cleanup(func() { _ = os.Remove(firstPath) })

	second, err := os.CreateTemp("", "llmux-bell-second-")
	if err != nil {
		t.Fatal(err)
	}
	secondPath := second.Name()
	_ = second.Close()
	t.Cleanup(func() { _ = os.Remove(secondPath) })

	AlertClients([]ClientInfo{
		{TTY: firstPath},
		{TTY: firstPath},
		{TTY: secondPath},
		{},
	}, true, false, "ignored")

	for _, path := range []string{firstPath, secondPath} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "\a" {
			t.Fatalf("bell output for %s = %q, want BEL", path, got)
		}
	}

	if err := os.WriteFile(firstPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	AlertClients([]ClientInfo{{TTY: firstPath}}, true, true, "api: Permission · Bash · npm test\n")
	got, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := "\a\x1b]9;api: Permission · Bash · npm test \x07"; string(got) != want {
		t.Fatalf("bell and desktop output = %q, want %q", got, want)
	}
}

func TestDesktopNotificationAvoidsConEmuCommands(t *testing.T) {
	if got := string(DesktopNotification("4;3 agents")); got != "\x1b]9;llmux: 4;3 agents\x07" {
		t.Fatalf("DesktopNotification() = %q", got)
	}
}

func TestBatchSeparatesCommandsAndKeepsTrailingSemicolons(t *testing.T) {
	got := Batch(
		SetWindowOptionArgs("@1", "@llm_label", "fix parser;"),
		nil,
		[]string{"wait-for", "-S", "channel"},
	)
	want := []string{"set-window-option", "-t", "@1", "@llm_label", `fix parser\;`, ";", "wait-for", "-S", "channel"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("Batch() = %q, want %q", got, want)
	}

	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	t.Setenv("TMUX", "")
	socketDir, err := os.MkdirTemp("/tmp", "llmux-batch-tmux-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	t.Setenv("TMUX_TMPDIR", socketDir)
	t.Cleanup(func() { _ = RunRaw([]string{"kill-server"}) })
	if _, err := Run([]string{"new-session", "-d", "-s", "target", "sleep 1000"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(Batch(
		SetWindowOptionArgs("target:", "@llm_label", "fix parser;"),
		SetGlobalOptionArgs("@after", "ran"),
	)); err != nil {
		t.Fatal(err)
	}
	if label, after := GetWindowOption("target:", "@llm_label"), GetGlobalOption("@after", ""); label != "fix parser;" || after != "ran" {
		t.Fatalf("label=%q after=%q, want the literal label and both commands run", label, after)
	}
}

func hasPane(paneID string) bool {
	result := RunRaw([]string{"display-message", "-p", "-t", paneID, "#{pane_id}"})
	return result.ExitCode == 0 && strings.TrimSpace(result.Stdout) == paneID
}
