package state

import (
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"llm-session-manager/internal/sessions"
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
		{
			name:    "automatic name and reason",
			session: types.Session{AutoLabel: "fix flaky test", ReasonKind: types.ReasonPermission, Reason: "Bash · npm test"},
			want:    "fix flaky test: Permission · Bash · npm test",
		},
		{
			name:    "reason without detail",
			session: types.Session{Path: "/work/api", ReasonKind: types.ReasonPlan},
			want:    "api: Plan ready",
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

func TestChimeEnabled(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  bool
	}{
		{value: "true", want: true},
		{value: "True", want: true},
		{value: "", want: true},
		{value: "anything", want: true},
		{value: "false", want: false},
		{value: " FALSE ", want: false},
	} {
		if got := chimeEnabled(tt.value); got != tt.want {
			t.Errorf("chimeEnabled(%q) = %v, want %v", tt.value, got, tt.want)
		}
	}
}

func TestNotifyModeHonorsChimeUntilNotifyIsSet(t *testing.T) {
	for _, tt := range []struct {
		notify, chime string
		want          alertMode
	}{
		{"", "true", alertMode{bell: true}},
		{"", "false", alertMode{}},
		{"desktop", "true", alertMode{desktop: true}},
		{"both", "false", alertMode{bell: true, desktop: true}},
		{"off", "true", alertMode{}},
		{" Bell ", "false", alertMode{bell: true}},
	} {
		if got := notifyMode(tt.notify, tt.chime); got != tt.want {
			t.Errorf("notifyMode(%q, %q) = %+v, want %+v", tt.notify, tt.chime, got, tt.want)
		}
	}
}

func TestAutoLabel(t *testing.T) {
	for prompt, want := range map[string]string{
		"\n  fix the flaky auth test\nthen run CI": "fix the flaky auth test",
		"/clear": "",
		"   ":    "",
		"why does /health return 503 under load when the cache is cold?": "why does /health return 503 under load when the cache is co…",
	} {
		if got := AutoLabel(prompt); got != want {
			t.Errorf("AutoLabel(%q) = %q, want %q", prompt, got, want)
		}
	}
}

func TestRecordKeepsReasonsUnseenMarksAndTaskNames(t *testing.T) {
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

	mustTmux(t, "new-session", "-d", "-s", "llm-test", "-n", "claude", "sleep 1000")
	mustTmux(t, "set-option", "-t", "llm-test", "@llm_ever_attached", "1")
	mustTmux(t, "set-option", "-w", "-t", "llm-test:0", "@llm_agent", "claude")
	window := mustTmux(t, "display-message", "-p", "-t", "llm-test:", "#{window_id}")
	t.Setenv("TMUX_PANE", mustTmux(t, "display-message", "-p", "-t", "llm-test:", "#{pane_id}"))
	originalNotify := sendWaitingNotification
	t.Cleanup(func() { sendWaitingNotification = originalNotify })
	var notified []types.Session
	sendWaitingNotification = func(_ string, session types.Session) { notified = append(notified, session) }
	option := func(name string) string { return mustTmux(t, "show-options", "-wqv", "-t", window, name) }
	record := func(report Report) {
		t.Helper()
		if err := Record(report); err != nil {
			t.Fatalf("Record(%+v): %v", report, err)
		}
	}

	record(Report{State: types.Working, Prompt: "fix the flaky auth test", AgentSession: "conv-1"})
	if option("@llm_label_auto") != "fix the flaky auth test" || option("@llm_agent_session") != "conv-1" {
		t.Fatalf("first prompt did not name the task: auto=%q session=%q", option("@llm_label_auto"), option("@llm_agent_session"))
	}
	record(Report{State: types.Working, Prompt: "also update the docs"})
	if option("@llm_label_auto") != "fix the flaky auth test" {
		t.Fatalf("a later prompt renamed the task to %q", option("@llm_label_auto"))
	}

	record(Report{State: types.Waiting, ReasonKind: types.ReasonPermission, Reason: "Bash · npm test"})
	record(Report{State: types.Waiting, ReasonKind: types.ReasonPermission, Reason: "Bash", KeepReason: true})
	if option("@llm_reason_kind") != "permission" || option("@llm_reason") != "Bash · npm test" {
		t.Fatalf("reason = %q/%q, want the specific permission reason kept", option("@llm_reason_kind"), option("@llm_reason"))
	}
	if len(notified) != 1 || notified[0].Reason != "Bash · npm test" || notified[0].AutoLabel != "fix the flaky auth test" {
		t.Fatalf("notifications = %+v, want one carrying the reason and task name", notified)
	}

	record(Report{State: types.Working})
	if option("@llm_reason_kind")+option("@llm_reason") != "" {
		t.Fatal("leaving the wait did not forget its reason")
	}
	record(Report{State: types.Idle})
	if option("@llm_unseen") != "1" {
		t.Fatal("a finished turn was not marked unseen")
	}
	if got := mustTmux(t, "show-options", "-gqv", "@llm_status"); got != "✓ 1 done" {
		t.Fatalf("@llm_status = %q, want one finished result", got)
	}
	record(Report{State: types.Waiting})
	if option("@llm_unseen") != "" {
		t.Fatal("new activity did not supersede the unseen mark")
	}
	if got := mustTmux(t, "show-options", "-gqv", "@llm_status"); got != "◆ 1 needs you" {
		t.Fatalf("@llm_status = %q, want one waiting agent", got)
	}

	record(Report{State: types.Idle, NewConversation: true})
	if option("@llm_label_auto") != "" {
		t.Fatal("a new conversation kept the previous automatic task name")
	}
	mustTmux(t, "set-option", "-w", "-t", window, "@llm_label", "Auth retries")
	record(Report{State: types.Working, Prompt: "something else"})
	if option("@llm_label_auto") != "" {
		t.Fatal("a manual label was shadowed by an automatic one")
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

func TestSetStateHeartbeatKeepsTransitionTimeAndSignalsOnlyChanges(t *testing.T) {
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

	mustTmux(t, "new-session", "-d", "-s", "llm-test", "-n", "claude", "sleep 1000")
	mustTmux(t, "set-option", "-t", "llm-test", "@llm_ever_attached", "1")
	window := mustTmux(t, "display-message", "-p", "-t", "llm-test:", "#{window_id}")
	t.Setenv("TMUX_PANE", mustTmux(t, "display-message", "-p", "-t", "llm-test:", "#{pane_id}"))
	originalNotify := sendWaitingNotification
	t.Cleanup(func() { sendWaitingNotification = originalNotify })
	sendWaitingNotification = func(string, types.Session) {}

	if err := SetState(types.Working); err != nil {
		t.Fatal(err)
	}
	if got := mustTmux(t, "show-options", "-wqv", "-t", window, "@llm_agent"); got != "1" {
		t.Fatalf("first report did not mark the window as managed: @llm_agent=%q", got)
	}
	mustTmux(t, "set-option", "-w", "-t", window, "@llm_state_at", "100")
	mustTmux(t, "set-option", "-w", "-t", window, "@llm_seen_at", "100")
	// Nothing was waiting when that first change signalled, so tmux holds the
	// wake for the next waiter. Consume it.
	mustTmux(t, "wait-for", sessions.ChangedChannel)

	// Repeating a state only refreshes the heartbeat, so the time shown and
	// sorted on stays the moment the agent started working.
	waiter := startWaiter(t)
	if err := SetState(types.Working); err != nil {
		t.Fatal(err)
	}
	if got := mustTmux(t, "show-options", "-wqv", "-t", window, "@llm_state_at"); got != "100" {
		t.Fatalf("@llm_state_at = %q after a repeated state, want 100", got)
	}
	if got := mustTmux(t, "show-options", "-wqv", "-t", window, "@llm_seen_at"); got == "100" || got == "" {
		t.Fatalf("@llm_seen_at = %q, want a refreshed heartbeat", got)
	}
	select {
	case <-waiter:
		t.Fatal("a repeated state woke Control Rooms")
	case <-time.After(300 * time.Millisecond):
	}

	if err := SetState(types.Waiting); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waiter:
	case <-time.After(2 * time.Second):
		t.Fatal("a state change did not wake Control Rooms")
	}
	if got := mustTmux(t, "show-options", "-wqv", "-t", window, "@llm_state_at"); got == "100" {
		t.Fatal("@llm_state_at was not updated by a state change")
	}
	if got := mustTmux(t, "show-options", "-gqv", "@llm_status"); got != "◆ 1 needs you" {
		t.Fatalf("@llm_status = %q, want one waiting agent", got)
	}
	if err := SetState(types.Idle); err != nil {
		t.Fatal(err)
	}
	if got := mustTmux(t, "show-options", "-gqv", "@llm_waiting_count") + "|" + mustTmux(t, "show-options", "-gqv", "@llm_status"); got != "0|" {
		t.Fatalf("waiting count|status = %q, want 0 and an empty status", got)
	}
}

// startWaiter blocks on the Control Room change channel in the background.
func startWaiter(t *testing.T) <-chan struct{} {
	t.Helper()
	cmd := exec.Command("tmux", "wait-for", sessions.ChangedChannel)
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	woken := make(chan struct{})
	go func() {
		if cmd.Wait() == nil {
			close(woken)
		}
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	// Give tmux a moment to register the waiter before anything signals.
	time.Sleep(100 * time.Millisecond)
	return woken
}

func TestSeedIdleKeepsReportedState(t *testing.T) {
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

	mustTmux(t, "new-session", "-d", "-s", "llm-test", "-n", "codex", "sleep 1000")
	if err := SeedIdle("llm-test:0"); err != nil {
		t.Fatalf("SeedIdle: %v", err)
	}
	if got := mustTmux(t, "show-options", "-wqv", "-t", "llm-test:0", "@llm_state"); got != "idle" {
		t.Fatalf("seeded state = %q, want idle", got)
	}
	if got := mustTmux(t, "show-options", "-wqv", "-t", "llm-test:0", "@llm_state_at"); got == "" {
		t.Fatal("seeded state has no timestamp")
	}

	mustTmux(t, "set-option", "-w", "-t", "llm-test:0", "@llm_state", "working")
	if err := SeedIdle("llm-test:0"); err != nil {
		t.Fatalf("SeedIdle: %v", err)
	}
	if got := mustTmux(t, "show-options", "-wqv", "-t", "llm-test:0", "@llm_state"); got != "working" {
		t.Fatalf("state after reseed = %q, want working", got)
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
