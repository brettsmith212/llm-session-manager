package picker

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"llm-session-manager/internal/ansi"
	"llm-session-manager/internal/types"
	"llm-session-manager/internal/worktree"
)

func TestFilteredMatchesSessionMeaning(t *testing.T) {
	now := time.Now().Unix()
	p := &picker{sessions: []types.Session{
		{
			Name:        "llm-api",
			WindowID:    "@1",
			WindowIndex: 2,
			WindowName:  "claude",
			State:       types.Working,
			StateAt:     now,
			Path:        "/work/API-Service",
			Label:       "Implement OAuth callback",
		},
		{
			Name:        "llm-web",
			WindowID:    "@2",
			WindowIndex: 0,
			WindowName:  "amp",
			State:       types.Waiting,
			StateAt:     now,
			Path:        "/work/web-client",
		},
		{
			Name:        "llm-old",
			WindowID:    "@3",
			WindowIndex: 1,
			WindowName:  "opencode",
			State:       types.Working,
			StateAt:     now - 301,
			Path:        "/work/legacy",
		},
	}, gitByPath: map[string]gitInfo{
		gitPathKey("/work/API-Service"): {valid: true, branch: "feat/oauth"},
	}}

	tests := []struct {
		name    string
		query   string
		wantIDs []string
	}{
		{name: "empty is attention ordered", wantIDs: []string{"@2", "@1", "@3"}},
		{name: "path is case insensitive", query: "api-service", wantIDs: []string{"@1"}},
		{name: "agent and state terms combine", query: "claude working", wantIDs: []string{"@1"}},
		{name: "task label is searchable", query: "oauth callback", wantIDs: []string{"@1"}},
		{name: "git branch is searchable", query: "feat/oauth", wantIDs: []string{"@1"}},
		{name: "human attention label is searchable", query: "needs you", wantIDs: []string{"@2"}},
		{name: "window number is searchable", query: "#2", wantIDs: []string{"@1"}},
		{name: "stale working session is effectively idle", query: "legacy idle", wantIDs: []string{"@3"}},
		{name: "all terms must match", query: "api amp", wantIDs: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p.query = tt.query
			got := p.filtered()
			gotIDs := make([]string, len(got))
			for i, session := range got {
				gotIDs[i] = session.WindowID
			}
			if fmt.Sprint(gotIDs) != fmt.Sprint(tt.wantIDs) {
				t.Fatalf("filtered IDs = %v, want %v", gotIDs, tt.wantIDs)
			}
		})
	}
}

func TestVisibleListRowsKeepSelectionOnScreen(t *testing.T) {
	list := []types.Session{
		{Name: "project-a", WindowID: "@1", State: types.Waiting},
		{Name: "project-a", WindowID: "@2", State: types.Working},
		{Name: "project-b", WindowID: "@3", State: types.Working},
		{Name: "project-c", WindowID: "@4", State: types.Idle},
		{Name: "project-c", WindowID: "@5", State: types.Idle},
	}
	p := &picker{sessions: list}
	ordered := p.filtered()
	allRows := p.buildListRows(ordered)

	for selected := range ordered {
		for available := 1; available <= 7; available++ {
			visible := visibleListRows(allRows, selected, available)
			if len(visible) > available {
				t.Fatalf("visible list uses %d rows, only %d available", len(visible), available)
			}
			found := false
			for _, row := range visible {
				if row.kind == listRowSession && row.sessionIndex == selected {
					found = true
				}
			}
			if !found {
				t.Fatalf("selection %d not visible with %d rows: %#v", selected, available, visible)
			}
		}
	}
}

