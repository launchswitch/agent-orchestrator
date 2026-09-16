package muse

// Read-only consumer for `muse export` transcripts.
//
// Muse's durable session.jsonl is an envelope-heavy event log: one tool call
// costs a dozen task/run records, so a raw JSONL tail carries little usable
// context per byte. The export document (export_schema_version 1) carries the
// same records with session/turn identity attached; this file parses the
// records handoff and orchestration care about — user prompts, assistant text,
// tool calls/results, approvals — into a small typed Transcript and renders it
// as bounded prose. ParseExport/Transcript stay reusable for a future live
// driver: the identity worth knowing is that hook turn_id values do NOT appear
// in the export, so turn joins are ordinal (derived.turn_count) until Muse
// surfaces turn ids there.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

var _ ports.AgentTranscriptExcerpter = (*Plugin)(nil)

const (
	museExportSchemaVersion = 1
	museExportTimeout       = 30 * time.Second
	// museExcerptReadMax caps the export document read. Past it the excerpt
	// fails and the caller falls back to the raw tail.
	museExcerptReadMax = 32 << 20

	museExcerptPromptMax     = 8 << 10
	museExcerptAssistantMax  = 16 << 10
	museExcerptToolArgsMax   = 4 << 10
	museExcerptToolResultMax = 8 << 10
	museExcerptApprovalMax   = 2 << 10
)

const museExcerptCutSuffix = "[... truncated ...]"

// Transcript is the parsed projection of one `muse export` document.
type Transcript struct {
	SessionID    string
	TrajectoryID string
	Turns        int
	Steps        int
	AbnormalEnd  bool
	Gaps         museExportGaps
	Messages     []TranscriptTurn
}

// TranscriptTurn groups one ordinal turn's user prompt, assistant text, tool
// calls with joined results, and approval outcomes.
type TranscriptTurn struct {
	Ordinal   int
	Prompt    string
	Assistant []string
	Tools     []TranscriptToolCall
	Approvals []TranscriptApproval
}

// TranscriptToolCall is one assistant tool call with its joined result text.
// Name is empty for results whose call record was absent.
type TranscriptToolCall struct {
	Name   string
	Args   string
	CallID string
	Result string
}

// TranscriptApproval is one requested tool approval and its outcome.
// Decision is empty while the request is still pending.
type TranscriptApproval struct {
	Tool     string
	Detail   string
	Decision string
}

type museExportGaps struct {
	Gaps            int `json:"gaps"`
	OmittedLiveOnly int `json:"omitted_live_only"`
	Unparseable     int `json:"unparseable_lines"`
	Duplicates      int `json:"duplicate_records"`
	UnknownKinds    int `json:"unknown_payload_kinds"`
}

func (g museExportGaps) any() bool {
	return g.Gaps > 0 || g.OmittedLiveOnly > 0 || g.Unparseable > 0 || g.Duplicates > 0 || g.UnknownKinds > 0
}

type museExportDoc struct {
	SchemaVersion int            `json:"export_schema_version"`
	AbnormalEnd   bool           `json:"session_terminated_abnormally"`
	Diagnostics   museExportGaps `json:"diagnostics"`
	Sessions      []struct {
		SessionID    string `json:"session_id"`
		TrajectoryID string `json:"trajectory_id"`
		TurnCount    int    `json:"turn_count"`
		StepCount    int    `json:"step_count"`
	} `json:"sessions"`
	Events []museExportEvent `json:"events"`
}

type museExportEvent struct {
	Kind     string `json:"kind"`
	Envelope struct {
		PayloadType string `json:"payload_type"`
		Payload     struct {
			Kind  string          `json:"kind"`
			Event json.RawMessage `json:"event"`
		} `json:"payload"`
	} `json:"envelope"`
	Derived struct {
		TurnCount int    `json:"turn_count"`
		SessionID string `json:"session_id"`
	} `json:"derived"`
}

