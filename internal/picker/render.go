package picker

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"llm-session-manager/internal/ansi"
	"llm-session-manager/internal/sessions"
	"llm-session-manager/internal/state"
	"llm-session-manager/internal/types"
)

// Control Room sections, in display order.
const (
	sectionPinned = iota
	sectionNeedsYou
	sectionActive
	sectionDone
	sectionIdle
	sectionCount
)

// stateExited marks an agent whose process exited with an error and whose
// pane tmux kept (remain-on-exit failed). Hooks can never report it.
const stateExited types.State = "exited"

// Layout of a session row: " ▌ ◆ " before the task, and the age column.
const (
	leadWidth = 5
	ageWidth  = 4
)

// Waits longer than these escalate the age color.
const (
	lateWait = 5 * time.Minute
	longWait = 15 * time.Minute
)

// palette returns the active theme. Pickers built without one (in tests)
// get the default.
func (p *picker) palette() ansi.Theme {
	if p.theme.Name == "" {
		return ansi.ThemeNamed("")
	}
	return p.theme
}

// presentation is how a session's state looks: its glyph, the word used for
// it in search, and its color.
func presentation(theme ansi.Theme, session types.Session) (glyph, label string, color ansi.RGB) {
	switch sessionState(session) {
	case stateExited:
		return "✕", "exited", theme.Error
	case types.Waiting:
		if session.ReasonKind == types.ReasonError {
			return "✕", "needs you error", theme.Error
		}
		return "◆", "needs you", theme.Attention
	case types.Working:
		return "●", "working", theme.Working
	case types.Idle:
		if session.Unseen {
			return "✓", "done", theme.Done
		}
		return "·", "idle", theme.Muted
	default:
		return "·", "starting", theme.Faint
	}
}

func sectionPresentation(theme ansi.Theme, section int) (name string, color ansi.RGB) {
	switch section {
	case sectionPinned:
		return "PINNED", theme.Pinned
	case sectionNeedsYou:
		return "NEEDS YOU", theme.Attention
	case sectionActive:
		return "ACTIVE", theme.Working
	case sectionDone:
		return "DONE", theme.Done
	default:
		return "IDLE", theme.Muted
	}
}

// cells builds one styled line while tracking its visible width.
type cells struct {
	out   strings.Builder
	width int
}

func (c *cells) add(style, text string) *cells {
	if text == "" {
		return c
	}
	c.out.WriteString(style)
	c.out.WriteString(text)
	c.out.WriteString(ansi.Reset)
	c.width += utf8.RuneCountInString(text)
	return c
}

func (c *cells) pad(n int) *cells {
	if n > 0 {
		c.out.WriteString(strings.Repeat(" ", n))
		c.width += n
	}
	return c
}

func (c *cells) segments(segs []segment) *cells {
	for _, s := range segs {
		c.add(s.style(), s.text)
	}
	return c
}

func (c *cells) String() string { return c.out.String() }

// joined places right at the end of a line cols wide, after left.
func joined(left, right *cells, cols int) string {
	left.pad(cols - left.width - right.width)
	return left.String() + right.String()
}

// segment is a run of text in one style.
type segment struct {
	text   string
	color  ansi.RGB
	bold   bool
	italic bool
}

func (s segment) style() string {
	style := ansi.Foreground(s.color)
	if s.bold {
		style += ansi.Bold
	}
	if s.italic {
		style += ansi.Italic
	}
	return style
}

func segmentsWidth(segs []segment) int {
	width := 0
	for _, s := range segs {
		width += utf8.RuneCountInString(s.text)
	}
	return width
}

func plain(segs []segment) string {
	var out strings.Builder
	for _, s := range segs {
		out.WriteString(s.text)
	}
	return out.String()
}

func color(c ansi.RGB) string  { return ansi.Foreground(c) }
func strong(c ansi.RGB) string { return ansi.Foreground(c) + ansi.Bold }
func soft(c ansi.RGB) string   { return ansi.Foreground(c) + ansi.Italic }

// ---------------------------------------------------------------- layout

