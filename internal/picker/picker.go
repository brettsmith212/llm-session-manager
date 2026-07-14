package picker

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"llm-session-manager/internal/agent"
	"llm-session-manager/internal/ansi"
	"llm-session-manager/internal/sessions"
	"llm-session-manager/internal/tmux"
	"llm-session-manager/internal/types"
)

const (
	windowName            = "llm-picker"
	pickerSelectionOption = "@llm_picker_selection"
	previewDelay          = 75 * time.Millisecond
	gitRefreshInterval    = 4 * time.Second
)

// Run starts the ANSI session picker.
func Run() error {
	prefix := tmux.GetGlobalOption("@llm_session_prefix", "llm-")
	parent := tmux.GetGlobalOption("@llm_parent", "")

	sess := sessions.GetAllSessions(prefix)
	_ = sessions.PublishWaitingStatus(sess)
	p := &picker{
		prefix:        prefix,
		parent:        parent,
		sessions:      sess,
		query:         "",
		selectedIndex: 0,
		isSearching:   false,
		popupClosed:   make(chan struct{}, 1),
		gitUpdated:    make(chan map[string]gitInfo, 1),
		gitByPath:     make(map[string]gitInfo),
	}
	p.previewTimer = time.NewTimer(time.Hour)
	if !p.previewTimer.Stop() {
		<-p.previewTimer.C
	}
	defer p.previewTimer.Stop()

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("failed to set raw mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	fmt.Print(ansi.HideCursor + ansi.BracketedPasteOn)
	defer fmt.Print(ansi.BracketedPasteOff + ansi.ShowCursor)

	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	defer signal.Stop(resize)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	input := make(chan string, 16)
	go readInput(input)

	// Paint the interactive pane before doing any tmux work for the preview.
	p.render()
	if first := p.filtered(); len(first) > 0 {
		p.updatePreview(first[0])
	}
	p.requestGitRefresh()

	lastSnapshot := p.snapshot()

	for {
		select {
		case <-ticker.C:
			next := sessions.GetAllSessions(prefix)
			_ = sessions.PublishWaitingStatus(next)
			p.replaceSessions(next)
			selectedCreated := p.selectCreatedSession()
			snap := p.snapshot()
			if snap != lastSnapshot || selectedCreated {
				lastSnapshot = snap
				p.render()
				if len(p.filtered()) == 0 {
					p.cancelPreview()
				}
			}
			if visible := p.filtered(); !selectedCreated && p.selectedIndex < len(visible) && visible[p.selectedIndex].WindowID != p.previewWindowID {
				p.queuePreview(visible[p.selectedIndex])
			}
			p.requestGitRefresh()
		case <-p.popupClosed:
			p.replaceSessions(sessions.GetAllSessions(prefix))
			p.selectCreatedSession()
			lastSnapshot = p.snapshot()
			p.render()
			p.requestGitRefresh()
		case info := <-p.gitUpdated:
			p.gitByPath = info
			p.gitRefreshing = false
			p.lastGitRefresh = time.Now()
			p.render()
		case <-resize:
			p.render()
		case <-p.previewTimer.C:
			p.flushPreview()
		case keys := <-input:
			if done := p.handleKeys(keys); done {
				return nil
			}
		}
	}
}

type picker struct {
	prefix            string
	parent            string
	sessions          []types.Session
	query             string
	selectedIndex     int
	isSearching       bool
	searchStart       string
	searchIndex       int
	isPasting         bool
	isEditingLabel    bool
	editLabelID       string
	editLabelValue    string
	editLabelCursor   int
	activateErr       string // set when activateSession fails to reach the origin window
	confirmStopID     string
	confirmLabel      string
	notice            string
	noticeError       bool
	popupClosed       chan struct{}
	previewTimer      *time.Timer
	pendingPreview    types.Session
	hasPendingPreview bool
	previewWindowID   string
	gitUpdated        chan map[string]gitInfo
	gitByPath         map[string]gitInfo
	gitRefreshing     bool
	lastGitRefresh    time.Time
}

func (p *picker) filtered() []types.Session {
	terms := strings.Fields(strings.ToLower(p.query))
	out := make([]types.Session, 0, len(p.sessions))
	for _, s := range p.sessions {
		_, stateLabel, _ := statePresentation(sessionState(s))
		branch := ""
		if info, ok := p.gitByPath[gitPathKey(s.Path)]; ok {
			branch = info.branch
		}
		haystack := strings.ToLower(fmt.Sprintf("%s %s %s %s %s %s #%d",
			s.Path, s.Name, s.WindowName, s.Label, branch, stateLabel, s.WindowIndex))
		matches := true
		for _, term := range terms {
			if !strings.Contains(haystack, term) {
				matches = false
				break
			}
		}
		if matches {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		leftRank := stateRank(sessionState(out[i]))
		rightRank := stateRank(sessionState(out[j]))
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		leftPath := strings.ToLower(filepath.Clean(out[i].Path))
		rightPath := strings.ToLower(filepath.Clean(out[j].Path))
		if leftPath != rightPath {
			return leftPath < rightPath
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].WindowIndex < out[j].WindowIndex
	})
	return out
}

func stateRank(state types.State) int {
	switch state {
	case types.Waiting:
		return 0
	case types.Idle:
		return 2
	default:
		return 1
	}
}

func sessionState(session types.Session) types.State {
	if types.IsState(string(session.DisplayState)) {
		return session.DisplayState
	}
	return sessions.EffectiveState(session)
}

func (p *picker) selectedWindowID() string {
	list := p.filtered()
	if p.selectedIndex < 0 || p.selectedIndex >= len(list) {
		return ""
	}
	return list[p.selectedIndex].WindowID
}

func (p *picker) replaceSessions(next []types.Session) {
	selectedID := p.selectedWindowID()
	for i := range next {
		if !types.IsState(string(next[i].DisplayState)) {
			next[i].DisplayState = sessions.EffectiveState(next[i])
		}
	}
	p.sessions = next
	list := p.filtered()
	if selectedID != "" {
		for i, session := range list {
			if session.WindowID == selectedID {
				p.selectedIndex = i
				return
			}
		}
	}
	p.selectedIndex = max(0, min(p.selectedIndex, len(list)-1))
}

func (p *picker) requestGitRefresh() {
	if p.gitUpdated == nil || p.gitRefreshing || time.Since(p.lastGitRefresh) < gitRefreshInterval {
		return
	}
	seen := make(map[string]bool)
	paths := make([]string, 0, len(p.sessions))
	for _, session := range p.sessions {
		key := gitPathKey(session.Path)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		paths = append(paths, session.Path)
	}
	p.gitRefreshing = true
	go func() {
		p.gitUpdated <- collectGitInfo(paths)
	}()
}

func (p *picker) sharedPathCounts() map[string]int {
	counts := make(map[string]int)
	for _, session := range p.sessions {
		if key := gitPathKey(session.Path); key != "" {
			counts[key]++
		}
	}
	return counts
}

// formatBadge renders an agent name as colored text (no brackets, no
// background) — just the agent name in its assigned color. Used for the
// per-row agent indicator.
func formatBadge(agentName string, color ansi.RGB) string {
	if agentName == "" {
		agentName = "?"
	}
	return ansi.Foreground(color) + agentName + ansi.Reset
}

// truncateVisible truncates a plain string (no ansi) so the total visible
// width of prefix+str+suffix fits maxVisible cells. Truncation appends "…"
// to str to signal the cut.
func truncateVisible(str, prefix, suffix string, maxVisible int) string {
	avail := maxVisible - utf8.RuneCountInString(prefix) - utf8.RuneCountInString(suffix)
	runes := []rune(str)
	if avail >= len(runes) {
		return str
	}
	if avail <= 0 {
		return ""
	}
	if avail == 1 {
		return "…"
	}
	return string(runes[:avail-1]) + "…"
}

func (p *picker) snapshot() string {
	parts := make([]string, len(p.sessions))
	for i, s := range p.sessions {
		parts[i] = fmt.Sprintf("%s:%s:%d:%s:%s:%d:%s:%s:%s", s.Name, s.WindowID, s.WindowIndex, s.State, sessionState(s), s.StateAt, s.Path, s.WindowName, s.Label)
	}
	return strings.Join(parts, "|")
}

func (p *picker) render() {
	list := p.filtered()
	cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		cols, rows = 80, 24
	}
	cols = max(1, cols)
	rows = max(1, rows)

	if p.selectedIndex >= len(list) {
		p.selectedIndex = max(0, len(list)-1)
	}

	frame := newScreenFrame(cols, rows)
	position := 0
	if len(list) > 0 {
		position = p.selectedIndex + 1
	}
	waiting := 0
	for _, session := range p.sessions {
		if sessionState(session) == types.Waiting {
			waiting++
		}
	}

	header := fmt.Sprintf(" %s%s◆ Agents%s  %s%d/%d%s",
		ansi.Foreground(ansi.Blue), ansi.Bold, ansi.Reset,
		ansi.Foreground(ansi.Overlay2), position, len(list), ansi.Reset)
	if waiting > 0 {
		header += fmt.Sprintf("  %s◆ %d needs you%s", ansi.Foreground(ansi.Yellow), waiting, ansi.Reset)
	}
	active := agent.Active()
	if active == "" {
		header += fmt.Sprintf("  %sagent %snot configured%s", ansi.Foreground(ansi.Overlay0), ansi.Foreground(ansi.Red), ansi.Reset)
	} else {
		header += fmt.Sprintf("  %sagent%s %s%s%s %s▾%s",
			ansi.Foreground(ansi.Overlay0), ansi.Reset,
			ansi.Foreground(agent.BadgeColor(active)), active, ansi.Reset,
			ansi.Foreground(ansi.Surface2), ansi.Reset)
	}
	frame.lineBg(1, header, ansi.Surface0)

	if p.isEditingLabel {
		before := p.editLabelValue[:p.editLabelCursor]
		after := p.editLabelValue[p.editLabelCursor:]
		frame.line(2, fmt.Sprintf("  %stask:%s %s%s%s_%s%s  %senter save · esc cancel · ^u clear%s",
			ansi.Foreground(ansi.Overlay0), ansi.Reset,
			ansi.Foreground(ansi.Text), before, ansi.Foreground(ansi.Blue),
			ansi.Foreground(ansi.Text), after,
			ansi.Foreground(ansi.Overlay0), ansi.Reset))
	} else if p.isSearching {
		frame.line(2, fmt.Sprintf("  %s/%s%s%s%s_%s  %sesc cancel · enter keep%s",
			ansi.Foreground(ansi.Overlay0), ansi.Reset,
			ansi.Foreground(ansi.Text), p.query,
			ansi.Foreground(ansi.Blue), ansi.Reset,
			ansi.Foreground(ansi.Overlay0), ansi.Reset))
	} else if p.query != "" {
		frame.line(2, fmt.Sprintf("  %sfilter:%s %s%s%s  %sesc clear · / replace%s",
			ansi.Foreground(ansi.Overlay0), ansi.Reset,
			ansi.Foreground(ansi.Text), p.query, ansi.Reset,
			ansi.Foreground(ansi.Overlay0), ansi.Reset))
	} else {
		frame.line(2, fmt.Sprintf("  %s↑↓%s %snav · %s⏎%s %slive · %so%s %spopup · %sr%s %sreview · %s/%s %sfind%s",
			ansi.Foreground(ansi.Surface2), ansi.Reset, ansi.Foreground(ansi.Overlay0),
			ansi.Foreground(ansi.Surface2), ansi.Reset, ansi.Foreground(ansi.Overlay0),
			ansi.Foreground(ansi.Surface2), ansi.Reset, ansi.Foreground(ansi.Overlay0),
			ansi.Foreground(ansi.Surface2), ansi.Reset, ansi.Foreground(ansi.Overlay0),
			ansi.Foreground(ansi.Surface2), ansi.Reset, ansi.Foreground(ansi.Overlay0), ansi.Reset))
	}
	frame.line(3, fmt.Sprintf("  %s%s%s", ansi.Foreground(ansi.Surface0), strings.Repeat("─", max(0, cols-4)), ansi.Reset))

	row := 4
	if p.confirmStopID != "" {
		const confirmationSuffix = "?  ^x confirm · esc cancel"
		labelWidth := max(1, cols-utf8.RuneCountInString("  stop "+confirmationSuffix))
		label := truncateVisible(p.confirmLabel, "", "", labelWidth)
		frame.line(row, fmt.Sprintf("  %sstop %s%s%s",
			ansi.Foreground(ansi.Yellow), label, confirmationSuffix, ansi.Reset))
		row++
	} else if p.activateErr != "" {
		frame.line(row, fmt.Sprintf("  %scouldn't switch to parent: %s%s",
			ansi.Foreground(ansi.Red), p.activateErr, ansi.Reset))
		row++
	} else if p.notice != "" {
		color := ansi.Green
		if p.noticeError {
			color = ansi.Red
		}
		frame.line(row, fmt.Sprintf("  %s%s%s", ansi.Foreground(color), p.notice, ansi.Reset))
		row++
	}

	footerRow := rows
	if rows >= 6 {
		footer := "  a add · e label · n needs you · s agent · ^x stop · q close"
		if cols < 60 {
			footer = "  a add · e label · n next · ^x stop · q"
		}
		frame.line(footerRow, fmt.Sprintf("%s%s%s", ansi.Foreground(ansi.Overlay0), footer, ansi.Reset))
	} else {
		footerRow = rows + 1
	}

	availableRows := max(0, footerRow-row)
	listRows := p.buildListRows(list)
	visibleRows := visibleListRows(listRows, p.selectedIndex, availableRows)
	if len(visibleRows) == 0 {
		if len(list) == 0 {
			if p.query != "" {
				frame.line(row, fmt.Sprintf("  %sno matches · %sesc%s clears the filter%s",
					ansi.Foreground(ansi.Overlay0), ansi.Foreground(ansi.Blue), ansi.Foreground(ansi.Overlay0), ansi.Reset))
			} else {
				frame.line(row, fmt.Sprintf("  %sno sessions · press %sa%s to create one%s",
					ansi.Foreground(ansi.Overlay0), ansi.Foreground(ansi.Blue), ansi.Foreground(ansi.Overlay0), ansi.Reset))
			}
		}
		frame.flush()
		return
	}

	for _, listRow := range visibleRows {
		if row >= footerRow {
			break
		}
		switch listRow.kind {
		case listRowSection:
			text := "  ── " + listRow.text + " "
			text += strings.Repeat("─", max(0, cols-utf8.RuneCountInString(text)-2))
			frame.line(row, fmt.Sprintf("%s%s%s", ansi.Foreground(listRow.color), text, ansi.Reset))
			row++
		case listRowProject:
			frame.line(row, fmt.Sprintf("  %s%s%s", ansi.Foreground(ansi.Blue), listRow.text, ansi.Reset))
			row++
		case listRowGit:
			frame.line(row, fmt.Sprintf("  %s%s%s", ansi.Foreground(ansi.Overlay0), listRow.text, ansi.Reset))
			row++
		case listRowWarning:
			frame.line(row, fmt.Sprintf("  %s⚠ %s%s", ansi.Foreground(ansi.Yellow), listRow.text, ansi.Reset))
			row++
		case listRowSession:
			row = drawItem(frame, list[listRow.sessionIndex], cols, listRow.sessionIndex == p.selectedIndex, row, listRow.isLast)
		}
	}
	frame.flush()
}

