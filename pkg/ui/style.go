package ui

import (
	"os"
	"strings"
	"sync"
)

const (
	ansiReset    = "\033[0m"
	ansiBold     = "\033[1m"
	ansiCyan     = "\033[36m"
	ansiBoldCyan = "\033[1;36m"
	ansiGreen    = "\033[32m"
	ansiYellow   = "\033[33m"
	ansiRed      = "\033[31m"
)

var (
	colorEnabled bool
	colorOnce    sync.Once
)

func IsColorEnabled() bool {
	colorOnce.Do(func() {
		colorEnabled = detectColorSupport()
	})
	return colorEnabled
}

func detectColorSupport() bool {
	if strings.TrimSpace(os.Getenv("NO_COLOR")) != "" {
		return false
	}

	switch strings.ToLower(strings.TrimSpace(os.Getenv("TRACER_COLOR"))) {
	case "always", "1", "true":
		return true
	case "never", "0", "false":
		return false
	}

	term := strings.TrimSpace(os.Getenv("TERM"))
	if term == "" || term == "dumb" {
		return false
	}

	stdoutInfo, stdoutErr := os.Stdout.Stat()
	if stdoutErr == nil && stdoutInfo.Mode()&os.ModeCharDevice != 0 {
		return true
	}

	stderrInfo, stderrErr := os.Stderr.Stat()
	return stderrErr == nil && stderrInfo.Mode()&os.ModeCharDevice != 0
}

func style(text string, code string) string {
	if text == "" || !IsColorEnabled() {
		return text
	}
	return code + text + ansiReset
}

func Section(text string) string {
	return style(strings.ToUpper(text), ansiBoldCyan)
}

func Command(text string) string {
	return style(text, ansiCyan)
}

func Success(text string) string {
	return style(text, ansiGreen)
}

func Warning(text string) string {
	return style(text, ansiYellow)
}

func Error(text string) string {
	return style(text, ansiRed)
}

func Bold(text string) string {
	return style(text, ansiBold)
}
