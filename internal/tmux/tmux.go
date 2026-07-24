package tmux

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Result captures the output and exit code of a tmux invocation.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// RunRaw runs a tmux command and returns raw output without checking the exit code.
func RunRaw(args []string) Result {
	cmd := exec.Command("tmux", args...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}
	return Result{
		Stdout:   strings.TrimRight(out.String(), "\n"),
		Stderr:   strings.TrimRight(errOut.String(), "\n"),
		ExitCode: exitCode,
	}
}

// Run runs a tmux command and returns its stdout, or an error if it fails.
func Run(args []string) (string, error) {
	result := RunRaw(args)
	if result.ExitCode != 0 {
		msg := result.Stderr
		if msg == "" {
			msg = result.Stdout
		}
		return "", fmt.Errorf("tmux %s failed with code %d: %s", strings.Join(args, " "), result.ExitCode, msg)
	}
	return result.Stdout, nil
}

// GetGlobalOption reads a global tmux option, returning defaultValue if unset.
func GetGlobalOption(name, defaultValue string) string {
	result := RunRaw([]string{"show-option", "-gqv", name})
	if result.Stdout == "" {
		return defaultValue
	}
	return result.Stdout
}

// GetSessionOption reads a session option, returning "" if unset.
func GetSessionOption(session, name string) string {
	result := RunRaw([]string{"show-options", "-qv", "-t", session, name})
	return result.Stdout
}

// SetSessionOption sets a session option.
func SetSessionOption(session, name, value string) error {
	_, err := Run([]string{"set-option", "-t", session, name, value})
	return err
}

// GetWindowOption reads a window option, returning "" if unset.
func GetWindowOption(window, name string) string {
	result := RunRaw([]string{"show-options", "-wqv", "-t", window, name})
	return result.Stdout
}

// SetWindowOption sets a window option.
func SetWindowOption(window, name, value string) error {
	_, err := Run([]string{"set-window-option", "-t", window, name, value})
	return err
}

// UnsetWindowOption clears a window option, returning an error if tmux rejects
// the command. Unsetting an already-unset option is not an error in tmux.
func UnsetWindowOption(window, name string) error {
	_, err := Run([]string{"set-window-option", "-t", window, "-u", name})
	return err
}

// SetGlobalOption sets a global option.
func SetGlobalOption(name, value string) error {
	_, err := Run([]string{"set-option", "-g", name, value})
	return err
}

// ShellQuote single-quotes a string for safe interpolation into a shell command.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// AttachCommand builds a tmux attach-session command string.
func AttachCommand(name string, clearTmuxEnv bool) string {
	env := ""
	if clearTmuxEnv {
		env = "env -u TMUX "
	}
	return fmt.Sprintf("%stmux attach-session -t %s", env, ShellQuote(name))
}

// ClientInfo describes an attached tmux client.
type ClientInfo struct {
	Client      string
	Session     string
	Window      string
	WindowID    string
	WindowWidth int
}

// ListClients returns the list of attached clients.
func ListClients() []ClientInfo {
	result := RunRaw([]string{"list-clients", "-F", "#{client_name}\t#{session_name}\t#{window_name}\t#{window_id}\t#{window_width}"})
	if result.ExitCode != 0 || result.Stdout == "" {
		return nil
	}
	lines := strings.Split(result.Stdout, "\n")
	clients := make([]ClientInfo, 0, len(lines))
	for _, line := range lines {
		parts := strings.SplitN(line, "\t", 5)
		if len(parts) != 5 {
			continue
		}
		width, _ := strconv.Atoi(parts[4])
		clients = append(clients, ClientInfo{
			Client:      parts[0],
			Session:     parts[1],
			Window:      parts[2],
			WindowID:    parts[3],
			WindowWidth: width,
		})
	}
	return clients
}

// HasSession reports whether a tmux session exists.
func HasSession(name string) bool {
	return RunRaw([]string{"has-session", "-t", name}).ExitCode == 0
}

