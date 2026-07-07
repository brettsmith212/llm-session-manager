package initcmd

import (
	"fmt"
	"os"
)

// zshHook is the shell code emitted for `llmux init zsh`.
// It pre-warms an llmux session when cd'ing into a git project root: the
// session boots in the background (hidden from the picker) so that launching
// is near-instant. Only fires inside tmux and at dirs containing .git.
const zshHook = `
_llmux_warm() {
  [[ -n "$TMUX" ]] || return
  [[ -e .git ]] || return
  command llmux warm "$PWD" &!
}
chpwd_functions+=(_llmux_warm)
`

// bashHook is the shell code emitted for `llmux init bash`.
// Bash has no cd hook, so we piggyback on PROMPT_COMMAND (fires every prompt).
// A last-PWD guard skips redundant warm calls when the directory hasn't
// changed since the previous prompt — without it, every Enter would spawn a
// subprocess even though the session already exists. The guard is updated
// before the .git check so that cd'ing away (to a non-git dir) and back
// re-evaluates correctly.
const bashHook = `
_llmux_warm() {
  [[ -n "$TMUX" ]] || return
  [[ "$PWD" == "$_LLMUX_LAST_WARM" ]] && return
  _LLMUX_LAST_WARM="$PWD"
  [[ -e .git ]] || return
  command llmux warm "$PWD" &>/dev/null & disown
}
PROMPT_COMMAND="_llmux_warm${PROMPT_COMMAND:+;$PROMPT_COMMAND}"
`

// Run emits the shell hook for the given shell.
func Run(shell string) error {
	switch shell {
	case "zsh":
		fmt.Print(zshHook)
	case "bash":
		fmt.Print(bashHook)
	default:
		fmt.Fprintf(os.Stderr, "unsupported shell: %s (only zsh and bash are supported)\n", shell)
		os.Exit(1)
	}
	return nil
}
