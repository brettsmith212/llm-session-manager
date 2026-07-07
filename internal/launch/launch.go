package launch

import (
	"fmt"
	"strings"

	"llm-session-manager/internal/sessions"
	"llm-session-manager/internal/tmux"
)

// Launch creates or reuses a session for cwd and opens it in a popup.
func Launch(cwd, origin string) error {
	prefix := tmux.GetGlobalOption("@llm_session_prefix", "llm-")
	command := tmux.GetGlobalOption("@llm_command", "opencode")
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

	if !tmux.HasSession(sessionName) {
		if err := tmux.NewSession(sessionName, cwd, command); err != nil {
			return fmt.Errorf("failed to create session: %w", err)
		}
	}

	// Promote the session: visible in the picker, protected from warm
	// eviction, window named correctly. Runs on both the create path and
	// the warm fast path (when a background warm session already exists).
	_ = tmux.SetWindowOption(sessionName+":0", "@llm_opencode", "1")
	_ = tmux.SetWindowOption(sessionName+":0", "@llm_path", cwd)
	_ = tmux.SetSessionOption(sessionName, "@llm_ever_attached", "1")
	_ = tmux.RenameWindow(sessionName+":0", "opencode")

	if err := tmux.SetSessionOption(sessionName, "@llm_path", cwd); err != nil {
		return err
	}
	if origin != "" {
		if err := tmux.SetSessionOption(sessionName, "@llm_origin", origin); err != nil {
			return err
		}
	}

	return tmux.DisplayPopup(tmux.DisplayPopupOptions{
		Width:   width,
		Height:  height,
		Command: fmt.Sprintf("tmux attach-session -t %s", sessionName),
	})
}
