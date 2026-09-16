package muse

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Synthetic export document mirroring `muse export` schema version 1. Values
// are invented for the test; only the envelope/record shapes follow the real
// exporter.
const museExportFixture = `{
  "export_schema_version": 1,
  "exporter_version": {"display": "Muse Code 0.0.0 (test)", "semver": "0.0.0", "sha": "test"},
  "redaction": "raw",
  "session_terminated_abnormally": false,
  "diagnostics": {"duplicate_records": 0, "gaps": 0, "omitted_live_only": 0, "unknown_payload_kinds": 0, "unparseable_lines": 0},
  "sessions": [{"session_id": "test-session-1", "trajectory_id": "trajectory_test_1", "turn_count": 2, "step_count": 10}],
  "events": [
    {"kind": "record", "outer_log_ordinal": 1,
     "envelope": {"payload_type": "runtime.session", "payload": {"kind": "run", "event": {"kind": "started", "prompt": "list the workspace files"}}},
     "derived": {"turn_count": 1, "session_id": "test-session-1"}},
    {"kind": "record", "outer_log_ordinal": 2,
     "envelope": {"payload_type": "runtime.session", "payload": {"kind": "run", "event": {"kind": "assistant_tool_calls_committed", "message_id": "msg-1", "tool_calls": [{"name": "bash", "args": "{\"command\":\"ls\"}", "call_id": "call-1", "id": "fc-1"}]}}},
     "derived": {"turn_count": 1, "session_id": "test-session-1"}},
    {"kind": "record", "outer_log_ordinal": 3,
     "envelope": {"payload_type": "runtime.session", "payload": {"kind": "run", "event": {"kind": "tool_result_batch_committed", "batch_id": "msg-1", "results": [{"text": "a.txt\nb.txt", "tool_call_id": "call-1", "tool_call_index": 0}]}}},
     "derived": {"turn_count": 1, "session_id": "test-session-1"}},
    {"kind": "record", "outer_log_ordinal": 4,
     "envelope": {"payload_type": "runtime.session", "payload": {"kind": "run", "event": {"kind": "assistant_message_committed", "text": "Found two files."}}},
     "derived": {"turn_count": 1, "session_id": "test-session-1"}},
    {"kind": "record", "outer_log_ordinal": 5,
     "envelope": {"payload_type": "runtime.session", "payload": {"kind": "approval", "event": {"kind": "requested", "tool_name": "network", "raw_args": "https registry.example.test:443", "pending_action_id": "pa-1"}}},
     "derived": {"turn_count": 1, "session_id": "test-session-1"}},
    {"kind": "record", "outer_log_ordinal": 6,
     "envelope": {"payload_type": "runtime.session", "payload": {"kind": "approval", "event": {"kind": "decision_applied", "decision": "approved_for_session", "pending_action_id": "pa-1", "policy_result": "allow"}}},
     "derived": {"turn_count": 1, "session_id": "test-session-1"}},
    {"kind": "record", "outer_log_ordinal": 7,
     "envelope": {"payload_type": "runtime.session", "payload": {"kind": "run", "event": {"kind": "started", "prompt": "now summarize"}}},
     "derived": {"turn_count": 2, "session_id": "test-session-1"}}
  ]
}`

func TestParseExportGroupsTurnsAndJoinsTools(t *testing.T) {
	transcript, err := ParseExport([]byte(museExportFixture))
	if err != nil {
		t.Fatal(err)
	}
	if transcript.SessionID != "test-session-1" || transcript.TrajectoryID != "trajectory_test_1" {
		t.Fatalf("identity = %q/%q", transcript.SessionID, transcript.TrajectoryID)
	}
	if transcript.Turns != 2 || transcript.Steps != 10 {
		t.Fatalf("turns/steps = %d/%d, want 2/10", transcript.Turns, transcript.Steps)
	}
	if len(transcript.Messages) != 2 {
		t.Fatalf("turns = %d, want 2", len(transcript.Messages))
	}
	first := transcript.Messages[0]
	if first.Ordinal != 1 || first.Prompt != "list the workspace files" {
		t.Fatalf("turn 1 prompt = %d/%q", first.Ordinal, first.Prompt)
	}
	if len(first.Assistant) != 1 || first.Assistant[0] != "Found two files." {
		t.Fatalf("turn 1 assistant = %#v", first.Assistant)
	}
	if len(first.Tools) != 1 {
		t.Fatalf("turn 1 tools = %d, want 1", len(first.Tools))
	}
	call := first.Tools[0]
	if call.Name != "bash" || !strings.Contains(call.Args, `"command":"ls"`) {
		t.Fatalf("tool call = %#v", call)
	}
	if call.Result != "a.txt\nb.txt" {
		t.Fatalf("tool result = %q, want joined batch text", call.Result)
	}
	if len(first.Approvals) != 1 {
		t.Fatalf("turn 1 approvals = %d, want 1", len(first.Approvals))
	}
	approval := first.Approvals[0]
	if approval.Tool != "network" || approval.Decision != "approved_for_session" {
		t.Fatalf("approval = %#v", approval)
	}
	if second := transcript.Messages[1]; second.Ordinal != 2 || second.Prompt != "now summarize" {
		t.Fatalf("turn 2 = %#v", second)
	}
}