// ParseExport decodes a `muse export` document into a Transcript. It fails
// closed on malformed JSON or an unexpected schema version; individual
// records that do not decode are skipped so one new provider shape cannot
// wipe out the whole excerpt.
func ParseExport(data []byte) (Transcript, error) {
	var doc museExportDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return Transcript{}, fmt.Errorf("muse: parse export: %w", err)
	}
	if doc.SchemaVersion != museExportSchemaVersion {
		return Transcript{}, fmt.Errorf("muse: unsupported export schema version %d", doc.SchemaVersion)
	}
	out := Transcript{Gaps: doc.Diagnostics, AbnormalEnd: doc.AbnormalEnd}
	if len(doc.Sessions) > 0 {
		out.SessionID = doc.Sessions[0].SessionID
		out.TrajectoryID = doc.Sessions[0].TrajectoryID
		out.Turns = doc.Sessions[0].TurnCount
		out.Steps = doc.Sessions[0].StepCount
	}
	parser := &museExportParser{turns: map[int]*TranscriptTurn{}}
	for _, event := range doc.Events {
		parser.record(event)
	}
	out.Messages = parser.ordered()
	return out, nil
}

type museExportParser struct {
	turns    map[int]*TranscriptTurn
	order    []int
	lastTurn int
	calls    map[string][]*TranscriptToolCall
	pending  map[string]*TranscriptApproval
}

func (p *museExportParser) turn(ordinal int) *TranscriptTurn {
	if ordinal <= 0 {
		ordinal = p.lastTurn
	}
	if ordinal <= 0 {
		ordinal = 1
	}
	turn, ok := p.turns[ordinal]
	if !ok {
		turn = &TranscriptTurn{Ordinal: ordinal}
		p.turns[ordinal] = turn
		p.order = append(p.order, ordinal)
	}
	if ordinal > p.lastTurn {
		p.lastTurn = ordinal
	}
	return turn
}

func (p *museExportParser) ordered() []TranscriptTurn {
	out := make([]TranscriptTurn, 0, len(p.order))
	for _, ordinal := range p.order {
		out = append(out, *p.turns[ordinal])
	}
	return out
}

func (p *museExportParser) record(event museExportEvent) {
	if event.Kind != "record" || event.Envelope.PayloadType != "runtime.session" {
		return
	}
	payload := event.Envelope.Payload
	turn := p.turn(event.Derived.TurnCount)
	switch payload.Kind + "/" + museEventKind(payload.Event) {
	case "run/started":
		var started struct {
			Prompt string `json:"prompt"`
		}
		if json.Unmarshal(payload.Event, &started) != nil {
			return
		}
		if prompt := boundExcerptText(started.Prompt, museExcerptPromptMax); prompt != "" {
			if turn.Prompt != "" {
				turn.Prompt += "\n" + prompt
			} else {
				turn.Prompt = prompt
			}
		}
	case "run/assistant_message_committed":
		var msg struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(payload.Event, &msg) != nil {
			return
		}
		if text := boundExcerptText(msg.Text, museExcerptAssistantMax); text != "" {
			turn.Assistant = append(turn.Assistant, text)
		}
	case "run/assistant_tool_calls_committed":
		var calls struct {
			MessageID string `json:"message_id"`
			ToolCalls []struct {
				Name   string `json:"name"`
				Args   string `json:"args"`
				CallID string `json:"call_id"`
			} `json:"tool_calls"`
		}
		if json.Unmarshal(payload.Event, &calls) != nil {
			return
		}
		for _, call := range calls.ToolCalls {
			entry := TranscriptToolCall{
				Name:   strings.TrimSpace(call.Name),
				Args:   boundExcerptText(call.Args, museExcerptToolArgsMax),
				CallID: strings.TrimSpace(call.CallID),
			}
			turn.Tools = append(turn.Tools, entry)
			if id := strings.TrimSpace(calls.MessageID); id != "" {
				if p.calls == nil {
					p.calls = map[string][]*TranscriptToolCall{}
				}
				p.calls[id] = append(p.calls[id], &turn.Tools[len(turn.Tools)-1])
			}
		}
	case "run/tool_result_batch_committed":
		var batch struct {
			BatchID string `json:"batch_id"`
			Results []struct {
				Text          string `json:"text"`
				ToolCallID    string `json:"tool_call_id"`
				ToolCallIndex int    `json:"tool_call_index"`
			} `json:"results"`
		}
		if json.Unmarshal(payload.Event, &batch) != nil {
			return
		}
		calls := p.calls[strings.TrimSpace(batch.BatchID)]
		for _, result := range batch.Results {
			text := boundExcerptText(result.Text, museExcerptToolResultMax)
			if text == "" {
				continue
			}
			attached := false
			if result.ToolCallIndex >= 0 && result.ToolCallIndex < len(calls) {
				if calls[result.ToolCallIndex].Result == "" {
					calls[result.ToolCallIndex].Result = text
				} else {
					calls[result.ToolCallIndex].Result += "\n" + text
				}
				attached = true
			}
			if !attached {
				turn.Tools = append(turn.Tools, TranscriptToolCall{Result: text})
			}
		}
	case "approval/requested":
		var req struct {
			ToolName        string `json:"tool_name"`
			RawArgs         string `json:"raw_args"`
			PendingActionID string `json:"pending_action_id"`
		}
		if json.Unmarshal(payload.Event, &req) != nil {
			return
		}
		entry := TranscriptApproval{
			Tool:   strings.TrimSpace(req.ToolName),
			Detail: boundExcerptText(req.RawArgs, museExcerptApprovalMax),
		}
		turn.Approvals = append(turn.Approvals, entry)
		if id := strings.TrimSpace(req.PendingActionID); id != "" {
			if p.pending == nil {
				p.pending = map[string]*TranscriptApproval{}
			}
			p.pending[id] = &turn.Approvals[len(turn.Approvals)-1]
		}
	case "approval/decision_applied":
		var decision struct {
			Decision        string `json:"decision"`
			PendingActionID string `json:"pending_action_id"`
		}
		if json.Unmarshal(payload.Event, &decision) != nil {
			return
		}
		if entry := p.pending[strings.TrimSpace(decision.PendingActionID)]; entry != nil {
			entry.Decision = strings.TrimSpace(decision.Decision)
		}
	}
}

