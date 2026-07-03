package ansi

import "fmt"

// RGB is an ANSI 24-bit color.
type RGB [3]int

// Catppuccin Mocha palette.
var (
	Base     = RGB{30, 30, 46}
	Mantle   = RGB{24, 24, 37}
	Crust    = RGB{17, 17, 27}
	Surface0 = RGB{49, 50, 68}
	Surface1 = RGB{69, 71, 90}
	Surface2 = RGB{88, 91, 112}
	Overlay0 = RGB{108, 112, 134}
	Overlay2 = RGB{147, 153, 178}
	Subtext0 = RGB{166, 173, 200}
	Text     = RGB{205, 214, 244}
	Blue     = RGB{137, 180, 250}
	Green    = RGB{166, 227, 161}
	Red      = RGB{243, 139, 168}
	Yellow   = RGB{249, 226, 175}
	Mauve    = RGB{203, 166, 247}
	Peach    = RGB{250, 179, 135}
	Teal     = RGB{148, 226, 213}
)

const (
	Reset      = "\x1b[0m"
	Bold       = "\x1b[1m"
	ClearScreen = "\x1b[2J\x1b[H"
	HideCursor  = "\x1b[?25l"
	ShowCursor  = "\x1b[?25h"
)

// Foreground returns a 24-bit foreground ANSI sequence.
func Foreground(c RGB) string {
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c[0], c[1], c[2])
}

// Background returns a 24-bit background ANSI sequence.
func Background(c RGB) string {
	return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", c[0], c[1], c[2])
}

// CursorPos returns an ANSI cursor positioning sequence (1-indexed).
func CursorPos(row, col int) string {
	return fmt.Sprintf("\x1b[%d;%dH", row, col)
}

// ClearLine returns an ANSI clear-line sequence.
func ClearLine() string {
	return "\x1b[2K"
}