func TestListRowsGroupByAttentionAndDemoteSharedCheckoutNotice(t *testing.T) {
	p := &picker{
		sessions: []types.Session{
			{Name: "shared", WindowID: "@1", Path: "/work/shared", State: types.Waiting},
			{Name: "shared", WindowID: "@2", Path: "/work/shared", State: types.Working},
			{Name: "idle", WindowID: "@3", Path: "/work/idle", State: types.Idle},
		},
		gitByPath: map[string]gitInfo{
			gitPathKey("/work/shared"): {valid: true, branch: "main", changed: 2, additions: 4, deletions: 1},
		},
	}
	rows := p.buildListRows(p.filtered())
	var text []string
	warnings := 0
	spacers := 0
	for _, row := range rows {
		text = append(text, row.text)
		if row.kind == listRowWarning {
			warnings++
		}
		if row.kind == listRowSpacer {
			spacers++
		}
	}
	joined := strings.Join(text, "|")
	for _, want := range []string{"NEEDS YOU · 1", "ACTIVE · 1", "IDLE · 1", "main · 2 files · +4 · -1 · shared checkout · 2 agents"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("grouped rows %q do not contain %q", joined, want)
		}
	}
	if warnings != 0 {
		t.Fatalf("shared-checkout warnings = %d, want none with only one working agent", warnings)
	}
	if spacers != 2 {
		t.Fatalf("section spacers = %d, want two between three sections", spacers)
	}
}

func TestListRowsWarnWhenMultipleAgentsAreActivelySharingCheckout(t *testing.T) {
	p := &picker{sessions: []types.Session{
		{Name: "shared", WindowID: "@1", Path: "/work/shared", State: types.Working},
		{Name: "shared", WindowID: "@2", Path: "/work/shared", State: types.Working},
	}}
	warnings := 0
	for _, row := range p.buildListRows(p.filtered()) {
		if row.kind == listRowWarning {
			warnings++
			if row.text != "2 agents are active in this checkout" {
				t.Fatalf("warning = %q", row.text)
			}
		}
	}
	if warnings != 1 {
		t.Fatalf("active shared-checkout warnings = %d, want one", warnings)
	}
}

func TestReplaceSessionsPreservesSelectedWindowAcrossStateReordering(t *testing.T) {
	p := &picker{sessions: []types.Session{
		{Name: "a", WindowID: "@1", Path: "/work/a", State: types.Working},
		{Name: "b", WindowID: "@2", Path: "/work/b", State: types.Waiting},
	}}
	// Attention ordering starts with @2, so index 1 selects @1.
	p.selectedIndex = 1
	p.replaceSessions([]types.Session{
		{Name: "a", WindowID: "@1", Path: "/work/a", State: types.Waiting},
		{Name: "b", WindowID: "@2", Path: "/work/b", State: types.Idle},
	})
	if got := p.selectedWindowID(); got != "@1" {
		t.Fatalf("selected window after state reorder = %q, want @1", got)
	}
}

func TestRepositorySortingKeepsPrimaryCheckoutBeforeItsWorktrees(t *testing.T) {
	const (
		commonDir = "/projects/zeta/.git"
		original  = "/projects/zeta"
		alphaTree = "/home/user/.local/share/llmux/worktrees/zeta/alpha"
		betaTree  = "/home/user/.local/share/llmux/worktrees/zeta/beta"
	)
	p := &picker{
		sessions: []types.Session{
			{Name: "beta", WindowID: "@3", Path: betaTree, State: types.Working},
			{Name: "other", WindowID: "@0", Path: "/projects/alpha", State: types.Working},
			{Name: "original", WindowID: "@1", Path: original, State: types.Working},
			{Name: "alpha", WindowID: "@2", Path: alphaTree, State: types.Working},
			{Name: "waiting-tree", WindowID: "@4", Path: alphaTree, State: types.Waiting},
		},
		gitByPath: map[string]gitInfo{
			gitPathKey("/projects/alpha"): {valid: true, commonDir: "/projects/alpha/.git", checkoutPath: "/projects/alpha"},
			gitPathKey(original):          {valid: true, commonDir: commonDir, checkoutPath: original},
			gitPathKey(alphaTree):         {valid: true, commonDir: commonDir, checkoutPath: alphaTree, linkedWorktree: true, worktreeLabel: "Alpha task"},
			gitPathKey(betaTree):          {valid: true, commonDir: commonDir, checkoutPath: betaTree, linkedWorktree: true, worktreeLabel: "Beta task"},
		},
	}

	got := p.filtered()
	gotIDs := make([]string, len(got))
	for i, session := range got {
		gotIDs[i] = session.WindowID
	}
	want := []string{"@4", "@0", "@1", "@2", "@3"}
	if fmt.Sprint(gotIDs) != fmt.Sprint(want) {
		t.Fatalf("repository-aware order = %v, want %v", gotIDs, want)
	}
}