type listRowKind int

const (
	listRowSection listRowKind = iota
	listRowProject
	listRowGit
	listRowWarning
	listRowSession
)

type pickerListRow struct {
	kind         listRowKind
	text         string
	color        ansi.RGB
	sessionIndex int
	isLast       bool
}

func (p *picker) buildListRows(list []types.Session) []pickerListRow {
	if len(list) == 0 {
		return nil
	}
	stateCounts := [3]int{}
	for _, session := range list {
		stateCounts[stateRank(sessionState(session))]++
	}
	shared := p.sharedPathCounts()
	rows := make([]pickerListRow, 0, len(list)*2)
	previousRank := -1
	previousProject := ""
	for i, session := range list {
		rank := stateRank(sessionState(session))
		if rank != previousRank {
			name := "ACTIVE"
			color := ansi.Blue
			if rank == 0 {
				name = "NEEDS YOU"
				color = ansi.Yellow
			} else if rank == 2 {
				name = "IDLE"
				color = ansi.Overlay0
			}
			rows = append(rows, pickerListRow{kind: listRowSection, text: fmt.Sprintf("%s · %d", name, stateCounts[rank]), color: color})
			previousRank = rank
			previousProject = ""
		}

		projectKey := gitPathKey(session.Path)
		if projectKey == "" {
			projectKey = session.Name
		}
		if projectKey != previousProject {
			path := sessions.FormatPath(session.Path)
			if path == "" {
				path = session.Name
			}
			rows = append(rows, pickerListRow{kind: listRowProject, text: path})
			if git := formatGitInfo(p.gitByPath[projectKey]); git != "" {
				rows = append(rows, pickerListRow{kind: listRowGit, text: git})
			}
			if count := shared[projectKey]; count > 1 {
				rows = append(rows, pickerListRow{kind: listRowWarning, text: fmt.Sprintf("%d agents share this worktree", count)})
			}
			previousProject = projectKey
		}

		nextSameProject := false
		if i+1 < len(list) {
			next := list[i+1]
			nextKey := gitPathKey(next.Path)
			if nextKey == "" {
				nextKey = next.Name
			}
			nextSameProject = stateRank(sessionState(next)) == rank && nextKey == projectKey
		}
		rows = append(rows, pickerListRow{kind: listRowSession, sessionIndex: i, isLast: !nextSameProject})
	}
	return rows
}

