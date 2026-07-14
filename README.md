# llm-session-manager

`llmux` is a small CLI that manages tmux sessions for LLM coding agents (Claude Code, OpenCode, Amp, Codex, etc.) and tracks per-session state (`working` / `waiting` / `idle`).

It pairs with thin plugins that translate agent lifecycle events into `llmux state` calls:

- **`plugins/claude/`** — Claude Code plugin (this repo)
- **`plugins/opencode/`** — OpenCode event adapter
- **`plugins/amp/`** — Amp plugin using its `ThreadState` observable and command palette

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
home.file.".claude/skills/llm-session-manager" = {
  source = inputs.llmux.packages.${pkgs.system}.claude-plugin;
};
```

Claude Code auto-loads plugins with a manifest from its personal `skills`
directory. Confirm it appears as `llm-session-manager@skills-dir` with
`claude plugin list`, or inspect each loaded event and its source with `/hooks`.

### Install the Amp plugin

```nix
xdg.configFile."amp/plugins/llmux-state.ts".source =
  "${inputs.llmux.packages.${pkgs.system}.amp-plugin}/share/amp/plugins/llmux-state.ts";
```

The plugin reports Amp's `working`, `waiting`, and `idle` states to llmux. It
also adds **llmux: Open agent control room** to Amp's command palette (`Ctrl+O`).

Without Nix, copy `plugins/amp/llmux-state.ts` to
`~/.config/amp/plugins/llmux-state.ts`, then run **plugins: reload** in Amp.

### Install the OpenCode plugin

```nix
xdg.configFile."opencode/plugins/tmux-session-manager.js".source =
  "${inputs.llmux.packages.${pkgs.system}.opencode-plugin}/share/opencode/plugins/tmux-session-manager.js";
```

## Build standalone

```bash
nix build .#llmux           # the binary
nix build .#claude-plugin   # the Claude Code plugin
nix build .#amp-plugin      # the Amp plugin
nix build .#opencode-plugin # the OpenCode plugin
```

## Usage

```bash
llmux launch <cwd> <window_id>   # create a session for a new agent
llmux warm <cwd>                 # pre-warm a session in the background (hidden from picker)
llmux list                         # open the agent control room
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

### Control room

Open the control room with `Ctrl+a u`. The left pane manages sessions while
the right pane is the selected agent's live terminal:

- `Enter` moves into the live agent without closing the control room.
- `Ctrl+a u` returns from the live agent to the session list.
- `o` opens the selected agent in the full popup over its project window while
  keeping the control room alive in the background.
- `Shift+Enter` also opens the popup when the terminal and tmux modified-key
  configuration distinguish it from Enter; `o` is the portable binding.
- `r` goes directly to the selected project window for Neovim/diff review.
- `a` creates another session, returns with it selected, and offers an optional
  task label.
- `e` edits the selected session's task label. Labels are stored in the tmux
  window's `@llm_label` option; internal hash-based session names do not change.
- `n` jumps to the next agent that needs attention, even when a filter hides it.
- `/` searches paths, task labels, branches, agents, and states.
- `Ctrl+x` stops the selected agent. Working and waiting agents require a
  second `Ctrl+x` confirmation; idle agents stop immediately.

Sessions are grouped into **Needs You**, **Active**, and **Idle** sections, then
by project. Git repositories show their local branch and compact working-tree
summary (`main · 3 files · +24 · -6 · ?1`). Clean repositories show
`main · clean`; non-Git or unavailable directories omit that line. Git reads
are local, cached, and never fetch from a remote. Projects with multiple agents
in the same working directory show a shared-worktree warning.

The popup handoff preserves the project-parent workflow: closing the popup
returns to the matching project window for Neovim, diff review, and shell work.
`Ctrl+a u` then reopens the same persistent control room rather than rebuilding
it.

### Switching agents

Open the control room (`Ctrl+a u`) and press `s` to cycle the active agent. The
current choice is shown in the header — `agent: opencode ▾`,
color-coded to match the per-row badge. Only **new** sessions pick up the
change: existing sessions keep running whatever agent they booted with, so
you can have opencode sessions and a fresh claude session side by side in
the control room. The override lives in `@llm_active_agent`, a runtime tmux
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
