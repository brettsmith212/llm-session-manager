package agent

import (
	"fmt"
	"strings"

	"llm-session-manager/internal/ansi"
	"llm-session-manager/internal/tmux"
)

// Hardcoded fallback catalog used when @llm_agents is not set in tmux.conf.
var defaultCatalog = []string{"opencode", "claude", "amp"}

// Catalog returns the list of agent names the picker can cycle through.
// Reads @llm_agents (space-separated) from tmux; falls back to a hardcoded
// list when unset. The active agent (from @llm_command or @llm_active_agent)
// is guaranteed to be present in the returned slice.
func Catalog() []string {
	raw := tmux.GetGlobalOption("@llm_agents", "")
	var list []string
	if strings.TrimSpace(raw) != "" {
		for _, a := range strings.Fields(raw) {
			a = strings.TrimSpace(a)
			if a != "" {
				list = append(list, a)
			}
		}
	}
	if len(list) == 0 {
		list = append(list, defaultCatalog...)
	}

	active := Active()
	found := false
	for _, a := range list {
		if a == active {
			found = true
			break
		}
	}
	if !found && active != "" {
		list = append(list, active)
	}
	return list
}

// Active returns the agent command to use for new sessions, resolving in
// order: @llm_active_agent (runtime picker override) → @llm_command
// (nix-baked default) → "". The caller is responsible for erroring on "".
func Active() string {
	if a := tmux.GetGlobalOption("@llm_active_agent", ""); a != "" {
		return a
	}
	return tmux.GetGlobalOption("@llm_command", "")
}

// Cycle rotates @llm_active_agent to the next entry in the catalog and
// returns the new active agent. If the current active agent is not in the
// catalog (e.g. it was set in tmux.conf but not listed in @llm_agents),
// Cycle sets @llm_active_agent to the first catalog entry.
func Cycle() string {
	catalog := Catalog()
	current := Active()

	next := catalog[0]
	for i, a := range catalog {
		if a == current && i+1 < len(catalog) {
			next = catalog[i+1]
			break
		}
		if a == current {
			next = catalog[0]
		}
	}
	_ = tmux.SetGlobalOption("@llm_active_agent", next)
	return next
}

// BadgeColor returns the ANSI color associated with an agent name. Known
// agents get a stable color; unknown agents fall back to a muted neutral
// so the badge still reads but doesn't compete for attention.
func BadgeColor(name string) ansi.RGB {
	switch name {
	case "opencode":
		return ansi.Blue
	case "claude":
		return ansi.Peach
	case "amp":
		return ansi.Red
	default:
		return ansi.Surface2
	}
}

// ErrNotConfigured is returned by callers that need an agent command and
// find neither @llm_active_agent nor @llm_command set.
func ErrNotConfigured() error {
	return fmt.Errorf("no agent configured; set @llm_command in tmux.conf (e.g. set -g @llm_command \"opencode\") or toggle with `s` in the picker")
}
