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

// ListCommand builds or focuses the tmux control-room window. clientName,
// when non-empty, is the tmux client that invoked the command (typically
// "#{client_name}" expanded by the calling key binding) and is used as the
// host client instead of guessing from the server-wide client list — guessing
// breaks down as soon as more than one non-managed client is attached anywhere
// on the tmux server.
func ListCommand(clientName string) error {
	prefix := tmux.GetGlobalOption("@llm_session_prefix", "llm-")
	host := resolveHost(prefix, clientName)

	// A live pane is a nested tmux client whose TTY is the pane TTY of an
	// existing control room. Return through that outer client rather than
	// treating the managed session as the place where a new room belongs.
	if roomHost := containingControlRoom(host); roomHost != nil {
		// Selecting the outer list reroutes physical input without detaching the
		// nested client, so the agent remains live and visible in the right pane.
		if focusExistingControlRoom(roomHost) {
			_ = tmux.SetGlobalOption("@llm_parent", roomHost.Client)
			return nil
		}
		host = roomHost
	}

	// Reuse a healthy room in the invoking client's session whether it is
	// currently visible or in the background. This makes Ctrl+a u both the
	// entry point from a project window and the escape hatch from the live pane.
	if host != nil {
		_ = tmux.SetGlobalOption("@llm_parent", host.Client)
		if focusExistingControlRoom(host) {
			return nil
		}
	}

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

	// clientName may have named the nested client that was just detached. Resolve
	// again so the host is the surviving outer project/control-room client.
	host = resolveHost(prefix, clientName)
	if host != nil {
		_ = tmux.SetGlobalOption("@llm_parent", host.Client)
	} else {
		_ = tmux.SetGlobalOption("@llm_parent", "")
	}

	allSessions := sessions.GetAllSessions(prefix)
	_ = sessions.PublishWaitingStatus(allSessions)

	bin := binaryPath()
	ts := ""
	if host != nil {
		ts = host.Session
	}
	pickerTarget := windowName
	if ts != "" {
		pickerTarget = ts + ":" + windowName
	}
	if focusExistingControlRoom(host) {
		return nil
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

	styleControlRoom(pickerTarget)
	_ = tmux.RunRaw([]string{"select-pane", "-t", pickerTarget + ".1", "-T", "LIVE AGENT · prefix u returns"})

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

func focusExistingControlRoom(host *tmux.ClientInfo) bool {
	if host == nil || host.Session == "" {
		return false
	}
	target := host.Session + ":" + windowName
	result := tmux.RunRaw([]string{"list-panes", "-t", target, "-F", "#{pane_index}\t#{pane_dead}"})
	if result.ExitCode != 0 {
		return false
	}
	alive := make(map[string]bool, 2)
	for _, line := range strings.Split(result.Stdout, "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) == 2 && parts[1] == "0" {
			alive[parts[0]] = true
		}
	}
	if !alive["0"] || !alive["1"] {
		return false
	}
	// Refresh the styling as well as the focus so persistent rooms immediately
	// pick up interface improvements without being torn down and rebuilt.
	styleControlRoom(target)
	if _, err := tmux.Run([]string{"select-pane", "-t", target + ".0"}); err != nil {
		return false
	}
	_ = tmux.RunRaw([]string{"switch-client", "-c", host.Client, "-t", target})
	return true
}

func styleControlRoom(target string) {
	borderOpts := [][2]string{
		{"pane-border-status", "top"},
		{"pane-border-format", "#{?pane_active,#[fg=#89b4fa bold] ● FOCUSED · #{pane_title} #[default],#[fg=#6c7086]   #{pane_title} #[default]}"},
		{"pane-border-style", "fg=#6c7086"},             // Catppuccin Overlay0
		{"pane-active-border-style", "fg=#89b4fa,bold"}, // Catppuccin Blue
		{"pane-border-lines", "heavy"},
	}
	for _, option := range borderOpts {
		_ = tmux.SetWindowOption(target, option[0], option[1])
	}
	_ = tmux.RunRaw([]string{"select-pane", "-t", target + ".0", "-T", "CONTROL ROOM"})
}

// containingControlRoom returns the outer client displaying the control room
// whose live pane owns the invoking nested client's TTY.
func containingControlRoom(nested *tmux.ClientInfo) *tmux.ClientInfo {
	if nested == nil || nested.Client == "" {
		return nil
	}
	result := tmux.RunRaw([]string{"list-panes", "-a", "-F",
		"#{session_name}\t#{window_name}\t#{pane_index}\t#{pane_tty}"})
	if result.ExitCode != 0 {
		return nil
	}
	roomSession := ""
	for _, line := range strings.Split(result.Stdout, "\n") {
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) == 4 && parts[1] == windowName && parts[2] == "1" && parts[3] == nested.Client {
			roomSession = parts[0]
			break
		}
	}
	if roomSession == "" {
		return nil
	}
	preferred := tmux.GetGlobalOption("@llm_parent", "")
	var fallback *tmux.ClientInfo
	for _, client := range tmux.ListClients() {
		if client.Session == roomSession && client.Window == windowName {
			client := client
			if client.Client == preferred {
				return &client
			}
			if fallback == nil {
				fallback = &client
			}
		}
	}
	return fallback
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
