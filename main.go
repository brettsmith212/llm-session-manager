package main

import (
	"fmt"
	"os"

	"llm-session-manager/internal/add"
	"llm-session-manager/internal/initcmd"
	"llm-session-manager/internal/launch"
	"llm-session-manager/internal/listcmd"
	"llm-session-manager/internal/picker"
	"llm-session-manager/internal/prompt"
	"llm-session-manager/internal/state"
	"llm-session-manager/internal/types"
	"llm-session-manager/internal/warm"
)

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: llmux <launch|add|warm|list|picker|prompt|state|init>")
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
