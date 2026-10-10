package add

import (
	"fmt"

	"llm-session-manager/internal/agent"
	"llm-session-manager/internal/sessions"
	"llm-session-manager/internal/state"
	"llm-session-manager/internal/tmux"
	"llm-session-manager/internal/warm"
)

// Add creates or reuses a managed tmux session for cwd and opens a new agent
// window in it. When showPopup is true, it then pops up attached to that
// session with the new window selected.
func Add(cwd, origin string, showPopup bool) (string, error) {
	prefix := tmux.GetGlobalOption("@llm_session_prefix", "llm-")
	command := agent.Active()
	if command == "" {
		return "", agent.ErrNotConfigured()
	}

	abs := sessions.CanonicalPath(cwd)
	sessionName := sessions.SessionNameForPath(abs, prefix)

	var windowID string
	agentName := agent.Name(command)
	if tmux.HasSession(sessionName) {
		// If the session was warm-only (created by `llmux warm`, never
		// attached), promote the existing warm window — the agent process
		// is already running. Otherwise, create a new window for the
		// additional agent.
		if tmux.GetSessionOption(sessionName, "@llm_ever_attached") == "" {
			claimed, ok, err := warm.Claim(sessionName, agentName)
			if err != nil {
				return "", err
			}
			if ok {
				windowID, agentName = claimed.WindowID, claimed.Agent
			}
		} else {
			result, err := tmux.Run([]string{"new-window", "-dP", "-t", sessionName + ":", "-c", abs, "-F", "#{window_id}", agent.LaunchCommand(command)})
			if err != nil {
				return "", fmt.Errorf("failed to create window: %w", err)
			}
			windowID = result
		}
	}
	if windowID == "" {
		result, err := tmux.Run([]string{"new-session", "-dP", "-s", sessionName, "-c", abs, "-F", "#{window_id}", agent.LaunchCommand(command)})
		if err != nil {
			return "", fmt.Errorf("failed to create session: %w", err)
		}
		windowID = result
	}

	originSession := origin
	if originSession == "" || !tmux.HasSession(originSession) {
		originSession = tmux.GetParentSession()
	}

	commands := [][]string{tmux.SetSessionOptionArgs(sessionName, "@llm_path", abs)}
	if originSession != "" {
		commands = append(commands, tmux.SetSessionOptionArgs(sessionName, "@llm_origin", originSession))
	}
	commands = append(commands,
		tmux.SetWindowOptionArgs(windowID, "@llm_agent", agentName),
		tmux.SetWindowOptionArgs(windowID, "@llm_path", abs),
		[]string{"rename-window", "-t", windowID, agentName},
		tmux.SetSessionOptionArgs(sessionName, "@llm_ever_attached", "1"),
	)
	if _, err := tmux.Run(tmux.Batch(commands...)); err != nil {
		return "", err
	}
	if agent.StartsSilent(agentName) {
		_ = state.SeedIdle(windowID)
	}

	if !showPopup {
		return windowID, nil
	}

	if originSession != "" && originSession != sessionName {
		_ = tmux.EnsureOriginWindow(originSession, abs, "")
	}

	width := tmux.GetGlobalOption("@llm_popup_width", "90%")
	height := tmux.GetGlobalOption("@llm_popup_height", "90%")
	parentClient := tmux.GetGlobalOption("@llm_parent", "")
	attachCmd := tmux.AttachCommand(sessionName, false) + " \\; select-window -t " + tmux.ShellQuote(windowID)
	err := tmux.DisplayPopup(tmux.DisplayPopupOptions{
		Width:   width,
		Height:  height,
		Command: attachCmd,
		Client:  parentClient,
	})
	return windowID, err
}
