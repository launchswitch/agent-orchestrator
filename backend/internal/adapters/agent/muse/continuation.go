package muse

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var (
	_ ports.AgentContinuationCapabilityProvider = (*Plugin)(nil)
	_ ports.AgentNativeSessionConfigProvider    = (*Plugin)(nil)
	_ ports.AgentNativeSessionProber            = (*Plugin)(nil)
	_ ports.AgentTranscriptLocator              = (*Plugin)(nil)
)

// ContinuationCapabilities reports that Muse assigns fresh native session ids,
// which AO captures through SessionStart hooks.
func (p *Plugin) ContinuationCapabilities() ports.ContinuationCapabilities {
	return ports.ContinuationCapabilities{
		FreshNativeSessionID: ports.FreshNativeSessionIDProviderAssigned,
	}
}

// NativeSessionConfigDir returns Muse's data directory, the state root whose
// sessions/ subtree holds the durable session.jsonl logs. It honors
// XDG_DATA_HOME the way the Muse launcher does, falling back to
// ~/.local/share/muse. An explicitly empty launch value selects the home
// default rather than the daemon environment, matching nativeconfig.Resolve.
func (p *Plugin) NativeSessionConfigDir(ctx context.Context, env map[string]string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dir, err := museDataDir(env)
	if err != nil {
		return "", fmt.Errorf("muse: resolve data dir: %w", err)
	}
	return dir, nil
}

func museDataDir(env map[string]string) (string, error) {
	if value, ok := env["XDG_DATA_HOME"]; ok {
		if dir := strings.TrimSpace(value); dir != "" {
			return filepath.Join(dir, "muse"), nil
		}
		return museHomeDataDir(env)
	}
	if dir := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); dir != "" {
		return filepath.Join(dir, "muse"), nil
	}
	return museHomeDataDir(env)
}

func museHomeDataDir(env map[string]string) (string, error) {
	if home := strings.TrimSpace(env["HOME"]); home != "" {
		return filepath.Join(home, ".local", "share", "muse"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "muse"), nil
}

// ProbeNativeSession reports whether the native session's durable log still
// exists. Muse keeps one sessions/ store with no archive tier, so a found
// transcript is authoritative backing state.
func (p *Plugin) ProbeNativeSession(ctx context.Context, ref ports.NativeSessionRef) (ports.NativeSessionAvailability, error) {
	if strings.TrimSpace(ref.ConfigDir) == "" {
		return ports.NativeSessionAvailabilityUnknown, nil
	}
	if _, err := validateMuseNativeSessionID(ref.NativeSessionID); err != nil {
		return ports.NativeSessionAvailabilityUnknown, err
	}
	_, ok, err := findMuseTranscript(ctx, filepath.Join(ref.ConfigDir, "sessions"), strings.TrimSpace(ref.NativeSessionID))
	if err != nil {
		return ports.NativeSessionAvailabilityUnknown, err
	}
	if ok {
		return ports.NativeSessionAvailabilityAvailable, nil
	}
	return ports.NativeSessionAvailabilityUnavailable, nil
}

// LocateTranscript finds Muse's provider-owned session.jsonl by native session
// id. Sessions live date-sharded under sessions/YYYY/MM/DD/<id>/session.jsonl;
// the walk matches the id directory exactly, so subagent logs (whose ids AO
// never observes through hooks) cannot shadow the main transcript.
func (p *Plugin) LocateTranscript(ctx context.Context, ref ports.NativeSessionRef) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	sessionID, err := validateMuseNativeSessionID(ref.NativeSessionID)
	if err != nil {
		return "", false, err
	}
	configDir := strings.TrimSpace(ref.ConfigDir)
	if configDir == "" {
		return "", false, nil
	}
	return findMuseTranscript(ctx, filepath.Join(configDir, "sessions"), sessionID)
}

func findMuseTranscript(ctx context.Context, root, sessionID string) (string, bool, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == sessionID {
			candidate := filepath.Join(path, "session.jsonl")
			if info, statErr := os.Stat(candidate); statErr == nil && info.Mode().IsRegular() {
				found = candidate
				return fs.SkipAll
			}
			return filepath.SkipDir
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("muse: scan transcripts: %w", err)
	}
	return found, found != "", nil
}

func validateMuseNativeSessionID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 256 || strings.ContainsAny(value, `/\`+"\x00") {
		return "", fmt.Errorf("muse: invalid native session id")
	}
	return value, nil
}
