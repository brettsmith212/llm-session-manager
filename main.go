package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"llm-session-manager/internal/add"
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
	fmt.Fprintln(os.Stderr, "Usage: llmux <launch|add|warm|list|picker|prompt|state|worktree|init>")
	os.Exit(1)
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
	}

	command := args[0]
	switch command {
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
			if samePath(currentDir(), owned.WorktreePath) {
				fmt.Fprintln(os.Stderr, "run worktree removal from outside the checkout being removed")
				os.Exit(1)
			}
			prefix := tmux.GetGlobalOption("@llm_session_prefix", "llm-")
			for _, session := range sessions.GetAllSessions(prefix) {
				if samePath(session.Path, owned.WorktreePath) {
					fmt.Fprintln(os.Stderr, "worktree still has an active agent; use D in the Control Room or stop it first")
					os.Exit(1)
				}
			}
			panes := tmux.RunRaw([]string{"list-panes", "-a", "-F", "#{pane_current_path}"})
			if panes.ExitCode == 0 {
				for _, panePath := range strings.Split(panes.Stdout, "\n") {
					if samePath(panePath, owned.WorktreePath) {
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

func samePath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr == nil && rightErr == nil && filepath.Clean(leftAbs) == filepath.Clean(rightAbs) {
		return true
	}
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}
