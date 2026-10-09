// Package codexhook translates Codex lifecycle hook payloads into llmux
// session states.
//
// Codex runs every handler that matches an event concurrently, so one
// handler per event decides the state from the payload rather than separate
// per-tool entries racing to write it.
package codexhook

import (
	"encoding/json"
	"fmt"
	"io"

	"llm-session-manager/internal/state"
	"llm-session-manager/internal/types"
)

// requestUserInputTool is the tool Codex uses to ask the user a question.
const requestUserInputTool = "request_user_input"

type payload struct {
	HookEventName string `json:"hook_event_name"`
	ToolName      string `json:"tool_name"`
}

// StateFor maps a Codex hook event to the state it implies. ok is false for
// events that do not reveal whether Codex is working or needs the user.
func StateFor(event, toolName string) (next types.State, ok bool) {
	switch event {
	case "UserPromptSubmit", "PostToolUse":
		return types.Working, true
	case "PreToolUse":
		if toolName == requestUserInputTool {
			return types.Waiting, true
		}
		// Codex skips PostToolUse when a tool fails, so the next tool call is
		// what moves an approved-but-failed command out of waiting.
		return types.Working, true
	case "PermissionRequest":
		return types.Waiting, true
	case "Stop", "Interrupt", "SessionEnd":
		return types.Idle, true
	}
	// SessionStart runs at the start of the first turn and again after
	// compaction, so it never means the session is idle.
	return "", false
}

// Run reads one hook payload and records the state it implies for the
// current tmux pane.
func Run(input io.Reader) error {
	var p payload
	if err := json.NewDecoder(input).Decode(&p); err != nil {
		return fmt.Errorf("decode codex hook payload: %w", err)
	}
	next, ok := StateFor(p.HookEventName, p.ToolName)
	if !ok {
		return nil
	}
	return state.SetState(next)
}
