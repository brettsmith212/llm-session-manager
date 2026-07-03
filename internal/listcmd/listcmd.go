package listcmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"llm-session-manager/internal/sessions"
	"llm-session-manager/internal/tmux"
)

const windowName = "llm-picker"

// ListCommand builds the tmux picker window.
func ListCommand() error {
	prefix := tmux.GetGlobalOption("@llm_session_prefix", "llm-")

	// Detach any nested managed session so the picker isn't nested inside one.
	nested := getNestedSession(prefix)
	if nested != "" {
		_ = tmux.DetachClient(nested)
		for i := 0; i < 100; i++ {
			if getNestedSession(prefix) == "" {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	host := getHost(prefix)
	if host != nil {
		_ = tmux.SetGlobalOption("@llm_parent", host.Client)
	} else {
		_ = tmux.SetGlobalOption("@llm_parent", "")
	}

	allSessions := sessions.GetAllSessions(prefix)
	if len(allSessions) == 0 {
		_, _ = tmux.Run([]string{"display-message", "No opencode sessions"})
		return nil
	}

	bin := binaryPath()
	ts := ""
	if host != nil {
		ts = host.Session
	}
	pickerTarget := windowName
	if ts != "" {
		pickerTarget = ts + ":" + windowName
	}

	_ = tmux.RunRaw([]string{"kill-window", "-t", pickerTarget})
	newWindowArgs := []string{"new-window"}
	if ts != "" {
		newWindowArgs = append(newWindowArgs, "-t", ts+":")
	}
	newWindowArgs = append(newWindowArgs,
		"-n", windowName,
		"-c", currentDir(),
		"sleep 1000",
	)
	if _, err := tmux.Run(newWindowArgs); err != nil {
		return fmt.Errorf("new-window failed: %w", err)
	}

	if _, err := tmux.Run([]string{
		"split-window", "-h", "-l", "67%",
		"-t", pickerTarget + ".0",
		tmux.AttachCommand(allSessions[0].Name, true),
	}); err != nil {
		return fmt.Errorf("split-window failed: %w", err)
	}

	if _, err := tmux.Run([]string{
		"respawn-pane", "-k",
		"-t", pickerTarget + ".0",
		"-c", currentDir(),
		bin + " picker",
	}); err != nil {
		return fmt.Errorf("respawn-pane failed: %w", err)
	}

	if _, err := tmux.Run([]string{"select-pane", "-t", pickerTarget + ".0"}); err != nil {
		return err
	}

	if host != nil {
		_ = tmux.RunRaw([]string{"switch-client", "-c", host.Client, "-t", pickerTarget})
	}

	return nil
}

func getNestedSession(prefix string) string {
	for _, c := range tmux.ListClients() {
		if strings.HasPrefix(c.Session, prefix) {
			return c.Session
		}
	}
	return ""
}

func getHost(prefix string) *tmux.ClientInfo {
	for _, c := range tmux.ListClients() {
		if !strings.HasPrefix(c.Session, prefix) {
			return &c
		}
	}
	return nil
}

func binaryPath() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	if path, err := exec.LookPath("llmux"); err == nil {
		return path
	}
	return os.Args[0]
}

func currentDir() string {
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return "."
}

// TargetBase returns the base window target without a session prefix.
func TargetBase() string { return windowName }

// BinaryName returns the expected binary name for the picker respawn command.
func BinaryName() string { return filepath.Base(binaryPath()) }