func TestRepositoryMetadataReorderPreservesSelectedWindow(t *testing.T) {
	const (
		original = "/z/repository"
		worktree = "/a/worktree"
	)
	p := &picker{sessions: []types.Session{
		{Name: "tree", WindowID: "@2", Path: worktree, State: types.Working},
		{Name: "original", WindowID: "@1", Path: original, State: types.Working},
	}}
	p.selectedIndex = 0
	selectedID := p.selectedWindowID()
	p.gitByPath = map[string]gitInfo{
		gitPathKey(original): {valid: true, commonDir: original + "/.git", checkoutPath: original},
		gitPathKey(worktree): {valid: true, commonDir: original + "/.git", checkoutPath: worktree, linkedWorktree: true},
	}
	p.restoreSelectedWindow(selectedID)
	if got := p.selectedWindowID(); got != "@2" {
		t.Fatalf("selected window after repository reorder = %q, want @2", got)
	}
}

func TestFrozenDisplayStateDoesNotDriftBetweenRefreshes(t *testing.T) {
	session := types.Session{
		State: types.Working, DisplayState: types.Idle,
		StateAt: time.Now().Unix(),
	}
	if got := sessionState(session); got != types.Idle {
		t.Fatalf("sessionState() = %q, want frozen idle despite recent raw working state", got)
	}
}

func TestNextWaitingPreservesFilterWhenNothingNeedsAttention(t *testing.T) {
	p := &picker{
		query: "api",
		sessions: []types.Session{{
			Name: "api", WindowID: "@1", Path: "/work/api", State: types.Idle,
		}},
	}
	captureStdout(t, p.selectNextWaiting)
	if p.query != "api" {
		t.Fatalf("query after n with no waiting sessions = %q, want api", p.query)
	}
}

