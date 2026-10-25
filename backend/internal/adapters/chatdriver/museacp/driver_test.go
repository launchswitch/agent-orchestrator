package museacp

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type fakeMusePlugin struct {
	binary string
	binErr error
	status ports.AgentAuthStatus
	err    error
}

func (p fakeMusePlugin) ResolveBinary(context.Context) (string, error) {
	return p.binary, p.binErr
}

func (p fakeMusePlugin) AuthStatus(context.Context) (ports.AgentAuthStatus, error) {
	return p.status, p.err
}

func installedAdapter() binaryResolver {
	return func(context.Context) (string, error) { return "/user/bin/muse-code-acp", nil }
}

func TestLaunchUsesPreinstalledAdapterAndPointsItAtTheVerifiedMuse(t *testing.T) {
	cfg := buildConfig(
		fakeMusePlugin{binary: "/user/bin/muse", status: ports.AgentAuthStatusAuthorized},
		installedAdapter(),
		nil,
	)
	if err := cfg.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	launchCfg := acpdriver.LaunchConfig{
		SessionID:     "session-1",
		DataDir:       t.TempDir(),
		WorkspacePath: "/workspace",
		Env: map[string]string{
			"META_API_KEY": "from-project",
			"PATH":         "/user/bin",
		},
	}
	launch, err := cfg.Launch(context.Background(), launchCfg)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if launch.Command != "/user/bin/muse-code-acp" || len(launch.Args) != 0 {
		t.Fatalf("launch = %#v, want exact pre-installed muse-code-acp with no installer arguments", launch)
	}
	for key, want := range launchCfg.Env {
		if got := launch.Env[key]; got != want {
			t.Errorf("env[%q] = %q, want %q", key, got, want)
		}
	}
	if got := launch.Env["MUSE_CODE_EXECUTABLE"]; got != "/user/bin/muse" {
		t.Fatalf("MUSE_CODE_EXECUTABLE = %q, want the plugin-verified muse binary", got)
	}
	if got := launch.Env["MUSE_CODE_ACP_ALLOW_YOLO"]; got != "1" {
		t.Fatalf("MUSE_CODE_ACP_ALLOW_YOLO = %q, want 1 so the adapter offers the sandbox option's disabled value", got)
	}
}

func TestLaunchKeepsAnExplicitSandboxGuard(t *testing.T) {
	for _, explicit := range []string{"1", "0", "false"} {
		t.Run("explicit "+explicit, func(t *testing.T) {
			cfg := buildConfig(fakeMusePlugin{binary: "/user/bin/muse"}, installedAdapter(), nil)
			launch, err := cfg.Launch(context.Background(), acpdriver.LaunchConfig{
				SessionID: "session-1",
				DataDir:   t.TempDir(),
				Env:       map[string]string{"MUSE_CODE_ACP_ALLOW_YOLO": explicit},
			})
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			if got := launch.Env["MUSE_CODE_ACP_ALLOW_YOLO"]; got != explicit {
				t.Fatalf("MUSE_CODE_ACP_ALLOW_YOLO = %q, want the environment's explicit %q", got, explicit)
			}
		})
	}
}