func visibleListRows(rows []pickerListRow, selected, maxRows int) []pickerListRow {
	if len(rows) == 0 || maxRows < 1 {
		return nil
	}
	selectedRow := 0
	for i, row := range rows {
		if row.kind == listRowSession && row.sessionIndex == selected {
			selectedRow = i
			break
		}
	}
	start := max(0, selectedRow-maxRows/2)
	end := min(len(rows), start+maxRows)
	start = max(0, end-maxRows)
	return rows[start:end]
}

func drawItem(frame *screenFrame, session types.Session, cols int, selected bool, row int, isLastInGroup bool) int {
	state := sessionState(session)
	stateSymbol, stateLabel, stateColor := statePresentation(state)
	ago := sessions.FormatAgo(session.StateAt)
	agentName := session.WindowName
	if agentName == "" {
		agentName = "?"
	}
	windowNumber := fmt.Sprintf(" #%d", session.WindowIndex)

	connector := "├─"
	if isLastInGroup {
		connector = "└─"
	}
	if selected {
		connector = "▸ "
	}

	statePadded := fmt.Sprintf("%-9s", stateLabel)
	const prefixWidth = 17 // indent + connector + symbol + padded state + separator
	agentWidth := min(16, max(0, cols-prefixWidth-utf8.RuneCountInString(windowNumber)-2))
	agentName = truncateVisible(agentName, "", "", agentWidth)
	badge := ""
	if agentName != "" {
		badge = formatBadge(agentName, agent.BadgeColor(session.WindowName))
	}
	suffixWidth := utf8.RuneCountInString(agentName) + utf8.RuneCountInString(windowNumber)
	if agentName != "" {
		suffixWidth += 2
	}
	detailWidth := max(0, cols-prefixWidth-suffixWidth)
	detail := strings.TrimSpace(session.Label)
	if detail == "" {
		detail = ago
	} else if detailWidth >= utf8.RuneCountInString(detail)+utf8.RuneCountInString(ago)+3 {
		detail += "  " + ago
	}
	detail = truncateVisible(detail, "", "", detailWidth)
	padding := strings.Repeat(" ", max(0, detailWidth-utf8.RuneCountInString(detail)))
	separator := ""
	if agentName != "" {
		separator = "  "
	}

	if selected {
		accent := ansi.Foreground(ansi.Blue)
		dot := ansi.Foreground(stateColor)
		txt := ansi.Foreground(ansi.Text)
		muted := ansi.Foreground(ansi.Overlay2)

		line1 := "  " +
			accent + connector + " " +
			dot + stateSymbol + " " + txt + statePadded + " " +
			txt + detail + muted + padding + separator +
			badge + ansi.Background(ansi.Surface0) + muted + windowNumber

		frame.lineBg(row, line1, ansi.Surface0)
	} else {
		tree := ansi.Foreground(ansi.Blue)
		dot := ansi.Foreground(stateColor)
		txt := ansi.Foreground(ansi.Subtext0)
		muted := ansi.Foreground(ansi.Overlay0)

		line1 := "  " +
			tree + connector + ansi.Reset + " " +
			dot + stateSymbol + " " + txt + statePadded + " " + ansi.Reset +
			txt + detail + muted + padding + separator + ansi.Reset +
			badge + muted + windowNumber + ansi.Reset

		frame.line(row, line1)
	}

	return row + 1
}

