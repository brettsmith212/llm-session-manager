package prompt

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"llm-session-manager/internal/add"
	"llm-session-manager/internal/agent"
	"llm-session-manager/internal/ansi"
	"llm-session-manager/internal/tmux"
)

const (
	pickerWindow          = "llm-picker"
	pickerSelectionOption = "@llm_picker_selection"
)

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

	reader := bufio.NewReader(os.Stdin)
	look := currentChrome()
	render(look, path, cursor, errMsg)

	for {
		keys, err := readKey(reader)
		if err != nil {
			return nil
		}
		if done := handleKey(keys, &path, &cursor, &errMsg, origin, oldState); done {
			return nil
		}
		render(look, path, cursor, errMsg)
	}
}

// chrome is what prompts read from tmux once, so typing costs no tmux calls.
type chrome struct {
	theme ansi.Theme
	agent string
}

func currentChrome() chrome {
	return chrome{
		theme: ansi.ThemeNamed(tmux.GetGlobalOption(ansi.ThemeOption, "")),
		agent: agent.Name(agent.Active()),
	}
}

func render(look chrome, path string, cursor int, errMsg string) {
	cols, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		cols = 80
	}
	theme := look.theme
	fmt.Print(ansi.ClearScreen)

	right := ""
	if look.agent != "" {
		right = ansi.Foreground(theme.Muted) + "new " + ansi.Foreground(theme.Agent(look.agent)) + look.agent + " "
	}
	header(1, cols, theme, "NEW AGENT", right)
	writeLine(3, cols, fmt.Sprintf("  %sDirectory%s", ansi.Foreground(theme.Faint), ansi.Reset))

	const label = "  › "
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
	band(4, cols, theme.SelectionBg, fmt.Sprintf("%s%s%s%s%s█%s%s%s",
		ansi.Foreground(theme.Accent), label,
		ansi.Foreground(theme.Text), before,
		ansi.Foreground(theme.Accent),
		ansi.Foreground(theme.Text), after, ansi.Reset))

	if errMsg != "" {
		writeLine(6, cols, fmt.Sprintf("  %s%s%s", ansi.Foreground(theme.Error), errMsg, ansi.Reset))
	} else {
		writeLine(6, cols, keyHints(theme, [][2]string{{"⏎", "create"}, {"tab", "complete"}, {"^T", "fzf"}, {"^U", "clear"}, {"esc", "cancel"}}))
	}
}

// header draws the room-style title band with optional right-aligned text.
func header(row, cols int, theme ansi.Theme, title, right string) {
	left := fmt.Sprintf("%s ▌ %s%s%s%s", ansi.Foreground(theme.Accent), ansi.Foreground(theme.Text), ansi.Bold, title, ansi.Reset)
	content := left
	if right != "" {
		gap := cols - visibleWidth(left) - visibleWidth(right)
		if gap > 0 {
			content += strings.Repeat(" ", gap) + right + ansi.Reset
		}
	}
	band(row, cols, theme.HeaderBg, content)
}

// band draws a full-width line on a background color.
func band(row, cols int, background ansi.RGB, content string) {
	bg := ansi.Background(background)
	fmt.Print(ansi.CursorPos(row, 1) + ansi.ClearLine() + bg + strings.Repeat(" ", max(0, cols)) +
		ansi.CursorPos(row, 1) + bg + strings.ReplaceAll(content, ansi.Reset, ansi.Reset+bg) + ansi.Reset)
}

// keyHints renders "key label" pairs the way the room's footer does.
func keyHints(theme ansi.Theme, pairs [][2]string) string {
	var out strings.Builder
	out.WriteString("  ")
	for _, pair := range pairs {
		out.WriteString(ansi.Foreground(theme.Subtext) + ansi.Bold + pair[0] + ansi.Reset)
		out.WriteString(ansi.Foreground(theme.Faint) + " " + pair[1] + "  " + ansi.Reset)
	}
	return out.String()
}