func (p *picker) render() {
	theme := p.palette()
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
	frame.lineBg(1, p.headerLine(list, cols), theme.HeaderBg)
	agentWidth := agentColumnWidth(list)

	row := 3
	switch {
	case p.isEditingLabel:
		p.drawLabelEditor(frame)
		row = 5
	case p.isSearching:
		frame.line(2, joined(new(cells).pad(2).add(color(theme.Muted), "/ ").add(color(theme.Text), p.query).add(color(theme.Accent), "▏"),
			new(cells).add(color(theme.Faint), "esc cancel · ⏎ keep "), cols))
	case p.query != "":
		frame.line(2, joined(new(cells).pad(2).add(color(theme.Muted), "filter: ").add(color(theme.Text), p.query),
			new(cells).add(color(theme.Faint), "esc clear · / replace "), cols))
	default:
		frame.line(2, p.columnHeader(cols, agentWidth))
	}
	// The notice line is reserved even when empty, so the list does not jump
	// as notices come and go.
	if notice := p.noticeLine(cols); notice != "" {
		if strings.HasPrefix(p.notice, "created ") && p.confirmStopID == "" && p.confirmWorktree == "" && p.activateErr == "" {
			frame.lineBg(row, notice, theme.HeaderBg)
		} else {
			frame.line(row, notice)
		}
	}
	row++

	footerRow := rows
	if rows >= 6 {
		frame.line(footerRow, p.footerLine(list, cols))
	} else {
		footerRow = rows + 1
	}

	if p.showHelp {
		p.drawHelp(frame, row, footerRow, cols)
		frame.flush()
		return
	}

	listRows := p.buildListRows(list)
	visibleRows := visibleListRows(listRows, p.selectedIndex, max(0, footerRow-row))
	if len(visibleRows) == 0 {
		if len(list) == 0 {
			hint := new(cells).pad(2)
			if p.query != "" {
				hint.add(color(theme.Muted), "no matches · ").add(color(theme.Accent), "esc").add(color(theme.Muted), " clears the filter")
			} else {
				hint.add(color(theme.Muted), "no agents yet · ").add(color(theme.Accent), "a").add(color(theme.Muted), " starts one, ").
					add(color(theme.Accent), "A").add(color(theme.Muted), " starts one in a new worktree")
			}
			frame.line(row, hint.String())
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
			frame.line(row, p.sectionLine(listRow, cols))
		case listRowSpacer:
		case listRowCheckout:
			frame.line(row, p.checkoutLine(list[listRow.sessionIndex], listRow.working, cols))
		case listRowSession:
			p.drawSession(frame, row, list[listRow.sessionIndex], cols, listRow.sessionIndex == p.selectedIndex, agentWidth)
		case listRowDetail:
			p.drawDetail(frame, row, list[listRow.sessionIndex], cols, listRow.sessionIndex == p.selectedIndex)
		}
		row++
	}
	frame.flush()
}

func (p *picker) headerLine(list []types.Session, cols int) string {
	theme := p.palette()
	position := 0
	if len(list) > 0 {
		position = p.selectedIndex + 1
	}
	var waiting, working, done, failed int
	for _, session := range p.sessions {
		switch {
		case previewState(session) == "error":
			failed++
		case sessionState(session) == types.Waiting:
			waiting++
		case sessionState(session) == types.Working:
			working++
		case session.Unseen:
			done++
		}
	}
	left := new(cells).add(color(theme.Accent), " ▌ ").add(strong(theme.Text), "AGENTS").
		add(color(theme.Faint), fmt.Sprintf("  %d/%d", position, len(list)))
	counts := []struct {
		n     int
		glyph string
		color ansi.RGB
		bold  bool
	}{
		{waiting, "◆", theme.Attention, true},
		{failed, "✕", theme.Error, true},
		{working, "●", theme.Working, false},
		{done, "✓", theme.Done, false},
	}
	gap := "   "
	for _, count := range counts {
		if count.n == 0 {
			continue
		}
		style := color(count.color)
		if count.bold {
			style = strong(count.color)
		}
		left.add(style, fmt.Sprintf("%s%s %d", gap, count.glyph, count.n))
		gap = "  "
	}
	right := new(cells)
	if p.activeAgent == "" {
		right.add(color(theme.Muted), "new ").add(color(theme.Error), "not configured ")
	} else {
		right.add(color(theme.Muted), "new ").add(color(theme.Agent(p.activeAgent)), p.activeAgent).add(color(theme.Faint), " ▾ ")
	}
	if left.width+right.width > cols {
		return left.String()
	}
	return joined(left, right, cols)
}

