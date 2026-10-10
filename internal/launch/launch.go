package launch

import (
	"fmt"
	"strings"

	"llm-session-manager/internal/agent"
	"llm-session-manager/internal/sessions"
	"llm-session-manager/internal/state"
	"llm-session-manager/internal/tmux"
	"llm-session-manager/internal/warm"
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

	cwd = sessions.CanonicalPath(cwd)
	sessionName := sessions.SessionNameForPath(cwd, prefix)

	// Promote a warm-only session: visible in the picker, protected from warm
	// eviction, window named correctly. An established session keeps the
	// identity of the agent it is already running; the globally active agent
	// only controls newly created sessions.
	windowID := ""
	agentName := ""
	if tmux.HasSession(sessionName) && tmux.GetSessionOption(sessionName, "@llm_ever_attached") == "" {
		claimed, ok, err := warm.Claim(sessionName, agent.Name(command))
		if err != nil {
			return err
		}
		if ok {
			windowID, agentName = claimed.WindowID, claimed.Agent
		}
	}
	if !tmux.HasSession(sessionName) {
		created, err := tmux.Run([]string{"new-session", "-dP", "-F", "#{window_id}",
			"-s", sessionName, "-c", cwd, agent.LaunchCommand(command)})
		if err != nil {
			return fmt.Errorf("failed to create session: %w", err)
		}
		windowID, agentName = created, agent.Name(command)
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

	commands := [][]string{
		tmux.SetSessionOptionArgs(sessionName, "@llm_path", cwd),
		tmux.SetSessionOptionArgs(sessionName, "@llm_ever_attached", "1"),
	}
	if originSession != "" && originSession != sessionName {
		commands = append(commands, tmux.SetSessionOptionArgs(sessionName, "@llm_origin", originSession))
	}
	if windowID != "" && agentName != "" {
		commands = append(commands,
			tmux.SetWindowOptionArgs(windowID, "@llm_agent", agentName),
			tmux.SetWindowOptionArgs(windowID, "@llm_path", cwd),
			[]string{"rename-window", "-t", windowID, agentName})
	}
	if _, err := tmux.Run(tmux.Batch(commands...)); err != nil {
		return err
	}
	if windowID != "" && agent.StartsSilent(agentName) {
		_ = state.SeedIdle(windowID)
	}

	return tmux.DisplayPopup(tmux.DisplayPopupOptions{
		Width:   width,
		Height:  height,
		Command: tmux.AttachCommand(sessionName, false),
	})
}