func TestLaunchKeepsAnExplicitMuseExecutable(t *testing.T) {
	cfg := buildConfig(
		fakeMusePlugin{binary: "/user/bin/muse"},
		installedAdapter(),
		nil,
	)
	launch, err := cfg.Launch(context.Background(), acpdriver.LaunchConfig{
		SessionID: "session-1",
		DataDir:   t.TempDir(),
		Env:       map[string]string{"MUSE_CODE_EXECUTABLE": "/pinned/muse-1.1.1"},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got := launch.Env["MUSE_CODE_EXECUTABLE"]; got != "/pinned/muse-1.1.1" {
		t.Fatalf("MUSE_CODE_EXECUTABLE = %q, want the environment's explicit pin", got)
	}
}

func TestMissingMuseACPDoesNotDownload(t *testing.T) {
	want := errors.New("not found")
	cfg := buildConfig(fakeMusePlugin{binary: "/user/bin/muse"}, func(context.Context) (string, error) {
		return "", want
	}, nil)
	err := cfg.Probe(context.Background())
	if !errors.Is(err, ports.ErrChatDriverUnavailable) || !errors.Is(err, want) {
		t.Fatalf("Probe error = %v", err)
	}
	if _, err := cfg.Launch(context.Background(), acpdriver.LaunchConfig{DataDir: t.TempDir()}); !errors.Is(err, ports.ErrChatDriverUnavailable) {
		t.Fatalf("Launch error = %v, want ErrChatDriverUnavailable", err)
	}
}

func TestMissingMuseBinaryRefusesTheSession(t *testing.T) {
	want := errors.New("muse: binary not found")
	cfg := buildConfig(fakeMusePlugin{binErr: want}, installedAdapter(), nil)
	err := cfg.Probe(context.Background())
	if !errors.Is(err, ports.ErrChatDriverUnavailable) || !errors.Is(err, want) {
		t.Fatalf("Probe error = %v", err)
	}
	if _, err := cfg.Launch(context.Background(), acpdriver.LaunchConfig{DataDir: t.TempDir()}); !errors.Is(err, ports.ErrChatDriverUnavailable) {
		t.Fatalf("Launch error = %v, want ErrChatDriverUnavailable", err)
	}
}

func TestInstallCheckNamesTheMissingAdapter(t *testing.T) {
	missing := buildConfig(fakeMusePlugin{}, func(context.Context) (string, error) { return "", errors.New("not found") }, nil)
	err := missing.InstallCheck(context.Background())
	if err == nil || !strings.Contains(err.Error(), "npm i -g @bex-co/muse-code-acp") || !strings.Contains(err.Error(), "Node 22+") {
		t.Fatalf("InstallCheck error = %v, want the install command and the Node floor", err)
	}
	installed := buildConfig(fakeMusePlugin{}, installedAdapter(), nil)
	if err := installed.InstallCheck(context.Background()); err != nil {
		t.Fatalf("InstallCheck with muse-code-acp installed = %v", err)
	}
}

func TestProbeReusesMuseAuth(t *testing.T) {
	cfg := buildConfig(
		fakeMusePlugin{binary: "/user/bin/muse", status: ports.AgentAuthStatusUnauthorized},
		installedAdapter(),
		nil,
	)
	if err := cfg.Probe(context.Background()); !errors.Is(err, ports.ErrChatAuthRequired) {
		t.Fatalf("Probe error = %v, want ErrChatAuthRequired", err)
	}
}

func TestProbeContinuesWhenMuseAuthIsInconclusive(t *testing.T) {
	cfg := buildConfig(
		fakeMusePlugin{binary: "/user/bin/muse", status: ports.AgentAuthStatusUnknown, err: errors.New("probe failed")},
		installedAdapter(),
		nil,
	)
	if err := cfg.Probe(context.Background()); err != nil {
		t.Fatalf("Probe error = %v, want to continue on an inconclusive auth probe", err)
	}
}

func TestMuseACPDistributionAndVersionPolicy(t *testing.T) {
	info := func(name, version string) acpsdk.InitializeResponse {
		return acpsdk.InitializeResponse{AgentInfo: &acpsdk.Implementation{Name: name, Version: version}}
	}
	for _, tc := range []struct {
		name    string
		init    acpsdk.InitializeResponse
		wantErr bool
	}{
		{name: "tested minimum", init: info(distributionName, minimumVersion)},
		{name: "newer compatible", init: info(distributionName, "0.11.0")},
		{name: "old", init: info(distributionName, "0.9.1"), wantErr: true},
		{name: "wrong distribution", init: info("community/muse-code-acp", minimumVersion), wantErr: true},
		{name: "missing info", init: acpsdk.InitializeResponse{}, wantErr: true},
		{name: "unparsable version", init: info(distributionName, "latest"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateInitialize(tc.init)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateInitialize error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestMuseACPFeatureMapping(t *testing.T) {
	cfg := buildConfig(fakeMusePlugin{}, installedAdapter(), nil)
	for _, capability := range []ports.ChatCapability{
		ports.ChatCapabilityStreaming,
		ports.ChatCapabilityTools,
		ports.ChatCapabilityApprovals,
		ports.ChatCapabilityInterrupt,
		ports.ChatCapabilityResume,
		ports.ChatCapabilityDiffs,
	} {
		if !cfg.Capabilities.Has(capability) {
			t.Errorf("capability %q is false", capability)
		}
	}
	if cfg.Capabilities.Has(ports.ChatCapabilityUsage) {
		t.Fatal("Muse ACP advertises usage even though muse-code-acp reports no per-turn token usage")
	}
	if missing := ports.MissingProductionCapabilities(cfg.Capabilities); len(missing) != 0 {
		t.Fatalf("missing production capabilities = %v, want none so mutating Chat admission is allowed", missing)
	}

	got := cfg.SessionOptions(ports.ChatTurnSettings{Model: "muse-model:%5B%22meta%22%2Cnull%2C%22muse-spark-1.3%22%5D", Effort: "high"})
	want := []acpdriver.SessionOption{
		{ID: "model", Value: "muse-model:%5B%22meta%22%2Cnull%2C%22muse-spark-1.3%22%5D"},
		{ID: "reasoningEffort", Value: "high"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("session options = %#v, want %#v", got, want)
	}
	if got := cfg.SessionOptions(ports.ChatTurnSettings{}); len(got) != 0 {
		t.Fatalf("session options without choices = %#v, want none", got)
	}

	got = cfg.LaunchSessionOptions(acpdriver.LaunchConfig{})
	want = []acpdriver.SessionOption{{ID: "sandbox", Value: "disabled"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("launch session options = %#v, want only sandbox=disabled", got)
	}
}

func TestMuseSessionModeMapping(t *testing.T) {
	cfg := buildConfig(fakeMusePlugin{}, installedAdapter(), nil)
	for _, tc := range []struct {
		permission ports.PermissionMode
		want       string
	}{
		{ports.PermissionModeDefault, "default"},
		{ports.PermissionModeAuto, "bypassApprovals"},
		{ports.PermissionModeBypassPermissions, "bypassApprovals"},
		// Muse has no edits-only mode; the provider's prompted default stays.
		{ports.PermissionModeAcceptEdits, ""},
	} {
		if got := cfg.SessionMode(tc.permission); got != tc.want {
			t.Errorf("SessionMode(%q) = %q, want %q", tc.permission, got, tc.want)
		}
	}
}

func TestHarnessIsMuse(t *testing.T) {
	cfg := buildConfig(fakeMusePlugin{}, installedAdapter(), nil)
	if cfg.Harness != domain.HarnessMuse {
		t.Fatalf("Harness = %q, want muse", cfg.Harness)
	}
}