// agentColumnWidth fits the widest agent name on screen, within reason.
func agentColumnWidth(list []types.Session) int {
	width := 5
	for _, session := range list {
		width = max(width, utf8.RuneCountInString(agentName(session)))
	}
	return min(width, 10)
}

func agentName(session types.Session) string {
	if session.WindowName == "" {
		return "?"
	}
	return session.WindowName
}

// rightColumns is the width of the agent and age columns, or of the age
// column alone when the pane is too narrow for both.
func rightColumns(cols, agentWidth int) (width int, showAgent bool) {
	width = agentWidth + 2 + ageWidth + 1
	if cols-leadWidth-width < 12 {
		return ageWidth + 1, false
	}
	return width, true
}

func (p *picker) columnHeader(cols, agentWidth int) string {
	theme := p.palette()
	_, showAgent := rightColumns(cols, agentWidth)
	right := new(cells)
	if showAgent {
		right.add(color(theme.Faint), padRight("AGENT", agentWidth)+"  ")
	}
	right.add(color(theme.Faint), padLeft("AGE", ageWidth)+" ")
	return joined(new(cells).pad(leadWidth).add(color(theme.Faint), "TASK"), right, cols)
}

func (p *picker) drawLabelEditor(frame *screenFrame) {
	theme := p.palette()
	editorContext := "selected session"
	for _, session := range p.sessions {
		if session.WindowID == p.editLabelID {
			editorContext = fmt.Sprintf("%s · %s", projectName(session), agentName(session))
			break
		}
	}
	before := p.editLabelValue[:p.editLabelCursor]
	after := p.editLabelValue[p.editLabelCursor:]
	frame.lineBg(2, new(cells).pad(2).add(strong(theme.Accent), "✎ EDIT TASK LABEL").pad(2).add(color(theme.Text), editorContext).String(), theme.HeaderBg)
	frame.lineBg(3, new(cells).pad(2).add(strong(theme.Pinned), "Task ›").pad(1).add(color(theme.Text), before).
		add(color(theme.Accent), "█").add(color(theme.Text), after).String(), theme.HeaderBg)
	frame.lineBg(4, new(cells).pad(2).add(color(theme.Muted), "⏎ save · esc cancel · ^U clear").String(), theme.HeaderBg)
}

// noticeLine renders the single status line under the header: a pending
// confirmation, an error, or the result of the last action.
func (p *picker) noticeLine(cols int) string {
	theme := p.palette()
	switch {
	case p.confirmWorktree != "":
		const suffix = "?  D confirm · esc cancel · branch kept"
		label := truncateVisible(filepath.Base(p.confirmWorktree), "  remove ", suffix, cols)
		return new(cells).add(color(theme.Attention), "  remove "+label+suffix).String()
	case p.confirmStopID != "":
		const suffix = "?  ^x confirm · esc cancel"
		label := truncateVisible(p.confirmLabel, "  stop ", suffix, cols)
		return new(cells).add(color(theme.Attention), "  stop "+label+suffix).String()
	case p.activateErr != "":
		return new(cells).add(color(theme.Error), "  couldn't switch to parent: "+p.activateErr).String()
	case strings.HasPrefix(p.notice, "created "):
		return new(cells).pad(2).add(strong(theme.Done), "✓ CREATED").pad(2).add(color(theme.Text), strings.TrimPrefix(p.notice, "created ")).String()
	case p.notice != "":
		c := theme.Done
		if p.noticeError {
			c = theme.Error
		}
		return new(cells).add(color(c), "  "+p.notice).String()
	}
	return ""
}

