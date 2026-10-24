package museacp_test

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/muse"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestMuseRemainsChatOnlyWithoutInterfaceHandoff(t *testing.T) {
	var plugin any = muse.New()
	if _, ok := plugin.(ports.AgentInterfaceHandoff); ok {
		t.Fatal("Muse unexpectedly implements TUI/Chat handoff without a verified shared conversation identity")
	}
}
