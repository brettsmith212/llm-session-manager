package warm

import (
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

	if err := tmux.NewSession(sessionName, cwd, command); err != nil {
		return err
	}

	// Hide from the picker: do NOT set @llm_agent (the picker filter keys
	// on it). Disable auto-rename so tmux doesn't relabel the window after
	// the running command. Name the window "warm" explicitly.
	_ = tmux.SetWindowOption(sessionName+":0", "automatic-rename", "off")
	_ = tmux.RenameWindow(sessionName+":0", "warm")
	_ = tmux.SetWindowOption(sessionName+":0", "@llm_path", cwd)
	_ = tmux.SetSessionOption(sessionName, "@llm_warmed_at",
		strconv.FormatInt(time.Now().UnixNano(), 10))
	return nil
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
