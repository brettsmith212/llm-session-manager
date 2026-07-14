package picker

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const gitCommandTimeout = 750 * time.Millisecond

type gitInfo struct {
	valid     bool
	branch    string
	changed   int
	additions int
	deletions int
	untracked int
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

func collectGitInfo(paths []string) map[string]gitInfo {
	result := make(map[string]gitInfo, len(paths))
	for _, path := range paths {
		key := gitPathKey(path)
		if key == "" {
			continue
		}
		if _, exists := result[key]; exists {
			continue
		}
		if info := inspectGit(path); info.valid {
			result[key] = info
		}
	}
	return result
}

func inspectGit(path string) gitInfo {
	status, ok := runGit(path, "status", "--porcelain=v2", "--branch", "--untracked-files=normal")
	if !ok {
		return gitInfo{}
	}

	info := gitInfo{valid: true}
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

	if diff, ok := runGit(path, "diff", "--numstat", "HEAD", "--"); ok {
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
	return info
}

func runGit(path string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), gitCommandTimeout)
	defer cancel()
	commandArgs := append([]string{"-C", path}, args...)
	output, err := exec.CommandContext(ctx, "git", commandArgs...).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(output)), true
}

func formatGitInfo(info gitInfo) string {
	if !info.valid {
		return ""
	}
	parts := []string{info.branch}
	if info.changed == 0 && info.untracked == 0 {
		parts = append(parts, "clean")
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
	return strings.Join(parts, " · ")
}
