package tmux

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
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
	Client  string
	Session string
}

// ListClients returns the list of attached clients.
func ListClients() []ClientInfo {
	result := RunRaw([]string{"list-clients", "-F", "#{client_name} #{session_name}"})
	if result.ExitCode != 0 || result.Stdout == "" {
		return nil
	}
	lines := strings.Split(result.Stdout, "\n")
	clients := make([]ClientInfo, 0, len(lines))
	for _, line := range lines {
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		clients = append(clients, ClientInfo{Client: parts[0], Session: parts[1]})
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