func museEventKind(raw json.RawMessage) string {
	var probe struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return ""
	}
	return probe.Kind
}

func boundExcerptText(value string, limit int) string {
	clean := excerptCleanText(value)
	if len(clean) <= limit {
		return clean
	}
	runes := []rune(clean)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return strings.TrimSpace(string(runes)) + museExcerptCutSuffix
}

func excerptCleanText(value string) string {
	value = museTerminalEscape.ReplaceAllString(value, "")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	var clean strings.Builder
	clean.Grow(len(value))
	for _, r := range value {
		switch {
		case r == '\n' || r == '\t':
			clean.WriteRune(r)
		case r >= 0x20 && r != 0x7f:
			clean.WriteRune(r)
		}
	}
	return strings.TrimSpace(clean.String())
}

// RenderExcerpt renders a Transcript as chronological prose: header, then per
// turn the user prompt, assistant text, tool calls with joined results, and
// approval outcomes. Empty sections are skipped. The caller applies the final
// byte/line ceiling; truncated reports provider-level export gaps only.
func RenderExcerpt(t Transcript) (text string, truncated bool) {
	var out strings.Builder
	header := "[muse transcript"
	if t.SessionID != "" {
		header += " session " + t.SessionID
	}
	if t.TrajectoryID != "" {
		header += " trajectory " + t.TrajectoryID
	}
	header += fmt.Sprintf(": %d turns, %d steps]", t.Turns, t.Steps)
	out.WriteString(header)
	if t.AbnormalEnd {
		out.WriteString("\n[muse session ended abnormally]")
	}
	if t.Gaps.any() {
		out.WriteString("\n[muse transcript gaps: " + renderGaps(t.Gaps) + "]")
	}
	for _, turn := range t.Messages {
		if turn.Prompt != "" {
			fmt.Fprintf(&out, "\n--- turn %d (user) ---\n%s", turn.Ordinal, turn.Prompt)
		}
		for _, assistant := range turn.Assistant {
			fmt.Fprintf(&out, "\n--- turn %d (assistant) ---\n%s", turn.Ordinal, assistant)
		}
		if len(turn.Tools) > 0 {
			fmt.Fprintf(&out, "\n--- turn %d (tools) ---", turn.Ordinal)
			for _, call := range turn.Tools {
				if call.Name == "" {
					fmt.Fprintf(&out, "\n= %s", call.Result)
					continue
				}
				line := "\n- " + call.Name
				if call.Args != "" {
					line += " " + call.Args
				}
				out.WriteString(line)
				if call.Result != "" {
					out.WriteString("\n  = " + call.Result)
				}
			}
		}
		if len(turn.Approvals) > 0 {
			fmt.Fprintf(&out, "\n--- turn %d (approvals) ---", turn.Ordinal)
			for _, approval := range turn.Approvals {
				line := "\n- " + approval.Tool
				if approval.Detail != "" {
					line += " (" + approval.Detail + ")"
				}
				if approval.Decision != "" {
					line += ": " + approval.Decision
				} else {
					line += ": pending"
				}
				out.WriteString(line)
			}
		}
	}
	return strings.TrimSpace(out.String()), t.Gaps.any()
}