func statePresentation(state types.State) (symbol, label string, color ansi.RGB) {
	switch state {
	case types.Waiting:
		return "◆", "needs you", ansi.Yellow
	case types.Working:
		return "●", "working", ansi.Blue
	case types.Idle:
		return "·", "idle", ansi.Overlay0
	default:
		return "·", "starting", ansi.Overlay0
	}
}

type screenLine struct {
	content       string
	background    ansi.RGB
	hasBackground bool
}

type screenFrame struct {
	cols  int
	rows  int
	lines []screenLine
}

func newScreenFrame(cols, rows int) *screenFrame {
	return &screenFrame{cols: cols, rows: rows, lines: make([]screenLine, rows)}
}

func (f *screenFrame) line(row int, content string) {
	if row < 1 || row > f.rows {
		return
	}
	f.lines[row-1] = screenLine{content: content}
}

func (f *screenFrame) lineBg(row int, content string, background ansi.RGB) {
	if row < 1 || row > f.rows {
		return
	}
	f.lines[row-1] = screenLine{content: content, background: background, hasBackground: true}
}

func (f *screenFrame) flush() {
	var out strings.Builder
	out.Grow(f.rows * (f.cols + 24))
	for row, line := range f.lines {
		out.WriteString(ansi.CursorPos(row+1, 1))
		out.WriteString(ansi.ClearLine())
		content := line.content
		if line.hasBackground {
			background := ansi.Background(line.background)
			out.WriteString(background)
			out.WriteString(strings.Repeat(" ", f.cols))
			out.WriteString(ansi.Reset)
			out.WriteString(ansi.CursorPos(row+1, 1))
			out.WriteString(background)
			content = strings.ReplaceAll(content, ansi.Reset, ansi.Reset+background)
		}
		out.WriteString(fitANSI(content, f.cols))
		out.WriteString(ansi.Reset)
	}
	_, _ = os.Stdout.WriteString(out.String())
}

