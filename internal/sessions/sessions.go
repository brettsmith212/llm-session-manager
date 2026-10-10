package sessions

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"llm-session-manager/internal/agent"
	"llm-session-manager/internal/tmux"
	"llm-session-manager/internal/types"
)

// StaleSeconds is the grace period after which "working" is downgraded to
// "idle" when an agent stops reporting altogether.
const StaleSeconds = 300

// ChangedChannel is the tmux wait-for channel signalled whenever an agent
// changes state, so open Control Rooms refresh immediately instead of waiting
// for their next poll.
const ChangedChannel = "llmux-changed"

const windowFormat = "#{session_name}\t#{window_id}\t#{window_index}\t#{@llm_state}\t#{@llm_state_at}\t#{@llm_path}\t#{@llm_origin}\t#{pane_current_path}\t#{@llm_agent}\t#{window_name}\t#{pane_current_command}\t#{pane_start_command}\t#{@llm_label}\t#{@llm_pinned}\t#{@llm_seen_at}"

// waitingMarksFormat expands to one "x" for every managed agent window that is
// waiting, across all sessions.
const waitingMarksFormat = "#{S:#{W:#{?#{&&:#{@llm_agent},#{==:#{@llm_state},waiting}},x,}}}"

// SessionHash returns a short SHA256 hash of path.
func SessionHash(path string) string {
	sum := sha256.Sum256([]byte(path))
	return fmt.Sprintf("%x", sum)[:8]
}

// SessionNameForPath builds the managed session name for a working directory.
// The physical path is hashed so the shell warm hook ($PWD), tmux bindings
// (#{pane_current_path}) and the Control Room all agree on one session per
// directory, even through symlinks or trailing slashes.
func SessionNameForPath(path, prefix string) string {
	return prefix + SessionHash(physicalPath(CanonicalPath(path)))
}

// CanonicalPath expands a leading ~ and returns the cleaned absolute path. It
// keeps symlinks intact so displayed paths match what the user typed.
func CanonicalPath(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home := os.Getenv("HOME"); home != "" {
			path = home + strings.TrimPrefix(path, "~")
		}
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

func physicalPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// FormatAgo renders a relative time string.
func FormatAgo(timestamp int64) string {
	if timestamp == 0 {
		return "-"
	}
	seconds := time.Now().Unix() - timestamp
	if seconds < 60 {
		return "now"
	}
	minutes := seconds / 60
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}
	hours := minutes / 60
	if hours < 24 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dd", hours/24)
}

// FormatPath shortens a path with ~ for the home directory.
func FormatPath(path string) string {
	home := os.Getenv("HOME")
	if home != "" && (path == home || strings.HasPrefix(path, home+"/")) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}

// EffectiveState returns the displayed state, downgrading working sessions
// that have stopped reporting. Every hook refreshes @llm_seen_at, so a busy
// agent stays "working" even though its state has not changed for a while.
func EffectiveState(s types.Session) types.State {
	lastReport := max(s.SeenAt, s.StateAt)
	if s.State == types.Working && lastReport != 0 {
		age := time.Now().Unix() - lastReport
		if age > StaleSeconds {
			return types.Idle
		}
	}
	return s.State
}

// WaitingStatusCommands recount waiting agents inside tmux and publish the
// lightweight options used by status bars. Running them in the same command
// list as a state change keeps concurrent hooks from publishing stale counts.
// The display value is empty when no sessions are waiting, so embedding
// #{@llm_status} adds no idle clutter.
func WaitingStatusCommands() [][]string {
	return [][]string{
		{"set-option", "-gF", "@llm_waiting_marks", waitingMarksFormat},
		{"set-option", "-gF", "@llm_waiting_count", "#{n:@llm_waiting_marks}"},
		{"set-option", "-gF", "@llm_status", "#{?#{==:#{@llm_waiting_count},0},,◆ #{@llm_waiting_count} needs you}"},
	}
}

// PublishWaitingStatus recounts waiting agents and updates the status options.
func PublishWaitingStatus() error {
	_, err := tmux.Run(tmux.Batch(WaitingStatusCommands()...))
	return err
}

// CountWaiting returns how many sessions currently need the user.
func CountWaiting(all []types.Session) int {
	waiting := 0
	for _, session := range all {
		if EffectiveState(session) == types.Waiting {
			waiting++
		}
	}
	return waiting
}

// GetAllSessions fetches all managed agent windows across all managed tmux
// sessions, sorted by project path, session, then window index.
func GetAllSessions(prefix string) []types.Session {
	result := tmux.RunRaw([]string{"list-windows", "-a", "-F", windowFormat})
	if result.ExitCode != 0 || result.Stdout == "" {
		return nil
	}

	lines := strings.Split(result.Stdout, "\n")
	sessions := make([]types.Session, 0, len(lines))
	for _, line := range lines {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		parts := strings.SplitN(line, "\t", 15)
		if len(parts) < 9 {
			continue
		}
		// parts[8] is @llm_agent — the marker that this window hosts a managed
		// LLM agent and, for newly created windows, its stable identity.
		// Legacy windows used "1" as the marker; infer those from the stable
		// pane start command, then the live command, before using the mutable
		// window name as a last resort.
		if parts[8] == "" {
			continue
		}

		name := parts[0]
		windowID := parts[1]
		windowIndex := 0
		if n, err := strconv.Atoi(parts[2]); err == nil {
			windowIndex = n
		}
		windowName := ""
		if parts[8] != "1" {
			windowName = parts[8]
		} else if len(parts) > 11 && agent.Name(parts[11]) != "" {
			windowName = agent.Name(parts[11])
		} else if len(parts) > 10 && parts[10] != "" {
			windowName = parts[10]
		} else if len(parts) > 9 {
			windowName = parts[9]
		}

		state := types.State(parts[3])
		if !types.IsState(string(state)) {
			state = ""
		}

		stateAt := int64(0)
		if parts[4] != "" {
			if n, err := strconv.ParseInt(parts[4], 10, 64); err == nil {
				stateAt = n
			}
		}

		path := parts[5]
		if path == "" {
			path = parts[7]
		}

		origin := parts[6]
		label := ""
		if len(parts) > 12 {
			label = parts[12]
		}

		pinned := false
		if len(parts) > 13 {
			pinned = parts[13] == "1"
		}

		seenAt := int64(0)
		if len(parts) > 14 && parts[14] != "" {
			if n, err := strconv.ParseInt(parts[14], 10, 64); err == nil {
				seenAt = n
			}
		}

		session := types.Session{
			Name:        name,
			WindowID:    windowID,
			WindowIndex: windowIndex,
			WindowName:  windowName,
			Label:       label,
			State:       state,
			StateAt:     stateAt,
			SeenAt:      seenAt,
			Path:        path,
			Origin:      origin,
			Pinned:      pinned,
		}
		session.DisplayState = EffectiveState(session)
		sessions = append(sessions, session)
	}

	sort.SliceStable(sessions, func(i, j int) bool {
		leftPath := strings.ToLower(filepath.Clean(sessions[i].Path))
		rightPath := strings.ToLower(filepath.Clean(sessions[j].Path))
		if leftPath != rightPath {
			return leftPath < rightPath
		}
		if sessions[i].Name != sessions[j].Name {
			return sessions[i].Name < sessions[j].Name
		}
		return sessions[i].WindowIndex < sessions[j].WindowIndex
	})

	return sessions
}
