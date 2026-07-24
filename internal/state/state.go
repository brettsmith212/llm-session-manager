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
	if !tmux.SupportsFloatingPanes() {
		return
	}
	message := waitingNotificationMessage(session)
	for _, window := range notificationWindows(prefix, tmux.ListClients()) {
		_, _ = tmux.DisplayFloatingNotification(window.id, window.width, message, waitingNotificationDuration)
	}
}

type notificationWindow struct {
	id    string
	width int
}

func notificationWindows(prefix string, clients []tmux.ClientInfo) []notificationWindow {
	targets := make([]notificationWindow, 0, len(clients))
	seen := make(map[string]bool)
	for _, client := range clients {
		if client.Client == "" || client.WindowID == "" || strings.HasPrefix(client.Session, prefix) || seen[client.WindowID] {
			continue
		}
		seen[client.WindowID] = true
		targets = append(targets, notificationWindow{id: client.WindowID, width: client.WindowWidth})
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
	return name + ": Needs Review"
}
