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
	if len(parts) != 7 || parts[0] != "1" || parts[1] != "0" || parts[4] != "0" || parts[5] != "3" || parts[6] != "1" {
		t.Fatalf("floating pane geometry = %q", geometry)
	}
	left, leftErr := strconv.Atoi(parts[2])
	width, widthErr := strconv.Atoi(parts[3])
	if leftErr != nil || widthErr != nil || left+width != 120 {
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

func hasPane(paneID string) bool {
	result := RunRaw([]string{"display-message", "-p", "-t", paneID, "#{pane_id}"})
	return result.ExitCode == 0 && strings.TrimSpace(result.Stdout) == paneID
}
