package muse

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
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
		{"plain idle composer 1.3", "idle_composer_1_3.txt", domain.ActivityIdle},
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

func TestDetectTerminalActivityMuse13ComposerArtifacts(t *testing.T) {
	// Muse 1.3 on a terminal that answers OSC color queries appends the replies
	// to the composer row, so the row is not exactly the glyph. Prefix
	// recognition must still read it as the idle composer (live capture).
	output := "◆ Worked for 9m 03s · 3:17 PM\n" +
		"\n" +
		"────────────────────────────────────────────────────────────────\n" +
		"❯ ]10;rgb:2424/2929/2f2f\\]11;rgb:f5f5/f5f5/f4f4\\\n" +
		"────────────────────────────────────────────────────────────────\n" +
		"  muse-spark-1.3-contributor · max · …/montamer2-90 · YOLO\n"
	got, ok := (&Plugin{}).DetectTerminalActivity(output)
	if got != domain.ActivityIdle || !ok {
		t.Fatalf("DetectTerminalActivity(muse 1.3 polluted composer) = (%q, %v), want (%q, true)", got, ok, domain.ActivityIdle)
	}
}

func TestDetectTerminalActivityFromViewport(t *testing.T) {
	if !(&Plugin{}).DetectTerminalActivityFromViewport() {
		t.Fatal("muse must read activity from the rendered viewport")
	}
}
