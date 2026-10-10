package listcmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"llm-session-manager/internal/sessions"
	"llm-session-manager/internal/tmux"
)

const windowName = "llm-picker"

// PlaceholderCommand keeps an empty live-agent pane open until the picker
// previews an agent there. It never exits, so an idle room stays healthy.
const PlaceholderCommand = "printf '\\n  No agent selected\\n'; exec tail -f /dev/null"

// ListCommand builds or focuses the tmux control-room window. clientName,
// when non-empty, is the tmux client that invoked the command (typically
// "#{client_name}" expanded by the calling key binding) and is used as the
// host client instead of guessing from the server-wide client list — guessing
// breaks down as soon as more than one non-managed client is attached anywhere
// on the tmux server.
func ListCommand(clientName string) error {
	prefix := tmux.GetGlobalOption("@llm_session_prefix", "llm-")
	clients := tmux.ListClients()
	rooms := loadRoomPanes()
	invoker := resolveInvoker(prefix, clientName, clients, rooms)

	// A live pane is a nested tmux client whose TTY is the pane TTY of an
	// existing control room. Return through that outer client rather than
	// treating the managed session as the place where a new room belongs.
	// Selecting the outer list reroutes physical input without detaching the
	// nested client, so the agent remains live and visible in the right pane.
	host := rooms.roomHost(invoker, clients)
	switch {
	case host != nil:
		// Invoked from a live pane; host is the client showing that room.
	case invoker != nil && strings.HasPrefix(invoker.Session, prefix):
		// An agent popup, or a terminal attached straight to an agent session.
		// Close just that client: other clients of the same agent session, such
		// as a Control Room's live pane, must keep running.
		detachAndWait(invoker.Client)
		host = outerHost(prefix, tmux.ListClients())
	case invoker != nil:
		host = invoker
	default:
		host = outerHost(prefix, clients)
	}
	if host == nil {
		_ = tmux.SetGlobalOption("@llm_parent", "")
		return fmt.Errorf("no attached tmux client outside an agent session can show the control room")
	}

	// Reuse a healthy room in the host client's session whether it is
	// currently visible or in the background. This makes Ctrl+a u both the
	// entry point from a project window and the escape hatch from the live pane.
	if focusExistingControlRoom(host) {
		return nil
	}

	pickerTarget := host.Session + ":" + windowName
	_ = tmux.RunRaw([]string{"kill-window", "-t", pickerTarget})
	commands := [][]string{
		tmux.SetGlobalOptionArgs("@llm_parent", host.Client),
		{"new-window", "-t", host.Session + ":", "-n", windowName, "-c", currentDir(), PlaceholderCommand},
		// The picker previews its first agent here as soon as it starts.
		{"split-window", "-h", "-l", "67%", "-t", pickerTarget + ".0", PlaceholderCommand},
	}
	commands = append(commands, styleCommands(pickerTarget)...)
	commands = append(commands,
		[]string{"select-pane", "-t", pickerTarget + ".1", "-T", "LIVE AGENT · prefix u returns"},
		[]string{"respawn-pane", "-k", "-t", pickerTarget + ".0", "-c", currentDir(), tmux.ShellQuote(binaryPath()) + " picker"},
		[]string{"select-pane", "-t", pickerTarget + ".0"},
		[]string{"switch-client", "-c", host.Client, "-t", pickerTarget},
	)
	if _, err := tmux.Run(tmux.Batch(commands...)); err != nil {
		return fmt.Errorf("couldn't build control room: %w", err)
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
	dead := make(map[string]bool, 2)
	for _, line := range strings.Split(result.Stdout, "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) == 2 {
			alive[parts[0]] = parts[1] == "0"
			dead[parts[0]] = parts[1] != "0"
		}
	}
	// The picker holds the room's state; without it the room is rebuilt. A
	// missing or dead live pane is recreated by the picker on its next refresh.
	if !alive["0"] {
		return false
	}
	var commands [][]string
	if dead["1"] {
		commands = append(commands, []string{"kill-pane", "-t", target + ".1"})
	}
	commands = append(commands, tmux.SetGlobalOptionArgs("@llm_parent", host.Client))
	// Refresh the styling as well as the focus so persistent rooms immediately
	// pick up interface improvements without being torn down and rebuilt.
	commands = append(commands, styleCommands(target)...)
	commands = append(commands,
		[]string{"select-pane", "-t", target + ".0"},
		[]string{"switch-client", "-c", host.Client, "-t", target},
		// Wake the picker so a backgrounded room is current the moment it shows.
		[]string{"wait-for", "-S", sessions.ChangedChannel},
	)
	_, err := tmux.Run(tmux.Batch(commands...))
	return err == nil
}

