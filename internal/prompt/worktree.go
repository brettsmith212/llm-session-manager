package prompt

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"llm-session-manager/internal/add"
	"llm-session-manager/internal/ansi"
	"llm-session-manager/internal/tmux"
	"llm-session-manager/internal/worktree"
)

// RunWorktree creates an isolated Git worktree and starts one agent there.
// It is invoked inside a tmux popup from the Control Room.
func RunWorktree(defaultPath, origin string) error {
	worktreeBase := tmux.GetGlobalOption(worktree.TmuxBaseOption, "")
	repository, err := worktree.Inspect(defaultPath, worktreeBase)
	if err != nil {
		return err
	}
	task := ""
	cursor := 0
	errMsg := ""
	creating := false

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("failed to set raw mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	fmt.Print(ansi.HideCursor)
	defer fmt.Print(ansi.ShowCursor)

	reader := bufio.NewReader(os.Stdin)
	renderWorktreePrompt(repository, task, cursor, errMsg, creating)
	for {
		keys, err := readKey(reader)
		if err != nil {
			return nil
		}
		if len(keys) == 0 {
			continue
		}
		code := keys[0]
		switch {
		case keys == "\x1b" || code == 3: // bare esc or ctrl-c
			return nil
		case code == 13: // enter
			plan, err := worktree.NewPlan(repository, task)
			if err != nil {
				errMsg = err.Error()
				break
			}
			creating = true
			errMsg = ""
			renderWorktreePrompt(repository, task, cursor, errMsg, creating)
			if err := submitWorktree(plan, origin); err != nil {
				creating = false
				errMsg = err.Error()
				break
			}
			return nil
		case code == 21: // ctrl-u
			task = ""
			cursor = 0
			errMsg = ""
		case code == 127 || code == 8: // backspace
			if cursor > 0 {
				task = task[:cursor-1] + task[cursor:]
				cursor--
			}
			errMsg = ""
		case keys == "\x1b[3~": // delete
			if cursor < len(task) {
				task = task[:cursor] + task[cursor+1:]
			}
			errMsg = ""
		case keys == "\x1b[H" || keys == "\x1b[1~": // home
			cursor = 0
		case keys == "\x1b[F" || keys == "\x1b[4~": // end
			cursor = len(task)
		case keys == "\x1b[D": // left
			if cursor > 0 {
				cursor--
			}
		case keys == "\x1b[C": // right
			if cursor < len(task) {
				cursor++
			}
		case code >= 32 && code <= 126:
			task = task[:cursor] + keys + task[cursor:]
			cursor += len(keys)
			errMsg = ""
		}
		renderWorktreePrompt(repository, task, cursor, errMsg, creating)
	}
}

func renderWorktreePrompt(repository worktree.Repository, task string, cursor int, errMsg string, creating bool) {
	cols, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		cols = 80
	}
	fmt.Print(ansi.ClearScreen)

	writeLine(2, cols, fmt.Sprintf("  %s%sCreate isolated worktree agent%s",
		ansi.Foreground(ansi.Blue), ansi.Bold, ansi.Reset))
	writeLine(4, cols, fmt.Sprintf("  %sRepository:%s %s",
		ansi.Foreground(ansi.Overlay0), ansi.Reset, repository.Name))
	writeLine(5, cols, fmt.Sprintf("  %sBase:%s       %s",
		ansi.Foreground(ansi.Overlay0), ansi.Reset, repository.Branch))

	before := task[:cursor]
	after := task[cursor:]
	writeLine(7, cols, fmt.Sprintf("  %sTask:%s       %s%s%s_%s%s",
		ansi.Foreground(ansi.Overlay0), ansi.Reset,
		ansi.Foreground(ansi.Text), before,
		ansi.Foreground(ansi.Blue), ansi.Foreground(ansi.Text), after+ansi.Reset))

	slug := worktree.Slug(task)
	if strings.TrimSpace(task) == "" {
		slug = "<task>"
	}
	destination := filepath.Join(repository.StorageDir, slug)
	writeLine(9, cols, fmt.Sprintf("  %sBranch:%s     llmux/%s",
		ansi.Foreground(ansi.Overlay0), ansi.Reset, slug))
	writeLine(10, cols, fmt.Sprintf("  %sCreates:%s    %s",
		ansi.Foreground(ansi.Overlay0), ansi.Reset, shortenHome(destination)))

	row := 12
	if repository.Dirty {
		writeLine(row, cols, fmt.Sprintf("  %s⚠ Source has uncommitted changes; worktree starts from committed HEAD.%s",
			ansi.Foreground(ansi.Yellow), ansi.Reset))
		row++
	}
	if errMsg != "" {
		writeLine(row, cols, fmt.Sprintf("  %s%s%s", ansi.Foreground(ansi.Red), errMsg, ansi.Reset))
	} else if creating {
		writeLine(row, cols, fmt.Sprintf("  %sCreating worktree and starting agent…%s",
			ansi.Foreground(ansi.Blue), ansi.Reset))
	} else {
		writeLine(row, cols, fmt.Sprintf("  %sEnter create · Esc cancel · branch is retained on cleanup%s",
			ansi.Foreground(ansi.Overlay0), ansi.Reset))
	}
}

func submitWorktree(plan worktree.Plan, origin string) error {
	if err := worktree.Create(plan); err != nil {
		return err
	}
	windowID, err := add.Add(plan.Path, origin, false)
	if err != nil {
		if rollbackErr := worktree.DiscardCreated(plan); rollbackErr != nil {
			return fmt.Errorf("agent launch failed: %v; worktree rollback also failed: %v", err, rollbackErr)
		}
		return err
	}

	// The manifest is the durable source of truth; tmux metadata makes the
	// active session self-describing and the label immediately useful.
	_ = tmux.SetWindowOption(windowID, "@llm_label", plan.Label)
	if sessionName, resolveErr := tmux.DisplayMessage("#{session_name}", windowID); resolveErr == nil {
		_ = tmux.SetSessionOption(sessionName, "@llm_worktree_path", plan.Path)
		_ = tmux.SetSessionOption(sessionName, "@llm_worktree_repo", plan.Repository.CommonDir)
		_ = tmux.SetSessionOption(sessionName, "@llm_worktree_branch", plan.Branch)
		_ = tmux.SetSessionOption(sessionName, "@llm_worktree_base", plan.Repository.Head)
	}
	_ = tmux.SetWindowOption(pickerWindow, pickerSelectionOption, windowID)
	return nil
}

func shortenHome(path string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}