// visibleWidth counts the cells a string with SGR sequences occupies.
func visibleWidth(value string) int {
	width := 0
	for i := 0; i < len(value); {
		if value[i] == '\x1b' {
			for i < len(value) && value[i] != 'm' {
				i++
			}
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(value[i:])
		i += size
		width++
	}
	return width
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

func handleKey(keys string, path *string, cursor *int, errMsg *string, origin string, oldState *term.State) bool {
	if len(keys) == 0 {
		return false
	}
	code := keys[0]
	switch {
	case keys == "\x1b" || code == 3: // bare esc or ctrl-c
		return true
	case code == 13: // enter
		abs, err := validatePath(*path)
		if err != nil {
			*errMsg = err.Error()
			return false
		}
		if err := submit(abs, origin); err != nil {
			*errMsg = err.Error()
			return false
		}
		return true
	case code == 21: // ctrl-u: clear entire line
		*path = ""
		*cursor = 0
		*errMsg = ""
	case code == 9: // tab
		completePath(path, cursor)
		*errMsg = ""
	case code == 20: // ctrl-t: fzf directory search
		selected, err := runFZF(*path, oldState)
		if err != nil {
			*errMsg = err.Error()
			return false
		}
		*path = selected
		*cursor = len(selected)
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

func submit(path, origin string) error {
	windowID, err := add.Add(path, origin, false)
	if err != nil {
		return err
	}
	// The picker consumes this after the prompt closes so it can highlight
	// and preview the exact window just created. This is best-effort because
	// `llmux prompt` can also be run manually without an open picker.
	_ = tmux.SetWindowOption(pickerWindow, pickerSelectionOption, windowID)
	return nil
}

// escapeSequenceDelay is how long a lone Escape waits for the rest of a
// possible escape sequence. Tmux may hold the second byte briefly while it
// decides whether the key is bound.
const escapeSequenceDelay = 100 * time.Millisecond

func readKey(reader *bufio.Reader) (string, error) {
	return readKeyFrom(reader, int(os.Stdin.Fd()))
}

// readKeyFrom reads one key press from reader, which wraps fd. Go cannot set
// read deadlines on a terminal, so a lone Escape is told apart from an escape
// sequence by waiting for more input with select(2).
func readKeyFrom(reader *bufio.Reader, fd int) (string, error) {
	b, err := reader.ReadByte()
	if err != nil {
		return "", err
	}

	var buf strings.Builder
	buf.WriteByte(b)
	if b != '\x1b' || !waitReadable(reader, fd, escapeSequenceDelay) {
		return buf.String(), nil
	}

	next, err := reader.ReadByte()
	if err != nil {
		return buf.String(), nil
	}
	buf.WriteByte(next)

	// ESC + printable byte (not [ or O) is a simple Alt+<key>. Return
	// immediately so Alt-C and friends feel snappy.
	if next != '[' && next != 'O' {
		return buf.String(), nil
	}

	// CSI / SS3 sequence: keep reading until the final byte.
	for waitReadable(reader, fd, 20*time.Millisecond) {
		next, err := reader.ReadByte()
		if err != nil {
			break
		}
		buf.WriteByte(next)
		if next >= 0x40 && next <= 0x7e {
			break
		}
	}
	return buf.String(), nil
}

// waitReadable reports whether input is available within timeout. It uses
// select(2) rather than poll(2) because macOS poll does not support terminals.
func waitReadable(reader *bufio.Reader, fd int, timeout time.Duration) bool {
	if reader.Buffered() > 0 {
		return true
	}
	for {
		var readable unix.FdSet
		readable.Set(fd)
		tv := unix.NsecToTimeval(timeout.Nanoseconds())
		n, err := unix.Select(fd+1, &readable, nil, nil, &tv)
		if err == unix.EINTR {
			continue
		}
		return err == nil && n > 0
	}
}

func runFZF(currentPath string, oldState *term.State) (string, error) {
	fd := int(os.Stdin.Fd())
	if err := term.Restore(fd, oldState); err != nil {
		return currentPath, err
	}
	fmt.Print(ansi.ShowCursor)

	if _, err := exec.LookPath("fzf"); err != nil {
		_, _ = term.MakeRaw(fd)
		fmt.Print(ansi.HideCursor)
		return currentPath, fmt.Errorf("fzf not found in PATH")
	}

	cmd := exec.Command("bash", "-c",
		`${FZF_ALT_C_COMMAND:-fd --type d --absolute-path . 2>/dev/null || find . -type d -print} | fzf --prompt="dir> " --height=100%`)
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	var out bytes.Buffer
	cmd.Stdout = &out

	err := cmd.Run()

	_, _ = term.MakeRaw(fd)
	fmt.Print(ansi.HideCursor)

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code := exitErr.ExitCode()
			if code == 1 || code == 130 {
				// No selection (1) or cancelled (130): keep current path.
				return currentPath, nil
			}
		}
		return currentPath, err
	}

	selected := strings.TrimSpace(out.String())
	if selected == "" {
		return currentPath, nil
	}

	abs, err := filepath.Abs(selected)
	if err != nil {
		return selected, nil
	}
	return abs, nil
}