func styleCommands(target string) [][]string {
	commands := [][]string{tmux.SetWindowOptionArgs(target, "@llm_control_room", "1")}
	borderOpts := [][2]string{
		{"pane-border-status", "top"},
		{"pane-border-format", "#{?pane_active,#[fg=#89b4fa bold] ● FOCUSED · #{pane_title} #[default],#[fg=#6c7086]   #{pane_title} #[default]}"},
		{"pane-border-style", "fg=#6c7086"},             // Catppuccin Overlay0
		{"pane-active-border-style", "fg=#89b4fa,bold"}, // Catppuccin Blue
		{"pane-border-lines", "heavy"},
	}
	for _, option := range borderOpts {
		commands = append(commands, tmux.SetWindowOptionArgs(target, option[0], option[1]))
	}
	return append(commands, []string{"select-pane", "-t", target + ".0", "-T", "CONTROL ROOM"})
}

// roomPanes maps the TTY of each control room's live pane to the session that
// hosts that room.
type roomPanes map[string]string

func loadRoomPanes() roomPanes {
	rooms := make(roomPanes)
	result := tmux.RunRaw([]string{"list-panes", "-a", "-F",
		"#{session_name}\t#{window_name}\t#{pane_index}\t#{pane_tty}"})
	if result.ExitCode != 0 {
		return rooms
	}
	for _, line := range strings.Split(result.Stdout, "\n") {
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) == 4 && parts[1] == windowName && parts[2] == "1" && parts[3] != "" {
			rooms[parts[3]] = parts[0]
		}
	}
	return rooms
}

// roomSession returns the session hosting the control room whose live pane
// owns client, or "" when client is not a live pane.
func (r roomPanes) roomSession(client *tmux.ClientInfo) string {
	if client == nil {
		return ""
	}
	if session, ok := r[client.TTY]; ok {
		return session
	}
	return r[client.Client]
}

// roomHost returns the outer client displaying the control room whose live
// pane owns nested.
func (r roomPanes) roomHost(nested *tmux.ClientInfo, clients []tmux.ClientInfo) *tmux.ClientInfo {
	roomSession := r.roomSession(nested)
	if roomSession == "" {
		return nil
	}
	preferred := tmux.GetGlobalOption("@llm_parent", "")
	var fallback *tmux.ClientInfo
	for _, client := range clients {
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

// containingControlRoom returns the outer client displaying the control room
// whose live pane owns the invoking nested client's TTY.
func containingControlRoom(nested *tmux.ClientInfo) *tmux.ClientInfo {
	if nested == nil || nested.Client == "" {
		return nil
	}
	return loadRoomPanes().roomHost(nested, tmux.ListClients())
}

// resolveInvoker finds the client the user pressed the key in. Key bindings
// pass "#{client_name}". Without one, as when an agent's command palette runs
// `llmux list`, the client is inferred from the calling pane's session,
// preferring a popup over a control room's live pane because a popup is
// drawn on top of everything else.
func resolveInvoker(prefix, clientName string, clients []tmux.ClientInfo, rooms roomPanes) *tmux.ClientInfo {
	if clientName != "" {
		for _, client := range clients {
			if client.Client == clientName {
				client := client
				return &client
			}
		}
		return nil
	}
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return nil
	}
	session, err := tmux.DisplayMessage("#{session_name}", pane)
	if err != nil || !strings.HasPrefix(session, prefix) {
		return nil
	}
	var livePane *tmux.ClientInfo
	for _, client := range clients {
		if client.Session != session {
			continue
		}
		client := client
		if rooms.roomSession(&client) == "" {
			return &client
		}
		if livePane == nil {
			livePane = &client
		}
	}
	return livePane
}

// outerHost picks the client that should show the control room: the room's
// last host when it is still attached outside agent sessions, otherwise the
// first such client.
func outerHost(prefix string, clients []tmux.ClientInfo) *tmux.ClientInfo {
	preferred := tmux.GetGlobalOption("@llm_parent", "")
	var first *tmux.ClientInfo
	for _, client := range clients {
		if strings.HasPrefix(client.Session, prefix) {
			continue
		}
		client := client
		if client.Client == preferred {
			return &client
		}
		if first == nil {
			first = &client
		}
	}
	return first
}

// detachAndWait detaches one client and waits briefly for tmux to drop it, so
// the host chosen next cannot be the client that is going away.
func detachAndWait(client string) {
	if client == "" {
		return
	}
	_ = tmux.RunRaw([]string{"detach-client", "-t", client})
	for i := 0; i < 40; i++ {
		attached := false
		for _, c := range tmux.ListClients() {
			if c.Client == client {
				attached = true
				break
			}
		}
		if !attached {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
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
