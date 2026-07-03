package sessions

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"llm-session-manager/internal/tmux"
	"llm-session-manager/internal/types"
)

// StaleSeconds is the grace period after which "working" is downgraded to "idle".
const StaleSeconds = 300

const sessionFormat = "#{session_name}\t#{@llm_state}\t#{@llm_state_at}\t#{@llm_path}\t#{@llm_origin}\t#{pane_current_path}"

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

// GetAllSessions fetches all managed sessions in a single tmux call.
func GetAllSessions(prefix string) []types.Session {
	result := tmux.RunRaw([]string{"list-sessions", "-F", sessionFormat})
	if result.ExitCode != 0 || result.Stdout == "" {
		return nil
	}

	lines := strings.Split(result.Stdout, "\n")
	sessions := make([]types.Session, 0, len(lines))
	for _, line := range lines {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		parts := strings.SplitN(line, "\t", 6)
		if len(parts) < 6 {
			continue
		}

		name := parts[0]
		state := types.State(parts[1])
		if !types.IsState(string(state)) {
			state = ""
		}

		stateAt := int64(0)
		if parts[2] != "" {
			if n, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
				stateAt = n
			}
		}

		path := parts[3]
		if path == "" {
			path = parts[5]
		}

		origin := parts[4]

		sessions = append(sessions, types.Session{
			Name:    name,
			State:   state,
			StateAt: stateAt,
			Path:    path,
			Origin:  origin,
		})
	}
	return sessions
}