func TestInspectGitReportsBranchDiffAndUntrackedFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q", "-b", "main")
	mustGit(t, repo, "config", "user.name", "llmux test")
	mustGit(t, repo, "config", "user.email", "llmux@example.com")
	tracked := repo + "/tracked.txt"
	if err := os.WriteFile(tracked, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "tracked.txt")
	mustGit(t, repo, "commit", "-qm", "initial")
	if err := os.WriteFile(tracked, []byte("after\nextra\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repo+"/untracked.txt", []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	info := inspectGit(repo)
	if !info.valid || info.branch != "main" || info.changed != 1 || info.untracked != 1 || info.additions != 2 || info.deletions != 1 {
		t.Fatalf("inspectGit() = %#v, want main with one changed, one untracked, +2/-1", info)
	}
	if info.checkoutPath != gitPathKey(repo) || info.commonDir != gitPathKey(filepath.Join(repo, ".git")) || info.linkedWorktree {
		t.Fatalf("primary checkout identity = %#v", info)
	}
	if got := formatGitInfo(info); got != "main · 1 file · +2 · -1 · ?1" {
		t.Fatalf("formatGitInfo() = %q", got)
	}

	linkedPath := filepath.Join(t.TempDir(), "linked")
	mustGit(t, repo, "worktree", "add", "-q", "-b", "feature/linked", linkedPath, "HEAD")
	linked := inspectGit(linkedPath)
	if !linked.valid || !linked.linkedWorktree || linked.checkoutPath != gitPathKey(linkedPath) || linked.commonDir != info.commonDir {
		t.Fatalf("linked checkout identity = %#v, primary = %#v", linked, info)
	}
}

func TestANSIContentIsClippedWithoutBreakingUTF8(t *testing.T) {
	value := ansi.Foreground(ansi.Blue) + "a-very-long-πroject-name" + ansi.Reset
	got := fitANSI(value, 12)
	if width := ansiWidth(got); width > 12 {
		t.Fatalf("clipped width = %d, want <= 12", width)
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("clipped value %q does not indicate truncation", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("clipped value is invalid UTF-8: %q", got)
	}

	if got := truncateVisible("claude-α", "", "", 6); got != "claud…" {
		t.Fatalf("Unicode-safe truncation = %q, want %q", got, "claud…")
	}
}

func TestSelectedRowHighlightContinuesBehindWindowNumber(t *testing.T) {
	frame := newScreenFrame(80, 1)
	drawItem(frame, types.Session{
		Name:        "llm-api",
		WindowIndex: 0,
		WindowName:  "claude",
		State:       types.Idle,
		Path:        "/work/api",
	}, 80, true, 1, true)

	output := captureStdout(t, frame.flush)
	marker := strings.LastIndex(output, "#0")
	if marker < 0 {
		t.Fatalf("selected row did not render its window number: %q", output)
	}
	beforeMarker := output[:marker]
	lastHighlight := strings.LastIndex(beforeMarker, ansi.Background(ansi.Surface0))
	lastReset := strings.LastIndex(beforeMarker, ansi.Reset)
	if lastHighlight < lastReset {
		t.Fatal("selected row reset its background before the window number")
	}
	if !strings.Contains(output, "unnamed task") {
		t.Fatalf("selected unlabeled session did not advertise explicit naming: %q", output)
	}
}

func TestPreviewQueueCoalescesToTheFinalSelection(t *testing.T) {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	t.Cleanup(func() { timer.Stop() })

	p := &picker{previewTimer: timer, previewWindowID: "@1"}
	p.queuePreview(types.Session{WindowID: "@2"})
	p.queuePreview(types.Session{WindowID: "@3"})
	if !p.hasPendingPreview || p.pendingPreview.WindowID != "@3" {
		t.Fatalf("pending preview = %#v, want final selection @3", p.pendingPreview)
	}

	// Returning to the preview that is already visible must cancel all
	// intermediate work instead of briefly attaching another session.
	p.queuePreview(types.Session{WindowID: "@1"})
	if p.hasPendingPreview {
		t.Fatal("returning to the displayed preview did not cancel pending work")
	}
}

func TestParseKeysPreservesModifiedEnterSequences(t *testing.T) {
	for _, sequence := range []string{"\x1b[13;2u", "\x1b[27;2;13~"} {
		got := parseKeys(sequence)
		if len(got) != 1 || got[0] != sequence {
			t.Fatalf("parseKeys(%q) = %#v, want one complete modified-enter token", sequence, got)
		}
	}
}

// TestPickerHelperProcess runs the real picker inside a tmux pane for the
// isolated workflow test below. It is a no-op during a normal test process.
func TestPickerHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_PICKER_HELPER_PROCESS") != "1" {
		return
	}
	if err := Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func TestPopupAndReviewKeepControlRoomAlive(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}

	t.Setenv("TMUX", "")
	socketDir, err := os.MkdirTemp("/tmp", "llm-picker-popup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	t.Setenv("TMUX_TMPDIR", socketDir)
	project := t.TempDir()
	mustTmux(t, "new-session", "-d", "-x", "120", "-y", "30", "-s", "origin", "-n", "project", "-c", project, "sleep 1000")
	t.Cleanup(func() { _, _ = runTmux("kill-server") })
	mustTmux(t, "new-window", "-d", "-t", "origin:", "-n", windowName, "sleep 1000")
	mustTmux(t, "new-session", "-d", "-x", "120", "-y", "30", "-s", "llm-agent", "-n", "amp", "-c", project, "sleep 1000")
	windowID := mustTmux(t, "display-message", "-p", "-t", "llm-agent:0", "#{window_id}")
	mustTmux(t, "set-option", "-t", "llm-agent", "@llm_origin", "origin")
	mustTmux(t, "new-session", "-d", "-x", "120", "-y", "30", "-s", "terminal", "-n", "display", "env -u TMUX tmux attach-session -t origin")

	client := ""
	waitFor(t, 2*time.Second, "attached project client", func() bool {
		clients, _ := runTmux("list-clients", "-F", "#{client_name}|#{session_name}")
		for _, line := range strings.Split(clients, "\n") {
			parts := strings.SplitN(line, "|", 2)
			if len(parts) == 2 && parts[1] == "origin" {
				client = parts[0]
				return true
			}
		}
		return false
	})
	mustTmux(t, "set-option", "-g", "@llm_parent", client)

	p := &picker{
		parent: client,
		sessions: []types.Session{{
			Name: "llm-agent", WindowID: windowID, WindowName: "amp",
			Path: project, Origin: "origin", State: types.Working,
		}},
	}
	p.activateSession()
	waitFor(t, 2*time.Second, "popup over project", func() bool {
		return clientWindow(client) == "project" &&
			strings.Contains(mustTmux(t, "list-windows", "-t", "origin", "-F", "#{window_name}"), windowName) &&
			strings.Contains(mustTmux(t, "capture-pane", "-p", "-t", "terminal:display.0"), "llm-agent")
	})
	mustTmux(t, "display-popup", "-c", client, "-C")
	waitFor(t, 2*time.Second, "project after popup close", func() bool {
		return clientWindow(client) == "project"
	})

	mustTmux(t, "switch-client", "-c", client, "-t", "origin:"+windowName)
	p.reviewProject()
	waitFor(t, 2*time.Second, "direct project review", func() bool {
		return clientWindow(client) == "project" &&
			strings.Contains(mustTmux(t, "list-windows", "-t", "origin", "-F", "#{window_name}"), windowName)
	})

	// A Control Room opened from a worktree can have the same pane path as
	// the desired review host. It must never satisfy project-window lookup,
	// or r merely switches to the already-visible Control Room and appears
	// to do nothing.
	worktreePath := t.TempDir()
	mustTmux(t, "respawn-pane", "-k", "-t", "origin:"+windowName+".0", "-c", worktreePath, "sleep 1000")
	mustTmux(t, "set-option", "-w", "-t", "origin:"+windowName, "@llm_control_room", "1")
	mustTmux(t, "new-window", "-d", "-t", "origin:", "-n", "worktree-review", "-c", worktreePath, "sleep 1000")
	mustTmux(t, "set-option", "-w", "-t", "origin:worktree-review", "automatic-rename", "off")
	p.sessions[0].Path = worktreePath
	mustTmux(t, "switch-client", "-c", client, "-t", "origin:"+windowName)
	p.reviewProject()
	waitFor(t, 2*time.Second, "worktree review ignores same-path control room", func() bool {
		return clientWindow(client) == "worktree-review"
	})
}

func TestCleanupSelectedWorktreeStopsAgentsAndKeepsBranch(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	t.Setenv("TMUX", "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	socketDir, err := os.MkdirTemp("/tmp", "llm-picker-cleanup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	t.Setenv("TMUX_TMPDIR", socketDir)
	t.Cleanup(func() { _, _ = runTmux("kill-server") })

	repository := t.TempDir()
	mustGit(t, repository, "init", "-q", "-b", "main")
	mustGit(t, repository, "config", "user.name", "llmux test")
	mustGit(t, repository, "config", "user.email", "llmux@example.com")
	if err := os.WriteFile(repository+"/flake.nix", []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repository, "add", "flake.nix")
	mustGit(t, repository, "commit", "-qm", "initial")
	repoInfo, err := worktree.Inspect(repository)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := worktree.NewPlan(repoInfo, "Try Darwin Settings")
	if err != nil {
		t.Fatal(err)
	}
	if err := worktree.Create(plan); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(plan.Path); err == nil {
			_ = worktree.DiscardCreated(plan)
		}
	})

	mustTmux(t, "new-session", "-d", "-s", "origin", "-n", "project", "sleep 1000")
	hostWindowID := mustTmux(t, "new-window", "-dP", "-t", "origin:", "-c", plan.Path, "-F", "#{window_id}", "sleep 1000")
	mustTmux(t, "set-option", "-w", "-t", hostWindowID, worktreeHostOption, plan.Path)
	mustTmux(t, "new-session", "-d", "-s", "llm-worktree", "-n", "amp", "-c", plan.Path, "sleep 1000")
	windowID := mustTmux(t, "display-message", "-p", "-t", "llm-worktree:0", "#{window_id}")
	mustTmux(t, "set-option", "-t", "llm-worktree", "@llm_path", plan.Path)
	mustTmux(t, "set-option", "-w", "-t", windowID, "@llm_path", plan.Path)
	mustTmux(t, "set-option", "-w", "-t", windowID, "@llm_agent", "amp")

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	t.Cleanup(func() { timer.Stop() })
	p := &picker{
		prefix:       "llm-",
		previewTimer: timer,
		gitByPath:    map[string]gitInfo{},
		sessions: []types.Session{{
			Name: "llm-worktree", WindowID: windowID, WindowName: "amp",
			Path: plan.Path, State: types.Idle, DisplayState: types.Idle,
		}},
	}

	captureStdout(t, p.cleanupSelectedWorktree)
	if gitPathKey(p.confirmWorktree) != gitPathKey(plan.Path) || !tmuxSucceeds("has-session", "-t", "llm-worktree") {
		t.Fatal("first cleanup key did not request confirmation without changing the worktree")
	}
	captureStdout(t, p.cleanupSelectedWorktree)
	if tmuxSucceeds("has-session", "-t", "llm-worktree") {
		t.Fatal("cleanup left the managed agent session running")
	}
	windows, _ := runTmux("list-windows", "-a", "-F", "#{window_id}|#{@llm_worktree_host}|#{pane_current_path}")
	if strings.Contains(windows, hostWindowID+"|") {
		t.Fatalf("cleanup left the llmux-created worktree host window:\n%s", windows)
	}
	if _, err := os.Stat(plan.Path); !os.IsNotExist(err) {
		t.Fatalf("cleanup left worktree directory: %v", err)
	}
	branchOutput, err := exec.Command("git", "-C", repository, "branch", "--list", plan.Branch).CombinedOutput()
	if err != nil || !strings.Contains(string(branchOutput), plan.Branch) {
		t.Fatalf("cleanup did not retain branch %s: %v\n%s", plan.Branch, err, branchOutput)
	}
}

func TestPickerWorkflowInIsolatedTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}

	t.Setenv("TMUX", "")
	socketDir, err := os.MkdirTemp("/tmp", "llm-picker-tmux-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	t.Setenv("TMUX_TMPDIR", socketDir)
	mustTmux(t, "new-session", "-d", "-x", "150", "-y", "36", "-s", "origin", "-n", "shell", "sleep 1000")
	t.Cleanup(func() { _, _ = runTmux("kill-server") })

	addManagedTestSession(t, "llm-zeta", "amp", "/tmp/zeta-project", types.Waiting)
	addManagedTestSession(t, "llm-alpha", "claude", "/tmp/alpha-project", types.Working)
	addManagedTestSession(t, "llm-beta", "opencode", "/tmp/beta-project", types.Idle)
	mustTmux(t, "set-option", "-g", "@llm_session_prefix", "llm-")
	mustTmux(t, "set-option", "-g", "@llm_command", "amp")

	mustTmux(t, "new-window", "-d", "-t", "origin:", "-n", windowName, "sleep 1000")
	mustTmux(t, "split-window", "-d", "-h", "-l", "67%", "-t", "origin:"+windowName+".0", "sleep 1000")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helperCommand := "GO_WANT_PICKER_HELPER_PROCESS=1 " + shellQuote(executable) + " -test.run=^TestPickerHelperProcess$"
	mustTmux(t, "respawn-pane", "-k", "-t", "origin:"+windowName+".0", helperCommand)

	waitFor(t, 5*time.Second, "picker startup", func() bool {
		return strings.Contains(capturePicker(), "NEEDS YOU") && previewTitle() == "LIVE AGENT · zeta-project · amp #0 · prefix u returns"
	})

	// Enter moves into the already-live agent without closing the control room.
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "Enter")
	waitFor(t, 2*time.Second, "live preview focus", func() bool {
		active := mustTmux(t, "display-message", "-p", "-t", "origin:"+windowName+".1", "#{pane_active}")
		return active == "1" && strings.Contains(mustTmux(t, "list-windows", "-t", "origin", "-F", "#{window_name}"), windowName)
	})
	mustTmux(t, "select-pane", "-t", "origin:"+windowName+".0")

	// Pasted command characters outside search mode are inert.
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "-l", "\x1b[200~q\x1b[201~")
	time.Sleep(100 * time.Millisecond)
	if !tmuxSucceeds("has-session", "-t", "origin") || !strings.Contains(mustTmux(t, "list-windows", "-t", "origin", "-F", "#{window_name}"), windowName) {
		t.Fatal("bracketed paste triggered a picker command")
	}

	// Search is semantic, and attention navigation is global rather than
	// being trapped inside a filter.
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "/", "alpha", "Enter")
	waitFor(t, 2*time.Second, "committed filter", func() bool {
		return strings.Contains(capturePicker(), "filter: alpha")
	})
	// A stale creation handoff must expire without silently eating the filter.
	mustTmux(t, "set-option", "-w", "-t", "origin:"+windowName, pickerSelectionOption, "@99999")
	waitFor(t, 4*time.Second, "stale creation handoff cleanup", func() bool {
		return strings.Contains(capturePicker(), "filter: alpha") &&
			mustTmux(t, "show-options", "-wqv", "-t", "origin:"+windowName, pickerSelectionOption) == ""
	})
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "n")
	waitFor(t, 2*time.Second, "global attention navigation", func() bool {
		return !strings.Contains(capturePicker(), "filter:") && previewTitle() == "LIVE AGENT · zeta-project · amp #0 · prefix u returns"
	})

	// A delayed preview update must not pull focus back from the live pane.
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "j")
	mustTmux(t, "select-pane", "-t", "origin:"+windowName+".1")
	waitFor(t, 2*time.Second, "focus-preserving preview update", func() bool {
		active := mustTmux(t, "display-message", "-p", "-t", "origin:"+windowName+".1", "#{pane_active}")
		return active == "1" && previewTitle() == "LIVE AGENT · alpha-project · claude #0 · prefix u returns"
	})
	mustTmux(t, "select-pane", "-t", "origin:"+windowName+".0")

	// This is the add prompt's real handoff contract: remain in the picker,
	// consume the exact new window ID, select it, and preview it.
	newWindowID := addManagedTestSession(t, "llm-delta", "codex", "/tmp/delta-project", types.Idle)
	mustTmux(t, "set-option", "-w", "-t", "origin:"+windowName, pickerSelectionOption, newWindowID)
	waitFor(t, 4*time.Second, "new session handoff", func() bool {
		screen := capturePicker()
		return tmuxSucceeds("has-session", "-t", "origin") &&
			strings.Contains(screen, "4/4") &&
			strings.Contains(screen, "✓ CREATED") &&
			strings.Contains(screen, "e name task · Enter live · o popup") &&
			!strings.Contains(screen, "EDIT TASK LABEL") &&
			previewTitle() == "LIVE AGENT · delta-project · codex #0 · prefix u returns"
	})
	// Creation returns to normal navigation. Naming is an explicit mode entered
	// with e, and that mode occupies a prominent full-width accent area.
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "e")
	waitFor(t, 2*time.Second, "explicit task label editor", func() bool {
		screen := capturePicker()
		return strings.Contains(screen, "EDIT TASK LABEL") &&
			strings.Contains(screen, "delta-project · codex #0") &&
			strings.Contains(screen, "Task ›")
	})
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "-l", "Refactor picker")
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "Enter")
	waitFor(t, 2*time.Second, "task label save", func() bool {
		return mustTmux(t, "show-options", "-wqv", "-t", newWindowID, "@llm_label") == "Refactor picker" &&
			previewTitle() == "LIVE AGENT · delta-project · Refactor picker · codex #0 · prefix u returns"
	})

	// Waiting/working sessions require confirmation, Escape cancels, and the
	// second Ctrl-X performs the stop. Idle sessions stop immediately.
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "n", "C-x")
	waitFor(t, 2*time.Second, "stop confirmation", func() bool {
		return strings.Contains(capturePicker(), "^x confirm")
	})
	if !tmuxSucceeds("has-session", "-t", "llm-zeta") {
		t.Fatal("first Ctrl-X stopped a session that required confirmation")
	}
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "Escape")
	waitFor(t, 2*time.Second, "confirmation cancellation", func() bool {
		return !strings.Contains(capturePicker(), "^x confirm")
	})
	if !tmuxSucceeds("has-session", "-t", "llm-zeta") {
		t.Fatal("Escape did not cancel the pending stop")
	}
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "C-x")
	waitFor(t, 2*time.Second, "second stop confirmation", func() bool {
		return strings.Contains(capturePicker(), "^x confirm")
	})
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "C-x")
	waitFor(t, 2*time.Second, "confirmed stop", func() bool {
		return !tmuxSucceeds("has-session", "-t", "llm-zeta")
	})

	// Find the newly-added idle session by its task label. Idle sessions stop
	// immediately without the working/waiting confirmation step.
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "/", "Refactor picker", "Enter")
	waitFor(t, 2*time.Second, "task-label search", func() bool {
		return strings.Contains(capturePicker(), "filter: Refactor picker")
	})
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "C-x")
	waitFor(t, 2*time.Second, "immediate idle stop", func() bool {
		return !tmuxSucceeds("has-session", "-t", "llm-delta")
	})
	if strings.Contains(capturePicker(), "^x confirm") {
		t.Fatal("idle session unexpectedly required stop confirmation")
	}

	// Clear the now-empty filter. Popup/review failures in this detached test
	// server must leave the persistent Control Room alive rather than destroy it.
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "Escape")
	mustTmux(t, "send-keys", "-t", "origin:"+windowName+".0", "o")
	waitFor(t, 2*time.Second, "persistent popup failure", func() bool {
		return strings.Contains(mustTmux(t, "list-windows", "-t", "origin", "-F", "#{window_name}"), windowName) &&
			strings.Contains(capturePicker(), "no parent tmux client")
	})
}