func fitANSI(value string, maxWidth int) string {
	if maxWidth <= 0 || value == "" {
		return ""
	}
	if ansiWidth(value) <= maxWidth {
		return value
	}

	var out strings.Builder
	width := 0
	target := maxWidth - 1
	for i := 0; i < len(value) && width < target; {
		if value[i] == '\x1b' && i+1 < len(value) && value[i+1] == '[' {
			end := i + 2
			for end < len(value) {
				end++
				if value[end-1] >= 0x40 && value[end-1] <= 0x7e {
					break
				}
			}
			out.WriteString(value[i:end])
			i = end
			continue
		}
		_, size := utf8.DecodeRuneInString(value[i:])
		out.WriteString(value[i : i+size])
		i += size
		width++
	}
	out.WriteString("…")
	return out.String()
}

func ansiWidth(value string) int {
	width := 0
	for i := 0; i < len(value); {
		if value[i] == '\x1b' && i+1 < len(value) && value[i+1] == '[' {
			i += 2
			for i < len(value) {
				final := value[i] >= 0x40 && value[i] <= 0x7e
				i++
				if final {
					break
				}
			}
			continue
		}
		_, size := utf8.DecodeRuneInString(value[i:])
		i += size
		width++
	}
	return width
}

func (p *picker) queuePreview(session types.Session) {
	if session.WindowID == p.previewWindowID {
		p.cancelPreview()
		return
	}
	p.pendingPreview = session
	p.hasPendingPreview = true
	if !p.previewTimer.Stop() {
		select {
		case <-p.previewTimer.C:
		default:
		}
	}
	p.previewTimer.Reset(previewDelay)
}

func (p *picker) cancelPreview() {
	p.hasPendingPreview = false
	if !p.previewTimer.Stop() {
		select {
		case <-p.previewTimer.C:
		default:
		}
	}
}

func (p *picker) flushPreview() {
	if !p.hasPendingPreview {
		return
	}
	session := p.pendingPreview
	p.hasPendingPreview = false
	p.updatePreview(session)
}

func (p *picker) renderSelection() {
	p.render()
	list := p.filtered()
	if p.selectedIndex >= len(list) {
		p.cancelPreview()
		return
	}
	p.queuePreview(list[p.selectedIndex])
}

func (p *picker) updatePreview(session types.Session) {
	cmd := tmux.AttachCommand(session.Name, true) + " \\; select-window -t " + tmux.ShellQuote(session.WindowID)
	result := tmux.RunRaw([]string{"list-panes", "-t", windowName, "-F", "#{pane_index}"})
	if result.ExitCode != 0 {
		return
	}
	panes := strings.Fields(result.Stdout)
	updated := false
	if len(panes) > 1 {
		updated = tmux.RunRaw([]string{"respawn-pane", "-k", "-t", ":" + windowName + ".1", cmd}).ExitCode == 0
	} else {
		_, err := tmux.Run([]string{"split-window", "-d", "-h", "-l", "67%", "-t", ":" + windowName + ".0", cmd})
		updated = err == nil
	}
	if !updated {
		return
	}
	p.setPreviewTitle(session)
	p.previewWindowID = session.WindowID
}

func (p *picker) setPreviewTitle(session types.Session) {
	// Refresh the live pane title with the name + window index so its border
	// always tracks what is shown there and advertises the outer-tmux escape.
	title := fmt.Sprintf("LIVE AGENT · %s", projectName(session))
	if session.Label != "" {
		title += " · " + session.Label
	}
	title += fmt.Sprintf(" · %s #%d · prefix u returns", session.WindowName, session.WindowIndex)
	_ = tmux.RunRaw([]string{"select-pane", "-T",
		title,
		"-t", ":" + windowName + ".1"})
}

func (p *picker) focusPreview() {
	list := p.filtered()
	if p.selectedIndex >= len(list) {
		return
	}
	// Navigation updates are debounced, so flush the final selection before
	// handing input to the live pane. Otherwise a quick j+Enter could focus the
	// previously selected agent for a fraction of a second.
	p.flushPreview()
	_ = tmux.RunRaw([]string{"select-pane", "-t", ":" + windowName + ".1"})
}

func (p *picker) changeSelection(delta int) {
	p.activateErr = ""
	p.confirmStopID = ""
	p.confirmLabel = ""
	p.notice = ""
	list := p.filtered()
	p.selectedIndex = max(0, min(len(list)-1, p.selectedIndex+delta))
	p.renderSelection()
}

