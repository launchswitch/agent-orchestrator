package chat_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// These tests lock the provider-identity invariant of an active conversation
// branch across a session replacement: a resume may adopt a predecessor's
// stale branch identity only when the resuming session durably owns its own
// handle and the stale owner is the session itself or a terminated
// predecessor. Any other mismatch stays rejected before provider launch.

const (
	stalePredecessorHandle = "ses_ece24acc5ffeDszVrhok8qa1Ox"
	staleReplacementHandle = "ses_ecd60036affeFjMrENb8JFIrm7"
	staleReservedUUID      = "3f6b9a52-1c4d-4e8f-9a21-7b5d0c8e2f10"
)

type staleBranchFixture struct {
	store        *sqlite.Store
	predecessor  domain.SessionRecord
	replacement  domain.SessionRecord
	conversation domain.ConversationRecord
	root         domain.ConversationBranch
	now          time.Time
}

// seedStaleBranchFixture reproduces the stale state a lost boundary
// publication leaves behind: the project conversation's root branch still
// carries the terminated predecessor's provider identity while the
// conversation is rebound to a live replacement whose own record durably
// holds its replacement handle.
func seedStaleBranchFixture(
	t *testing.T,
	predecessorHarness domain.AgentHarness,
	replacementHarness domain.AgentHarness,
) staleBranchFixture {
	t.Helper()
	ctx := context.Background()
	st := openStore(t)
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)

	predecessor, found, err := st.GetSession(ctx, testSession)
	if err != nil || !found {
		t.Fatalf("load predecessor session: found=%v err=%v", found, err)
	}
	predecessor.Harness = predecessorHarness
	predecessor.Metadata.ProviderConversationID = stalePredecessorHandle
	predecessor.Metadata.ControllerGeneration = "generation-428"
	predecessor.Metadata.WorkspacePath = "/worktrees/orchestrator-428"
	predecessor.Metadata.Branch = "ao/orchestrator"
	predecessor.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now}
	predecessor.UpdatedAt = now
	if err := st.UpdateSession(ctx, predecessor); err != nil {
		t.Fatalf("seed predecessor provider owner: %v", err)
	}
	conversation, err := st.CreateConversation(ctx, "project-conversation",
		domain.ConversationScopeProject, testProject, predecessor.ID, now)
	if err != nil {
		t.Fatalf("create project conversation: %v", err)
	}
	if created, err := st.AppendUserMessage(ctx, conversation.ID, predecessor.ID,
		predecessor.Metadata.ControllerGeneration, domain.ConversationMessage{
			ID: "old-user-message", Text: "preserve the old orchestrator transcript",
			Origin: domain.MessageOriginHuman, ClientMessageID: "old-client-message",
		}, "old-turn", now.Add(time.Second)); err != nil || !created {
		t.Fatalf("seed predecessor transcript: created=%v err=%v", created, err)
	}

	// Retire the predecessor exactly as RetireForReplacement leaves it.
	predecessor.IsTerminated = true
	predecessor.Activity = domain.Activity{State: domain.ActivityExited, LastActivityAt: now.Add(2 * time.Second)}
	predecessor.UpdatedAt = now.Add(2 * time.Second)
	if err := st.UpdateSession(ctx, predecessor); err != nil {
		t.Fatalf("terminate predecessor: %v", err)
	}

	replacement, err := st.CreateSession(ctx, domain.SessionRecord{
		ProjectID: testProject, Kind: domain.KindOrchestrator,
		Harness: replacementHarness, Mode: domain.SessionModeChat,
		Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(3 * time.Second)},
		Metadata: domain.SessionMetadata{
			ProviderConversationID: staleReplacementHandle,
			ControllerGeneration:   "generation-435",
			WorkspacePath:          "/worktrees/orchestrator-435",
			Branch:                 "ao/orchestrator",
		},
		CreatedAt: now.Add(3 * time.Second), UpdatedAt: now.Add(3 * time.Second),
	})
	if err != nil {
		t.Fatalf("create replacement: %v", err)
	}
	// Rebind the project conversation to the replacement the way a spawn's
	// CreateProjectConversationWithContextReset does.
	if _, err := st.CreateConversation(ctx, "unused-rebound-conversation",
		domain.ConversationScopeProject, testProject, replacement.ID, now.Add(4*time.Second)); err != nil {
		t.Fatalf("rebind project conversation: %v", err)
	}

	root, err := st.ConversationBranch(ctx, conversation.ID, conversation.ActiveBranchID)
	if err != nil {
		t.Fatalf("load root branch: %v", err)
	}
	if root.SessionID != predecessor.ID || root.ProviderConversationID != stalePredecessorHandle {
		t.Fatalf("root = %+v, want stale predecessor identity", root)
	}
	return staleBranchFixture{
		store: st, predecessor: predecessor, replacement: replacement,
		conversation: conversation, root: root, now: now,
	}
}

