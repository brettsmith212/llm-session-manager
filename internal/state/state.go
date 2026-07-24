package state

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"llm-session-manager/internal/sessions"
	"llm-session-manager/internal/tmux"
	"llm-session-manager/internal/types"
)

const waitingNotificationDuration = 5 * time.Second

var sendWaitingNotification = notifyWaitingClients

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

	previousState := tmux.GetWindowOption(windowID, "@llm_state")
	if err := tmux.SetWindowOption(windowID, "@llm_state", string(state)); err != nil {
		return err
	}
	if err := tmux.SetWindowOption(windowID, "@llm_state_at", fmt.Sprintf("%d", time.Now().Unix())); err != nil {
		return err
	}

	// Keep status-bar options synchronized with every lifecycle transition.
	// Recounting all managed windows also self-heals if an earlier hook was
	// interrupted after updating its window but before publishing the count.
	allSessions := sessions.GetAllSessions(prefix)
	statusErr := sessions.PublishWaitingStatus(allSessions)
	if previousState != string(types.Waiting) && state == types.Waiting {
		for _, session := range allSessions {
			if session.WindowID == windowID {
				sendWaitingNotification(prefix, session)
				break
			}
		}
	}
	return statusErr
}

func notifyWaitingClients(prefix string, session types.Session) {
	message := waitingNotificationMessage(session)
	for _, client := range notificationClients(prefix, tmux.ListClients()) {
		_ = tmux.DisplayClientMessage(client, message, waitingNotificationDuration)
	}
}

func notificationClients(prefix string, clients []tmux.ClientInfo) []string {
	targets := make([]string, 0, len(clients))
	for _, client := range clients {
		if client.Client == "" || strings.HasPrefix(client.Session, prefix) {
			continue
		}
		targets = append(targets, client.Client)
	}
	return targets
}

func waitingNotificationMessage(session types.Session) string {
	name := strings.TrimSpace(session.Label)
	if name == "" && session.Path != "" {
		name = filepath.Base(filepath.Clean(session.Path))
	}
	if name == "" || name == "." {
		name = session.Name
	}
	if name == "" {
		name = "LLM session"
	}
	name = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, name)
	// Escape user-controlled tmux format markers while retaining our color.
	name = strings.ReplaceAll(name, "#", "##")
	return "#[fg=yellow]" + name + ": Needs Review#[default]"
}
