# llm-session-manager

`llmux` is a small CLI that manages tmux sessions for LLM coding agents (Claude Code, OpenCode, Codex, etc.) and tracks per-session state (`working` / `waiting` / `idle`).

It pairs with two thin plugins that translate agent lifecycle events into `llmux state` calls:

- **`plugins/claude/`** — Claude Code plugin (this repo)
- **`opencode/llmux-plugin/`** — OpenCode plugin (lives in your nix-config)

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
llmux list                         # list sessions (used by the picker)
llmux state <working|waiting|idle> # update session state from a hook
```

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
