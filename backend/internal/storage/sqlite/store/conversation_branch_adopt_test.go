package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// AdoptReplacedSessionBranchProvider is a guarded CAS: it may re-point an
// active branch at a replacement session's own provider identity only when
// the branch still carries exactly the stale identity of the recorded owner
// the caller proved. Anything else must be a no-op.
func TestAdoptReplacedSessionBranchProviderGuard(t *testing.T) {
	ctx := context.Background()
	s, predecessor, conversation := seededChatConversation(t)
	root, err := s.ConversationBranch(ctx, conversation.ID, conversation.ActiveBranchID)
	if err != nil {
		t.Fatalf("ConversationBranch: %v", err)
	}
	if root.SessionID != predecessor.ID || root.ProviderConversationID != "thread-root" {
		t.Fatalf("root = %+v, want predecessor identity", root)
	}

	replacement, err := s.CreateSession(ctx, domain.SessionRecord{
		ProjectID: domain.ProjectID("branches"), Kind: domain.KindWorker,
		Harness:   domain.HarnessClaudeCode,
		Activity:  domain.Activity{State: domain.ActivityIdle, LastActivityAt: testNow},
		Metadata:  domain.SessionMetadata{Branch: "feat/x", WorkspacePath: "/ws"},
		CreatedAt: testNow.Add(time.Minute), UpdatedAt: testNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	adopt := func(staleOwner domain.SessionID, staleHandle string) bool {
		t.Helper()
		adopted, err := s.AdoptReplacedSessionBranchProvider(
			ctx, conversation.ID, root.ID,
			staleOwner, staleHandle,
			replacement.ID, "thread-fresh",
		)
		if err != nil {
			t.Fatalf("AdoptReplacedSessionBranchProvider: %v", err)
		}
		return adopted
	}

	// Wrong stale handle: not the identity the caller proved.
	if adopt(predecessor.ID, "other-thread") {
		t.Fatal("adoption with wrong stale handle succeeded, want no-op")
	}
	// Wrong stale owner: not the branch's recorded owner.
	if adopt(replacement.ID, "thread-root") {
		t.Fatal("adoption with wrong stale owner succeeded, want no-op")
	}
	// Exact stale identity and owner: adopt once.
	if !adopt(predecessor.ID, "thread-root") {
		t.Fatal("guarded adoption failed, want the branch re-pointed")
	}
	got, err := s.ConversationBranch(ctx, conversation.ID, root.ID)
	if err != nil {
		t.Fatalf("reload branch: %v", err)
	}
	if got.SessionID != replacement.ID || got.ProviderConversationID != "thread-fresh" {
		t.Fatalf("adopted branch = %+v, want replacement identity", got)
	}
	// Provider scope stays in the namespace events were projected under.
	if got.ProviderScopeID != root.ProviderScopeID || got.ProviderIDsScoped != root.ProviderIDsScoped {
		t.Fatalf("adopted branch provider scope = %q scoped=%v, want %q scoped=%v",
			got.ProviderScopeID, got.ProviderIDsScoped, root.ProviderScopeID, root.ProviderIDsScoped)
	}
	// Replaying the same adoption is a no-op: the stale identity is gone.
	if adopt(predecessor.ID, "thread-root") {
		t.Fatal("replayed adoption succeeded, want no-op after adoption")
	}
}
