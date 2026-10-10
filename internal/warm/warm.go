package warm

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"llm-session-manager/internal/agent"
	"llm-session-manager/internal/sessions"
	"llm-session-manager/internal/tmux"
)

// Warm creates or refreshes a managed session for cwd without opening a
// popup. The session is hidden from the picker until launch/add promotes it.
//
// Warm-only sessions are capped (default 5, configurable via
// @llm_warm_cap; 0 = unlimited). When over cap, the oldest warm-only session
// (by @llm_warmed_at) is evicted. Sessions that were ever attached
// (@llm_ever_attached) are in-use, don't count against the cap, and are never
// evicted by warming.
func Warm(cwd string) error {
	prefix := tmux.GetGlobalOption("@llm_session_prefix", "llm-")
	command := agent.Active()
	if command == "" {
		return agent.ErrNotConfigured()
	}
	capStr := tmux.GetGlobalOption("@llm_warm_cap", "5")
	cap, err := strconv.Atoi(capStr)
	if err != nil || cap < 0 {
		cap = 5
	}

	cwd = sessions.CanonicalPath(cwd)
	sessionName := sessions.SessionNameForPath(cwd, prefix)

	// Already exists (warm or in-use) — refresh timestamp and bail.
	if tmux.HasSession(sessionName) {
		_ = tmux.SetSessionOption(sessionName, "@llm_warmed_at",
			strconv.FormatInt(time.Now().UnixNano(), 10))
		return nil
	}

	// Enforce the warm-only cap before creating a new session.
	if cap > 0 {
		evictWarmOnly(prefix, cap)
	}

	windowID, err := tmux.Run([]string{"new-session", "-dP", "-F", "#{window_id}",
		"-s", sessionName, "-c", cwd, agent.LaunchCommand(command)})
	if err != nil {
		return err
	}

	// Hide from the picker: do NOT set @llm_agent (the picker filter keys
	// on it). Disable auto-rename so tmux doesn't relabel the window after
	// the running command. Name the window "warm" explicitly.
	_, err = tmux.Run(tmux.Batch(
		tmux.SetWindowOptionArgs(windowID, "automatic-rename", "off"),
		[]string{"rename-window", "-t", windowID, "warm"},
		tmux.SetWindowOptionArgs(windowID, "@llm_warm_agent", agent.Name(command)),
		tmux.SetWindowOptionArgs(windowID, "@llm_path", cwd),
		tmux.SetSessionOptionArgs(sessionName, "@llm_warmed_at", strconv.FormatInt(time.Now().UnixNano(), 10)),
	))
	return err
}

// Claimed is the pre-warmed agent window that launch or add promotes.
type Claimed struct {
	WindowID string
	Agent    string
}

// Claim prepares a warm-only session for promotion by launch or add and
// returns the window already running its agent. A warm session running a
// different agent than activeAgent is discarded instead, so the agent chosen
// with s in the Control Room applies to every session the user opens; ok is
// then false and the caller creates a fresh session.
func Claim(sessionName, activeAgent string) (claimed Claimed, ok bool, err error) {
	details, err := tmux.DisplayMessage("#{window_id}\t#{@llm_warm_agent}\t#{pane_start_command}\t#{pane_current_command}", sessionName+":")
	if err != nil {
		return Claimed{}, false, fmt.Errorf("failed to resolve warm window: %w", err)
	}
	parts := strings.SplitN(details, "\t", 4)
	if len(parts) != 4 || parts[0] == "" {
		return Claimed{}, false, fmt.Errorf("failed to resolve warm window: unexpected details %q", details)
	}
	claimed = Claimed{WindowID: parts[0], Agent: parts[1]}
	if claimed.Agent == "" {
		claimed.Agent = agent.Name(parts[2])
	}
	if claimed.Agent == "" {
		claimed.Agent = parts[3]
	}
	if activeAgent != "" && claimed.Agent != "" && claimed.Agent != activeAgent {
		if err := tmux.KillSession(sessionName); err != nil {
			return Claimed{}, false, fmt.Errorf("failed to replace warm %s session: %w", claimed.Agent, err)
		}
		return Claimed{}, false, nil
	}
	return claimed, true, nil
}

// evictWarmOnly kills the oldest warm-only managed sessions until the pool
// size is cap-1 (leaving room for one new session). A warm-only session has
// the llm- prefix and no @llm_ever_attached option set.
func evictWarmOnly(prefix string, cap int) {
	result := tmux.RunRaw([]string{"list-sessions", "-F",
		"#{session_name}\t#{@llm_ever_attached}\t#{@llm_warmed_at}"})
	if result.ExitCode != 0 || result.Stdout == "" {
		return
	}

	type candidate struct {
		name     string
		warmedAt int64
	}
	var pool []candidate

	for _, line := range strings.Split(result.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, prefix) {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		name := parts[0]
		attached := ""
		warmedAt := int64(0)
		if len(parts) > 1 {
			attached = parts[1]
		}
		if len(parts) > 2 {
			if n, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
				warmedAt = n
			}
		}
		if attached != "" {
			continue // in-use; doesn't count against the warm cap
		}
		pool = append(pool, candidate{name: name, warmedAt: warmedAt})
	}

	if len(pool) < cap {
		return
	}

	sort.Slice(pool, func(i, j int) bool {
		return pool[i].warmedAt < pool[j].warmedAt
	})

	toEvict := len(pool) - (cap - 1)
	if toEvict < 0 {
		toEvict = 0
	}
	for i := 0; i < toEvict && i < len(pool); i++ {
		_ = tmux.KillSession(pool[i].name)
	}
}
