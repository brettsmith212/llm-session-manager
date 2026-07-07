package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnsureOriginWindow switches the parent tmux client to a window in
// originSession whose working directory matches cwd. If no such window exists,
// one is created. This ensures that when a popup closes the user lands in a
// persistent host window at the project cwd.
func EnsureOriginWindow(originSession, cwd, parentClient string) error {
	if originSession == "" || cwd == "" {
		return nil
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
			return nil
		}
		originSession = resolved
	}
	if !HasSession(originSession) {
		return nil
	}
	if parentClient == "" {
		parentClient = GetGlobalOption("@llm_parent", "")
	}
	if parentClient == "" {
		return nil
	}

	target, err := findOrCreateWindowAtCwd(originSession, cwd)
	if err != nil {
		return err
	}
	if target == "" {
		return nil
	}

	res := RunRaw([]string{"switch-client", "-c", parentClient, "-t", originSession + ":" + target})
	if res.ExitCode != 0 {
		msg := res.Stderr
		if msg == "" {
			msg = res.Stdout
		}
		return fmt.Errorf("switch-client failed: %s", msg)
	}
	return nil
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

func findOrCreateWindowAtCwd(session, cwd string) (string, error) {
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	absCwd = filepath.Clean(absCwd)

	result := RunRaw([]string{"list-windows", "-t", session, "-F", "#{window_id}\t#{pane_current_path}"})
	if result.ExitCode != 0 {
		return "", fmt.Errorf("list-windows failed: %s", result.Stdout)
	}

	for _, line := range strings.Split(result.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		windowID := strings.TrimSpace(parts[0])
		windowPath := strings.TrimSpace(parts[1])
		if windowPath == "" {
			continue
		}
		if samePath(absCwd, windowPath) {
			return windowID, nil
		}
	}

	out, err := Run([]string{"new-window", "-dP", "-t", session + ":", "-c", absCwd, "-F", "#{window_id}"})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
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
