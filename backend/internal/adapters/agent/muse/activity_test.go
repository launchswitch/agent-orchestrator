package muse

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func readMuseFixture(t *testing.T, name string) string {
	t.Helper()
	output, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func TestDeriveActivityState(t *testing.T) {
	tests := []struct {
		name   string
		event  string
		want   domain.ActivityState
		wantOK bool
	}{
		{"prompt submitted", "user-prompt-submit", domain.ActivityActive, true},
		{"permission requested", "permission-request", domain.ActivityBlocked, true},
		{"turn stopped", "stop", domain.ActivityIdle, true},
		{"session start is metadata only", "session-start", "", false},
		{"session ended", "session-end", domain.ActivityExited, true},
		{"unknown", "unknown", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := DeriveActivityState(tt.event, nil)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("DeriveActivityState(%q) = (%q, %v), want (%q, %v)", tt.event, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestDetectTerminalActivityCapturedMuseFrames(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    domain.ActivityState
	}{
		{"awaiting structured input", "awaiting_user_input.txt", domain.ActivityWaitingInput},
		{"awaiting compact structured input", "awaiting_user_input_compact.txt", domain.ActivityWaitingInput},
		{"resumed generation", "active_generation.txt", domain.ActivityActive},
		{"plain idle composer", "idle_composer.txt", domain.ActivityIdle},
		{"idle after a paint retained a finished generation", "idle_with_retained_generation.txt", domain.ActivityIdle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := (&Plugin{}).DetectTerminalActivity(readMuseFixture(t, tt.fixture))
			if got != tt.want || !ok {
				t.Fatalf("DetectTerminalActivity(%s) = (%q, %v), want (%q, true)", tt.fixture, got, ok, tt.want)
			}
		})
	}
}

func TestDetectTerminalActivityIgnoresRetainedGenerationMarker(t *testing.T) {
	// montamer2-83: after the stop hook reported idle, a paint re-emitted the
	// TUI below the finished turn's retained "· esc to interrupt" status line.
	// The capture therefore contains a completed generation frame above an idle
	// composer, and the oldest marker in the window must not win.
	got, ok := (&Plugin{}).DetectTerminalActivity(readMuseFixture(t, "idle_with_retained_generation.txt"))
	if got != domain.ActivityIdle || !ok {
		t.Fatalf("DetectTerminalActivity(retained generation above idle composer) = (%q, %v), want (%q, true)", got, ok, domain.ActivityIdle)
	}
}

func TestDetectTerminalActivityUsesNewestMarker(t *testing.T) {
	waiting := readMuseFixture(t, "awaiting_user_input.txt")
	active := readMuseFixture(t, "active_generation.txt")
	tests := []struct {
		name   string
		output string
		want   domain.ActivityState
	}{
		{"generation after picker", waiting + "\n" + active, domain.ActivityActive},
		{"picker after generation", active + "\n" + waiting, domain.ActivityWaitingInput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := (&Plugin{}).DetectTerminalActivity(tt.output)
			if got != tt.want || !ok {
				t.Fatalf("DetectTerminalActivity() = (%q, %v), want (%q, true)", got, ok, tt.want)
			}
		})
	}
}

func TestDetectTerminalActivityRejectsTranscriptText(t *testing.T) {
	got, ok := (&Plugin{}).DetectTerminalActivity("The documentation says Enter to select an optional note.\n")
	if ok {
		t.Fatalf("DetectTerminalActivity(transcript) = (%q, true), want no signal", got)
	}
}

func TestPluginSamplesRenderedScreen(t *testing.T) {
	// Muse repaints its live region with cursor control, so buffered output can
	// retain a status line the rendered screen has already erased. The observer
	// only knows to prefer the rendered screen when the adapter opts in.
	var detector ports.RenderedScreenTerminalActivityDetector = &Plugin{}
	if !detector.TerminalActivityUsesRenderedScreen() {
		t.Fatal("TerminalActivityUsesRenderedScreen() = false, want true")
	}
}
