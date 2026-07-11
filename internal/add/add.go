package add

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"llm-session-manager/internal/agent"
	"llm-session-manager/internal/sessions"
	"llm-session-manager/internal/tmux"
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

	cwd = expandPath(cwd)
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}

	sessionName := sessions.SessionNameForPath(abs, prefix)

	var windowID string
	agentName := agent.Name(command)
	if !tmux.HasSession(sessionName) {
		result, err := tmux.Run([]string{"new-session", "-dP", "-s", sessionName, "-c", abs, "-F", "#{window_id}", command})
		if err != nil {
			return "", fmt.Errorf("failed to create session: %w", err)
		}
		windowID = result
	} else {
		// If the session was warm-only (created by `llmux warm`, never
		// attached), promote the existing warm window — the agent process
		// is already running. Otherwise, create a new window for the
		// additional agent.
		if tmux.GetSessionOption(sessionName, "@llm_ever_attached") == "" {
			wid, err := tmux.DisplayMessage("#{window_id}", sessionName+":0")
			if err != nil {
				return "", fmt.Errorf("failed to resolve warm window: %w", err)
			}
			windowID = wid
			if warmAgent := tmux.GetWindowOption(windowID, "@llm_warm_agent"); warmAgent != "" {
				agentName = warmAgent
			} else if startCommand, err := tmux.DisplayMessage("#{pane_start_command}", windowID); err == nil && agent.Name(startCommand) != "" {
				agentName = agent.Name(startCommand)
			} else if currentCommand, err := tmux.DisplayMessage("#{pane_current_command}", windowID); err == nil && currentCommand != "" {
				agentName = currentCommand
			}
		} else {
			result, err := tmux.Run([]string{"new-window", "-dP", "-t", sessionName + ":", "-c", abs, "-F", "#{window_id}", command})
			if err != nil {
				return "", fmt.Errorf("failed to create window: %w", err)
			}
			windowID = result
		}
	}

	if err := tmux.SetSessionOption(sessionName, "@llm_path", abs); err != nil {
		return "", err
	}

	originSession := origin
	if originSession == "" || !tmux.HasSession(originSession) {
		originSession = tmux.GetParentSession()
	}
	if originSession != "" {
		if err := tmux.SetSessionOption(sessionName, "@llm_origin", originSession); err != nil {
			return "", err
		}
	}

	_ = tmux.SetWindowOption(windowID, "@llm_agent", agentName)
	_ = tmux.SetWindowOption(windowID, "@llm_path", abs)
	_ = tmux.RenameWindow(windowID, agentName)
	_ = tmux.SetSessionOption(sessionName, "@llm_ever_attached", "1")

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
	err = tmux.DisplayPopup(tmux.DisplayPopupOptions{
		Width:   width,
		Height:  height,
		Command: attachCmd,
		Client:  parentClient,
	})
	return windowID, err
}

func expandPath(path string) string {
	if strings.HasPrefix(path, "~") {
		home := os.Getenv("HOME")
		if home != "" {
			return home + strings.TrimPrefix(path, "~")
		}
	}
	return path
}