func TestParseExportRejectsSchemaVersion(t *testing.T) {
	doc := strings.Replace(museExportFixture, `"export_schema_version": 1`, `"export_schema_version": 99`, 1)
	if _, err := ParseExport([]byte(doc)); err == nil {
		t.Fatal("ParseExport accepted schema version 99")
	}
}

func TestParseExportRejectsMalformedJSON(t *testing.T) {
	if _, err := ParseExport([]byte("{not json")); err == nil {
		t.Fatal("ParseExport accepted malformed JSON")
	}
}

func TestParseExportSkipsUndecodableRecords(t *testing.T) {
	doc := strings.Replace(museExportFixture,
		`{"kind": "started", "prompt": "list the workspace files"}`,
		`{"kind": "started", "prompt": {"unexpected": "object"}}`, 1)
	transcript, err := ParseExport([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Messages) != 2 {
		t.Fatalf("turns = %d, want 2 despite bad record", len(transcript.Messages))
	}
	if got := transcript.Messages[0].Prompt; got != "" {
		t.Fatalf("turn 1 prompt = %q, want dropped", got)
	}
	if len(transcript.Messages[0].Tools) != 1 {
		t.Fatalf("turn 1 tools = %d, want surviving tool call", len(transcript.Messages[0].Tools))
	}
}

func TestRenderExcerptShape(t *testing.T) {
	transcript, err := ParseExport([]byte(museExportFixture))
	if err != nil {
		t.Fatal(err)
	}
	text, truncated := RenderExcerpt(transcript)
	if truncated {
		t.Fatal("truncated = true, want false for a gapless export")
	}
	for _, want := range []string{
		"[muse transcript session test-session-1 trajectory trajectory_test_1: 2 turns, 10 steps]",
		"--- turn 1 (user) ---",
		"list the workspace files",
		"--- turn 1 (assistant) ---",
		"Found two files.",
		"--- turn 1 (tools) ---",
		"- bash",
		"= a.txt",
		"--- turn 1 (approvals) ---",
		"network (https registry.example.test:443): approved_for_session",
		"--- turn 2 (user) ---",
		"now summarize",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("excerpt missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "--- turn 2 (assistant) ---") || strings.Contains(text, "--- turn 2 (tools) ---") {
		t.Fatalf("excerpt rendered empty turn 2 sections:\n%s", text)
	}
}

func TestRenderExcerptReportsGaps(t *testing.T) {
	transcript, err := ParseExport([]byte(museExportFixture))
	if err != nil {
		t.Fatal(err)
	}
	transcript.Gaps.OmittedLiveOnly = 295
	text, truncated := RenderExcerpt(transcript)
	if !truncated {
		t.Fatal("truncated = false, want true with export gaps")
	}
	if !strings.Contains(text, "[muse transcript gaps: omitted_live_only=295]") {
		t.Fatalf("excerpt missing gaps line:\n%s", text)
	}
}

func TestBoundExcerptText(t *testing.T) {
	if got := boundExcerptText("  hello  ", 100); got != "hello" {
		t.Fatalf("bound = %q, want trimmed", got)
	}
	got := boundExcerptText(strings.Repeat("x", 100), 10)
	if !strings.HasSuffix(got, museExcerptCutSuffix) || len(got) != 10+len(museExcerptCutSuffix) {
		t.Fatalf("bound = %q, want 10 runes plus suffix", got)
	}
	if got := boundExcerptText("a\x1b[31mb\x00c", 100); got != "abc" {
		t.Fatalf("bound = %q, want controls stripped", got)
	}
}

func TestTranscriptExcerptRequiresAbsolutePath(t *testing.T) {
	p := &Plugin{resolvedBinary: "muse"}
	for _, path := range []string{"", "   ", "relative/session.jsonl"} {
		if _, _, err := p.TranscriptExcerpt(context.Background(), path); err == nil {
			t.Fatalf("TranscriptExcerpt(%q) succeeded, want error", path)
		}
	}
}

func TestTranscriptExcerptRunsExportAndCleansScratch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	fixture := filepath.Join(dir, "export.json")
	if err := os.WriteFile(fixture, []byte(museExportFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "muse")
	script := "#!/bin/sh\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  if [ \"$1\" = \"--out\" ]; then shift; cat " + fixture + " > \"$1\"; exit 0; fi\n" +
		"  shift\ndone\nexit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := filepath.Glob(filepath.Join(os.TempDir(), "muse-export-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	p := &Plugin{resolvedBinary: fake}
	text, truncated, err := p.TranscriptExcerpt(context.Background(), filepath.Join(dir, "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("truncated = true, want false")
	}
	for _, want := range []string{"list the workspace files", "Found two files.", "approved_for_session"} {
		if !strings.Contains(text, want) {
			t.Fatalf("excerpt missing %q:\n%s", want, text)
		}
	}
	after, err := filepath.Glob(filepath.Join(os.TempDir(), "muse-export-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("scratch files before=%d after=%d, want scratch removed", len(before), len(after))
	}
}

func TestTranscriptExcerptExportFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	fake := filepath.Join(t.TempDir(), "muse")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 'no such session' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := &Plugin{resolvedBinary: fake}
	if _, _, err := p.TranscriptExcerpt(context.Background(), "/absent/session.jsonl"); err == nil {
		t.Fatal("TranscriptExcerpt succeeded despite export failure")
	}
}
