package claudehook

import (
	"encoding/json"
	"testing"

	"llm-session-manager/internal/state"
	"llm-session-manager/internal/types"
)

func TestReportFor(t *testing.T) {
	tests := []struct {
		name    string
		state   types.State
		payload string
		want    state.Report
	}{
		{
			name:    "a prompt names the task and records the conversation",
			state:   types.Working,
			payload: `{"hook_event_name":"UserPromptSubmit","session_id":"abc-123","prompt":"fix the flaky auth test"}`,
			want:    state.Report{State: types.Working, AgentSession: "abc-123", Prompt: "fix the flaky auth test"},
		},
		{
			name:    "clear starts a new conversation",
			state:   types.Idle,
			payload: `{"hook_event_name":"SessionStart","session_id":"def","source":"clear"}`,
			want:    state.Report{State: types.Idle, AgentSession: "def", NewConversation: true},
		},
		{
			name:    "permission requests name the command",
			state:   types.Waiting,
			payload: `{"hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"npm run db:migrate"}}`,
			want:    state.Report{State: types.Waiting, ReasonKind: types.ReasonPermission, Reason: "Bash · npm run db:migrate"},
		},
		{
			name:    "questions show what they ask",
			state:   types.Waiting,
			payload: `{"hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":"Which cache?"}]}}`,
			want:    state.Report{State: types.Waiting, ReasonKind: types.ReasonQuestion, Reason: "Which cache?"},
		},
		{
			name:    "plans show their first line",
			state:   types.Waiting,
			payload: `{"hook_event_name":"PreToolUse","tool_name":"ExitPlanMode","tool_input":{"plan":"# Add retries\n..."}}`,
			want:    state.Report{State: types.Waiting, ReasonKind: types.ReasonPlan, Reason: "Add retries"},
		},
		{
			name:    "notifications only fill in a missing reason",
			state:   types.Waiting,
			payload: `{"hook_event_name":"Notification","notification_type":"permission_prompt","message":"Claude needs your permission to use Bash"}`,
			want:    state.Report{State: types.Waiting, ReasonKind: types.ReasonPermission, Reason: "Bash", KeepReason: true},
		},
		{
			name:    "failed turns are errors",
			state:   types.Waiting,
			payload: `{"hook_event_name":"StopFailure","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			want:    state.Report{State: types.Waiting, ReasonKind: types.ReasonError, Reason: "Overloaded"},
		},
		{
			name:    "an unreadable payload still records the configured state",
			state:   types.Working,
			payload: `{`,
			want:    state.Report{State: types.Working},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p payload
			_ = json.Unmarshal([]byte(tt.payload), &p)
			if got := ReportFor(tt.state, p); got != tt.want {
				t.Fatalf("ReportFor() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
