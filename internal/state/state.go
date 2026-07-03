package state

import (
	"fmt"
	"os"
	"strings"
	"time"

	"llm-session-manager/internal/tmux"
	"llm-session-manager/internal/types"
)

// SetState updates the current tmux session's state and timestamp.
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

	if err := tmux.SetSessionOption(session, "@llm_state", string(state)); err != nil {
		return err
	}
	return tmux.SetSessionOption(session, "@llm_state_at", fmt.Sprintf("%d", time.Now().Unix()))
}