func (p *picker) selectNextWaiting() {
	p.confirmStopID = ""
	p.confirmLabel = ""
	currentID := ""
	filtered := p.filtered()
	if p.selectedIndex < len(filtered) {
		currentID = filtered[p.selectedIndex].WindowID
	}

	hasWaiting := false
	for _, session := range p.sessions {
		if sessionState(session) == types.Waiting {
			hasWaiting = true
			break
		}
	}
	if !hasWaiting {
		p.notice = "no sessions need attention"
		p.noticeError = false
		p.render()
		return
	}

	// Attention navigation is intentionally global. A committed project or
	// agent filter should never hide a session that needs the user.
	p.query = ""
	list := p.filtered()
	start := -1
	for i, session := range list {
		if session.WindowID == currentID {
			start = i
			break
		}
	}
	p.selectedIndex = max(0, start)
	for offset := 1; offset <= len(list); offset++ {
		index := (start + offset) % len(list)
		if sessionState(list[index]) != types.Waiting {
			continue
		}
		p.selectedIndex = index
		p.notice = ""
		p.activateErr = ""
		p.renderSelection()
		return
	}
	p.notice = "no sessions need attention"
	p.noticeError = false
	p.render()
}

func projectName(session types.Session) string {
	if session.Path != "" {
		name := filepath.Base(filepath.Clean(session.Path))
		if name != "" && name != "." {
			return name
		}
	}
	return session.Name
}

func sessionLabel(session types.Session) string {
	agentName := session.WindowName
	if agentName == "" {
		agentName = "agent"
	}
	if session.Label != "" {
		return fmt.Sprintf("%s · %s · %s #%d", projectName(session), session.Label, agentName, session.WindowIndex)
	}
	return fmt.Sprintf("%s · %s #%d", projectName(session), agentName, session.WindowIndex)
}

// selectCreatedSession consumes the window ID left by the create prompt and
// moves the picker and live preview to that exact new session.
func (p *picker) selectCreatedSession() bool {
	windowID := tmux.GetWindowOption(windowName, pickerSelectionOption)
	if windowID == "" {
		return false
	}
	var created types.Session
	found := false
	for _, session := range p.sessions {
		if session.WindowID == windowID {
			created = session
			found = true
			break
		}
	}
	if !found {
		// Add sets this option only after marking the new window as managed, so
		// a missing window is stale (for example, the agent exited immediately).
		_ = tmux.SetWindowOption(windowName, pickerSelectionOption, "")
		return false
	}

	p.query = ""
	for i, session := range p.filtered() {
		if session.WindowID != created.WindowID {
			continue
		}
		p.selectedIndex = i
		p.activateErr = ""
		p.confirmStopID = ""
		p.confirmLabel = ""
		p.notice = ""
		_ = tmux.SetWindowOption(windowName, pickerSelectionOption, "")
		p.queuePreview(session)
		p.beginLabelEdit(session)
		return true
	}
	return false
}

func (p *picker) sessionOrigin(session types.Session) string {
	origin := session.Origin
	if origin == "" {
		origin = tmux.GetSessionOption(session.Name, "@llm_origin")
	}
	if origin == "" {
		origin = tmux.GetParentSession()
	}
	return origin
}

func (p *picker) switchToProject(session types.Session) bool {
	parent := tmux.GetGlobalOption("@llm_parent", p.parent)
	if parent == "" {
		p.activateErr = "no parent tmux client is available"
		p.render()
		return false
	}
	if session.Path == "" {
		p.activateErr = "no project path is available"
		p.render()
		return false
	}
	origin := p.sessionOrigin(session)
	if origin == "" || origin == session.Name {
		p.activateErr = "no project origin is available"
		p.render()
		return false
	}
	if strings.HasPrefix(origin, "@") {
		if resolved, err := tmux.DisplayMessage("#{session_name}", origin); err == nil {
			origin = resolved
		}
	}
	if origin == "" || !tmux.HasSession(origin) {
		p.activateErr = "project origin is no longer available"
		p.render()
		return false
	}
	if err := tmux.EnsureOriginWindow(origin, session.Path, parent); err != nil {
		p.activateErr = err.Error()
		p.render()
		return false
	}
	p.activateErr = ""
	return true
}

func (p *picker) reviewProject() {
	list := p.filtered()
	if p.selectedIndex >= len(list) {
		return
	}
	p.switchToProject(list[p.selectedIndex])
}

// activateSession switches the parent client to the selected project's
// review window and opens the agent over it while the Control Room remains
// alive in the background.
func (p *picker) activateSession() {
	list := p.filtered()
	if p.selectedIndex >= len(list) {
		return
	}
	session := list[p.selectedIndex]
	if !p.switchToProject(session) {
		return
	}
	width := tmux.GetGlobalOption("@llm_popup_width", "90%")
	height := tmux.GetGlobalOption("@llm_popup_height", "90%")
	parent := tmux.GetGlobalOption("@llm_parent", p.parent)
	attachCmd := tmux.AttachCommand(session.Name, false) + " \\; select-window -t " + tmux.ShellQuote(session.WindowID)
	cmd := exec.Command("tmux", "display-popup",
		"-c", parent,
		"-w", width,
		"-h", height,
		"-E",
		attachCmd)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		p.activateErr = "couldn't open popup: " + err.Error()
		p.render()
		return
	}
	go func() { _ = cmd.Wait() }()
}

