package launch

import (
	"fmt"
	"strings"

	"llm-session-manager/internal/agent"
	"llm-session-manager/internal/sessions"
	"llm-session-manager/internal/tmux"
)

// Launch creates or reuses a session for cwd and opens it in a popup.
func Launch(cwd, origin string) error {
	prefix := tmux.GetGlobalOption("@llm_session_prefix", "llm-")
	command := agent.Active()
	if command == "" {
		return agent.ErrNotConfigured()
	}
	width := tmux.GetGlobalOption("@llm_popup_width", "90%")
	height := tmux.GetGlobalOption("@llm_popup_height", "90%")

	currentSession, err := tmux.DisplayMessage("#S", "")
	if err != nil {
		// Not running inside tmux; continue with launch.
		currentSession = ""
	}
	if currentSession != "" && currentSession != "__unknown__" && strings.HasPrefix(currentSession, prefix) {
		// Already inside a managed session.
		_, _ = tmux.DisplayMessage("Popup window already open", "")
		return nil
	}

	sessionName := sessions.SessionNameForPath(cwd, prefix)

	created := false
	if !tmux.HasSession(sessionName) {
		if err := tmux.NewSession(sessionName, cwd, command); err != nil {
			return fmt.Errorf("failed to create session: %w", err)
		}
		created = true
	}

	// Promote the session: visible in the picker, protected from warm
	// eviction, window named correctly. An established session keeps the
	// identity of the agent it is already running; the globally active agent
	// only controls newly created sessions.
	window := sessionName + ":0"
	agentName := ""
	if created {
		agentName = agent.Name(command)
	} else if tmux.GetSessionOption(sessionName, "@llm_ever_attached") == "" {
		agentName = tmux.GetWindowOption(window, "@llm_warm_agent")
		if agentName == "" {
			startCommand, _ := tmux.DisplayMessage("#{pane_start_command}", window)
			agentName = agent.Name(startCommand)
		}
		if agentName == "" {
			agentName, _ = tmux.DisplayMessage("#{pane_current_command}", window)
		}
	}
	if agentName != "" {
		_ = tmux.SetWindowOption(window, "@llm_agent", agentName)
		_ = tmux.RenameWindow(window, agentName)
	}
	_ = tmux.SetWindowOption(sessionName+":0", "@llm_path", cwd)
	_ = tmux.SetSessionOption(sessionName, "@llm_ever_attached", "1")

	if err := tmux.SetSessionOption(sessionName, "@llm_path", cwd); err != nil {
		return err
	}

	// Prefer the session we're actually running in (already resolved above
	// via "#S") over the caller-supplied origin — it's derived the same way
	// tmux itself would resolve "current session" and can't be stale. Only
	// fall back to the passed-in value if that lookup failed. Guard against
	// a window ID (e.g. "@3") ever being stored here: EnsureOriginWindow
	// needs a session name, and a stray window ID is a symptom of a caller
	// bug rather than a usable origin.
	originSession := currentSession
	if originSession == "" || originSession == "__unknown__" {
		originSession = origin
	}
	if strings.HasPrefix(originSession, "@") {
		originSession = ""
	}
	if originSession != "" && originSession != sessionName {
		if err := tmux.SetSessionOption(sessionName, "@llm_origin", originSession); err != nil {
			return err
		}
	}

	return tmux.DisplayPopup(tmux.DisplayPopupOptions{
		Width:   width,
		Height:  height,
		Command: fmt.Sprintf("tmux attach-session -t %s", sessionName),
	})
}
