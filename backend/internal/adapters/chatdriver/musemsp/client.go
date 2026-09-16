// Package musemsp speaks the Muse Session Protocol (MSP) over `muse serve`
// stdio. Today it serves live model discovery only.
//
// A full Chat driver needs the view/resume plane — view/subscribe, view/page,
// session/resume, session/read — which is schema-stable but UNSERVED by muse
// 1.3.0 (all return methodNotFound, and no turn/item notifications flow), so
// this package deliberately exposes only the verified command-plane surface:
// handshake, model/list, and the session/turn calls a driver will reuse.
//
// Served by 1.3.0 (verified by empty-params enumeration): initialize,
// model/list, session/list, session/start, session/compact, session/setModel,
// session/setReasoningEffort, session/setApprovalMode, turn/start,
// turn/steer, turn/cancel, turn/interrupt, turn/unqueue, approval/decide,
// approval/listPending, userInput/answer, userInput/cancel, userInput/clarify,
// usage/read, skill/list, subagent/*, task/*, goal/*, workflow/*,
// view/unsubscribe, session/userShell (capability-gated). Deferred
// (methodNotFound): view/subscribe, view/page, session/resume, session/read,
// session/fork, session/rename, item/readOutput. The TUI cross-session
// `session-message` CLI is likewise closed (external_agent_ingress_closed
// even with a live session), so native TUI steering has no CLI path either.
// Before growing this into a driver, re-run the enumeration against the
// installed binary and confirm view/subscribe, view/page, and session/resume
// no longer return -32601.
package musemsp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

const (
	mspSchemaVersion = 1
	// clientName identifies AO to the host. It must match ^[a-z0-9_]+$; the
	// host rejects anything else during initialize.
	clientName    = "agent_orchestrator"
	clientVersion = "0.1.0"

	// maxFrameBytes caps one newline-delimited JSON-RPC frame. Model and
	// handshake frames are kilobytes; the margin covers future view pages.
	maxFrameBytes = 16 << 20

	// closeWait bounds the graceful shutdown drain before the host is killed.
	closeWait = 2 * time.Second
)

// JSON-RPC error codes the client classifies.
const (
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// RPCError is a JSON-RPC error response.
type RPCError struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("msp: rpc error %d: %s", e.Code, e.Message)
}

// IsMethodNotFound reports whether err is a JSON-RPC methodNotFound. Callers
// use it to probe which schema methods the installed host serves.
func IsMethodNotFound(err error) bool {
	var rpcErr *RPCError
	return errors.As(err, &rpcErr) && rpcErr.Code == codeMethodNotFound
}

// Notification is a server-initiated JSON-RPC notification.
type Notification struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// ServerInfo summarizes the verified handshake.
type ServerInfo struct {
	Name        string
	Version     string
	Fingerprint string
	Ephemeral   bool
}

// Model is one model/list entry.
type Model struct {
	ID           string
	Label        string
	Provider     string
	Default      bool
	ContextLimit int64
	OutputLimit  int64
}

type rpcResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// Client is one `muse serve` stdio connection. It is safe for concurrent use;
// requests correlate by id while a reader goroutine dispatches responses and
// notifications.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	log    *slog.Logger
	nextID atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan rpcResponse

	notifyMu sync.Mutex
	handlers []func(Notification)

	done chan struct{}
	wg   sync.WaitGroup
}

// Spawn starts `muse serve` and returns the connected client. Handshake runs
// separately so callers control its budget. When ephemeral is true the host
// keeps memory-only sessions, which keeps offline probes out of the session
// store; model discovery never creates sessions either way.
func Spawn(ctx context.Context, museBinary, workingDir string, env map[string]string, ephemeral bool, log *slog.Logger) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(museBinary) == "" {
		return nil, errors.New("musemsp: muse binary is not installed")
	}
	args := []string{"serve"}
	if ephemeral {
		args = append(args, "--no-session-log")
	}
	return spawnHost(ctx, museBinary, args, workingDir, env, log)
}