func (p *picker) killSelected() {
	list := p.filtered()
	if p.selectedIndex >= len(list) {
		return
	}
	session := list[p.selectedIndex]
	label := sessionLabel(session)

	if sessionState(session) != types.Idle && p.confirmStopID != session.WindowID {
		p.confirmStopID = session.WindowID
		p.confirmLabel = label
		p.notice = ""
		p.activateErr = ""
		p.render()
		return
	}
	p.confirmStopID = ""
	p.confirmLabel = ""

	if err := tmux.KillWindow(session.Name + ":" + session.WindowID); err != nil {
		p.notice = fmt.Sprintf("couldn't stop %s: %s", label, err)
		p.noticeError = true
		p.render()
		return
	}

	// If the parent session has no agent windows left, clean it up too.
	remaining := tmux.RunRaw([]string{"list-windows", "-t", session.Name, "-F", "#{@llm_agent}"})
	hasAgent := false
	if remaining.ExitCode == 0 && remaining.Stdout != "" {
		for _, line := range strings.Split(remaining.Stdout, "\n") {
			if strings.TrimSpace(line) != "" {
				hasAgent = true
				break
			}
		}
	}
	if !hasAgent {
		_ = tmux.KillSession(session.Name)
	}

	p.replaceSessions(sessions.GetAllSessions(p.prefix))
	_ = sessions.PublishWaitingStatus(p.sessions)
	p.notice = "stopped " + label
	p.noticeError = false
	p.activateErr = ""
	next := p.filtered()
	if p.selectedIndex >= len(next) {
		p.selectedIndex = max(0, len(next)-1)
	}
	p.renderSelection()
}

func (p *picker) beginLabelEdit(session types.Session) {
	p.isEditingLabel = true
	p.editLabelID = session.WindowID
	p.editLabelValue = session.Label
	p.editLabelCursor = len(session.Label)
	p.activateErr = ""
	p.confirmStopID = ""
	p.confirmLabel = ""
	p.notice = ""
}

func (p *picker) editSelectedLabel() {
	list := p.filtered()
	if p.selectedIndex >= len(list) {
		return
	}
	p.beginLabelEdit(list[p.selectedIndex])
	p.render()
}

func (p *picker) cancelLabelEdit() {
	p.isEditingLabel = false
	p.editLabelID = ""
	p.editLabelValue = ""
	p.editLabelCursor = 0
	p.render()
}

func (p *picker) saveLabelEdit() {
	label := strings.TrimSpace(p.editLabelValue)
	if err := tmux.SetWindowOption(p.editLabelID, "@llm_label", label); err != nil {
		p.notice = "couldn't save task label: " + err.Error()
		p.noticeError = true
		p.render()
		return
	}
	selectedID := p.editLabelID
	var updated types.Session
	for i := range p.sessions {
		if p.sessions[i].WindowID == selectedID {
			p.sessions[i].Label = label
			updated = p.sessions[i]
			break
		}
	}
	p.isEditingLabel = false
	p.editLabelID = ""
	p.editLabelValue = ""
	p.editLabelCursor = 0
	p.notice = "task label updated"
	if label == "" {
		p.notice = "task label cleared"
	}
	p.noticeError = false

	list := p.filtered()
	selected := false
	for i, session := range list {
		if session.WindowID == selectedID {
			p.selectedIndex = i
			selected = true
			break
		}
	}
	if !selected {
		p.query = ""
		for i, session := range p.filtered() {
			if session.WindowID == selectedID {
				p.selectedIndex = i
				break
			}
		}
	}
	if updated.WindowID != "" && p.previewWindowID == updated.WindowID {
		p.setPreviewTitle(updated)
	}
	p.renderSelection()
}

func (p *picker) handleLabelKey(key string) {
	if len(key) == 0 {
		return
	}
	code := key[0]
	switch {
	case key == "\x1b":
		p.cancelLabelEdit()
		return
	case code == 13:
		p.saveLabelEdit()
		return
	case code == 21: // ctrl-u
		p.editLabelValue = ""
		p.editLabelCursor = 0
	case code == 127 || code == 8:
		if p.editLabelCursor > 0 {
			_, size := utf8.DecodeLastRuneInString(p.editLabelValue[:p.editLabelCursor])
			start := p.editLabelCursor - size
			p.editLabelValue = p.editLabelValue[:start] + p.editLabelValue[p.editLabelCursor:]
			p.editLabelCursor = start
		}
	case key == "\x1b[3~":
		if p.editLabelCursor < len(p.editLabelValue) {
			_, size := utf8.DecodeRuneInString(p.editLabelValue[p.editLabelCursor:])
			p.editLabelValue = p.editLabelValue[:p.editLabelCursor] + p.editLabelValue[p.editLabelCursor+size:]
		}
	case key == "\x1b[D":
		if p.editLabelCursor > 0 {
			_, size := utf8.DecodeLastRuneInString(p.editLabelValue[:p.editLabelCursor])
			p.editLabelCursor -= size
		}
	case key == "\x1b[C":
		if p.editLabelCursor < len(p.editLabelValue) {
			_, size := utf8.DecodeRuneInString(p.editLabelValue[p.editLabelCursor:])
			p.editLabelCursor += size
		}
	case key == "\x1b[H" || key == "\x1b[1~":
		p.editLabelCursor = 0
	case key == "\x1b[F" || key == "\x1b[4~":
		p.editLabelCursor = len(p.editLabelValue)
	case code >= 32 && code <= 126 && len(p.editLabelValue) < 120:
		p.editLabelValue = p.editLabelValue[:p.editLabelCursor] + key + p.editLabelValue[p.editLabelCursor:]
		p.editLabelCursor += len(key)
	}
	p.render()
}

// cycleAgent rotates the global @llm_active_agent to the next entry in
// the catalog and re-renders so the header reflects the new choice. Only
// affects new sessions — already-running sessions keep their agent.
func (p *picker) cycleAgent() {
	agent.Cycle()
	p.activateErr = ""
	p.confirmStopID = ""
	p.confirmLabel = ""
	p.notice = ""
	p.render()
}

