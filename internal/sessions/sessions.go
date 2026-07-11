package sessions

import (
	"crypto/sha256"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"llm-session-manager/internal/agent"
	"llm-session-manager/internal/tmux"
	"llm-session-manager/internal/types"
)

// StaleSeconds is the grace period after which "working" is downgraded to "idle".
const StaleSeconds = 300

const windowFormat = "#{session_name}\t#{window_id}\t#{window_index}\t#{@llm_state}\t#{@llm_state_at}\t#{@llm_path}\t#{@llm_origin}\t#{pane_current_path}\t#{@llm_agent}\t#{window_name}\t#{pane_current_command}\t#{pane_start_command}"

// SessionHash returns a short SHA256 hash of path.
func SessionHash(path string) string {
	sum := sha256.Sum256([]byte(path))
	return fmt.Sprintf("%x", sum)[:8]
}

// SessionNameForPath builds the managed session name for a working directory.
func SessionNameForPath(path, prefix string) string {
	return prefix + SessionHash(path)
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
	if home != "" && strings.HasPrefix(path, home) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}

// EffectiveState returns the displayed state, downgrading stale working sessions.
func EffectiveState(s types.Session) types.State {
	if s.State == types.Working && s.StateAt != 0 {
		age := time.Now().Unix() - s.StateAt
		if age > StaleSeconds {
			return types.Idle
		}
	}
	return s.State
}

// GetAllSessions fetches all managed agent windows across all managed tmux
// sessions, grouped and sorted by session then window index.
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
		parts := strings.SplitN(line, "\t", 12)
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

		sessions = append(sessions, types.Session{
			Name:        name,
			WindowID:    windowID,
			WindowIndex: windowIndex,
			WindowName:  windowName,
			State:       state,
			StateAt:     stateAt,
			Path:        path,
			Origin:      origin,
		})
	}

	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].Name != sessions[j].Name {
			return sessions[i].Name < sessions[j].Name
		}
		return sessions[i].WindowIndex < sessions[j].WindowIndex
	})

	return sessions
}
