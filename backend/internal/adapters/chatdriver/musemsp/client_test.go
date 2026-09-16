package musemsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHelperMSPHost re-executes the test binary as a scripted MSP host. The
// script maps method names to result payloads; methods absent from the script
// answer methodNotFound. A result of {"__error": {"code": N, "message": M}}
// answers that JSON-RPC error instead.
func TestHelperMSPHost(t *testing.T) {
	if os.Getenv("GO_WANT_MSP_HELPER") != "1" {
		return
	}
	script := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(os.Getenv("MSP_HELPER_SCRIPT")), &script); err != nil {
		fmt.Fprintf(os.Stderr, "bad script: %v\n", err)
		os.Exit(2)
	}
	if os.Getenv("MUSE_NO_AUTO_UPDATE") != "1" {
		fmt.Fprintln(os.Stderr, "missing MUSE_NO_AUTO_UPDATE=1")
		os.Exit(2)
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), maxFrameBytes)
	out := bufio.NewWriter(os.Stdout)
	defer func() { _ = out.Flush() }()
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var frame struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			continue
		}
		if frame.ID == nil {
			if logPath := os.Getenv("MSP_HELPER_LOG"); logPath != "" {
				f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
				if err == nil {
					_, _ = f.WriteString("NOTIFIED:" + frame.Method + "\n")
					_ = f.Close()
				}
			}
			continue
		}
		payload, ok := script[frame.Method]
		if !ok {
			_, _ = fmt.Fprintf(out, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"error\":{\"code\":-32601,\"message\":\"method not found\"}}\n", *frame.ID)
			_ = out.Flush()
			continue
		}
		var errShape struct {
			Err *RPCError `json:"__error"`
		}
		if json.Unmarshal(payload, &errShape) == nil && errShape.Err != nil {
			_, _ = fmt.Fprintf(out, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"error\":{\"code\":%d,\"message\":%s}}\n",
				*frame.ID, errShape.Err.Code, quoteJSON(errShape.Err.Message))
			_ = out.Flush()
			continue
		}
		_, _ = fmt.Fprintf(out, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":%s}\n", *frame.ID, string(payload))
		_ = out.Flush()
	}
	os.Exit(0)
}

func quoteJSON(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}

func spawnHelper(t *testing.T, script map[string]string) *Client {
	t.Helper()
	raw := map[string]json.RawMessage{}
	for method, payload := range script {
		raw[method] = json.RawMessage(payload)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	client, err := spawnHost(ctx, os.Args[0], []string{"-test.run=TestHelperMSPHost"}, "", map[string]string{
		"GO_WANT_MSP_HELPER": "1",
		"MSP_HELPER_SCRIPT":  string(encoded),
		"MSP_HELPER_LOG":     t.TempDir() + "/notifications.log",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

const helperHandshake = `{"serverInfo":{"name":"muse","version":"9.9.9"},"schema":{"version":1,"fingerprint":"sha256:test"},"sessionDurability":"ephemeral"}`

func TestHandshakeVerifiesSchemaVersion(t *testing.T) {
	client := spawnHelper(t, map[string]string{"initialize": helperHandshake})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := client.Handshake(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "muse" || info.Version != "9.9.9" || !info.Ephemeral {
		t.Fatalf("server info = %+v", info)
	}
	if info.Fingerprint != "sha256:test" {
		t.Fatalf("fingerprint = %q", info.Fingerprint)
	}
}

func TestHandshakeRejectsSchemaVersion(t *testing.T) {
	bad := strings.Replace(helperHandshake, `"version":1`, `"version":99`, 1)
	client := spawnHelper(t, map[string]string{"initialize": bad})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Handshake(ctx); err == nil {
		t.Fatal("Handshake accepted schema version 99")
	}
}

func TestListModelsNormalizesEntries(t *testing.T) {
	client := spawnHelper(t, map[string]string{
		"initialize": helperHandshake,
		"model/list": `{"models":[
			{"modelId":"muse-spark-1.3","displayLabel":"muse-spark-1.3","providerId":"meta","isDefault":false,"contextLimit":100,"outputLimit":10},
			{"modelId":"  ","displayLabel":"blank","providerId":"meta"},
			{"modelId":"muse-spark-1.3-contributor","displayLabel":"","providerId":"meta","isDefault":true}
		]}`,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Handshake(ctx); err != nil {
		t.Fatal(err)
	}
	models, err := client.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2 with the blank id dropped", len(models))
	}
	if models[0].ID != "muse-spark-1.3" || models[0].Label != "muse-spark-1.3" || models[0].Default {
		t.Fatalf("models[0] = %+v", models[0])
	}
	if models[1].ID != "muse-spark-1.3-contributor" || models[1].Label != models[1].ID || !models[1].Default {
		t.Fatalf("models[1] = %+v", models[1])
	}
}

func TestListModelsEmptyIsAnError(t *testing.T) {
	client := spawnHelper(t, map[string]string{
		"initialize": helperHandshake,
		"model/list": `{"models":[]}`,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Handshake(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListModels(ctx); err == nil {
		t.Fatal("ListModels succeeded with no models")
	}
}

func TestMethodNotFoundClassification(t *testing.T) {
	client := spawnHelper(t, map[string]string{"initialize": helperHandshake})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Handshake(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := client.ListModels(ctx)
	if !IsMethodNotFound(err) {
		t.Fatalf("err = %v, want methodNotFound classification", err)
	}
	if IsMethodNotFound(errors.New("boom")) {
		t.Fatal("IsMethodNotFound(boom) = true")
	}
}

func TestConcurrentRequestsCorrelate(t *testing.T) {
	client := spawnHelper(t, map[string]string{
		"initialize": helperHandshake,
		"model/list": `{"models":[{"modelId":"m","displayLabel":"m","providerId":"meta"}]}`,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Handshake(ctx); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.ListModels(ctx); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestRequestHonorsContextCancel(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pr.Close(); _ = pw.Close() }()
	// The reader watches a silent pipe: no server answers, so the request
	// must fail when its context lapses.
	qr, qw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{stdin: pw, pending: map[int64]chan rpcResponse{}, done: make(chan struct{})}
	client.wg.Add(1)
	go client.readLoop(qr)
	defer func() { _ = qw.Close(); client.wg.Wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = client.request(ctx, "model/list", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "model/list") {
		t.Fatalf("err = %v, want method-scoped cancel", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("request outlived its context")
	}
}

func TestNewCommandIDIsUUIDv7(t *testing.T) {
	first, second := NewCommandID(), NewCommandID()
	for _, id := range []string{first, second} {
		if len(id) != 36 || id[14] != '7' {
			t.Fatalf("command id %q is not UUIDv7 shaped", id)
		}
		if variant := id[19]; variant != '8' && variant != '9' && variant != 'a' && variant != 'b' {
			t.Fatalf("command id %q has a bad variant %q", id, variant)
		}
		parts := strings.Split(id, "-")
		if len(parts) != 5 {
			t.Fatalf("command id %q has %d parts", id, len(parts))
		}
	}
	if first[0:13] > second[0:13] {
		t.Fatalf("command ids not time-ordered: %q then %q", first, second)
	}
}
