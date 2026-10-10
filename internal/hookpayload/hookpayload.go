// Package hookpayload extracts the short, human-readable details the Control
// Room shows from agent hook payloads: what a permission prompt wants to run,
// what a question asks and how a plan begins. Payload shapes differ between
// agents and releases, so every helper degrades to "" rather than failing.
package hookpayload

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
)

// detailKeys are the tool input fields that best describe a tool call, in the
// order they are tried.
var detailKeys = []string{"command", "cmd", "file_path", "notebook_path", "path", "url", "query", "pattern", "description", "prompt"}

// ToolSummary describes a tool call as "<tool> · <detail>", for example
// "Bash · npm run db:migrate" or "Edit · picker.go".
func ToolSummary(tool string, input json.RawMessage) string {
	name := displayTool(tool)
	detail := ""
	fields := object(input)
	if strings.Contains(strings.ToLower(tool), "patch") {
		detail = patchFile(stringField(fields, "input"))
	}
	for _, key := range detailKeys {
		if detail != "" {
			break
		}
		raw, ok := fields[key]
		if !ok {
			continue
		}
		switch key {
		case "command", "cmd":
			detail = commandText(raw)
		case "file_path", "notebook_path", "path":
			if value := text(raw); value != "" {
				detail = filepath.Base(value)
			}
		case "url":
			detail = host(text(raw))
		default:
			detail = text(raw)
		}
	}
	detail = firstLine(detail)
	switch {
	case name == "":
		return detail
	case detail == "":
		return name
	default:
		return name + " · " + detail
	}
}

// Question returns the first question a question tool asks.
func Question(input json.RawMessage) string {
	fields := object(input)
	var questions []map[string]json.RawMessage
	if raw, ok := fields["questions"]; ok && json.Unmarshal(raw, &questions) == nil {
		for _, question := range questions {
			if value := firstLine(stringField(question, "question")); value != "" {
				return value
			}
		}
	}
	for _, key := range []string{"question", "prompt", "message"} {
		if value := firstLine(stringField(fields, key)); value != "" {
			return value
		}
	}
	return ""
}

// PlanTitle returns the first line of a proposed plan without its Markdown
// heading or list markers.
func PlanTitle(input json.RawMessage) string {
	for _, line := range strings.Split(stringField(object(input), "plan"), "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#*->"))
		if line != "" {
			return line
		}
	}
	return ""
}

// ErrorText describes a failed turn from an error value that may be a string
// or an object with a message, falling back to the first non-empty alternative.
func ErrorText(raw json.RawMessage, alternatives ...string) string {
	if value := text(raw); value != "" {
		return firstLine(value)
	}
	fields := object(raw)
	for _, key := range []string{"message", "type", "code"} {
		if value := firstLine(stringField(fields, key)); value != "" {
			return value
		}
	}
	for _, value := range alternatives {
		if value = firstLine(value); value != "" {
			return value
		}
	}
	return ""
}

func displayTool(tool string) string {
	if rest, ok := strings.CutPrefix(tool, "mcp__"); ok {
		return strings.ReplaceAll(rest, "__", " · ")
	}
	return tool
}

// commandText reads a command given as a string or as an argument vector,
// unwrapping the `bash -lc "<script>"` form Codex uses.
func commandText(raw json.RawMessage) string {
	if value := text(raw); value != "" {
		return value
	}
	var args []string
	if json.Unmarshal(raw, &args) != nil || len(args) == 0 {
		return ""
	}
	if len(args) >= 3 && isShell(args[0]) && (args[1] == "-c" || args[1] == "-lc") {
		return args[2]
	}
	return strings.Join(args, " ")
}

func isShell(name string) bool {
	switch filepath.Base(name) {
	case "bash", "sh", "zsh", "fish":
		return true
	}
	return false
}

// patchFile names the first file an apply_patch body touches.
func patchFile(patch string) string {
	for _, line := range strings.Split(patch, "\n") {
		if _, path, ok := strings.Cut(line, " File: "); ok && strings.HasPrefix(line, "***") {
			return filepath.Base(strings.TrimSpace(path))
		}
	}
	return ""
}

func host(value string) string {
	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return value
}

func object(raw json.RawMessage) map[string]json.RawMessage {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	return fields
}

func stringField(fields map[string]json.RawMessage, key string) string {
	return text(fields[key])
}

func text(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func firstLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
