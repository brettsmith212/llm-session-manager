package state

import (
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"llm-session-manager/internal/tmux"
	"llm-session-manager/internal/types"
)

func TestWaitingNotificationMessage(t *testing.T) {
	tests := []struct {
		name    string
		session types.Session
		want    string
	}{
		{
			name:    "label preferred",
			session: types.Session{Label: "Review API", Path: "/work/api", Name: "llm-api"},
			want:    "Review API: Needs Review",
		},
		{
			name:    "directory fallback",
			session: types.Session{Label: "  ", Path: "/work/api-server", Name: "llm-api"},
			want:    "api-server: Needs Review",
		},
		{
			name:    "session fallback and escaping",
			session: types.Session{Name: "llm-#danger\nname"},
			want:    "llm-#danger name: Needs Review",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := waitingNotificationMessage(tt.session); got != tt.want {
				t.Fatalf("waitingNotificationMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNotificationWindowsExcludeManagedSessionsAndDeduplicateWindows(t *testing.T) {
	clients := []tmux.ClientInfo{
		{Client: "/dev/ttys001", Session: "0", WindowID: "@1", WindowWidth: 120},
		{Client: "/dev/ttys002", Session: "work", WindowID: "@2", WindowWidth: 100},
		{Client: "/dev/ttys003", Session: "other", WindowID: "@1", WindowWidth: 120},
		{Client: "/dev/ttys004", Session: "llm-api", WindowID: "@3", WindowWidth: 80},
		{Session: "0", WindowID: "@4", WindowWidth: 80},
	}
	want := []notificationWindow{{id: "@1", width: 120}, {id: "@2", width: 100}}
	if got := notificationWindows("llm-", clients); !reflect.DeepEqual(got, want) {
		t.Fatalf("notificationWindows() = %v, want %v", got, want)
	}
}

func TestSetStateNotifiesOnlyOnTransitionToWaiting(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	t.Setenv("TMUX", "")
	socketDir, err := os.MkdirTemp("/tmp", "llm-state-tmux-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	t.Setenv("TMUX_TMPDIR", socketDir)
	t.Cleanup(func() { _ = runTmux("kill-server") })

	mustTmux(t, "new-session", "-d", "-s", "llm-test", "-n", "opencode", "sleep 1000")
	mustTmux(t, "set-option", "-g", "@llm_session_prefix", "llm-")
	mustTmux(t, "set-option", "-t", "llm-test", "@llm_ever_attached", "1")
	mustTmux(t, "set-option", "-w", "-t", "llm-test:0", "@llm_agent", "opencode")
	mustTmux(t, "set-option", "-w", "-t", "llm-test:0", "@llm_path", "/work/test")
	pane := mustTmux(t, "display-message", "-p", "-t", "llm-test:0", "#{pane_id}")
	t.Setenv("TMUX_PANE", pane)

	originalNotify := sendWaitingNotification
	t.Cleanup(func() { sendWaitingNotification = originalNotify })
	var notifications []types.Session
	sendWaitingNotification = func(prefix string, session types.Session) {
		if prefix != "llm-" {
			t.Fatalf("notification prefix = %q", prefix)
		}
		notifications = append(notifications, session)
	}

	for _, next := range []types.State{types.Working, types.Waiting, types.Waiting, types.Idle, types.Waiting} {
		if err := SetState(next); err != nil {
			t.Fatalf("SetState(%s): %v", next, err)
		}
	}
	if len(notifications) != 2 {
		t.Fatalf("waiting notifications = %d, want 2", len(notifications))
	}
	for _, notification := range notifications {
		if notification.Path != "/work/test" || notification.WindowID == "" {
			t.Fatalf("notification session = %+v", notification)
		}
	}
	if got := mustTmux(t, "show-options", "-wqv", "-t", "llm-test:0", "@llm_state"); got != "waiting" {
		t.Fatalf("stored state = %q, want waiting", got)
	}
	if got := mustTmux(t, "show-options", "-gqv", "@llm_waiting_count"); got != "1" {
		t.Fatalf("waiting count = %q, want 1", got)
	}
}

func mustTmux(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("tmux", args...)
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func runTmux(args ...string) error {
	cmd := exec.Command("tmux", args...)
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	if err != nil && !strings.Contains(string(output), "no server running") {
		return fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, output)
	}
	return nil
}