func (p *picker) sectionLine(listRow pickerListRow, cols int) string {
	theme := p.palette()
	name, c := sectionPresentation(theme, listRow.section)
	left := new(cells).pad(1).add(strong(c), name).add(color(theme.Faint), " "+strconv.Itoa(listRow.count))
	right := new(cells)
	if listRow.oldestWait > 0 {
		right.add(color(theme.Faint), "oldest ").add(strong(waitColor(theme, listRow.oldestWait)), sessions.FormatAgo(listRow.oldestWait)).pad(1)
	}
	if left.width+right.width > cols {
		return left.String()
	}
	return joined(left, right, cols)
}

// checkoutLine names a checkout and summarizes its Git state on one line.
func (p *picker) checkoutLine(session types.Session, working, cols int) string {
	theme := p.palette()
	info := p.gitByPath[p.pathKey(session.Path)]

	var name []segment
	if info.valid && info.linkedWorktree {
		name = append(name,
			segment{text: repositoryName(info, session), color: theme.Subtext, bold: true},
			segment{text: " ⎇ " + worktreeHeading(info, session), color: theme.Branch})
	} else {
		path := sessions.FormatPath(session.Path)
		if path == "" {
			path = session.Name
		}
		parent, base := filepath.Split(path)
		name = append(name, segment{text: parent, color: theme.Muted}, segment{text: base, color: theme.Subtext, bold: true})
	}

	var warning []segment
	if working > 1 {
		warning = append(warning, segment{text: fmt.Sprintf("⚠ %d writing", working), color: theme.Attention})
	}
	summary := gitSegments(theme, info)
	right := func() []segment {
		var segs []segment
		if len(warning) > 0 {
			segs = append(segs, warning...)
			if len(summary) > 0 {
				segs = append(segs, segment{text: "  "})
			}
		}
		segs = append(segs, summary...)
		if len(segs) > 0 {
			segs = append(segs, segment{text: " "})
		}
		return segs
	}
	// Drop the least important Git details before shortening the name.
	minimumName := min(segmentsWidth(name), 18)
	for len(summary) > 1 && 2+minimumName+1+segmentsWidth(right()) > cols {
		summary = summary[:len(summary)-1]
	}
	rightSegs := right()
	nameWidth := cols - 2 - 1 - segmentsWidth(rightSegs)
	name = truncateSegmentsLeft(name, nameWidth)
	left := new(cells).pad(2).segments(name)
	r := new(cells).segments(rightSegs)
	if left.width+r.width > cols {
		return left.String()
	}
	return joined(left, r, cols)
}

// gitSegments summarizes a checkout: branch, changed files, line counts,
// untracked files and, for llmux worktrees, commits since branching.
func gitSegments(theme ansi.Theme, info gitInfo) []segment {
	if !info.valid {
		return nil
	}
	var segs []segment
	if !info.linkedWorktree {
		segs = append(segs, segment{text: info.branch, color: theme.Branch})
	}
	if info.changed == 0 && info.untracked == 0 {
		segs = append(segs, segment{text: "clean", color: theme.Muted})
	}
	if info.changed > 0 {
		segs = append(segs, segment{text: fmt.Sprintf("±%d", info.changed), color: theme.Muted})
	}
	if info.additions > 0 {
		segs = append(segs, segment{text: fmt.Sprintf("+%d", info.additions), color: theme.Added})
	}
	if info.deletions > 0 {
		segs = append(segs, segment{text: fmt.Sprintf("−%d", info.deletions), color: theme.Removed})
	}
	if info.untracked > 0 {
		segs = append(segs, segment{text: fmt.Sprintf("?%d", info.untracked), color: theme.Muted})
	}
	if info.ahead > 0 {
		segs = append(segs, segment{text: fmt.Sprintf("↑%d", info.ahead), color: theme.Ahead})
	}
	if info.merged {
		segs = append(segs, segment{text: "merged", color: theme.Done})
	}
	// Separate fields with spaces, wider after the branch name.
	out := make([]segment, 0, len(segs)*2)
	for i, s := range segs {
		if i > 0 {
			gap := " "
			if i == 1 && !info.linkedWorktree {
				gap = "  "
			}
			out = append(out, segment{text: gap})
		}
		out = append(out, s)
	}
	return out
}

