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
// decide what to write. Free text (label, reason, paths) comes last.
const paneInfoFormat = "#{session_name}\t#{window_id}\t#{@llm_agent}\t#{@llm_state}\t#{@llm_ever_attached}\t#{@llm_session_prefix}\t#{@llm_agent_session}\t#{@llm_reason_kind}\t#{@llm_unseen}\t#{@llm_label}\t#{@llm_label_auto}\t#{@llm_reason}\t#{@llm_path}\t#{pane_current_path}"

const paneInfoFields = 14

// Lengths kept for free text taken from agent payloads.
const (
	reasonLimit    = 90
	autoLabelLimit = 60
)

// Report is one agent lifecycle report. Only State is required; the other
// fields come from agents whose hooks describe what happened.
type Report struct {
	State types.State
	// ReasonKind and Reason say why a waiting agent needs the user.
	ReasonKind types.ReasonKind
	Reason     string
	// KeepReason preserves a reason already recorded for this wait, so a
	// generic notification never replaces a permission prompt's command.
	KeepReason bool
	// Prompt names an unlabeled task after the user's first prompt.
	Prompt string
	// NewConversation forgets the automatic task name, as after /clear.
	NewConversation bool
	// AgentSession is the agent's own conversation id, used to resume it.
	AgentSession string
}

// SetState records a state report for the current tmux window.
func SetState(state types.State) error {
	return Record(Report{State: state})
}

// Record writes a report for the current tmux window. Agents report on every
// tool call, so an unchanged report costs two tmux calls: one read and one
// heartbeat write. A change additionally recounts waiting agents and wakes
// any open Control Room in the same tmux command list.
func Record(report Report) error {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		// Not running inside tmux; state updates are best-effort.
		return nil
	}

	result := tmux.RunRaw([]string{"display-message", "-p", "-t", pane, paneInfoFormat})
	if result.ExitCode != 0 {
		return fmt.Errorf("failed to resolve session from pane: %s", result.Stderr)
	}
	fields := strings.SplitN(result.Stdout, "\t", paneInfoFields)
	if len(fields) != paneInfoFields {
		return fmt.Errorf("unexpected pane details from tmux: %q", result.Stdout)
	}
	session, windowID, agentMarker, previousState, everAttached, prefix := fields[0], fields[1], fields[2], fields[3], fields[4], fields[5]
	agentSession, reasonKind, unseen := fields[6], types.ReasonKind(fields[7]), fields[8] == "1"
	label, autoLabel, reason := fields[9], fields[10], fields[11]
	if prefix == "" {
		prefix = "llm-"
	}
	if session == "" || !strings.HasPrefix(session, prefix) {
		return nil
	}
	if windowID == "" {
		return fmt.Errorf("empty window id from pane")
	}

	state := report.State
	now := strconv.FormatInt(time.Now().Unix(), 10)
	changed := previousState != string(state)
	visible := changed
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

	// Why the agent is waiting. Leaving the wait forgets the reason.
	if state == types.Waiting && report.ReasonKind != "" && !(report.KeepReason && reasonKind != "") {
		nextReason := sessions.Clean(report.Reason, reasonLimit)
		if report.ReasonKind != reasonKind || nextReason != reason {
			commands = append(commands,
				tmux.SetWindowOptionArgs(windowID, "@llm_reason_kind", string(report.ReasonKind)),
				tmux.SetWindowOptionArgs(windowID, "@llm_reason", nextReason))
			reasonKind, reason = report.ReasonKind, nextReason
			visible = true
		}
	} else if state != types.Waiting && reasonKind != "" {
		commands = append(commands,
			unsetWindowOptionArgs(windowID, "@llm_reason_kind"),
			unsetWindowOptionArgs(windowID, "@llm_reason"))
		reasonKind, reason = "", ""
	}

	// A finished turn stays marked until the user looks at it; any new
	// activity supersedes the mark.
	if changed && state == types.Idle && previousState == string(types.Working) {
		commands = append(commands, tmux.SetWindowOptionArgs(windowID, "@llm_unseen", "1"))
	} else if changed && state != types.Idle && unseen {
		commands = append(commands, unsetWindowOptionArgs(windowID, "@llm_unseen"))
	}

	// Name an unlabeled task after its first prompt.
	if report.NewConversation && autoLabel != "" {
		commands = append(commands, unsetWindowOptionArgs(windowID, "@llm_label_auto"))
		autoLabel = ""
		visible = true
	}
	if name := AutoLabel(report.Prompt); name != "" && label == "" && autoLabel == "" {
		commands = append(commands, tmux.SetWindowOptionArgs(windowID, "@llm_label_auto", name))
		autoLabel = name
		visible = true
	}
	if id := sessions.Clean(report.AgentSession, 0); id != "" && id != agentSession {
		commands = append(commands, tmux.SetWindowOptionArgs(windowID, "@llm_agent_session", id))
	}

	if changed {
		// Recounting every managed window keeps the status bar correct even if
		// an earlier hook was interrupted.
		commands = append(commands, sessions.WaitingStatusCommands()...)
	}
	if visible {
		// Wake open Control Rooms so they redraw immediately.
		commands = append(commands, []string{"wait-for", "-S", sessions.ChangedChannel})
	}
	if _, err := tmux.Run(tmux.Batch(commands...)); err != nil {
		return err
	}

	if changed && state == types.Waiting {
		path := fields[12]
		if path == "" {
			path = fields[13]
		}
		sendWaitingNotification(prefix, types.Session{
			Name:       session,
			WindowID:   windowID,
			Label:      label,
			AutoLabel:  autoLabel,
			Path:       path,
			State:      state,
			ReasonKind: reasonKind,
			Reason:     reason,
		})
	}
	return nil
}

