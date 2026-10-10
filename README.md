# llm-session-manager

`llmux` is a small CLI that manages tmux sessions for LLM coding agents (Claude Code, OpenCode, Amp, Codex, etc.) and tracks per-session state (`working` / `waiting` / `idle`).

It pairs with thin plugins that translate agent lifecycle events into `llmux state` calls:

- **`plugins/claude/`** — Claude Code plugin (this repo)
- **`plugins/opencode/`** — OpenCode event adapter
- **`plugins/amp/`** — Amp plugin using its `ThreadState` observable and command palette
- **`plugins/codex/`** — Codex hooks that feed `llmux codex-hook`

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

The plugin routes every Claude Code hook through `llmux claude-hook <state>`.
The state comes from the hook configuration; the payload Claude passes on stdin
adds why the agent is waiting, the first prompt (to name the task), and the
conversation id used to resume it.

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

### Install the Codex hooks

```nix
home.file.".codex/hooks.json".source =
  "${inputs.llmux.packages.${pkgs.system}.codex-plugin}/share/codex/hooks.json";
```

Without Nix, copy `plugins/codex/hooks.json` to `~/.codex/hooks.json`, or merge
its `hooks` entries into an existing file.

Codex skips new or changed hooks until you trust them. After installing (and
after any update that changes the file), start Codex, run `/hooks`, and trust
the `llmux codex-hook` entries. Until then Codex shows a startup warning and
llmux cannot see its state.

Each hook pipes the event's JSON payload to `llmux codex-hook`, which maps it
to a state: a prompt or tool call is `working`, an approval request or a
`request_user_input` question is `waiting`, and the end of a turn or an
interrupt is `idle`. The payload also supplies the command an approval request
is for, the first prompt (to name the task), and the session id `R` resumes.

llmux adjusts Codex in two ways that other agents do not need:

- It starts Codex with `--no-daemon`. Codex otherwise runs sessions inside a
  shared background server, and hooks inherit that server's `TMUX_PANE`, so
  every Codex window would report into whichever pane started the server.
- It marks new Codex windows `idle` itself, because Codex runs no hooks until
  the first prompt is submitted.

After you approve a command, the window keeps showing `needs you` until that
command finishes, because Codex has no hook for approval decisions.

## Build standalone

```bash
nix build .#default         # the binary
nix build .#claude-plugin   # the Claude Code plugin
nix build .#amp-plugin      # the Amp plugin
nix build .#opencode-plugin # the OpenCode plugin
nix build .#codex-plugin    # the Codex hooks
```

## Usage

```bash
llmux launch <cwd> <window_id>   # create a session for a new agent
llmux warm <cwd>                 # pre-warm a session in the background (hidden from picker)
llmux list                         # open the agent control room
llmux state <working|waiting|idle> # update session state from a hook
llmux claude-hook <state>          # record a Claude Code hook; reads its payload on stdin
llmux codex-hook                   # update session state from a Codex hook payload on stdin
llmux worktree list                # list llmux-owned isolated worktrees
llmux worktree remove <path>       # safely remove a clean worktree; keep its branch
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
| `@llm_theme` | `catppuccin-mocha` | Control Room colors: `catppuccin-mocha` or `tokyonight`. Also styles the room's borders and the create popups. |
| `@llm_notify` | *(unset)* | How an agent asks for you: `bell`, `desktop` (a system notification through the terminal, OSC 9), `both`, or `off`. When unset, `@llm_chime` decides whether the bell rings. |
| `@llm_chime` | `true` | Ring attached tmux clients when an agent enters `waiting`. Set to `false` to disable. Ignored once `@llm_notify` is set. |
| `@llm_review_command` | `hunk diff $LLMUX_BASE --watch` when hunk is installed, else a paged `git diff` | Command `d` runs in a popup at the selected checkout. `$LLMUX_BASE` is empty for an ordinary checkout and is the starting commit of an llmux worktree. |
| `@llm_worktree_base` | XDG data home (`~/.local/share`) | Parent directory for new isolated checkouts. llmux appends `llmux/worktrees/<repository>-<hash>/<task>`. Existing worktrees are unaffected. |
| `@llm_worktree_branch_prefix` | `llmux` | Namespace for new worktree branches, such as `brett/patch-one`. Set to `none` for an unprefixed branch such as `patch-one`. |
| `@llm_parent` | *(unset)* | Target tmux client for popup anchoring. Set automatically by the tmux binding; usually not set manually. |

A minimal `tmux.conf` block:

```tmux
set -g @llm_command 'opencode'
set -g @llm_agents 'opencode claude amp'

# Optional: match a TokyoNight terminal.
set -g @llm_theme 'tokyonight'

# Optional: a desktop notification as well as the bell when an agent needs you.
set -g @llm_notify 'both'

# Optional: what d runs to review a checkout's diff.
set -g @llm_review_command 'hunk diff $LLMUX_BASE --watch'

# Optional: place new isolated checkouts under
# ~/Developer/llmux/worktrees/<repository>-<hash>/<task>/
set -g @llm_worktree_base '~/Developer'