func (p *picker) drawSession(frame *screenFrame, row int, session types.Session, cols int, selected bool, agentWidth int) {
	theme := p.palette()
	glyph, _, stateColor := presentation(theme, session)

	marker := " "
	if selected {
		marker = "▌"
	}
	left := new(cells).pad(1).add(color(stateColor), marker).pad(1).add(strong(stateColor), glyph).pad(1)

	rightWidth, showAgent := rightColumns(cols, agentWidth)
	right := new(cells)
	if showAgent {
		name := truncateVisible(agentName(session), "", "", agentWidth)
		right.add(color(theme.Agent(session.WindowName)), padRight(name, agentWidth)).pad(2)
	}
	age := sessions.FormatAgo(session.StateAt)
	ageStyle := color(theme.Muted)
	switch {
	case sessionState(session) == types.Waiting:
		ageStyle = strong(waitColor(theme, session.StateAt))
	case session.Unseen:
		ageStyle = color(theme.Done)
	}
	right.add(ageStyle, padLeft(age, ageWidth)).pad(1)

	titleWidth := max(1, cols-leadWidth-rightWidth-1)
	title, style := session.Label, color(theme.Subtext)
	switch {
	case session.Label != "":
		if selected {
			style = strong(theme.Text)
		}
	case session.AutoLabel != "":
		title, style = session.AutoLabel, soft(theme.Muted)
		if selected {
			style = soft(theme.Subtext)
		}
	default:
		title, style = "unnamed task", soft(theme.Faint)
	}
	left.add(style, truncateVisible(title, "", "", titleWidth))

	if selected {
		frame.lineBg(row, joined(left, right, cols), theme.SelectionBg)
		return
	}
	frame.line(row, joined(left, right, cols))
}

// drawDetail explains, under a session, why it needs the user.
func (p *picker) drawDetail(frame *screenFrame, row int, session types.Session, cols int, selected bool) {
	theme := p.palette()
	line := new(cells).pad(leadWidth)
	if sessionState(session) == stateExited {
		hint := " · R restarts"
		if resumeCommand(session) != "" {
			hint = " · R resumes"
		}
		line.add(color(theme.Error), "Exited").add(color(theme.Muted), truncateVisible(fmt.Sprintf(" · status %d%s", session.ExitStatus, hint), "", "", cols-line.width-6))
	} else {
		kindColor := theme.Attention
		if session.ReasonKind == types.ReasonError {
			kindColor = theme.Error
		}
		title := session.ReasonKind.Title()
		line.add(color(kindColor), title)
		if session.Reason != "" {
			line.add(color(theme.Muted), truncateVisible(" · "+session.Reason, "", "", cols-line.width-1))
		}
	}
	if selected {
		frame.lineBg(row, line.String(), theme.SelectionBg)
		return
	}
	frame.line(row, line.String())
}

// hasDetail reports whether a session gets an explanation row.
func hasDetail(session types.Session) bool {
	switch sessionState(session) {
	case stateExited:
		return true
	case types.Waiting:
		return session.ReasonKind.Title() != ""
	}
	return false
}

func waitColor(theme ansi.Theme, since int64) ansi.RGB {
	waited := time.Since(time.Unix(since, 0))
	switch {
	case waited >= longWait:
		return theme.Error
	case waited >= lateWait:
		return theme.Late
	default:
		return theme.Attention
	}
}