func spawnHost(ctx context.Context, museBinary string, args []string, workingDir string, env map[string]string, log *slog.Logger) (*Client, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	cmd := aoprocess.CommandContext(ctx, museBinary, args...)
	if workingDir != "" {
		cmd.Dir = workingDir
	}
	cmd.Env = append(os.Environ(), "MUSE_NO_AUTO_UPDATE=1")
	for key, value := range env {
		if strings.TrimSpace(key) != "" {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("musemsp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("musemsp: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("musemsp: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("musemsp: start serve: %w", err)
	}
	client := &Client{
		cmd:     cmd,
		stdin:   stdin,
		log:     log,
		pending: map[int64]chan rpcResponse{},
		done:    make(chan struct{}),
	}
	client.wg.Add(2)
	go client.readLoop(stdout)
	go client.drainStderr(stderr)
	return client, nil
}

// OnNotification registers a handler invoked for every server notification.
// Handlers run on the reader goroutine and must not block.
func (c *Client) OnNotification(handler func(Notification)) {
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	c.handlers = append(c.handlers, handler)
}

// Handshake runs initialize plus the initialized notification and verifies
// the envelope schema version. It must precede any other call.
func (c *Client) Handshake(ctx context.Context) (ServerInfo, error) {
	var result struct {
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Schema struct {
			Version     int    `json:"version"`
			Fingerprint string `json:"fingerprint"`
		} `json:"schema"`
		SessionDurability string `json:"sessionDurability"`
	}
	if err := c.request(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": clientName, "version": clientVersion},
	}, &result); err != nil {
		return ServerInfo{}, err
	}
	if result.Schema.Version != mspSchemaVersion {
		return ServerInfo{}, fmt.Errorf("musemsp: unsupported schema version %d", result.Schema.Version)
	}
	if err := c.notify(ctx, "initialized", nil); err != nil {
		return ServerInfo{}, err
	}
	return ServerInfo{
		Name:        result.ServerInfo.Name,
		Version:     result.ServerInfo.Version,
		Fingerprint: result.Schema.Fingerprint,
		Ephemeral:   result.SessionDurability == "ephemeral",
	}, nil
}

// ListModels calls model/list and normalizes the entries. It needs no
// session and works against an ephemeral host.
func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	var result struct {
		Models []struct {
			ModelID      string `json:"modelId"`
			DisplayLabel string `json:"displayLabel"`
			ProviderID   string `json:"providerId"`
			IsDefault    bool   `json:"isDefault"`
			ContextLimit int64  `json:"contextLimit"`
			OutputLimit  int64  `json:"outputLimit"`
		} `json:"models"`
	}
	if err := c.request(ctx, "model/list", nil, &result); err != nil {
		return nil, err
	}
	models := make([]Model, 0, len(result.Models))
	for _, entry := range result.Models {
		id := strings.TrimSpace(entry.ModelID)
		if id == "" {
			continue
		}
		label := strings.TrimSpace(entry.DisplayLabel)
		if label == "" {
			label = id
		}
		models = append(models, Model{
			ID:           id,
			Label:        label,
			Provider:     strings.TrimSpace(entry.ProviderID),
			Default:      entry.IsDefault,
			ContextLimit: entry.ContextLimit,
			OutputLimit:  entry.OutputLimit,
		})
	}
	if len(models) == 0 {
		return nil, errors.New("musemsp: model/list returned no models")
	}
	return models, nil
}

// Session is a started or resumed MSP session.
type Session struct {
	ID            string
	Provider      string
	WorkspaceRoot string
	Model         string
	ViewCursor    string
}

// TurnAck is the accepted acknowledgment for turn/start.
type TurnAck struct {
	TurnID         string
	Disposition    string
	StartedNewTurn bool
}

// StartSession opens a session on the host. providerID selects the model
// backend ("echo" answers deterministically offline, which is what the live
// test uses); workspaceRoot scopes the session's working directory.
func (c *Client) StartSession(ctx context.Context, providerID, workspaceRoot string) (Session, error) {
	var result struct {
		Session struct {
			SessionID     string `json:"sessionId"`
			ProviderID    string `json:"providerId"`
			WorkspaceRoot string `json:"workspaceRoot"`
			ModelID       string `json:"modelId"`
		} `json:"session"`
		ViewCursor string `json:"viewCursor"`
	}
	if err := c.request(ctx, "session/start", map[string]any{
		"commandId":     NewCommandID(),
		"providerId":    providerID,
		"workspaceRoot": workspaceRoot,
	}, &result); err != nil {
		return Session{}, err
	}
	if strings.TrimSpace(result.Session.SessionID) == "" {
		return Session{}, errors.New("musemsp: session/start returned no session id")
	}
	return Session{
		ID:            result.Session.SessionID,
		Provider:      result.Session.ProviderID,
		WorkspaceRoot: result.Session.WorkspaceRoot,
		Model:         result.Session.ModelID,
		ViewCursor:    result.ViewCursor,
	}, nil
}

// StartTurn submits one text turn. The acknowledgment is transport-level: the
// turn's progress arrives as notifications, which this build does not emit
// for the view plane (see the package doc).
func (c *Client) StartTurn(ctx context.Context, sessionID, text string) (TurnAck, error) {
	var result struct {
		TurnID         string `json:"turnId"`
		Disposition    string `json:"disposition"`
		StartedNewTurn bool   `json:"startedNewTurn"`
	}
	if err := c.request(ctx, "turn/start", map[string]any{
		"commandId": NewCommandID(),
		"sessionId": sessionID,
		"input":     []map[string]any{{"type": "text", "text": text}},
	}, &result); err != nil {
		return TurnAck{}, err
	}
	return TurnAck{TurnID: result.TurnID, Disposition: result.Disposition, StartedNewTurn: result.StartedNewTurn}, nil
}

// Close shuts the host down: stdin closes, the reader drains, and the process
// is killed past the graceful wait.
func (c *Client) Close() error {
	select {
	case <-c.done:
		return nil
	default:
	}
	_ = c.stdin.Close()
	waited := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(closeWait):
		_ = c.cmd.Process.Kill()
		<-waited
	}
	_ = c.cmd.Wait()
	return nil
}