func addManagedTestSession(t *testing.T, name, agentName, path string, state types.State) string {
	t.Helper()
	mustTmux(t, "new-session", "-d", "-x", "150", "-y", "36", "-s", name, "-n", agentName, "-c", "/tmp", "sleep 1000")
	mustTmux(t, "set-option", "-t", name, "@llm_path", path)
	mustTmux(t, "set-option", "-t", name, "@llm_ever_attached", "1")
	target := name + ":0"
	mustTmux(t, "set-option", "-w", "-t", target, "@llm_agent", agentName)
	mustTmux(t, "set-option", "-w", "-t", target, "@llm_path", path)
	mustTmux(t, "set-option", "-w", "-t", target, "@llm_state", string(state))
	mustTmux(t, "set-option", "-w", "-t", target, "@llm_state_at", fmt.Sprint(time.Now().Unix()))
	return mustTmux(t, "display-message", "-p", "-t", target, "#{window_id}")
}

func capturePicker() string {
	output, _ := runTmux("capture-pane", "-p", "-t", "origin:"+windowName+".0")
	return output
}

func previewTitle() string {
	output, _ := runTmux("display-message", "-p", "-t", "origin:"+windowName+".1", "#{pane_title}")
	return output
}

func clientWindow(client string) string {
	clients, _ := runTmux("list-clients", "-F", "#{client_name}|#{window_name}")
	for _, line := range strings.Split(clients, "\n") {
		parts := strings.SplitN(line, "|", 2)
		if len(parts) == 2 && parts[0] == client {
			return parts[1]
		}
	}
	return ""
}

func waitFor(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s\npicker:\n%s", description, capturePicker())
}

func mustTmux(t *testing.T, args ...string) string {
	t.Helper()
	output, err := runTmux(args...)
	if err != nil {
		t.Fatalf("tmux %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

func runTmux(args ...string) (string, error) {
	command := exec.Command("tmux", args...)
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func tmuxSucceeds(args ...string) bool {
	_, err := runTmux(args...)
	return err == nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func mustGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func captureStdout(t *testing.T, write func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	write()
	os.Stdout = original
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return string(output)
}
