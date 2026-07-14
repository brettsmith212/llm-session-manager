package state

import (
	"fmt"
	"os"
	"strings"
	"time"

	"llm-session-manager/internal/sessions"
	"llm-session-manager/internal/tmux"
	"llm-session-manager/internal/types"
)

// SetState updates the current tmux window's state and timestamp.
func SetState(state types.State) error {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		// Not running inside tmux; state updates are best-effort.
		return nil
	}

	prefix := tmux.GetGlobalOption("@llm_session_prefix", "llm-")
	result := tmux.RunRaw([]string{"display-message", "-p", "-t", pane, "#{session_name}"})
	if result.ExitCode != 0 {
		return fmt.Errorf("failed to resolve session from pane")
	}

	session := strings.TrimSpace(result.Stdout)
	if session == "" || !strings.HasPrefix(session, prefix) {
		return nil
	}

	result = tmux.RunRaw([]string{"display-message", "-p", "-t", pane, "#{window_id}"})
	if result.ExitCode != 0 {
		return fmt.Errorf("failed to resolve window from pane")
	}
	windowID := strings.TrimSpace(result.Stdout)
	if windowID == "" {
		return fmt.Errorf("empty window id from pane")
	}

	// Mark this window as hosting a managed agent if it isn't already.
	// Skip for warm-only sessions (never attached) — they stay hidden
	// from the picker until launch/add promotes them.
	if tmux.GetWindowOption(windowID, "@llm_agent") == "" {
		if tmux.GetSessionOption(session, "@llm_ever_attached") != "" {
			if err := tmux.SetWindowOption(windowID, "@llm_agent", "1"); err != nil {
				return err
			}
		}
	}

	if err := tmux.SetWindowOption(windowID, "@llm_state", string(state)); err != nil {
		return err
	}
	if err := tmux.SetWindowOption(windowID, "@llm_state_at", fmt.Sprintf("%d", time.Now().Unix())); err != nil {
		return err
	}

	// Keep status-bar options synchronized with every lifecycle transition.
	// Recounting all managed windows also self-heals if an earlier hook was
	// interrupted after updating its window but before publishing the count.
	return sessions.PublishWaitingStatus(sessions.GetAllSessions(prefix))
}