// footerLine shows the keys that apply to the selected row. Pairs that do
// not fit are dropped from the end; ? always lists every key.
func (p *picker) footerLine(list []types.Session, cols int) string {
	theme := p.palette()
	type pair struct{ key, label string }
	var pairs []pair
	switch {
	case p.showHelp:
		pairs = []pair{{"?", "close help"}}
	case len(list) == 0:
		pairs = []pair{{"a", "add"}, {"A", "worktree"}, {"s", "agent"}, {"q", "close"}}
	default:
		session := list[min(p.selectedIndex, len(list)-1)]
		if sessionState(session) == stateExited {
			pairs = append(pairs, pair{"R", "restart"})
		}
		pairs = append(pairs, pair{"⏎", "live"}, pair{"o", "popup"}, pair{"r", "review"}, pair{"d", "diff"})
		if p.isManagedWorktree(list) {
			pairs = append(pairs, pair{"D", "clean"})
		}
		pairs = append(pairs, pair{"e", "label"}, pair{"^x", "stop"})
	}
	right := new(cells).add(color(theme.Faint), "? keys ")
	if p.showHelp {
		right = new(cells)
	}
	for len(pairs) > 0 {
		left := new(cells).pad(1)
		for _, pr := range pairs {
			left.add(strong(theme.Subtext), pr.key).add(color(theme.Faint), " "+pr.label+"  ")
		}
		if left.width+right.width <= cols {
			return joined(left, right, cols)
		}
		pairs = pairs[:len(pairs)-1]
	}
	return right.String()
}

var helpGroups = []struct {
	title string
	keys  [][2]string
}{
	{"Move", [][2]string{
		{"j k ↑ ↓", "select"},
		{"gg G", "first / last"},
		{"n", "next agent that needs you, then finished work"},
		{"/", "search tasks, paths, branches, agents, states"},
	}},
	{"Selected agent", [][2]string{
		{"⏎", "type into the live pane (prefix u comes back)"},
		{"o", "open in a popup over its project window"},
		{"r", "go to its project window"},
		{"d", "review its diff in a popup"},
		{"e", "name the task"},
		{"p", "pin to the top"},
		{"^x", "stop (asks first unless idle)"},
		{"R", "restart an agent that exited"},
	}},
	{"Create", [][2]string{
		{"a", "new agent in the selected project"},
		{"A", "new agent in an isolated worktree"},
		{"D", "remove a clean llmux worktree, keeping its branch"},
		{"s", "cycle the agent used for new sessions"},
	}},
	{"Room", [][2]string{
		{"?", "close this help"},
		{"q", "close the control room"},
	}},
}

func (p *picker) drawHelp(frame *screenFrame, row, footerRow, cols int) {
	theme := p.palette()
	for i, group := range helpGroups {
		if i > 0 {
			row++
		}
		if row >= footerRow {
			return
		}
		frame.line(row, new(cells).pad(1).add(strong(theme.Accent), group.title).String())
		row++
		for _, key := range group.keys {
			if row >= footerRow {
				return
			}
			line := new(cells).pad(3).add(strong(theme.Text), padRight(key[0], 9))
			line.add(color(theme.Subtext), truncateVisible(key[1], "", "", cols-line.width-1))
			frame.line(row, line.String())
			row++
		}
	}
}

// ---------------------------------------------------------------- list rows

type listRowKind int

const (
	listRowSection listRowKind = iota
	listRowSpacer
	listRowCheckout
	listRowSession
	listRowDetail
)

type pickerListRow struct {
	kind         listRowKind
	section      int   // section rows: which section
	count        int   // section rows: sessions in the section
	oldestWait   int64 // Needs You section: when its longest wait began
	sessionIndex int   // checkout, session and detail rows: index into the list
	working      int   // checkout rows: agents actively working there in this section
}

