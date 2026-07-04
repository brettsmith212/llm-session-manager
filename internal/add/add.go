package add

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"llm-session-manager/internal/sessions"
	"llm-session-manager/internal/tmux"
)

// Add creates or reuses a managed tmux session for cwd and opens a new
// opencode window in it, then pops up attached to that session with the new
// window selected.
func Add(cwd, origin string) error {
	prefix := tmux.GetGlobalOption("@llm_session_prefix", "llm-")
	command := tmux.GetGlobalOption("@llm_command", "opencode")
	width := tmux.GetGlobalOption("@llm_popup_width", "90%")
	height := tmux.GetGlobalOption("@llm_popup_height", "90%")

	cwd = expandPath(cwd)
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}

	sessionName := sessions.SessionNameForPath(abs, prefix)

	var windowID string
	if !tmux.HasSession(sessionName) {
		result, err := tmux.Run([]string{"new-session", "-d", "-s", sessionName, "-c", abs, "-F", "#{window_id}", command})
		if err != nil {
			return fmt.Errorf("failed to create session: %w", err)
		}
		windowID = result
	} else {
		result, err := tmux.Run([]string{"new-window", "-d", "-t", sessionName + ":", "-c", abs, "-F", "#{window_id}", command})
		if err != nil {
			return fmt.Errorf("failed to create window: %w", err)
		}
		windowID = result
	}

	if err := tmux.SetSessionOption(sessionName, "@llm_path", abs); err != nil {
		return err
	}
	if origin != "" {
		if err := tmux.SetSessionOption(sessionName, "@llm_origin", origin); err != nil {
			return err
		}
	}

	_ = tmux.SetWindowOption(windowID, "@llm_opencode", "1")
	_ = tmux.SetWindowOption(windowID, "@llm_path", abs)

	attachCmd := tmux.AttachCommand(sessionName, false) + " \\; select-window -t " + tmux.ShellQuote(windowID)
	return tmux.DisplayPopup(tmux.DisplayPopupOptions{
		Width:   width,
		Height:  height,
		Command: attachCmd,
	})
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
