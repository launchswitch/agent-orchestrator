package musemsp

import (
	"context"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// MuseEffortLevels is the reasoning-effort vocabulary `muse --help`
// documents for --reasoning-effort. model/list carries no per-model effort
// data, so discovery advertises the provider-wide set and the provider
// validates at launch.
var MuseEffortLevels = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// MuseDefaultEffort is the provider default when no effort is selected.
const MuseDefaultEffort = "high"

// ListCatalogModels opens an ephemeral host, reads model/list, and returns
// the normalized picker rows. It never creates a session.
func ListCatalogModels(ctx context.Context, binary, workingDir string, env map[string]string) ([]ports.AgentModelInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client, err := Spawn(ctx, binary, workingDir, env, true, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Handshake(ctx); err != nil {
		return nil, err
	}
	models, err := client.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	return MapModels(models), nil
}

// MapModels normalizes MSP models to picker rows, attaching the documented
// effort vocabulary so the effort picker lights up for Muse sessions.
func MapModels(models []Model) []ports.AgentModelInfo {
	out := make([]ports.AgentModelInfo, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		label := strings.TrimSpace(model.Label)
		if label == "" {
			label = id
		}
		out = append(out, ports.AgentModelInfo{
			ID:            id,
			Label:         label,
			Provider:      strings.TrimSpace(model.Provider),
			IsDefault:     model.Default,
			Efforts:       append([]string(nil), MuseEffortLevels...),
			DefaultEffort: MuseDefaultEffort,
		})
	}
	return out
}
