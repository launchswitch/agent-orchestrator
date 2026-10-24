// Package museacp binds AO's Muse harness to a user-installed muse-code-acp
// adapter.
//
// AO does not package or download @bex-co/muse-code-acp; installing it with
// npm requires Node 22+ (the adapter's engines floor). AO speaks ACP to the
// adapter over stdio, and the adapter starts and drives the user's own
// `muse serve` over MSP, so Muse execution, sessions, and approvals stay with
// the Muse install. The existing Muse plugin remains the single answer for
// muse binary resolution and auth: this driver points the adapter at the exact
// muse binary that plugin verified (MUSE_CODE_EXECUTABLE), unless the session
// environment already pins one.
package museacp

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/binaryutil"
	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	distributionName = "@bex-co/muse-code-acp"
	minimumVersion   = "0.10.0"
)

// muse-code-acp's advertised ACP session modes.
const (
	modeDefault         = "default"
	modeBypassApprovals = "bypassApprovals"
)

type musePlugin interface {
	ResolveBinary(context.Context) (string, error)
	AuthStatus(context.Context) (ports.AgentAuthStatus, error)
}

type binaryResolver func(context.Context) (string, error)

// New constructs Muse's Chat-only driver. Muse intentionally does not
// implement AgentInterfaceHandoff: muse-code-acp's durable session id is not
// yet proven identical to the terminal adapter's continuation id across both
// implementations.
func New(plugin musePlugin, log *slog.Logger) ports.ChatDriver {
	return acpdriver.New(buildConfig(plugin, ResolveBinary, log), log)
}

func buildConfig(plugin musePlugin, resolve binaryResolver, log *slog.Logger) acpdriver.Config {
	return acpdriver.Config{
		Harness: domain.HarnessMuse,
		Capabilities: ports.ChatCapabilities{
			ports.ChatCapabilityStreaming: true,
			ports.ChatCapabilityTools:     true,
			// muse-code-acp routes Muse's approval requests to the client as ACP
			// permission requests, so AO's production floor is fully met.
			ports.ChatCapabilityApprovals: true,
			ports.ChatCapabilityInterrupt: true,
			ports.ChatCapabilityResume:    true,
			// Muse reports no per-turn token usage; muse-code-acp answers a
			// prompt with only a stop reason.
			ports.ChatCapabilityUsage: false,
			ports.ChatCapabilityDiffs: true,
		},
		InstallCheck: func(ctx context.Context) error {
			if _, err := resolve(ctx); err != nil {
				return fmt.Errorf("muse-code-acp is not installed; install it with npm i -g %s (Node 22+)", distributionName)
			}
			return nil
		},
		Probe: func(ctx context.Context) error {
			if _, err := resolve(ctx); err != nil {
				return fmt.Errorf("%w: muse-code-acp is not installed: %w", ports.ErrChatDriverUnavailable, err)
			}
			if _, err := plugin.ResolveBinary(ctx); err != nil {
				return fmt.Errorf("%w: Muse Code is not installed: %w", ports.ErrChatDriverUnavailable, err)
			}
			status, err := plugin.AuthStatus(ctx)
			if err == nil && status == ports.AgentAuthStatusUnauthorized {
				return ports.ErrChatAuthRequired
			}
			if err != nil && log != nil {
				log.Debug("Muse auth probe inconclusive; continuing", "error", err)
			}
			return nil
		},
		Launch: func(ctx context.Context, cfg acpdriver.LaunchConfig) (acpdriver.Launch, error) {
			binary, err := resolve(ctx)
			if err != nil {
				return acpdriver.Launch{}, fmt.Errorf("%w: muse-code-acp is not installed: %w", ports.ErrChatDriverUnavailable, err)
			}
			muse, err := plugin.ResolveBinary(ctx)
			if err != nil {
				return acpdriver.Launch{}, fmt.Errorf("%w: Muse Code is not installed: %w", ports.ErrChatDriverUnavailable, err)
			}
			env := cloneEnv(cfg.Env)
			// muse-code-acp discovers `muse` on PATH, where the name is shared
			// with unrelated tools. Point it at the exact binary the Muse plugin
			// verified; an explicit MUSE_CODE_EXECUTABLE in the session
			// environment keeps precedence.
			if env["MUSE_CODE_EXECUTABLE"] == "" {
				env["MUSE_CODE_EXECUTABLE"] = muse
			}
			return acpdriver.Launch{Command: binary, Env: env}, nil
		},
		ValidateInitialize: validateInitialize,
		SessionMode:        sessionMode,
		SessionOptions:     sessionOptions,
	}
}

