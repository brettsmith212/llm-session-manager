package picker

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"llm-session-manager/internal/worktree"
)

const (
	gitCommandTimeout = 1500 * time.Millisecond
	// gitInspectWorkers bounds how many checkouts are inspected at once.
	gitInspectWorkers = 4
)

var errGitTimeout = errors.New("git timed out")

// gitRefresh is the result of one background inspection pass. failed holds
// the keys of checkouts whose inspection timed out; their previous metadata
// is kept rather than dropped.
type gitRefresh struct {
	info   map[string]gitInfo
	failed map[string]bool
}

type gitInfo struct {
	valid           bool
	branch          string
	changed         int
	additions       int
	deletions       int
	untracked       int
	checkoutPath    string
	commonDir       string
	linkedWorktree  bool
	worktreeLabel   string
	managedWorktree bool
	repository      string
	baseCommit      string // where an llmux worktree started
	ahead           int    // commits an llmux worktree has made since baseCommit
	merged          bool   // those commits are on the branch the worktree started from
}

func gitPathKey(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

func collectGitInfo(paths []string) gitRefresh {
	result := gitRefresh{info: make(map[string]gitInfo, len(paths)), failed: make(map[string]bool)}
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, gitInspectWorkers)
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		key := gitPathKey(path)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		wg.Add(1)
		slots <- struct{}{}
		go func(key, path string) {
			defer wg.Done()
			defer func() { <-slots }()
			info, err := inspectGitChecked(path)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, errGitTimeout):
				result.failed[key] = true
			case info.valid:
				result.info[key] = info
			}
		}(key, path)
	}
	wg.Wait()
	return result
}

func inspectGit(path string) gitInfo {
	info, _ := inspectGitChecked(path)
	return info
}

// inspectGitChecked reads a checkout's branch and working-tree summary. Its
// error is errGitTimeout when git was too slow to answer, which says nothing
// about whether path is a repository.
func inspectGitChecked(path string) (gitInfo, error) {
	status, err := runGit(path, "status", "--porcelain=v2", "--branch", "--untracked-files=normal")
	if err != nil {
		return gitInfo{}, err
	}

	info := gitInfo{valid: true}
	if identity, err := runGit(path, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-dir", "--git-common-dir"); err == nil {
		parts := strings.Split(identity, "\n")
		if len(parts) >= 3 {
			info.checkoutPath = gitPathKey(strings.TrimSpace(parts[0]))
			gitDir := gitPathKey(strings.TrimSpace(parts[1]))
			info.commonDir = gitPathKey(strings.TrimSpace(parts[2]))
			info.linkedWorktree = gitDir != "" && info.commonDir != "" && gitDir != info.commonDir
		}
	}
	if manifest, err := worktree.Load(path); err == nil {
		info.managedWorktree = true
		info.repository = manifest.Repository
		info.worktreeLabel = manifest.Label
		info.baseCommit = manifest.BaseCommit
		inspectWorktreeProgress(path, manifest, &info)
	}
	oid := ""
	for _, line := range strings.Split(status, "\n") {
		switch {
		case strings.HasPrefix(line, "# branch.head "):
			info.branch = strings.TrimPrefix(line, "# branch.head ")
		case strings.HasPrefix(line, "# branch.oid "):
			oid = strings.TrimPrefix(line, "# branch.oid ")
		case strings.HasPrefix(line, "1 "), strings.HasPrefix(line, "2 "), strings.HasPrefix(line, "u "):
			info.changed++
		case strings.HasPrefix(line, "? "):
			info.untracked++
		}
	}
	if info.branch == "(detached)" && oid != "" && oid != "(initial)" {
		if len(oid) > 7 {
			oid = oid[:7]
		}
		info.branch = "detached@" + oid
	}
	if info.branch == "" || info.branch == "(detached)" {
		info.branch = "git"
	}

	// Untracked files never appear in the diff, so a checkout without tracked
	// changes needs no second pass over its files.
	if info.changed == 0 {
		return info, nil
	}
	if diff, err := runGit(path, "diff", "--numstat", "HEAD", "--"); err == nil {
		for _, line := range strings.Split(diff, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			if additions, err := strconv.Atoi(fields[0]); err == nil {
				info.additions += additions
			}
			if deletions, err := strconv.Atoi(fields[1]); err == nil {
				info.deletions += deletions
			}
		}
	}
	return info, nil
}

// inspectWorktreeProgress counts the commits an llmux worktree has made since
// it branched, and whether the branch it started from already contains them.
// An unanswered question leaves the zero value, which shows nothing.
func inspectWorktreeProgress(path string, manifest worktree.Manifest, info *gitInfo) {
	if manifest.BaseCommit == "" {
		return
	}
	count, err := runGit(path, "rev-list", "--count", manifest.BaseCommit+"..HEAD")
	if err != nil {
		return
	}
	info.ahead, _ = strconv.Atoi(count)
	if info.ahead == 0 || manifest.BaseBranch == "" {
		return
	}
	// merge-base exits 1 when HEAD is not an ancestor, which runGit reports
	// as an error.
	_, err = runGit(path, "merge-base", "--is-ancestor", "HEAD", "refs/heads/"+manifest.BaseBranch)
	info.merged = err == nil
}

// runGit runs a read-only git command for the Control Room. Agents run git in
// these same checkouts, so --no-optional-locks keeps status from taking
// .git/index.lock to refresh the index, which would make their commands fail.
// A slow command is stopped with SIGTERM so git can clean up after itself.
func runGit(path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitCommandTimeout)
	defer cancel()
	commandArgs := append([]string{"--no-optional-locks", "-C", path}, args...)
	cmd := exec.CommandContext(ctx, "git", commandArgs...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 500 * time.Millisecond
	output, err := cmd.Output()
	if ctx.Err() != nil {
		return "", errGitTimeout
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func formatGitInfo(info gitInfo) string {
	if !info.valid {
		return ""
	}
	parts := []string{info.branch}
	if info.changed == 0 && info.untracked == 0 {
		parts = append(parts, "clean")
		if info.managedWorktree {
			parts = append(parts, "isolated")
		}
		return strings.Join(parts, " · ")
	}
	if info.changed > 0 {
		files := "files"
		if info.changed == 1 {
			files = "file"
		}
		parts = append(parts, fmt.Sprintf("%d %s", info.changed, files))
		if info.additions > 0 {
			parts = append(parts, fmt.Sprintf("+%d", info.additions))
		}
		if info.deletions > 0 {
			parts = append(parts, fmt.Sprintf("-%d", info.deletions))
		}
	}
	if info.untracked > 0 {
		parts = append(parts, fmt.Sprintf("?%d", info.untracked))
	}
	if info.managedWorktree {
		parts = append(parts, "isolated")
	}
	return strings.Join(parts, " · ")
}