func unsetWindowOptionArgs(window, name string) []string {
	return []string{"set-window-option", "-u", "-t", window, name}
}

// AutoLabel turns a prompt into a short task name: its first non-empty line,
// cleaned and shortened. Slash commands such as /clear name nothing.
func AutoLabel(prompt string) string {
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			return ""
		}
		return sessions.Clean(line, autoLabelLimit)
	}
	return ""
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
	// Nested clients (Control Room live panes and agent popups) share the
	// outer terminal, so alerting them as well would alert twice.
	mode := notifyMode(tmux.GetGlobalOption("@llm_notify", ""), tmux.GetGlobalOption("@llm_chime", "true"))
	message := waitingNotificationMessage(session)
	tmux.AlertClients(outerClients(prefix, clients), mode.bell, mode.desktop, message)
	if !tmux.SupportsFloatingPanes() {
		return
	}
	for _, window := range notificationWindows(prefix, clients) {
		_, _ = tmux.DisplayFloatingNotification(window.id, window.width, message, waitingNotificationDuration)
	}
}

type alertMode struct {
	bell    bool
	desktop bool
}

// notifyMode resolves @llm_notify (bell, desktop, both or off). When it is
// unset, the older @llm_chime option decides whether the bell rings.
func notifyMode(notify, chime string) alertMode {
	switch strings.ToLower(strings.TrimSpace(notify)) {
	case "bell":
		return alertMode{bell: true}
	case "desktop":
		return alertMode{desktop: true}
	case "both":
		return alertMode{bell: true, desktop: true}
	case "off", "none", "false":
		return alertMode{}
	}
	return alertMode{bell: chimeEnabled(chime)}
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
	name := strings.TrimSpace(session.Title())
	if name == "" && session.Path != "" {
		name = filepath.Base(filepath.Clean(session.Path))
	}
	if name == "" || name == "." {
		name = session.Name
	}
	if name == "" {
		name = "LLM session"
	}
	return sessions.Clean(name, 0) + ": " + ReasonText(session.ReasonKind, session.Reason)
}

// ReasonText describes why an agent is waiting, for notifications and titles.
func ReasonText(kind types.ReasonKind, reason string) string {
	title := kind.Title()
	switch {
	case title == "":
		return "Needs Review"
	case reason == "":
		return title
	default:
		return title + " · " + sessions.Clean(reason, 0)
	}
}
