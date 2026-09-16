package musemsp

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestLiveMuseServe exercises the client against a real `muse serve` with the
// echo provider, so it runs fully offline. Ephemeral sessions keep it out of
// the session store.
//
//	AO_MUSE_LIVE=1 go test ./internal/adapters/chatdriver/musemsp/ -run Live -v
func TestLiveMuseServe(t *testing.T) {
	if os.Getenv("AO_MUSE_LIVE") != "1" {
		t.Skip("set AO_MUSE_LIVE=1 to run against a real muse serve")
	}
	bin := os.Getenv("AO_MUSE_BIN")
	if bin == "" {
		bin = "muse"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("muse binary %q not on PATH: %v", bin, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := Spawn(ctx, bin, t.TempDir(), nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	info, err := client.Handshake(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name == "" || info.Fingerprint == "" || !info.Ephemeral {
		t.Fatalf("handshake = %+v, want identified ephemeral host", info)
	}
	models, err := client.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) == 0 || models[0].ID == "" {
		t.Fatalf("models = %+v, want at least one identified model", models)
	}
	session, err := client.StartSession(ctx, "echo", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if session.ID == "" {
		t.Fatal("session id is empty")
	}
	ack, err := client.StartTurn(ctx, session.ID, "say hi live probe")
	if err != nil {
		t.Fatal(err)
	}
	if ack.TurnID == "" || (ack.Disposition != "started" && ack.Disposition != "queued") {
		t.Fatalf("turn ack = %+v, want an accepted turn", ack)
	}
	catalog, err := ListCatalogModels(ctx, bin, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog) == 0 || catalog[0].ID == "" || len(catalog[0].Efforts) == 0 {
		t.Fatalf("catalog = %+v, want identified models with efforts", catalog)
	}
}
