# llm-session-manager

`llmux` is a small CLI that manages tmux sessions for LLM coding agents (Claude Code, OpenCode, Amp, Codex, etc.) and tracks per-session state (`working` / `waiting` / `idle`).

It pairs with thin plugins that translate agent lifecycle events into `llmux state` calls:

- **`plugins/claude/`** — Claude Code plugin (this repo)
- **`opencode/llmux-plugin/`** — OpenCode plugin (lives in your nix-config)
- Amp plugin — TypeScript plugin using Amp's Plugin API `ThreadState` observable (lives in your nix-config)

## Install

### Nix (flake input)

```nix
inputs.llmux = {
  url = "github:brettsmith212/llm-session-manager";
  inputs.nixpkgs.follows = "nixpkgs";
};
```

Then in your home/packages:

```nix
llmux = inputs.llmux.packages.${pkgs.system}.default;
```

### Install the Claude Code plugin

```nix
home.file.".claude/plugins/llm-session-manager" = {
  source = inputs.llmux.packages.${pkgs.system}.claude-plugin;
};
```

## Build standalone

```bash
nix build .#llmux           # the binary
nix build .#claude-plugin   # the Claude Code plugin
```

## Usage

```bash
llmux launch <cwd> <window_id>   # create a session for a new agent
llmux warm <cwd>                 # pre-warm a session in the background (hidden from picker)
llmux list                         # list sessions (used by the picker)
llmux state <working|waiting|idle> # update session state from a hook
```

### Tmux options

All configuration is done via tmux global options (`set -g @... ...` in
`tmux.conf`). None are strictly required — `llmux` boots with sane
defaults — but you'll usually want to set at least `@llm_command` so new
sessions know which agent to launch.

| Option | Default | Purpose |
|---|---|---|
| `@llm_command` | *(unset)* | Agent binary launched for new sessions when no runtime override is active. Setting this is the minimum configuration; `llmux` falls back to this even after the picker toggles to a new agent and the tmux server restarts. |
| `@llm_agents` | `opencode claude amp` | Space-separated list of agents the picker cycles through. Add new ones here when you install a new tool. Falls back to the hardcoded list when unset. |
| `@llm_session_prefix` | `llm-` | Prefix for managed tmux session names. Sessions are named `<prefix><sha256(path)[:8]>`. |
| `@llm_popup_width` | `90%` | Width of the popup opened by `launch`/`add`. Any tmux size spec. |
| `@llm_popup_height` | `90%` | Height of the popup opened by `launch`/`add`. |
| `@llm_warm_cap` | `5` | Max number of warm-only background sessions. `0` = unlimited. Oldest evicted LRU-style. |
| `@llm_parent` | *(unset)* | Target tmux client for popup anchoring. Set automatically by the tmux binding; usually not set manually. |

A minimal `tmux.conf` block:

```tmux
set -g @llm_command 'opencode'
set -g @llm_agents 'opencode claude amp'

# Optional ambient indicator; llmux keeps @llm_status updated and leaves it
# empty when no sessions need attention.
set -ag status-right ' #[fg=#f9e2af]#{@llm_status}#[default]'
```

`llmux` also maintains the numeric `@llm_waiting_count` option for custom
status formats. Both values update whenever an agent reports a state change.

### Switching agents

Open the picker (`Ctrl+a u`) and press `s` to cycle the active agent. The
current choice is shown in the picker header — `agent: opencode ▾`,
color-coded to match the per-row badge. Only **new** sessions pick up the
change: existing sessions keep running whatever agent they booted with, so
you can have opencode sessions and a fresh claude session side by side in
the picker. The override lives in `@llm_active_agent`, a runtime tmux
option, so a fresh tmux server silently reverts to `@llm_command`.

### Pre-warming

Add one of these lines to your `.zshrc` or `.bashrc` (depending on your shell)
so sessions pre-warm in the background when you `cd` into a git project root.
The agent boots detached and hidden from the picker, so by the time you open it
with `Ctrl+a y` it's already ready — no multi-second cold start:

```zsh
# add to ~/.zshrc
eval "$(llmux init zsh)"
```

```bash
# add to ~/.bashrc
eval "$(llmux init bash)"
```

Only fires inside tmux and at directories containing `.git`. Warm sessions are
capped (default 5, configurable via the `@llm_warm_cap` tmux option; `0` for
unlimited); the oldest is evicted LRU-style. A session is promoted out of the
warm pool the first time you launch it and won't be evicted by warming.

## Build without Nix

Prereqs: Go 1.25+ (only external dep is `golang.org/x/term`).

```bash
git clone https://github.com/brettsmith212/llm-session-manager
cd llm-session-manager
go build -o llmux .
mv llmux ~/.local/bin/   # or anywhere on $PATH
```

Cross-compile from any host:

```bash
GOOS=darwin GOARCH=arm64 go build -o llmux-darwin-arm64 .
GOOS=linux  GOARCH=amd64 go build -o llmux-linux-amd64 .
```

For the Claude Code plugin, just copy the directory:

```bash
cp -r plugins/claude ~/.claude/plugins/llm-session-manager
```

## License

MIT