func (c *Client) request(ctx context.Context, method string, params, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id := c.nextID.Add(1)
	frame := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		frame["params"] = params
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("musemsp: encode %s: %w", method, err)
	}
	reply := make(chan rpcResponse, 1)
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return errors.New("musemsp: client closed")
	default:
	}
	c.pending[id] = reply
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if _, err := c.stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("musemsp: send %s: %w", method, err)
	}
	select {
	case <-ctx.Done():
		return fmt.Errorf("musemsp: %s: %w", method, ctx.Err())
	case <-c.done:
		return fmt.Errorf("musemsp: %s: connection closed", method)
	case response := <-reply:
		if response.Error != nil {
			return response.Error
		}
		if out == nil || len(response.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(response.Result, out); err != nil {
			return fmt.Errorf("musemsp: decode %s: %w", method, err)
		}
		return nil
	}
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	frame := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		frame["params"] = params
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("musemsp: encode %s: %w", method, err)
	}
	if _, err := c.stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("musemsp: send %s: %w", method, err)
	}
	return nil
}

func (c *Client) readLoop(stdout io.ReadCloser) {
	defer c.wg.Done()
	defer func() { _ = stdout.Close() }()
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		select {
		case <-c.done:
		default:
			close(c.done)
		}
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maxFrameBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var frame struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *RPCError       `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			c.log.Debug("musemsp: skip non-JSON frame")
			continue
		}
		if frame.ID != nil {
			c.mu.Lock()
			reply, ok := c.pending[*frame.ID]
			c.mu.Unlock()
			if ok {
				reply <- rpcResponse{Result: frame.Result, Error: frame.Error}
			}
			continue
		}
		if frame.Method != "" {
			c.notifyMu.Lock()
			handlers := append([]func(Notification){}, c.handlers...)
			c.notifyMu.Unlock()
			event := Notification{Method: frame.Method, Params: frame.Params}
			for _, handler := range handlers {
				handler(event)
			}
			continue
		}
		c.log.Debug("musemsp: skip frame without id or method")
	}
	if err := scanner.Err(); err != nil {
		c.log.Debug("musemsp: stdout read ended", "err", err)
	}
}

func (c *Client) drainStderr(stderr io.ReadCloser) {
	defer c.wg.Done()
	defer func() { _ = stderr.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(stderr, 1<<20))
}

// NewCommandID mints a UUIDv7 command id, the only commandId shape the host
// accepts. Mutating calls take one for idempotent retries.
func NewCommandID() string {
	var unixMS [8]byte
	binary.BigEndian.PutUint64(unixMS[:], uint64(time.Now().UnixMilli()))
	var random [10]byte
	_, _ = rand.Read(random[:])
	var id [16]byte
	copy(id[0:6], unixMS[2:8])
	id[6] = 0x70 | (random[0] & 0x0f)
	copy(id[7:8], random[1:2])
	id[8] = 0x80 | (random[2] & 0x3f)
	copy(id[9:16], random[3:10])
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		binary.BigEndian.Uint32(id[0:4]),
		binary.BigEndian.Uint16(id[4:6]),
		binary.BigEndian.Uint16(id[6:8]),
		binary.BigEndian.Uint16(id[8:10]),
		uint64(id[10])<<40|uint64(id[11])<<32|uint64(id[12])<<24|uint64(id[13])<<16|uint64(id[14])<<8|uint64(id[15]),
	)
}
