package codexhook

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"llm-session-manager/internal/types"
)

func TestStateFor(t *testing.T) {
	tests := []struct {
		event    string
		toolName string
		want     types.State
		wantOK   bool
	}{
		{event: "UserPromptSubmit", want: types.Working, wantOK: true},
		{event: "PreToolUse", toolName: "exec_command", want: types.Working, wantOK: true},
		{event: "PreToolUse", toolName: "request_user_input", want: types.Waiting, wantOK: true},
		{event: "PermissionRequest", toolName: "exec_command", want: types.Waiting, wantOK: true},
		{event: "PostToolUse", toolName: "request_user_input", want: types.Working, wantOK: true},
		{event: "Stop", want: types.Idle, wantOK: true},
		{event: "Interrupt", want: types.Idle, wantOK: true},
		{event: "SessionEnd", want: types.Idle, wantOK: true},
		{event: "SessionStart"},
		{event: "PostCompact"},
		{event: "SubagentStop"},
	}
	for _, tt := range tests {
		t.Run(tt.event+"/"+tt.toolName, func(t *testing.T) {
			got, ok := StateFor(tt.event, tt.toolName)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("StateFor(%q, %q) = %q, %v; want %q, %v", tt.event, tt.toolName, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestReportForAddsReasonsPromptsAndSession(t *testing.T) {
	decode := func(raw string) payload {
		var p payload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	report, ok := ReportFor(decode(`{"hook_event_name":"PermissionRequest","session_id":"s1","tool_name":"exec_command","tool_input":{"command":["bash","-lc","npm test"]}}`))
	if !ok || report.State != types.Waiting || report.ReasonKind != types.ReasonPermission || report.Reason != "exec_command · npm test" || report.AgentSession != "s1" {
		t.Fatalf("permission report = %+v, %v", report, ok)
	}
	report, _ = ReportFor(decode(`{"hook_event_name":"PreToolUse","tool_name":"request_user_input","tool_input":{"questions":[{"question":"Which port?"}]}}`))
	if report.ReasonKind != types.ReasonQuestion || report.Reason != "Which port?" {
		t.Fatalf("question report = %+v", report)
	}
	report, _ = ReportFor(decode(`{"hook_event_name":"UserPromptSubmit","prompt":"add rate limiting"}`))
	if report.State != types.Working || report.Prompt != "add rate limiting" {
		t.Fatalf("prompt report = %+v", report)
	}
	if _, ok := ReportFor(decode(`{"hook_event_name":"SessionStart"}`)); ok {
		t.Fatal("SessionStart produced a report")
	}
}

func TestRunRecordsStateForCurrentPane(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	t.Setenv("TMUX", "")
	socketDir, err := os.MkdirTemp("/tmp", "llm-codexhook-tmux-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	t.Setenv("TMUX_TMPDIR", socketDir)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-server").Run() })

	mustTmux(t, "new-session", "-d", "-s", "llm-test", "-n", "codex", "sleep 1000")
	mustTmux(t, "set-option", "-g", "@llm_session_prefix", "llm-")
	mustTmux(t, "set-option", "-g", "@llm_chime", "false")
	mustTmux(t, "set-option", "-t", "llm-test", "@llm_ever_attached", "1")
	mustTmux(t, "set-option", "-w", "-t", "llm-test:0", "@llm_agent", "codex")
	t.Setenv("TMUX_PANE", mustTmux(t, "display-message", "-p", "-t", "llm-test:0", "#{pane_id}"))

	steps := []struct {
		payload string
		want    string
	}{
		{`{"hook_event_name":"UserPromptSubmit","prompt":"hi"}`, "working"},
		{`{"hook_event_name":"PreToolUse","tool_name":"request_user_input","tool_input":{}}`, "waiting"},
		{`{"hook_event_name":"PostToolUse","tool_name":"request_user_input","tool_input":{}}`, "working"},
		{`{"hook_event_name":"PermissionRequest","tool_name":"exec_command","tool_input":{}}`, "waiting"},
		{`{"hook_event_name":"PreToolUse","tool_name":"exec_command","tool_input":{}}`, "working"},
		{`{"hook_event_name":"Stop","stop_hook_active":false}`, "idle"},
		{`{"hook_event_name":"SessionStart","source":"compact"}`, "idle"},
	}
	for _, step := range steps {
		if err := Run(strings.NewReader(step.payload)); err != nil {
			t.Fatalf("Run(%s): %v", step.payload, err)
		}
		if got := mustTmux(t, "show-options", "-wqv", "-t", "llm-test:0", "@llm_state"); got != step.want {
			t.Fatalf("after %s state = %q, want %q", step.payload, got, step.want)
		}
	}

	if err := Run(strings.NewReader("not json")); err == nil {
		t.Fatal("Run accepted a malformed payload")
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
