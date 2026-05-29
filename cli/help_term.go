package cli

import (
	"io"
	"os"

	"golang.org/x/term"
)

// termInfo carries the display targets the help renderer needs:
// available width (for word-wrap) and whether ANSI colour escapes
// are appropriate.
type termInfo struct {
	width int
	color bool
}

const defaultWidth = 80

// detectTermInfo inspects w to decide rendering parameters. Width
// comes from [term.GetSize] when w is a terminal; otherwise the
// default width is used. Colour is enabled only when w is a
// terminal AND the NO_COLOR environment variable is unset, per
// https://no-color.org/.
func detectTermInfo(w io.Writer) termInfo {
	info := termInfo{width: defaultWidth}
	f, ok := w.(*os.File)
	if !ok {
		return info
	}
	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		return info
	}
	if cols, _, err := term.GetSize(fd); err == nil && cols > 0 {
		info.width = cols
	}
	if os.Getenv("NO_COLOR") == "" {
		info.color = true
	}
	return info
}

func (t termInfo) bold(s string) string {
	if !t.color {
		return s
	}
	return "\033[1m" + s + "\033[0m"
}
