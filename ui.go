package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// ui is the package-global terminal UI helper. It supports ANSI color when
// stdout is a TTY, and falls back to plain text otherwise.
var ui = newTermUI()

type termUI struct {
	mu    sync.Mutex
	color bool
}

func newTermUI() *termUI {
	fi, _ := os.Stdout.Stat()
	isTTY := fi != nil && (fi.Mode()&os.ModeCharDevice) != 0
	if os.Getenv("NO_COLOR") != "" {
		isTTY = false
	}
	return &termUI{color: isTTY}
}

const (
	cReset  = "\x1b[0m"
	cBold   = "\x1b[1m"
	cDim    = "\x1b[2m"
	cRed    = "\x1b[31m"
	cGreen  = "\x1b[32m"
	cYellow = "\x1b[33m"
	cBlue   = "\x1b[34m"
	cPurple = "\x1b[35m"
	cCyan   = "\x1b[36m"
)

func (t *termUI)wrap(c, s string) string {
	if !t.color {
		return s
	}
	return c + s + cReset
}

func (t *termUI)Info(format string, a ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(os.Stdout, "%s %s\n", t.wrap(cCyan, "›"), fmt.Sprintf(format, a...))
}

func (t *termUI)OK(format string, a ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(os.Stdout, "%s %s\n", t.wrap(cGreen, "✓"), fmt.Sprintf(format, a...))
}

func (t *termUI)Warn(format string, a ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(os.Stderr, "%s %s\n", t.wrap(cYellow, "!"), fmt.Sprintf(format, a...))
}

func (t *termUI)Fail(format string, a ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(os.Stderr, "%s %s\n", t.wrap(cRed, "✗"), fmt.Sprintf(format, a...))
}

func (t *termUI)Dim(format string, a ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(os.Stdout, "  %s\n", t.wrap(cDim, fmt.Sprintf(format, a...)))
}

func (t *termUI)Section(title string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(os.Stdout, "\n%s %s\n", t.wrap(cBold+cPurple, "▸"), t.wrap(cBold, title))
}

func (t *termUI)Banner() {
	t.mu.Lock()
	defer t.mu.Unlock()
	logo := "smartmail"
	fmt.Fprintf(os.Stdout, "\n%s %s\n", t.wrap(cBold+cBlue, logo), t.wrap(cDim, "· LLM-powered inbox organizer"))
	fmt.Fprintf(os.Stdout, "%s\n\n", t.wrap(cDim, strings.Repeat("─", 48)))
}

// Prompt reads a single line, with optional default value shown in brackets.
func (t *termUI)Prompt(label, def string) string {
	if def != "" {
		fmt.Fprintf(os.Stdout, "%s %s %s ", t.wrap(cCyan, "?"), label, t.wrap(cDim, "["+def+"]"))
	} else {
		fmt.Fprintf(os.Stdout, "%s %s ", t.wrap(cCyan, "?"), label)
	}
	var out string
	fmt.Scanln(&out)
	out = strings.TrimSpace(out)
	if out == "" {
		return def
	}
	return out
}

// Decision renders a single classification outcome as a compact, readable line.
func (t *termUI)Decision(subject, from, action, folder, reason string, dry bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	icon := t.wrap(cGreen, "→")
	switch action {
	case "spam":
		icon = t.wrap(cRed, "✗")
	case "keep":
		icon = t.wrap(cYellow, "★")
	}
	prefix := ""
	if dry {
		prefix = t.wrap(cDim, "[dry] ")
	}
	subj := truncate(subject, 50)
	fr := truncate(from, 32)
	fmt.Fprintf(os.Stdout, "%s%s %-50s %s %s  %s\n",
		prefix, icon,
		t.wrap(cBold, subj),
		t.wrap(cDim, "←"),
		t.wrap(cDim, fr),
		t.wrap(cPurple, folder),
	)
	if reason != "" {
		fmt.Fprintf(os.Stdout, "    %s %s\n", t.wrap(cDim, "why:"), t.wrap(cDim, truncate(reason, 120)))
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// Progress emits a single-line progress update (overwrites previous line).
func (t *termUI)Progress(current, total int, what string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.color {
		fmt.Fprintf(os.Stdout, "\r%s %d/%d %s", t.wrap(cDim, "·"), current, total, t.wrap(cDim, what))
	} else {
		fmt.Fprintf(os.Stdout, "%d/%d %s\n", current, total, what)
	}
}

func (t *termUI)ProgressDone() {
	if t.color {
		fmt.Fprintln(os.Stdout)
	}
}

func since(t0 time.Time) string {
	d := time.Since(t0).Round(time.Millisecond)
	return d.String()
}