func (p *picker) handleKeys(data string) (done bool) {
	keys := parseKeys(data)
	for _, key := range keys {
		switch key {
		case "\x1b[200~":
			p.isPasting = true
			continue
		case "\x1b[201~":
			p.isPasting = false
			continue
		}
		if p.isPasting && !p.isSearching && !p.isEditingLabel {
			continue
		}
		if p.isEditingLabel {
			p.handleLabelKey(key)
			continue
		}
		if p.isSearching {
			done := p.handleSearchKey(key)
			if done {
				return true
			}
			continue
		}

		switch key {
		case "\x1b[A", "k":
			p.changeSelection(-1)
		case "\x1b[B", "j":
			p.changeSelection(1)
		case "\r":
			p.focusPreview()
		case "o", "\x1b[13;2u", "\x1b[27;2;13~":
			p.activateSession()
		case "r":
			p.reviewProject()
		case "/":
			p.searchStart = p.query
			p.searchIndex = p.selectedIndex
			p.isSearching = true
			p.query = ""
			p.selectedIndex = 0
			p.confirmStopID = ""
			p.confirmLabel = ""
			p.notice = ""
			p.render()
		case "a":
			p.confirmStopID = ""
			p.confirmLabel = ""
			p.openCreatePopup()
		case "e":
			p.editSelectedLabel()
		case "n":
			p.selectNextWaiting()
		case "s":
			p.cycleAgent()
		case "\x18": // ^x
			p.killSelected()
		case "\x1b": // esc
			if p.confirmStopID != "" {
				p.confirmStopID = ""
				p.confirmLabel = ""
				p.render()
				continue
			}
			if p.query != "" {
				p.query = ""
				p.selectedIndex = 0
				p.notice = ""
				p.renderSelection()
				continue
			}
			_ = tmux.RunRaw([]string{"kill-window", "-t", windowName})
			return true
		case "\x03", "q": // ^c, q
			_ = tmux.RunRaw([]string{"kill-window", "-t", windowName})
			return true
		}
	}
	return false
}

func (p *picker) handleSearchKey(key string) (done bool) {
	if len(key) == 0 {
		return false
	}
	code := key[0]
	switch {
	case code == 27: // esc: cancel this search and restore its previous filter
		p.query = p.searchStart
		p.isSearching = false
		p.selectedIndex = p.searchIndex
		if list := p.filtered(); len(list) > 0 {
			p.selectedIndex = min(p.selectedIndex, len(list)-1)
		}
		p.renderSelection()
	case code == 13: // enter: keep the current filter
		p.isSearching = false
		p.render()
	case code == 127 || code == 8: // backspace
		if len(p.query) > 0 {
			p.query = p.query[:len(p.query)-1]
		}
		p.selectedIndex = 0
		p.renderSelection()
	case code >= 32 && code <= 126:
		p.query += key
		p.selectedIndex = 0
		p.renderSelection()
	}
	return false
}

func (p *picker) openCreatePopup() {
	list := p.filtered()
	defaultPath := ""
	if p.selectedIndex < len(list) {
		defaultPath = list[p.selectedIndex].Path
	}
	if defaultPath == "" {
		if cwd, err := os.Getwd(); err == nil {
			defaultPath = cwd
		}
	}

	width := tmux.GetGlobalOption("@llm_popup_width", "90%")
	height := "30%"

	cmd := exec.Command("tmux", "display-popup",
		"-w", width,
		"-h", height,
		"-E",
		binaryPath()+" prompt "+tmux.ShellQuote(defaultPath))
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return
	}
	go func() {
		_ = cmd.Wait()
		select {
		case p.popupClosed <- struct{}{}:
		default:
		}
	}()
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
	bytes := make(chan byte, 64)
	go func() {
		defer close(bytes)
		for {
			b, err := reader.ReadByte()
			if err != nil {
				return
			}
			bytes <- b
		}
	}()

	for b := range bytes {
		if b != '\x1b' {
			out <- string(b)
			continue
		}

		// A terminal sends Escape both as a standalone key and as the prefix
		// for CSI sequences. File deadlines are not reliable for tmux PTYs, so
		// use a byte channel and a short timer to distinguish the two without
		// ever blocking a standalone Escape indefinitely.
		var buf strings.Builder
		buf.WriteByte(b)
		timer := time.NewTimer(20 * time.Millisecond)
	escapeSequence:
		for {
			select {
			case next, ok := <-bytes:
				if !ok {
					break escapeSequence
				}
				buf.WriteByte(next)
				if buf.Len() == 2 && next != '[' {
					break escapeSequence
				}
				// CSI sequences end at 0x40-0x7e after params/intermediates.
				if next >= 0x40 && next <= 0x7e && buf.Len() > 2 {
					break escapeSequence
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(20 * time.Millisecond)
			case <-timer.C:
				break escapeSequence
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		out <- buf.String()
	}
}

func parseKeys(data string) []string {
	var tokens []string
	i := 0
	for i < len(data) {
		if data[i] == '\x1b' && i+1 < len(data) && data[i+1] == '[' {
			j := i + 2
			for j < len(data) && data[j] >= 0x30 && data[j] <= 0x3f {
				j++
			}
			for j < len(data) && data[j] >= 0x20 && data[j] <= 0x2f {
				j++
			}
			if j < len(data) && data[j] >= 0x40 && data[j] <= 0x7e {
				j++
			}
			tokens = append(tokens, data[i:j])
			i = j
		} else {
			tokens = append(tokens, string(data[i]))
			i++
		}
	}
	return tokens
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
