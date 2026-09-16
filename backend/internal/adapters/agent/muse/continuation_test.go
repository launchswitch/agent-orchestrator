package muse

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const museContinuationTestSessionID = "01a06a28-e10e-7860-ab58-426733be015c"

func writeMuseSessionFixture(t *testing.T, configDir, sessionID string) string {
	t.Helper()
	transcript := filepath.Join(configDir, "sessions", "2026", "09", "03", sessionID, "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return transcript
}

func TestLocateAndProbeNativeSessionTranscript(t *testing.T) {
	configDir := t.TempDir()
	transcript := writeMuseSessionFixture(t, configDir, museContinuationTestSessionID)

	ref := ports.NativeSessionRef{NativeSessionID: museContinuationTestSessionID, ConfigDir: configDir}
	p := &Plugin{}
	path, ok, err := p.LocateTranscript(context.Background(), ref)
	if err != nil || !ok {
		t.Fatalf("LocateTranscript = (%q, %v, %v), want transcript", path, ok, err)
	}
	if path != transcript {
		t.Fatalf("path = %q, want %q", path, transcript)
	}
	availability, err := p.ProbeNativeSession(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if availability != ports.NativeSessionAvailabilityAvailable {
		t.Fatalf("availability = %q, want available", availability)
	}
}

func TestLocateTranscriptMissingSession(t *testing.T) {
	configDir := t.TempDir()
	ref := ports.NativeSessionRef{NativeSessionID: museContinuationTestSessionID, ConfigDir: configDir}
	p := &Plugin{}
	path, ok, err := p.LocateTranscript(context.Background(), ref)
	if err != nil || ok || path != "" {
		t.Fatalf("LocateTranscript = (%q, %v, %v), want not found", path, ok, err)
	}
	availability, err := p.ProbeNativeSession(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if availability != ports.NativeSessionAvailabilityUnavailable {
		t.Fatalf("availability = %q, want unavailable", availability)
	}
}

func TestLocateTranscriptRejectsTraversalID(t *testing.T) {
	configDir := t.TempDir()
	p := &Plugin{}
	for _, id := range []string{"", "   ", "../escape", "a/b", `a\b`} {
		if path, ok, err := p.LocateTranscript(context.Background(), ports.NativeSessionRef{NativeSessionID: id, ConfigDir: configDir}); err == nil || ok || path != "" {
			t.Fatalf("LocateTranscript(%q) = (%q, %v, %v), want invalid-id error", id, path, ok, err)
		}
	}
}

func TestProbeNativeSessionUnknownWithoutConfigDir(t *testing.T) {
	availability, err := (&Plugin{}).ProbeNativeSession(context.Background(), ports.NativeSessionRef{
		NativeSessionID: museContinuationTestSessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if availability != ports.NativeSessionAvailabilityUnknown {
		t.Fatalf("availability = %q, want unknown", availability)
	}
}

func TestContinuationCapabilitiesAreProviderAssigned(t *testing.T) {
	caps := (&Plugin{}).ContinuationCapabilities()
	if caps.FreshNativeSessionID != ports.FreshNativeSessionIDProviderAssigned {
		t.Fatalf("FreshNativeSessionID = %q, want provider_assigned", caps.FreshNativeSessionID)
	}
}

func TestNativeSessionConfigDirHonorsXDGDataHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir, err := (&Plugin{}).NativeSessionConfigDir(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(os.Getenv("XDG_DATA_HOME"), "muse")
	if dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
}

func TestNativeSessionConfigDirLaunchEnvWins(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	launch := t.TempDir()
	dir, err := (&Plugin{}).NativeSessionConfigDir(context.Background(), map[string]string{"XDG_DATA_HOME": launch})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(launch, "muse")
	if dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
}

func TestNativeSessionConfigDirEmptyLaunchValueFallsBackHome(t *testing.T) {
	daemon := t.TempDir()
	t.Setenv("XDG_DATA_HOME", daemon)
	home := t.TempDir()
	dir, err := (&Plugin{}).NativeSessionConfigDir(context.Background(), map[string]string{"XDG_DATA_HOME": "  ", "HOME": home})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".local", "share", "muse")
	if dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
}