func startStaleFixtureReplacement(
	t *testing.T,
	fixture staleBranchFixture,
	providerConversationID string,
) (*chatsvc.Service, ports.ChatResumeConfig, error) {
	t.Helper()
	resumedConversation := newFakeConversation()
	resumedConversation.providerConversationID = providerConversationID
	var resumed ports.ChatResumeConfig
	svc := chatsvc.New(chatsvc.Options{
		Store: fixture.store, Sessions: fixture.store,
		Drivers: fakeRegistry{driver: fakeDriver{
			conv:      resumedConversation,
			resumeCfg: &resumed,
		}},
		Log:   slog.New(slog.DiscardHandler),
		NewID: func() string { return "unused-stale-branch-identity" },
		Now:   func() time.Time { return fixture.now.Add(time.Hour) },
	})
	t.Cleanup(func() { _ = svc.Stop(context.Background(), fixture.replacement.ID) })
	_, err := svc.Start(context.Background(), chatsvc.StartConfig{
		SessionID: fixture.replacement.ID, ProjectID: testProject,
		Kind: domain.KindOrchestrator, Harness: fixture.replacement.Harness,
		WorkspacePath:          fixture.replacement.Metadata.WorkspacePath,
		ProviderConversationID: providerConversationID,
		HistoryMode:            ports.ChatHistoryDeferred,
	})
	return svc, resumed, err
}

func assertStaleBranchIdentityAdopted(t *testing.T, fixture staleBranchFixture) {
	t.Helper()
	root, err := fixture.store.ConversationBranch(
		context.Background(), fixture.conversation.ID, fixture.root.ID)
	if err != nil {
		t.Fatalf("reload root branch: %v", err)
	}
	if root.SessionID != fixture.replacement.ID ||
		root.ProviderConversationID != staleReplacementHandle {
		t.Fatalf("root after resume = %+v, want replacement identity adopted", root)
	}
	// Provider scope is deliberately untouched so events already projected
	// under the branch's scope keep their namespace.
	if root.ProviderScopeID != fixture.root.ProviderScopeID ||
		root.ProviderIDsScoped != fixture.root.ProviderIDsScoped {
		t.Fatalf("root provider scope changed: %+v", root)
	}
}

func TestReplacedOrchestratorResumeAdoptsStaleBranchIdentity(t *testing.T) {
	fixture := seedStaleBranchFixture(t, domain.HarnessCodex, domain.HarnessCodex)
	_, resumed, err := startStaleFixtureReplacement(t, fixture, staleReplacementHandle)
	if err != nil {
		t.Fatalf("resume after replacement = %v, want stale branch identity adopted", err)
	}
	if resumed.ProviderConversationID != staleReplacementHandle {
		t.Fatalf("resumed provider handle = %q, want %q", resumed.ProviderConversationID, staleReplacementHandle)
	}
	assertStaleBranchIdentityAdopted(t, fixture)
}