// sessionMode maps AO's permission vocabulary onto muse-code-acp's offered
// mode ids. Muse has no edits-only mode, so AcceptEdits leaves the provider's
// prompted default instead of widening to auto-approve. BypassPermissions and
// Auto select bypassApprovals, whose OS sandbox stays on: the adapter's yolo
// mode is opt-in through MUSE_CODE_ACP_ALLOW_YOLO, which AO does not set.
func sessionMode(permission ports.PermissionMode) string {
	switch ports.NormalizePermissionMode(permission) {
	case ports.PermissionModeDefault:
		return modeDefault
	case ports.PermissionModeAuto, ports.PermissionModeBypassPermissions:
		return modeBypassApprovals
	default:
		return ""
	}
}

// sessionOptions maps AO's per-turn choices onto muse-code-acp's advertised
// config option ids: `model` (provider-qualified catalog values) and
// `reasoningEffort` (none through ultra). The adapter rejects a value it did
// not offer, so a wrong choice fails visibly.
func sessionOptions(settings ports.ChatTurnSettings) []acpdriver.SessionOption {
	options := make([]acpdriver.SessionOption, 0, 2)
	if settings.Model != "" {
		options = append(options, acpdriver.SessionOption{ID: "model", Value: settings.Model})
	}
	if settings.Effort != "" {
		options = append(options, acpdriver.SessionOption{ID: "reasoningEffort", Value: settings.Effort})
	}
	return options
}

var versionPattern = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:[-+].*)?$`)

func validateInitialize(init acpsdk.InitializeResponse) error {
	if init.AgentInfo == nil || init.AgentInfo.Name != distributionName {
		got := "missing agentInfo"
		if init.AgentInfo != nil {
			got = init.AgentInfo.Name
		}
		return fmt.Errorf("unsupported Muse ACP distribution %q; install %s", got, distributionName)
	}
	installed, ok := parseVersion(init.AgentInfo.Version)
	minimum, _ := parseVersion(minimumVersion)
	if !ok {
		return fmt.Errorf("unrecognized %s version %q", distributionName, init.AgentInfo.Version)
	}
	if installed.less(minimum) {
		return fmt.Errorf("%s %s is older than AO's tested minimum %s", distributionName, init.AgentInfo.Version, minimumVersion)
	}
	return nil
}

type version [3]int

func parseVersion(value string) (version, bool) {
	match := versionPattern.FindStringSubmatch(strings.TrimSpace(value))
	if len(match) != 4 {
		return version{}, false
	}
	var out version
	for i := range out {
		n, err := strconv.Atoi(match[i+1])
		if err != nil {
			return version{}, false
		}
		out[i] = n
	}
	return out, true
}

func (v version) less(other version) bool {
	for i := range v {
		if v[i] != other[i] {
			return v[i] < other[i]
		}
	}
	return false
}

func cloneEnv(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+1)
	for key, value := range in {
		out[key] = value
	}
	return out
}

var museACPBinarySpec = binaryutil.BinarySpec{
	Label:         "muse-code-acp",
	Names:         []string{"muse-code-acp"},
	WinNames:      []string{"muse-code-acp.cmd", "muse-code-acp.exe", "muse-code-acp"},
	UnixPaths:     []string{"/usr/local/bin/muse-code-acp", "/opt/homebrew/bin/muse-code-acp"},
	UnixHomePaths: binaryutil.NodeManagedUnixHomePaths("muse-code-acp"),
	NodeManaged:   true,
	WinPaths: []binaryutil.WinPath{
		{Base: binaryutil.WinAppData, Parts: []string{"npm", "muse-code-acp.cmd"}},
		{Base: binaryutil.WinAppData, Parts: []string{"npm", "muse-code-acp.exe"}},
	},
}

// ResolveBinary finds a pre-installed muse-code-acp. It never invokes npm,
// npx, or any network installer.
func ResolveBinary(ctx context.Context) (string, error) {
	return binaryutil.ResolveBinary(ctx, museACPBinarySpec)
}
