package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"llm-session-manager/internal/add"
	"llm-session-manager/internal/claudehook"
	"llm-session-manager/internal/codexhook"
	"llm-session-manager/internal/initcmd"
	"llm-session-manager/internal/launch"
	"llm-session-manager/internal/listcmd"
	"llm-session-manager/internal/picker"
	"llm-session-manager/internal/prompt"
	"llm-session-manager/internal/sessions"
	"llm-session-manager/internal/state"
	"llm-session-manager/internal/tmux"
	"llm-session-manager/internal/types"
	"llm-session-manager/internal/warm"
	"llm-session-manager/internal/worktree"
)

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: llmux <launch|add|warm|list|picker|prompt|state|claude-hook|codex-hook|worktree|init>")
	os.Exit(1)
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
	}

	switch args[0] {
	case "launch":
		cwd := currentDir()
		origin := ""
		if len(args) > 1 {
			cwd = args[1]
		}
		if len(args) > 2 {
			origin = args[2]
		}
		if err := launch.Launch(cwd, origin); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

	case "add":
		cwd := currentDir()
		origin := ""
		if len(args) > 1 {
			cwd = args[1]
		}
		if len(args) > 2 {
			origin = args[2]
		}
		if _, err := add.Add(cwd, origin, true); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

	case "warm":
		cwd := currentDir()
		if len(args) > 1 {
			cwd = args[1]
		}
		if err := warm.Warm(cwd); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

	case "list":
		clientName := ""
		if len(args) > 1 {
			clientName = args[1]
		}
		if err := listcmd.ListCommand(clientName); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

	case "picker":
		if err := picker.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

	case "prompt":
		defaultPath := currentDir()
		origin := ""
		if len(args) > 1 {
			defaultPath = args[1]
		}
		if len(args) > 2 {
			origin = args[2]
		}
		if err := prompt.Run(defaultPath, origin); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

	case "worktree-prompt":
		defaultPath := currentDir()
		origin := ""
		if len(args) > 1 {
			defaultPath = args[1]
		}
		if len(args) > 2 {
			origin = args[2]
		}
		if err := prompt.RunWorktree(defaultPath, origin); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

	case "state":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: llmux state <working|waiting|idle>")
			os.Exit(1)
		}
		if !types.IsState(args[1]) {
			fmt.Fprintf(os.Stderr, "Invalid state: %s\n", args[1])
			os.Exit(1)
		}
		if err := state.SetState(types.State(args[1])); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

	case "claude-hook":
		// Claude reads exit code 2 from some hooks as "block this action", so
		// state updates stay best-effort: report failures on stderr and exit 0.
		if len(args) < 2 || !types.IsState(args[1]) {
			fmt.Fprintln(os.Stderr, "Usage: llmux claude-hook <working|waiting|idle>")
			return
		}
		var input io.Reader = os.Stdin
		if term.IsTerminal(int(os.Stdin.Fd())) {
			input = nil
		}
		if err := claudehook.Run(types.State(args[1]), input); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case "codex-hook":
		// Codex reads stdout and non-zero exits as hook decisions, so state
		// updates stay best-effort: report failures on stderr and exit 0.
		if err := codexhook.Run(os.Stdin); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case "worktree":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: llmux worktree <list|remove> [path]")
			os.Exit(1)
		}
		switch args[1] {
		case "list":
			manifests, err := worktree.List()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			for _, manifest := range manifests {
				fmt.Printf("%s\t%s\t%s\n", manifest.Repository, manifest.Branch, manifest.WorktreePath)
			}
		case "remove":
			path := currentDir()
			if len(args) > 2 {
				path = args[2]
			}
			owned, err := worktree.Load(path)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			if worktree.Within(currentDir(), owned.WorktreePath) {
				fmt.Fprintln(os.Stderr, "run worktree removal from outside the checkout being removed")
				os.Exit(1)
			}
			prefix := tmux.GetGlobalOption("@llm_session_prefix", "llm-")
			for _, session := range sessions.GetAllSessions(prefix) {
				if worktree.Within(session.Path, owned.WorktreePath) {
					fmt.Fprintln(os.Stderr, "worktree still has an active agent; use D in the Control Room or stop it first")
					os.Exit(1)
				}
			}
			panes := tmux.RunRaw([]string{"list-panes", "-a", "-F", "#{pane_current_path}"})
			if panes.ExitCode == 0 {
				for _, panePath := range strings.Split(panes.Stdout, "\n") {
					if worktree.Within(panePath, owned.WorktreePath) {
						fmt.Fprintln(os.Stderr, "worktree is still open in a tmux pane; close it before removing the checkout")
						os.Exit(1)
					}
				}
			}
			manifest, err := worktree.Remove(path)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			fmt.Printf("Removed %s; branch %s kept.\n", manifest.WorktreePath, manifest.Branch)
		default:
			fmt.Fprintln(os.Stderr, "Usage: llmux worktree <list|remove> [path]")
			os.Exit(1)
		}

	case "init":
		shell := "zsh"
		if len(args) > 1 {
			shell = args[1]
		}
		if err := initcmd.Run(shell); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

	default:
		usage()
	}
}

func currentDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}