func TestReplacedOrchestratorResumeAdoptsStaleBranchIdentityAcrossHarnesses(t *testing.T) {
	fixture := seedStaleBranchFixture(t, domain.HarnessClaudeCode, domain.HarnessOpenCode)
	_, resumed, err := startStaleFixtureReplacement(t, fixture, staleReplacementHandle)
	if err != nil {
		t.Fatalf("cross-harness resume after replacement = %v, want stale branch identity adopted", err)
	}
	if resumed.ProviderConversationID != staleReplacementHandle {
		t.Fatalf("resumed provider handle = %q, want %q", resumed.ProviderConversationID, staleReplacementHandle)
	}
	assertStaleBranchIdentityAdopted(t, fixture)
}

func TestReplacedOrchestratorResumeAdoptsOwnStaleReservedBranchIdentity(t *testing.T) {
	// The earlier incident shape: the branch belongs to the resuming session
	// itself but still holds a reserved, never-published boundary id while the
	// session's record has since advanced to its real handle.
	ctx := context.Background()
	st := openStore(t)
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	session, found, err := st.GetSession(ctx, testSession)
	if err != nil || !found {
		t.Fatalf("load session: found=%v err=%v", found, err)
	}
	session.Metadata.ProviderConversationID = staleReservedUUID
	session.Metadata.ControllerGeneration = "generation-reserved"
	if err := st.UpdateSession(ctx, session); err != nil {
		t.Fatalf("seed reserved boundary: %v", err)
	}
	conversation, err := st.CreateConversation(ctx, "own-stale-conversation",
		domain.ConversationScopeProject, testProject, session.ID, now)
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	session.Metadata.ProviderConversationID = staleReplacementHandle
	session.UpdatedAt = now.Add(time.Second)
	if err := st.UpdateSession(ctx, session); err != nil {
		t.Fatalf("advance session handle: %v", err)
	}
	root, err := st.ConversationBranch(ctx, conversation.ID, conversation.ActiveBranchID)
	if err != nil {
		t.Fatalf("load root branch: %v", err)
	}
	if root.SessionID != session.ID || root.ProviderConversationID != staleReservedUUID {
		t.Fatalf("root = %+v, want own reserved boundary identity", root)
	}

	fixture := staleBranchFixture{
		store: st, replacement: session, conversation: conversation, root: root, now: now,
	}
	_, resumed, err := startStaleFixtureReplacement(t, fixture, staleReplacementHandle)
	if err != nil {
		t.Fatalf("resume with own stale reserved identity = %v, want adopted", err)
	}
	if resumed.ProviderConversationID != staleReplacementHandle {
		t.Fatalf("resumed provider handle = %q, want %q", resumed.ProviderConversationID, staleReplacementHandle)
	}
	assertStaleBranchIdentityAdopted(t, fixture)
}

