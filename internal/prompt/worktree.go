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
	branchPrefix := tmux.GetGlobalOption(worktree.TmuxBranchPrefixOption, worktree.DefaultBranchPrefix)
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
	theme := currentChrome().theme
	renderWorktreePrompt(theme, repository, task, branchPrefix, cursor, errMsg, creating)
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
			plan, err := worktree.NewPlan(repository, task, branchPrefix)
			if err != nil {
				errMsg = err.Error()
				break
			}
			creating = true
			errMsg = ""
			renderWorktreePrompt(theme, repository, task, branchPrefix, cursor, errMsg, creating)
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
		renderWorktreePrompt(theme, repository, task, branchPrefix, cursor, errMsg, creating)
	}
}

func renderWorktreePrompt(theme ansi.Theme, repository worktree.Repository, task, branchPrefix string, cursor int, errMsg string, creating bool) {
	cols, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		cols = 80
	}
	fmt.Print(ansi.ClearScreen)

	header(1, cols, theme, "NEW WORKTREE AGENT", ansi.Foreground(theme.Muted)+repository.Name+" ")
	writeLine(3, cols, fmt.Sprintf("  %sTask%s", ansi.Foreground(theme.Faint), ansi.Reset))
	before := task[:cursor]
	after := task[cursor:]
	band(4, cols, theme.SelectionBg, fmt.Sprintf("%s  › %s%s%s█%s%s%s",
		ansi.Foreground(theme.Accent), ansi.Foreground(theme.Text), before,
		ansi.Foreground(theme.Accent), ansi.Foreground(theme.Text), after, ansi.Reset))

	slug := worktree.Slug(task)
	if strings.TrimSpace(task) == "" {
		slug = "<task>"
	}
	destination := filepath.Join(repository.StorageDir, slug)
	field := func(row int, name string, value string, valueColor ansi.RGB) {
		writeLine(row, cols, fmt.Sprintf("  %s%-13s%s%s%s", ansi.Foreground(theme.Faint), name, ansi.Foreground(valueColor), value, ansi.Reset))
	}
	field(6, "Starts from", repository.Branch, theme.Branch)
	field(7, "Branch", worktree.BranchName(branchPrefix, slug), theme.Branch)
	field(8, "Checkout", shortenHome(destination), theme.Muted)

	row := 10
	if repository.Dirty {
		writeLine(row, cols, fmt.Sprintf("  %s⚠ Uncommitted changes stay behind; the worktree starts from the last commit.%s",
			ansi.Foreground(theme.Attention), ansi.Reset))
		row++
	}
	switch {
	case errMsg != "":
		writeLine(row, cols, fmt.Sprintf("  %s%s%s", ansi.Foreground(theme.Error), errMsg, ansi.Reset))
	case creating:
		writeLine(row, cols, fmt.Sprintf("  %sCreating worktree and starting agent…%s", ansi.Foreground(theme.Accent), ansi.Reset))
	default:
		writeLine(row, cols, keyHints(theme, [][2]string{{"⏎", "create"}, {"^U", "clear"}, {"esc", "cancel"}})+
			ansi.Foreground(theme.Faint)+"branch is kept on cleanup"+ansi.Reset)
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