func (p *picker) buildListRows(list []types.Session) []pickerListRow {
	if len(list) == 0 {
		return nil
	}
	var counts [sectionCount]int
	var oldestWait int64
	// working counts actively working agents per (checkout, section), so the
	// shared-checkout warning describes exactly the rows beneath it.
	working := make(map[string]int)
	for _, session := range list {
		section := sectionRank(session)
		counts[section]++
		if section == sectionNeedsYou && session.StateAt > 0 && (oldestWait == 0 || session.StateAt < oldestWait) {
			oldestWait = session.StateAt
		}
		if sessionState(session) == types.Working {
			working[p.checkoutKey(session)+"\x00"+strconv.Itoa(section)]++
		}
	}

	rows := make([]pickerListRow, 0, len(list)*3)
	previousSection := -1
	previousCheckout := ""
	for i, session := range list {
		section := sectionRank(session)
		if section != previousSection {
			if previousSection != -1 {
				rows = append(rows, pickerListRow{kind: listRowSpacer})
			}
			heading := pickerListRow{kind: listRowSection, section: section, count: counts[section]}
			if section == sectionNeedsYou {
				heading.oldestWait = oldestWait
			}
			rows = append(rows, heading)
			previousSection = section
			previousCheckout = ""
		}
		checkout := p.checkoutKey(session)
		if checkout != previousCheckout {
			rows = append(rows, pickerListRow{
				kind:         listRowCheckout,
				sessionIndex: i,
				working:      working[checkout+"\x00"+strconv.Itoa(section)],
			})
			previousCheckout = checkout
		}
		rows = append(rows, pickerListRow{kind: listRowSession, sessionIndex: i})
		if hasDetail(session) {
			rows = append(rows, pickerListRow{kind: listRowDetail, sessionIndex: i})
		}
	}
	return rows
}

func (p *picker) checkoutKey(session types.Session) string {
	if key := p.pathKey(session.Path); key != "" {
		return key
	}
	return session.Name
}

func repositoryName(info gitInfo, session types.Session) string {
	if info.repository != "" {
		return info.repository
	}
	if filepath.Base(info.commonDir) == ".git" {
		return filepath.Base(filepath.Dir(info.commonDir))
	}
	return projectName(session)
}

func worktreeHeading(info gitInfo, session types.Session) string {
	if info.worktreeLabel != "" {
		return info.worktreeLabel
	}
	if info.checkoutPath != "" {
		return filepath.Base(info.checkoutPath)
	}
	if info.branch != "" {
		return info.branch
	}
	return filepath.Base(session.Path)
}

// visibleListRows scrolls the list so the selected session stays on screen,
// together with its detail row when it has one.
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

// ---------------------------------------------------------------- text fitting

func padRight(value string, width int) string {
	return value + strings.Repeat(" ", max(0, width-utf8.RuneCountInString(value)))
}

func padLeft(value string, width int) string {
	return strings.Repeat(" ", max(0, width-utf8.RuneCountInString(value))) + value
}

// truncateSegmentsLeft shortens a path-like name from the left, keeping its
// final segment (the checkout name) as long as possible.
func truncateSegmentsLeft(segs []segment, width int) []segment {
	if segmentsWidth(segs) <= width {
		return segs
	}
	if width <= 1 {
		return nil
	}
	out := append([]segment(nil), segs...)
	for len(out) > 1 && segmentsWidth(out) > width {
		first := []rune(out[0].text)
		over := segmentsWidth(out) - width
		if len(first) > over+1 {
			out[0].text = "…" + string(first[over+1:])
			return out
		}
		out = out[1:]
	}
	if segmentsWidth(out) > width {
		out[0].text = truncateVisible(out[0].text, "", "", width)
	}
	return out
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

// ---------------------------------------------------------------- frame

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

// previewState is the state shown in the live pane's border.
func previewState(session types.Session) string {
	switch {
	case sessionState(session) == stateExited:
		return "error"
	case sessionState(session) == types.Waiting && session.ReasonKind == types.ReasonError:
		return "error"
	case sessionState(session) == types.Waiting:
		return "waiting"
	case session.Unseen:
		return "done"
	}
	return string(sessionState(session))
}

// liveTitle names the agent shown in the live pane and, while it waits,
// what it is waiting for.
func liveTitle(session types.Session) string {
	parts := []string{"LIVE AGENT", projectName(session)}
	if name := session.Title(); name != "" {
		parts = append(parts, name)
	}
	if session.WindowName != "" {
		parts = append(parts, session.WindowName)
	}
	if sessionState(session) == types.Waiting && session.ReasonKind != "" {
		parts = append(parts, state.ReasonText(session.ReasonKind, session.Reason))
	}
	return strings.Join(append(parts, "prefix u returns"), " · ")
}