// NewSession creates a new detached session running command in cwd.
func NewSession(name, cwd, command string) error {
	_, err := Run([]string{"new-session", "-d", "-s", name, "-c", cwd, command})
	return err
}

// KillSession kills a tmux session.
func KillSession(name string) error {
	_, err := Run([]string{"kill-session", "-t", name})
	return err
}

// KillWindow kills a tmux window.
func KillWindow(target string) error {
	_, err := Run([]string{"kill-window", "-t", target})
	return err
}

// KillPane kills a tmux pane.
func KillPane(target string) error {
	_, err := Run([]string{"kill-pane", "-t", target})
	return err
}

// RenameWindow renames a tmux window.
func RenameWindow(target, name string) error {
	_, err := Run([]string{"rename-window", "-t", target, name})
	return err
}

// DetachClient detaches all clients attached to session.
func DetachClient(session string) error {
	_, err := Run([]string{"detach-client", "-s", session})
	return err
}

// DisplayMessage runs display-message with an optional target.
func DisplayMessage(format string, target string) (string, error) {
	args := []string{"display-message", "-p"}
	if target != "" {
		args = append(args, "-t", target)
	}
	args = append(args, format)
	return Run(args)
}

// SupportsFloatingPanes reports whether the connected tmux server provides
// the non-modal new-pane command introduced in tmux 3.7.
func SupportsFloatingPanes() bool {
	return RunRaw([]string{"list-commands", "new-pane"}).ExitCode == 0
}

// DisplayFloatingNotification creates a detached top-right floating pane. A
// previous llmux notification in the same window is replaced, and the pane is
// removed automatically when its command exits.
func DisplayFloatingNotification(windowID string, windowWidth int, message string, duration time.Duration) (string, error) {
	if windowID == "" || windowWidth < 10 {
		return "", nil
	}

	result := RunRaw([]string{"list-panes", "-t", windowID, "-F", "#{pane_id}\t#{@llm_notification}"})
	if result.ExitCode == 0 {
		for _, line := range strings.Split(result.Stdout, "\n") {
			parts := strings.SplitN(line, "\t", 2)
			if len(parts) == 2 && parts[1] == "1" {
				_ = KillPane(parts[0])
			}
		}
	}

	const (
		minimumWidth = 24
		panePadding  = 4
	)
	width := max(minimumWidth, utf8.RuneCountInString(message)+panePadding)
	width = min(width, windowWidth)
	message = truncateRunes(message, max(1, width-panePadding))
	x := max(0, windowWidth-width)
	seconds := strconv.FormatFloat(duration.Seconds(), 'f', 3, 64)
	args := []string{
		"new-pane", "-d", "-P", "-F", "#{pane_id}",
		"-t", windowID,
		"-x", strconv.Itoa(width), "-y", "3",
		"-X", strconv.Itoa(x), "-Y", "0",
		"-s", "bg=yellow,fg=black",
		"-S", "fg=yellow", "-R", "fg=yellow",
		"sh", "-c", `printf '\n  %s' "$1"; sleep "$2"`,
		"llmux-notification", message, seconds,
	}
	paneID, err := Run(args)
	if err != nil {
		return "", err
	}
	paneID = strings.TrimSpace(paneID)
	if paneID == "" {
		return "", fmt.Errorf("tmux new-pane returned an empty pane id")
	}
	if _, err := Run([]string{"set-option", "-p", "-t", paneID, "@llm_notification", "1"}); err != nil {
		_ = KillPane(paneID)
		return "", err
	}
	return paneID, nil
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit == 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

// DisplayPopupOptions configures a tmux display-popup command.
type DisplayPopupOptions struct {
	Width   string
	Height  string
	Command string
	Client  string
}

// DisplayPopup opens a tmux popup.
func DisplayPopup(opts DisplayPopupOptions) error {
	args := []string{"display-popup", "-w", opts.Width, "-h", opts.Height, "-E", opts.Command}
	if opts.Client != "" {
		args = append(args[:1], append([]string{"-c", opts.Client}, args[1:]...)...)
	}
	_, err := Run(args)
	return err
}