func TestResumeRejectsLivePredecessorBranchIdentity(t *testing.T) {
	// A live predecessor still owns its narrative. No replacement happened,
	// so even the replacement's own durable handle cannot steal the branch.
	ctx := context.Background()
	fixture := seedStaleBranchFixture(t, domain.HarnessCodex, domain.HarnessCodex)
	predecessor, found, err := fixture.store.GetSession(ctx, fixture.predecessor.ID)
	if err != nil || !found {
		t.Fatalf("load predecessor: found=%v err=%v", found, err)
	}
	predecessor.IsTerminated = false
	if err := fixture.store.UpdateSession(ctx, predecessor); err != nil {
		t.Fatalf("revive predecessor: %v", err)
	}

	resumeCalls := 0
	var resumed ports.ChatResumeConfig
	svc := chatsvc.New(chatsvc.Options{
		Store: fixture.store, Sessions: fixture.store,
		Drivers: fakeRegistry{driver: fakeDriver{
			conv: newFakeConversation(), resumeCfg: &resumed,
			resume: func(ports.ChatResumeConfig) (ports.ChatConversation, error) {
				resumeCalls++
				return newFakeConversation(), nil
			},
		}},
		Log:   slog.New(slog.DiscardHandler),
		NewID: func() string { return "unused-stale-branch-identity" },
		Now:   func() time.Time { return fixture.now.Add(time.Hour) },
	})
	t.Cleanup(func() { _ = svc.Stop(context.Background(), fixture.replacement.ID) })
	_, err = svc.Start(context.Background(), chatsvc.StartConfig{
		SessionID: fixture.replacement.ID, ProjectID: testProject,
		Kind: domain.KindOrchestrator, Harness: fixture.replacement.Harness,
		WorkspacePath:          fixture.replacement.Metadata.WorkspacePath,
		ProviderConversationID: staleReplacementHandle,
		HistoryMode:            ports.ChatHistoryDeferred,
	})
	if err == nil || !strings.Contains(err.Error(), "does not match session handle") {
		t.Fatalf("live predecessor resume error = %v, want mismatch rejection", err)
	}
	if resumeCalls != 0 {
		t.Fatalf("provider resume calls = %d, want rejection before provider launch", resumeCalls)
	}
	root, err := fixture.store.ConversationBranch(ctx, fixture.conversation.ID, fixture.root.ID)
	if err != nil {
		t.Fatalf("reload root branch: %v", err)
	}
	if root.SessionID != fixture.predecessor.ID || root.ProviderConversationID != stalePredecessorHandle {
		t.Fatalf("root after rejected resume = %+v, want predecessor identity preserved", root)
	}
}

func TestResumeRejectsHandleOutsideReplacedSessionRecord(t *testing.T) {
	// Even with a terminated predecessor, a caller claiming a handle the
	// replacement's own record never published is a true conflict.
	ctx := context.Background()
	fixture := seedStaleBranchFixture(t, domain.HarnessCodex, domain.HarnessCodex)

	resumeCalls := 0
	var resumed ports.ChatResumeConfig
	svc := chatsvc.New(chatsvc.Options{
		Store: fixture.store, Sessions: fixture.store,
		Drivers: fakeRegistry{driver: fakeDriver{
			conv: newFakeConversation(), resumeCfg: &resumed,
			resume: func(ports.ChatResumeConfig) (ports.ChatConversation, error) {
				resumeCalls++
				return newFakeConversation(), nil
			},
		}},
		Log:   slog.New(slog.DiscardHandler),
		NewID: func() string { return "unused-stale-branch-identity" },
		Now:   func() time.Time { return fixture.now.Add(time.Hour) },
	})
	t.Cleanup(func() { _ = svc.Stop(context.Background(), fixture.replacement.ID) })
	_, err := svc.Start(context.Background(), chatsvc.StartConfig{
		SessionID: fixture.replacement.ID, ProjectID: testProject,
		Kind: domain.KindOrchestrator, Harness: fixture.replacement.Harness,
		WorkspacePath:          fixture.replacement.Metadata.WorkspacePath,
		ProviderConversationID: "unowned-provider-thread",
		HistoryMode:            ports.ChatHistoryDeferred,
	})
	if err == nil || !strings.Contains(err.Error(), "does not match session handle") {
		t.Fatalf("unowned handle resume error = %v, want mismatch rejection", err)
	}
	if resumeCalls != 0 {
		t.Fatalf("provider resume calls = %d, want rejection before provider launch", resumeCalls)
	}
	root, err := fixture.store.ConversationBranch(ctx, fixture.conversation.ID, fixture.root.ID)
	if err != nil {
		t.Fatalf("reload root branch: %v", err)
	}
	if root.SessionID != fixture.predecessor.ID || root.ProviderConversationID != stalePredecessorHandle {
		t.Fatalf("root after rejected resume = %+v, want predecessor identity preserved", root)
	}
}
