// Package claudehook records Claude Code lifecycle hooks. The state for each
// hook comes from the hook configuration, so a payload llmux cannot read still
// records the state; the payload adds what the Control Room shows: why the
// agent is waiting, the prompt that names the task and the conversation id
// used to resume it.
package claudehook

import (
	"encoding/json"
	"io"
	"strings"

	"llm-session-manager/internal/hookpayload"
	"llm-session-manager/internal/state"
	"llm-session-manager/internal/types"
)

// payloadLimit bounds how much of a hook payload is read. Tool inputs can
// carry whole files; everything llmux needs comes first or is small.
const payloadLimit = 8 << 20

type payload struct {
	HookEventName    string          `json:"hook_event_name"`
	SessionID        string          `json:"session_id"`
	Source           string          `json:"source"`
	Prompt           string          `json:"prompt"`
	ToolName         string          `json:"tool_name"`
	ToolInput        json.RawMessage `json:"tool_input"`
	Message          string          `json:"message"`
	NotificationType string          `json:"notification_type"`
	Error            json.RawMessage `json:"error"`
	ErrorMessage     string          `json:"error_message"`
}

const permissionNotice = "Claude needs your permission to use "

// ReportFor builds the report for one hook whose configured state is next.
func ReportFor(next types.State, p payload) state.Report {
	report := state.Report{State: next, AgentSession: p.SessionID}
	switch p.HookEventName {
	case "SessionStart":
		report.NewConversation = p.Source == "clear"
	case "UserPromptSubmit":
		report.Prompt = p.Prompt
	}
	if next != types.Waiting {
		return report
	}

	switch p.HookEventName {
	case "PermissionRequest":
		report.ReasonKind = types.ReasonPermission
		report.Reason = hookpayload.ToolSummary(p.ToolName, p.ToolInput)
	case "PreToolUse":
		switch p.ToolName {
		case "AskUserQuestion":
			report.ReasonKind = types.ReasonQuestion
			report.Reason = hookpayload.Question(p.ToolInput)
		case "ExitPlanMode":
			report.ReasonKind = types.ReasonPlan
			report.Reason = hookpayload.PlanTitle(p.ToolInput)
		default:
			report.ReasonKind = types.ReasonPermission
			report.Reason = hookpayload.ToolSummary(p.ToolName, p.ToolInput)
		}
	case "Elicitation":
		report.ReasonKind = types.ReasonInput
		report.Reason = p.Message
	case "Notification":
		// Notifications repeat what a PermissionRequest or question already
		// said, in less detail, so they only fill in a missing reason.
		report.KeepReason = true
		switch p.NotificationType {
		case "permission_prompt":
			report.ReasonKind = types.ReasonPermission
			report.Reason = strings.TrimPrefix(p.Message, permissionNotice)
		case "agent_needs_input":
			report.ReasonKind = types.ReasonQuestion
			report.Reason = p.Message
		default:
			report.ReasonKind = types.ReasonInput
			report.Reason = p.Message
		}
	case "StopFailure":
		report.ReasonKind = types.ReasonError
		report.Reason = hookpayload.ErrorText(p.Error, p.ErrorMessage, p.Message)
	}
	return report
}

// Run records the hook whose configured state is next, enriched with the
// payload read from input. input may be nil when no payload is available.
func Run(next types.State, input io.Reader) error {
	var p payload
	if input != nil {
		// A payload that cannot be decoded still records the state.
		_ = json.NewDecoder(io.LimitReader(input, payloadLimit)).Decode(&p)
	}
	return state.Record(ReportFor(next, p))
}