func renderGaps(gaps museExportGaps) string {
	var parts []string
	if gaps.Gaps > 0 {
		parts = append(parts, fmt.Sprintf("gaps=%d", gaps.Gaps))
	}
	if gaps.OmittedLiveOnly > 0 {
		parts = append(parts, fmt.Sprintf("omitted_live_only=%d", gaps.OmittedLiveOnly))
	}
	if gaps.Unparseable > 0 {
		parts = append(parts, fmt.Sprintf("unparseable_lines=%d", gaps.Unparseable))
	}
	if gaps.Duplicates > 0 {
		parts = append(parts, fmt.Sprintf("duplicate_records=%d", gaps.Duplicates))
	}
	if gaps.UnknownKinds > 0 {
		parts = append(parts, fmt.Sprintf("unknown_payload_kinds=%d", gaps.UnknownKinds))
	}
	return strings.Join(parts, " ")
}

// TranscriptExcerpt implements ports.AgentTranscriptExcerpter. It exports the
// located session.jsonl through the Muse CLI (offline, local files only) into
// an AO-owned 0600 scratch file, parses it, and renders the excerpt. The
// scratch file is removed before returning. Any failure or blank excerpt
// returns an error so the caller falls back to the raw tail.
func (p *Plugin) TranscriptExcerpt(ctx context.Context, transcriptPath string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	path := strings.TrimSpace(transcriptPath)
	if path == "" || !filepath.IsAbs(path) {
		return "", false, errors.New("muse: transcript excerpt requires an absolute transcript path")
	}
	binary, err := p.museBinary(ctx)
	if err != nil {
		return "", false, err
	}
	scratch, err := os.CreateTemp("", "muse-export-*.json")
	if err != nil {
		return "", false, fmt.Errorf("muse: transcript excerpt scratch: %w", err)
	}
	scratchPath := scratch.Name()
	_ = scratch.Close()
	defer func() { _ = os.Remove(scratchPath) }()

	exportCtx, cancel := context.WithTimeout(ctx, museExportTimeout)
	defer cancel()
	cmd := aoprocess.CommandContext(exportCtx, binary, "export", "--session", path, "--out", scratchPath)
	cmd.Env = append(os.Environ(), "MUSE_NO_AUTO_UPDATE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", false, fmt.Errorf("muse: export transcript: %w: %s", err, strings.TrimSpace(string(out)))
	}
	data, err := readCappedFile(scratchPath, museExcerptReadMax)
	if err != nil {
		return "", false, err
	}
	transcript, err := ParseExport(data)
	if err != nil {
		return "", false, err
	}
	text, truncated := RenderExcerpt(transcript)
	if strings.TrimSpace(text) == "" {
		return "", truncated, errors.New("muse: transcript excerpt is empty")
	}
	return text, truncated, nil
}

func readCappedFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // AO-owned scratch file from TranscriptExcerpt.
	if err != nil {
		return nil, fmt.Errorf("muse: read export: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("muse: read export: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("muse: export exceeds %d bytes", limit)
	}
	return data, nil
}
