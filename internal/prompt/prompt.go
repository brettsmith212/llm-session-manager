package prompt

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"llm-session-manager/internal/ansi"
	"llm-session-manager/internal/tmux"
)

const pickerWindow = "llm-picker"

// Run displays a full-width path input prompt inside a tmux popup.
func Run(defaultPath, origin string) error {
	path := defaultPath
	cursor := len(path)
	var errMsg string

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("failed to set raw mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	fmt.Print(ansi.HideCursor)
	defer fmt.Print(ansi.ShowCursor)

	input := make(chan string, 16)
	go readInput(input)

	render(path, cursor, errMsg)

	for {
		select {
		case keys := <-input:
			if done := handleKey(keys, &path, &cursor, &errMsg, origin); done {
				return nil
			}
			render(path, cursor, errMsg)
		}
	}
}

func render(path string, cursor int, errMsg string) {
	cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		cols, rows = 80, 24
	}

	fmt.Print(ansi.ClearScreen)

	const title = "  Create new session"
	const label = "  Create in: "
	const help = "  Enter to create  ·  Ctrl-C to cancel"

	writeLine(2, cols, fmt.Sprintf("%s%s%s", ansi.Foreground(ansi.Blue)+ansi.Bold, title, ansi.Reset))

	maxPathWidth := cols - len(label) - 2
	start, end := windowAroundCursor(path, cursor, maxPathWidth)
	display := path[start:end]
	displayCursor := cursor - start

	if start > 0 && len(display) > 0 {
		display = "…" + display[1:]
	}
	if end < len(path) && len(display) > 0 {
		display = display[:len(display)-1] + "…"
	}

	before := display[:displayCursor]
	after := ""
	if displayCursor < len(display) {
		after = display[displayCursor:]
	}

	prompt := fmt.Sprintf("%s%s%s%s%s%s%s",
		label,
		ansi.Foreground(ansi.Text), before,
		ansi.Foreground(ansi.Blue), "_",
		ansi.Foreground(ansi.Text), after+ansi.Reset)
	writeLine(4, cols, prompt)

	if errMsg != "" {
		writeLine(6, cols, fmt.Sprintf("  %s%s%s", ansi.Foreground(ansi.Red), errMsg, ansi.Reset))
	} else {
		writeLine(6, cols, fmt.Sprintf("%s%s%s", ansi.Foreground(ansi.Overlay0), help, ansi.Reset))
	}

	_ = rows
}

func windowAroundCursor(path string, cursor, maxWidth int) (int, int) {
	if maxWidth <= 0 {
		maxWidth = 1
	}
	if len(path) <= maxWidth {
		return 0, len(path)
	}
	if cursor < maxWidth {
		return 0, maxWidth
	}
	if cursor > len(path) {
		cursor = len(path)
	}
	end := cursor + 1
	if end > len(path) {
		end = len(path)
	}
	start := end - maxWidth
	if start < 0 {
		start = 0
		end = maxWidth
	}
	return start, end
}

func writeLine(row, cols int, content string) {
	fmt.Printf("%s%s%s%s", ansi.CursorPos(row, 1), ansi.ClearLine(), content, ansi.Reset)
	_ = cols
}