# Optional: create brett/<task> branches instead of llmux/<task>.
# Use 'none' if you want branches named only <task>.
set -g @llm_worktree_branch_prefix 'brett'

# Optional ambient indicator; llmux keeps @llm_status updated, for example
# "◆ 2 needs you · ✓ 1 done", and leaves it empty when nothing needs you.
set -ag status-right ' #[fg=#f9e2af]#{@llm_status}#[default]'

# Ctrl+a y opens (or, inside a popup, closes) the project's agent popup;
# Ctrl+a u opens the control room.
bind y if-shell -F "#{m:llm-*,#{session_name}}" \
  "detach-client" \
  "run-shell 'llmux launch \"#{pane_current_path}\" \"#{session_name}\"'"
bind u run-shell 'llmux list "#{client_name}"'
```

Bind `y` and `u` without `-r`. A repeatable binding keeps tmux in the prefix
table for `repeat-time`, so keys such as `j`, `k`, or `g` typed right after
opening the control room would run any repeatable bindings of your own instead
of reaching the picker.

`llmux` also maintains the numeric `@llm_waiting_count` and `@llm_done_count`
options for custom status formats. They are recounted inside tmux whenever an
agent changes state, and an open control room corrects them if an agent exits
while waiting.

On tmux 3.7 or newer, when an agent first enters `waiting`, llmux also shows a
yellow notification for about five seconds in the top-right of every window
currently displayed by an attached client on the tmux server. Clients attached
directly to managed `llm-*` sessions are excluded. This means an agent in one
project can request attention while you work in Neovim in another project
window. Repeated `waiting` hooks do not repeat the notification or chime;
leaving and re-entering `waiting` does. By default, llmux writes a terminal bell
to every attached tmux client on macOS and Linux. Whether the bell is audible
depends on the terminal emulator's bell settings. Set `@llm_notify` to
`desktop` or `both` for a system notification as well: llmux writes an OSC 9
sequence straight to the terminal, which Ghostty, iTerm2 and WezTerm show even
while another app is in front. A newer notice replaces an existing llmux notice
in the same window. The text names the task, then says why the agent is
waiting when its hooks report it, for example
`api-server: Permission · Bash · npm run db:migrate`, and otherwise
`api-server: Needs Review`.

Notifications use tmux 3.7's non-modal floating panes, so they do not change
focus, consume input, or create a persistent window or pane. On older tmux
versions, transient notifications are disabled; Control Room state,
`@llm_waiting_count`, `@llm_status`, and chimes continue to work normally.

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
- `d` reviews the selected checkout's diff in a popup over the control room,
  using `@llm_review_command`. For an llmux worktree the review starts where
  the worktree branched, so commits the agent already made are included.
  Closing the viewer returns to the room with the same row selected.
- `a` creates another session and returns to normal navigation with it selected
  and previewed. A creation bar advertises `e` for optional naming; typing is
  never captured implicitly. When the selected project already has a managed
  session, the new agent shares that checkout.
- `A` creates an agent in a new isolated Git worktree based on the selected
  project's committed `HEAD`. It asks for the task name before creating it.
- `D` removes a selected llmux-created worktree after confirmation. Cleanup
  refuses dirty worktrees, stops every agent sharing it, and keeps its branch.
- `e` opens the selected session's prominent task-label editor. Labels are
  stored in the tmux window's `@llm_label` option; internal hash-based session
  names do not change. An automatic name, when there is one, is the starting
  text.
- `n` jumps to the next agent that needs you, even when a filter hides it. When
  nothing needs you, it visits finished results you have not looked at yet.
- `/` searches paths, task names, wait reasons, branches, agents, and states.
- `gg` jumps to the first session and `G` jumps to the last session in the
  current list. A committed search filter limits both jumps to matching sessions.
- `p` pins the selected session to a **Pinned** section at the top of the
  control room. Pinned sessions stay there regardless of state but keep their
  live state glyph. Press `p` again to unpin. Pin state lives in the window's
  `@llm_pinned` option and survives picker restarts and tmux refreshes.
- `Ctrl+x` stops the selected agent. Working and waiting agents require a
  second `Ctrl+x` confirmation; idle and exited agents stop immediately.
- `R` restarts an agent that exited. Claude and Codex agents resume the
  conversation they last reported; other agents start fresh.
- `?` lists every key. The footer only shows the keys for the selected row.

Sessions are grouped into **Pinned**, **Needs You**, **Active**, **Done**, and
**Idle** sections, then by project recency. The **Pinned** section appears only
when one or more sessions have been pinned with `p` and stays at the top
regardless of state. **Done** holds agents that finished a turn you have not
looked at yet; a result counts as seen once you open the agent, review its
diff, or leave it in the live pane for two seconds. Within each section,
repository families and their checkouts sort by the most recent state change
among their visible sessions. Agents report on every tool call, but only a
change of state (for example from working to needs you) moves a group, so busy
agents do not reshuffle the list. Checkouts from the same Git repository remain
together; equal recency keeps the primary checkout before its worktrees and
uses task, branch, and path as deterministic fallbacks.

Each checkout gets one heading line: its path, or for a worktree its
repository and task (`api-server ⎇ Fix auth retry`), followed on the right by
its branch and working-tree summary (`main  ±3 +24 −6 ?1`, or `main  clean`).
llmux worktrees also show the commits made since they branched (`↑2`) and
`merged` once the branch they started from contains those commits. Non-Git
directories show no summary. Git reads are local, cached, and never fetch from a
remote. When more than one agent is actively working in the same checkout, its
heading warns `⚠ 2 writing`. Within a single checkout, agents sort by most
recent state change first. Each row shows the task, the agent, and how long it
has been in its current state; waits turn orange after five minutes and red
after fifteen.

Rows explain themselves:

- A task without a label is named after its first prompt, shown in italics,
  for agents whose hooks pass the prompt (Claude Code and Codex).
- An agent that needs you shows why on the line below: `Permission · Bash ·
  npm test`, `Question · Which cache?`, `Plan ready · …`, or a red `Error`
  when a turn failed.
- An agent that crashes stays on screen as `✕ Exited · status 3` instead of
  disappearing with its task name (new agent windows set tmux's
  `remain-on-exit failed`; a clean exit still closes the window). `R` restarts
  it.
- Agents that keep printing output stay **working** through long tool calls;
  only an agent that stops reporting and stops printing for five minutes is
  shown as idle.

The live pane's title names the agent it shows and, while it waits, why; the
title turns yellow when the agent needs you, red when it failed, and teal when
it has an unseen result.

The control room refreshes the moment any agent changes state. Git status is
read only while the room is on screen, without taking Git's index lock, so it
never interferes with Git commands the agents run in the same checkouts.

The popup handoff preserves the project-parent workflow: closing the popup
returns to the matching project window for Neovim, diff review, and shell work.
`Ctrl+a u` then reopens the same persistent control room rather than rebuilding
it.

### Isolated worktree agents

Uppercase `A` is an additive alternative to the ordinary `a` flow. By default,
it creates a task branch and checkout beneath the XDG data directory:

```text
~/.local/share/llmux/worktrees/<repository>-<hash>/<task>/
```

Set `@llm_worktree_base` to move new checkouts to another disk or directory.
The value is a base directory—not the final worktree root—so this Linux-friendly
tmux configuration:

```tmux
set -g @llm_worktree_base '~/Developer'
```

produces:

```text
~/Developer/llmux/worktrees/<repository>-<hash>/<task>/
```

Relative values are resolved beneath the home directory, and `~` is expanded
by llmux. Existing worktrees keep their recorded locations. Ownership manifests
remain in the XDG data directory so `llmux worktree list` and cleanup continue
to find checkouts across old and customized roots.

For example, an isolated `Try Nixvim Upgrade` task selected from
`~/.config/nix-config` gets a branch such as
`llmux/try-nixvim-upgrade` and a separate checkout under the directory above.
The original `~/.config/nix-config` checkout is not moved or modified. Git
objects and history remain shared with it.

The `llmux/` branch namespace makes tool-created branches easy to identify, but
it is configurable for personal or workplace conventions:

```tmux
set -g @llm_worktree_branch_prefix 'brett'
```

New branches then use names such as `brett/try-nixvim-upgrade`. Set the option
to `none` to create `try-nixvim-upgrade` with no namespace. This only affects
new worktrees; existing branches and checkouts are not renamed.

The worktree starts from the selected checkout's committed `HEAD`.
Uncommitted and untracked source-checkout changes are not copied; the creation
prompt warns when they exist. Gitignored environment files are likewise not
copied automatically. If you later add ignored files inside the isolated
checkout, move anything important elsewhere before cleanup: like Git's native
`worktree remove`, llmux may remove ignored files without treating them as
pending changes.

`r` creates or selects a normal parent tmux window at the isolated checkout,
and `o` opens the agent over that exact window. Closing the popup therefore
returns to the correct worktree for Neovim and Git review.

llmux records ownership manifests under `~/.local/share/llmux/tasks`, so
worktrees remain discoverable with `llmux worktree list` even after their tmux
sessions stop. `D` and `llmux worktree remove` only remove clean llmux-owned
worktrees and always retain their branches. Delete a merged branch separately
with Git when you no longer need it.

### Switching agents

Open the control room (`Ctrl+a u`) and press `s` to cycle the active agent. The
current choice is shown in the header — `agent: opencode ▾`,
color-coded to match the per-row badge. Only **new** sessions pick up the
change: existing sessions keep running whatever agent they booted with, so
you can have opencode sessions and a fresh claude session side by side in
the control room. A pre-warmed session you have not opened yet counts as new:
if it was warmed with a different agent, opening it starts the selected agent
instead. The override lives in `@llm_active_agent`, a runtime tmux option, so
a fresh tmux server silently reverts to `@llm_command`.

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
