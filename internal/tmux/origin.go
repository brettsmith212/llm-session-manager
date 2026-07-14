package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// OriginWindow identifies the project window selected for an agent. Created is
// true only when llmux had to create the window during this call.
type OriginWindow struct {
	ID      string
	Created bool
}

// EnsureOriginWindow switches the parent tmux client to a window in
// originSession whose working directory matches cwd. If no such window exists,
// one is created. This ensures that when a popup closes the user lands in a
// persistent host window at the project cwd.
func EnsureOriginWindow(originSession, cwd, parentClient string) error {
	_, err := EnsureOriginWindowTarget(originSession, cwd, parentClient)
	return err
}

// EnsureOriginWindowTarget performs EnsureOriginWindow and returns the exact
// host window so callers can mark newly-created task-worktree hosts for later
// cleanup without taking ownership of pre-existing user windows.
func EnsureOriginWindowTarget(originSession, cwd, parentClient string) (OriginWindow, error) {
	if originSession == "" || cwd == "" {
		return OriginWindow{}, nil
	}
	if strings.HasPrefix(originSession, "@") {
		// Sessions created before the launch.go fix may have a window ID
		// (e.g. "@0") stored as their origin instead of a session name —
		// has-session accepts window-ID-shaped targets (resolving them to
		// their enclosing session), which let this slip past validation at
		// creation time, but switch-client's "session:window" target
		// requires an actual session name and fails. Resolve the window ID
		// back to its real session so already-created sessions heal
		// themselves instead of erroring forever.
		resolved, err := DisplayMessage("#{session_name}", originSession)
		if err != nil || resolved == "" {
			return OriginWindow{}, nil
		}
		originSession = resolved
	}
	if !HasSession(originSession) {
		return OriginWindow{}, nil
	}
	if parentClient == "" {
		parentClient = GetGlobalOption("@llm_parent", "")
	}
	if parentClient == "" {
		return OriginWindow{}, nil
	}

	target, created, err := findOrCreateWindowAtCwd(originSession, cwd)
	if err != nil {
		return OriginWindow{}, err
	}
	if target == "" {
		return OriginWindow{}, nil
	}

	res := RunRaw([]string{"switch-client", "-c", parentClient, "-t", originSession + ":" + target})
	if res.ExitCode != 0 {
		msg := res.Stderr
		if msg == "" {
			msg = res.Stdout
		}
		return OriginWindow{}, fmt.Errorf("switch-client failed: %s", msg)
	}
	return OriginWindow{ID: target, Created: created}, nil
}

// GetParentSession returns the tmux session name of the client stored in the
// global @llm_parent option. It returns "" if the option is unset or the
// client is no longer attached.
func GetParentSession() string {
	parentClient := GetGlobalOption("@llm_parent", "")
	if parentClient == "" {
		return ""
	}
	for _, c := range ListClients() {
		if c.Client == parentClient {
			return c.Session
		}
	}
	return ""
}

func findOrCreateWindowAtCwd(session, cwd string) (string, bool, error) {
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return "", false, err
	}
	absCwd = filepath.Clean(absCwd)

	result := RunRaw([]string{"list-windows", "-t", session, "-F", "#{window_id}\t#{@llm_control_room}\t#{pane_current_path}"})
	if result.ExitCode != 0 {
		return "", false, fmt.Errorf("list-windows failed: %s", result.Stdout)
	}

	for _, line := range strings.Split(result.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 || parts[1] == "1" {
			continue
		}
		windowID := strings.TrimSpace(parts[0])
		windowPath := strings.TrimSpace(parts[2])
		if windowPath == "" {
			continue
		}
		if samePath(absCwd, windowPath) {
			return windowID, false, nil
		}
	}

	out, err := Run([]string{"new-window", "-dP", "-t", session + ":", "-c", absCwd, "-F", "#{window_id}"})
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(out), true, nil
}

func samePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if a == b {
		return true
	}
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	if errA != nil || errB != nil {
		return false
	}
	return os.SameFile(infoA, infoB)
}