func handleKey(keys string, path *string, cursor *int, errMsg *string, origin string) bool {
	if len(keys) == 0 {
		return false
	}
	code := keys[0]
	switch {
	case code == 27 || code == 3: // esc or ctrl-c
		return true
	case code == 13: // enter
		abs, err := validatePath(*path)
		if err != nil {
			*errMsg = err.Error()
			return false
		}
		submit(abs, origin)
		return true
	case code == 9: // tab
		completePath(path, cursor)
		*errMsg = ""
	case code == 127 || code == 8: // backspace
		if *cursor > 0 {
			*path = (*path)[:*cursor-1] + (*path)[*cursor:]
			*cursor--
		}
		*errMsg = ""
	case keys == "\x1b[3~": // delete
		if *cursor < len(*path) {
			*path = (*path)[:*cursor] + (*path)[*cursor+1:]
		}
		*errMsg = ""
	case keys == "\x1b[H" || keys == "\x1b[1~": // home
		*cursor = 0
		*errMsg = ""
	case keys == "\x1b[F" || keys == "\x1b[4~": // end
		*cursor = len(*path)
		*errMsg = ""
	case keys == "\x1b[D": // left
		if *cursor > 0 {
			*cursor--
		}
		*errMsg = ""
	case keys == "\x1b[C": // right
		if *cursor < len(*path) {
			*cursor++
		}
		*errMsg = ""
	case code >= 32 && code <= 126:
		*path = (*path)[:*cursor] + keys + (*path)[*cursor:]
		*cursor++
		*errMsg = ""
	}
	return false
}

func validatePath(path string) (string, error) {
	if strings.HasPrefix(path, "~") {
		home := os.Getenv("HOME")
		if home != "" {
			if path == "~" {
				path = home + "/"
			} else {
				path = home + strings.TrimPrefix(path, "~")
			}
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("invalid path: %s", path)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("path does not exist: %s", abs)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory: %s", abs)
	}
	return abs, nil
}

func completePath(path *string, cursor *int) {
	p := *path
	if p == "" {
		if cwd, err := os.Getwd(); err == nil {
			p = cwd + "/"
		}
	}
	if strings.HasPrefix(p, "~") {
		home := os.Getenv("HOME")
		if home != "" {
			if p == "~" {
				p = home + "/"
			} else {
				p = home + strings.TrimPrefix(p, "~")
			}
		}
	}

	abs, err := filepath.Abs(p)
	if err != nil {
		return
	}
	abs = filepath.Clean(abs)
	if abs == "/" {
		abs = "/"
	}

	dir := filepath.Dir(abs)
	prefix := filepath.Base(abs)
	listingDir := false
	if strings.HasSuffix(*path, "/") || strings.HasSuffix(p, "/") || abs == "/" {
		dir = abs
		prefix = ""
		listingDir = true
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	var matches []os.DirEntry
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, prefix) {
			matches = append(matches, e)
		}
	}
	if len(matches) == 0 {
		return
	}

	common := matches[0].Name()
	for _, m := range matches[1:] {
		common = commonPrefix(common, m.Name())
		if common == prefix {
			break
		}
	}

	var completed string
	if listingDir {
		completed = dir
		if !strings.HasSuffix(completed, "/") {
			completed += "/"
		}
		completed += common
	} else {
		completed = filepath.Dir(abs) + "/" + common
	}

	if len(matches) == 1 && matches[0].IsDir() {
		completed += "/"
	}

	*path = completed
	*cursor = len(*path)
}

func commonPrefix(a, b string) string {
	minLen := len(a)
	if len(b) < minLen {
		minLen = len(b)
	}
	i := 0
	for i < minLen && a[i] == b[i] {
		i++
	}
	return a[:i]
}

func submit(path, origin string) {
	cmd := exec.Command(binaryPath(), "add", path, origin)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	_ = cmd.Start()
	_ = tmux.RunRaw([]string{"kill-window", "-t", pickerWindow})
}

func binaryPath() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	if path, err := exec.LookPath("llmux"); err == nil {
		return path
	}
	return os.Args[0]
}

func readInput(out chan<- string) {
	reader := bufio.NewReader(os.Stdin)
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return
		}

		var buf strings.Builder
		buf.WriteByte(b)

		if b == '\x1b' {
			_ = os.Stdin.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
			for {
				next, err := reader.ReadByte()
				_ = os.Stdin.SetReadDeadline(time.Time{})
				if err != nil {
					break
				}
				buf.WriteByte(next)
				if next >= 0x40 && next <= 0x7e && buf.Len() > 2 {
					break
				}
				_ = os.Stdin.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
			}
		}

		out <- buf.String()
	}
}
