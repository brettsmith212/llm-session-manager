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

// ListCommand builds the tmux picker window. clientName, when non-empty, is
// the tmux client that invoked the command (typically "#{client_name}"
// expanded by the calling key binding) and is used as the host client
// instead of guessing from the server-wide client list — guessing breaks
// down as soon as more than one non-managed client is attached anywhere on
// the tmux server.
func ListCommand(clientName string) error {
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

	host := resolveHost(prefix, clientName)
	if host != nil {
		_ = tmux.SetGlobalOption("@llm_parent", host.Client)
	} else {
		_ = tmux.SetGlobalOption("@llm_parent", "")
	}

	allSessions := sessions.GetAllSessions(prefix)

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

	previewCmd := "sleep 1000"
	if len(allSessions) > 0 {
		previewCmd = tmux.AttachCommand(allSessions[0].Name, true)
	}
	if _, err := tmux.Run([]string{
		"split-window", "-h", "-l", "67%",
		"-t", pickerTarget + ".0",
		previewCmd,
	}); err != nil {
		return fmt.Errorf("split-window failed: %w", err)
	}

	// ── A: pane borders + titles so picker (left) and preview (right) are
	// visually distinct. Only the active pane gets a bright blue border; the
	// inactive one stays muted. Each pane has its own title shown on top.
	borderOpts := [][2]string{
		{"pane-border-status", "top"},
		{"pane-border-format", " #{pane_title} "},
		{"pane-border-style", "fg=#6c7086"},            // Catppuccin Overlay0
		{"pane-active-border-style", "fg=#89b4fa,bold"}, // Catppuccin Blue
		{"pane-border-lines", "heavy"},
	}
	for _, o := range borderOpts {
		_ = tmux.SetWindowOption(pickerTarget, o[0], o[1])
	}
	_ = tmux.RunRaw([]string{"select-pane", "-t", pickerTarget + ".0", "-T", "◆ Sessions"})
	_ = tmux.RunRaw([]string{"select-pane", "-t", pickerTarget + ".1", "-T", "▶ Preview"})

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

// resolveHost finds the ClientInfo for clientName among attached clients. If
// clientName is empty or no longer attached, it falls back to getHost's
// best-effort scan (e.g. when ListCommand is invoked manually with no
// binding-supplied client name).
func resolveHost(prefix, clientName string) *tmux.ClientInfo {
	if clientName != "" {
		for _, c := range tmux.ListClients() {
			if c.Client == clientName {
				return &c
			}
		}
	}
	return getHost(prefix)
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
