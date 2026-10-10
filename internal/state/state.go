package state

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"llm-session-manager/internal/sessions"
	"llm-session-manager/internal/tmux"
	"llm-session-manager/internal/types"
)

const waitingNotificationDuration = 5 * time.Second

var sendWaitingNotification = notifyWaitingClients

// paneInfoFormat resolves, in one tmux round trip, everything a hook needs to
// decide what to write. Label and paths come last because they are free text.
const paneInfoFormat = "#{session_name}\t#{window_id}\t#{@llm_agent}\t#{@llm_state}\t#{@llm_ever_attached}\t#{@llm_session_prefix}\t#{@llm_label}\t#{@llm_path}\t#{pane_current_path}"

// SetState records a state report for the current tmux window. Agents report
// on every tool call, so an unchanged state costs two tmux calls: one read and
// one heartbeat write. A change additionally recounts waiting agents and wakes
// any open Control Room in the same tmux command list.
func SetState(state types.State) error {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		// Not running inside tmux; state updates are best-effort.
		return nil
	}

	result := tmux.RunRaw([]string{"display-message", "-p", "-t", pane, paneInfoFormat})
	if result.ExitCode != 0 {
		return fmt.Errorf("failed to resolve session from pane: %s", result.Stderr)
	}
	fields := strings.SplitN(result.Stdout, "\t", 9)
	if len(fields) != 9 {
		return fmt.Errorf("unexpected pane details from tmux: %q", result.Stdout)
	}
	session, windowID, agentMarker, previousState, everAttached, prefix := fields[0], fields[1], fields[2], fields[3], fields[4], fields[5]
	if prefix == "" {
		prefix = "llm-"
	}
	if session == "" || !strings.HasPrefix(session, prefix) {
		return nil
	}
	if windowID == "" {
		return fmt.Errorf("empty window id from pane")
	}

	now := strconv.FormatInt(time.Now().Unix(), 10)
	changed := previousState != string(state)
	var commands [][]string
	if changed {
		commands = append(commands,
			tmux.SetWindowOptionArgs(windowID, "@llm_state", string(state)),
			tmux.SetWindowOptionArgs(windowID, "@llm_state_at", now))
	}
	commands = append(commands, tmux.SetWindowOptionArgs(windowID, "@llm_seen_at", now))
	// Mark this window as hosting a managed agent if it isn't already.
	// Skip for warm-only sessions (never attached) — they stay hidden
	// from the picker until launch/add promotes them.
	if agentMarker == "" && everAttached != "" {
		commands = append(commands, tmux.SetWindowOptionArgs(windowID, "@llm_agent", "1"))
	}
	if changed {
		// Recounting every managed window keeps the status bar correct even if
		// an earlier hook was interrupted, and the signal lets open Control
		// Rooms redraw immediately.
		commands = append(commands, sessions.WaitingStatusCommands()...)
		commands = append(commands, []string{"wait-for", "-S", sessions.ChangedChannel})
	}
	if _, err := tmux.Run(tmux.Batch(commands...)); err != nil {
		return err
	}

	if changed && state == types.Waiting {
		path := fields[7]
		if path == "" {
			path = fields[8]
		}
		sendWaitingNotification(prefix, types.Session{
			Name:     session,
			WindowID: windowID,
			Label:    fields[6],
			Path:     path,
			State:    state,
		})
	}
	return nil
}

// SeedIdle marks a newly created agent window idle unless the agent has
// already reported a state of its own.
func SeedIdle(window string) error {
	if tmux.GetWindowOption(window, "@llm_state") != "" {
		return nil
	}
	now := strconv.FormatInt(time.Now().Unix(), 10)
	_, err := tmux.Run(tmux.Batch(
		tmux.SetWindowOptionArgs(window, "@llm_state", string(types.Idle)),
		tmux.SetWindowOptionArgs(window, "@llm_state_at", now),
	))
	return err
}

func notifyWaitingClients(prefix string, session types.Session) {
	clients := tmux.ListClients()
	if chimeEnabled(tmux.GetGlobalOption("@llm_chime", "true")) {
		// Nested clients (Control Room live panes and agent popups) share the
		// outer terminal, so ringing them as well would chime twice.
		tmux.RingClientBells(outerClients(prefix, clients))
	}
	if !tmux.SupportsFloatingPanes() {
		return
	}
	message := waitingNotificationMessage(session)
	for _, window := range notificationWindows(prefix, clients) {
		_, _ = tmux.DisplayFloatingNotification(window.id, window.width, message, waitingNotificationDuration)
	}
}

func outerClients(prefix string, clients []tmux.ClientInfo) []tmux.ClientInfo {
	outer := make([]tmux.ClientInfo, 0, len(clients))
	for _, client := range clients {
		if !strings.HasPrefix(client.Session, prefix) {
			outer = append(outer, client)
		}
	}
	return outer
}

func chimeEnabled(value string) bool {
	return !strings.EqualFold(strings.TrimSpace(value), "false")
}

type notificationWindow struct {
	id    string
	width int
}

func notificationWindows(prefix string, clients []tmux.ClientInfo) []notificationWindow {
	targets := make([]notificationWindow, 0, len(clients))
	seen := make(map[string]bool)
	for _, client := range clients {
		if client.Client == "" || client.WindowID == "" || strings.HasPrefix(client.Session, prefix) || seen[client.WindowID] {
			continue
		}
		seen[client.WindowID] = true
		targets = append(targets, notificationWindow{id: client.WindowID, width: client.WindowWidth})
	}
	return targets
}

func waitingNotificationMessage(session types.Session) string {
	name := strings.TrimSpace(session.Label)
	if name == "" && session.Path != "" {
		name = filepath.Base(filepath.Clean(session.Path))
	}
	if name == "" || name == "." {
		name = session.Name
	}
	if name == "" {
		name = "LLM session"
	}
	name = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, name)
	return name + ": Needs Review"
}
