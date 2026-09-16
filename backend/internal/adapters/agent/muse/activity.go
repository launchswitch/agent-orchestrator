package muse

import (
	"regexp"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

var museTerminalEscape = regexp.MustCompile(`\x1b(?:\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\][^\x07]*(?:\x07|\x1b\\))`)

// DeriveActivityState maps Muse's AO hook callbacks onto activity states.
// permission-request stays blocked (not waiting_input): it is a pending tool
// decision where automated input must never land. Blocked clears at the next
// stop; without a pre/post-tool-use trio it cannot clear mid-turn, so the
// adapter deliberately does not implement ports.BlockedActivitySignaler.
func DeriveActivityState(event string, _ []byte) (domain.ActivityState, bool) {
	switch event {
	case "user-prompt-submit":
		return domain.ActivityActive, true
	case "permission-request":
		return domain.ActivityBlocked, true
	case "stop":
		return domain.ActivityIdle, true
	case "session-end":
		return domain.ActivityExited, true
	case "session-start":
		return "", false
	default:
		return "", false
	}
}

// ContinuouslyDetectTerminalActivity opts Muse into terminal reconciliation
// on every observer tick. Unlike hook-driven agents, Muse's structured input
// picker emits no lifecycle callback, and its answer likewise emits no prompt
// callback, so both pause and resume must be observed from the TUI.
func (p *Plugin) ContinuouslyDetectTerminalActivity() bool { return true }

// TerminalActivityUsesRenderedScreen opts Muse into rendered-screen sampling.
// Muse repaints its live region with cursor control, and the pty-host's
// buffered output keeps every repaint: after a turn ends, the completion's
// line erase never reaches the buffered text, so the finished turn's
// "· esc to interrupt" status outlives the idle composer. The rendered screen
// already has the erase applied and shows only the current viewport.
func (p *Plugin) TerminalActivityUsesRenderedScreen() bool { return true }

// DetectTerminalActivity recognizes authoritative states in Meta Muse's TUI.
// Only markers inside the newest output block can describe the session: the
// captured stream keeps earlier repaints, so a finished turn's
// "· esc to interrupt" status line can sit above the idle composer after a
// paint. The newest marker inside the current block still wins, so the picker
// and resumed-generation states keep resolving.
func (p *Plugin) DetectTerminalActivity(output string) (domain.ActivityState, bool) {
	lines := museTerminalLines(output)
	if len(lines) == 0 {
		return "", false
	}
	start := len(lines) - 30
	if start < 0 {
		start = 0
	}
	current := museCurrentBlock(lines[start:])

	for i := len(current) - 1; i >= 0; i-- {
		line := strings.ToLower(current[i])
		if strings.HasPrefix(line, "◆ request user input") {
			return domain.ActivityWaitingInput, true
		}
		if strings.Contains(line, "· esc to interrupt") && !strings.Contains(line, "enter to select") {
			return domain.ActivityActive, true
		}
		if strings.Contains(line, "enter to select") && strings.Contains(line, "optional note") &&
			strings.Contains(line, "esc to interrupt") {
			return domain.ActivityWaitingInput, true
		}
	}

	hasComposer := false
	hasFooter := false
	for _, line := range current {
		// An empty composer is a bare glyph: Muse 1.2 renders ⟩, current
		// releases render ❯. Requiring the exact line keeps composer drafts
		// from being mistaken for idle.
		if line == "⟩" || line == "❯" {
			hasComposer = true
		}
		if strings.Contains(line, " · ") && strings.Contains(line, "muse-") {
			hasFooter = true
		}
	}
	if hasComposer && hasFooter {
		return domain.ActivityIdle, true
	}
	return "", false
}

// museBlockAnchor reports whether a line begins a Muse block that can still be
// the live one. Muse leads assistant output, its turn status line, and the
// structured input picker with these glyphs, so the newest anchor opens the
// only region whose markers describe the session right now. Lines above it are
// retained output from an earlier paint.
func museBlockAnchor(line string) bool {
	return strings.HasPrefix(line, "◆") || strings.HasPrefix(line, "◇")
}

// museCurrentBlock trims retained repaints above the newest block anchor. A
// window with no anchor at all (glyph-less provider output, or anchors already
// scrolled out of the capture) is returned unchanged so marker matching still
// applies to what is visible.
func museCurrentBlock(recent []string) []string {
	for i := len(recent) - 1; i >= 0; i-- {
		if museBlockAnchor(recent[i]) {
			return recent[i:]
		}
	}
	return recent
}

func museTerminalLines(output string) []string {
	plain := museTerminalEscape.ReplaceAllString(strings.ReplaceAll(output, "\r", "\n"), "")
	raw := strings.Split(plain, "\n")
	lines := raw[:0]
	for _, line := range raw {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
