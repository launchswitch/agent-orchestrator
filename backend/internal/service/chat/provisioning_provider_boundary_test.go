package chat_test

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

// Provisioning describes this session, not the project narrative it inherits.
// Its first controller must not reuse a retired orchestrator's provider scope,
// even before a first prompt has materialized the new provider's native thread.
func TestProvisioningOrchestratorPublishesProviderBoundaryBeforeFirstTurn(t *testing.T) {
	for _, sourceHandle := range []string{"source-provider-thread", ""} {
		for _, queued := range []bool{false, true} {
			t.Run(fmt.Sprintf("source_handle=%t/queued=%t", sourceHandle != "", queued), func(t *testing.T) {
				ctx := context.Background()
				st := openStore(t)
				now := time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)
				source, found, err := st.GetSession(ctx, testSession)
				if err != nil || !found {
					t.Fatalf("source session: found=%v err=%v", found, err)
				}
				source.Kind = domain.KindOrchestrator
				source.Harness = domain.HarnessOpenCodeV2
				source.Metadata.ProviderConversationID = sourceHandle
				source.Metadata.ControllerGeneration = "source-generation"
				source.CreatedAt = now
				if err := st.UpdateSession(ctx, source); err != nil {
					t.Fatal(err)
				}
				before, err := st.CreateConversation(ctx, "inherited-project",
					domain.ConversationScopeProject, testProject, source.ID, now)
				if err != nil {
					t.Fatal(err)
				}
				if err := st.UpsertActivity(ctx, before.ID, "", domain.ConversationActivity{
					ID: "source-history", Kind: domain.ActivityKindSystem,
					Status: domain.ActivityStatusCompleted, Summary: "retained predecessor history",
					ProviderItemID: "source-history",
				}, now); err != nil {
					t.Fatal(err)
				}
				sourceBranch, err := st.ConversationBranch(ctx, before.ID, before.ActiveBranchID)
				if err != nil {
					t.Fatal(err)
				}
				source.IsTerminated = true
				if err := st.UpdateSession(ctx, source); err != nil {
					t.Fatal(err)
				}
				target, err := st.CreateSession(ctx, domain.SessionRecord{
					ProjectID: testProject, Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex,
					Mode: domain.SessionModeChat, ProvisionState: domain.SessionProvisionProvisioning,
					CreatedAt: now.Add(time.Minute), UpdatedAt: now.Add(time.Minute),
				})
				if err != nil {
					t.Fatal(err)
				}

				fresh := newFakeConversation()
				fresh.providerConversationID = "fresh-provider-thread"
				var started ports.ChatStartConfig
				nextID := 0
				svc := chatsvc.New(chatsvc.Options{
					Store: st, Sessions: st, Reader: fullSnapshotReader(st),
					Drivers: fakeRegistry{driver: fakeDriver{conv: fresh, startCfg: &started}},
					Log:     slog.New(slog.DiscardHandler),
					NewID: func() string {
						nextID++
						return fmt.Sprintf("replacement-%d", nextID)
					},
					Now: func() time.Time { return now.Add(2 * time.Minute) },
				})
				t.Cleanup(func() { _ = svc.Stop(context.Background(), target.ID) })
				var queuedTurn domain.ConversationTurn
				if queued {
					queuedTurn, err = svc.Send(ctx, target.ID, ports.ChatUserMessage{
						Text: "opening brief", Origin: domain.MessageOriginHuman, ClientMessageID: "opening-brief",
					})
					if err != nil {
						t.Fatal(err)
					}
				}
				lcm := lifecycle.New(st, nil)
				if _, err := svc.Start(ctx, chatsvc.StartConfig{
					SessionID: target.ID, ProjectID: testProject, Kind: domain.KindOrchestrator,
					Harness: domain.HarnessCodex, WorkspacePath: t.TempDir(),
					ControllerReady: func(result chatsvc.StartResult) (chatsvc.ControllerCommit, error) {
						if result.ProviderBoundary == nil {
							return chatsvc.ControllerCommit{}, fmt.Errorf("provisioning replacement did not reserve a provider boundary")
						}
						if err := lcm.MarkChatSpawned(ctx, target.ID, domain.SessionMetadata{
							ProviderConversationID: result.ProviderConversationID,
							ControllerGeneration:   result.ControllerGeneration,
						}, *result.ProviderBoundary); err != nil {
							return chatsvc.ControllerCommit{}, err
						}
						committed := result.Conversation
						committed.ActiveBranchID = result.ProviderBoundary.ID
						return chatsvc.ControllerCommit{Conversation: committed}, nil
					},
				}); err != nil {
					t.Fatalf("start provisioning replacement: %v", err)
				}
				after, err := st.ConversationForSession(ctx, target.ID)
				if err != nil {
					t.Fatal(err)
				}
				boundary, err := st.ConversationBranch(ctx, after.ID, after.ActiveBranchID)
				if err != nil {
					t.Fatal(err)
				}
				if after.ID != before.ID || boundary.ParentBranchID != sourceBranch.ID ||
					boundary.SessionID != target.ID || boundary.ProviderConversationID != fresh.providerConversationID ||
					boundary.ProviderScopeID != started.ProviderScopeID || boundary.ID != started.ProviderScopeID ||
					boundary.ProviderScopeID == sourceBranch.ProviderScopeID {
					t.Fatalf("incorrect replacement boundary: %+v; provider start: %+v", boundary, started)
				}
				unchanged, err := st.ConversationBranch(ctx, before.ID, sourceBranch.ID)
				if err != nil {
					t.Fatal(err)
				}
				unchanged.Active = sourceBranch.Active
				if !reflect.DeepEqual(unchanged, sourceBranch) {
					t.Fatalf("predecessor branch was rewritten: before=%+v after=%+v", sourceBranch, unchanged)
				}
				if queued {
					snapshot, err := svc.Snapshot(ctx, target.ID)
					if err != nil {
						t.Fatal(err)
					}
					if len(snapshot.Turns) != 1 || snapshot.Turns[0].ID != queuedTurn.ID ||
						snapshot.Turns[0].State != domain.TurnStateQueued {
						t.Fatalf("opening brief did not survive ownership publication: %+v", snapshot.Turns)
					}
					if err := svc.DrainQueued(ctx, target.ID); err != nil {
						t.Fatal(err)
					}
					snapshot, err = svc.Snapshot(ctx, target.ID)
					if err != nil {
						t.Fatal(err)
					}
					if len(snapshot.Turns) != 1 || snapshot.Turns[0].ID != queuedTurn.ID ||
						snapshot.Turns[0].State != domain.TurnStateRunning {
						t.Fatalf("opening brief did not dispatch on the replacement: %+v", snapshot.Turns)
					}
				}
				if err := svc.Stop(ctx, target.ID); err != nil {
					t.Fatal(err)
				}

				// A new service instance models a cold daemon: resume must reach the
				// target's own provider handle and namespace, not reject the old head.
				resumed := newFakeConversation()
				resumed.providerConversationID = fresh.providerConversationID
				var resumeCfg ports.ChatResumeConfig
				restarted := chatsvc.New(chatsvc.Options{
					Store: st, Sessions: st,
					Drivers: fakeRegistry{driver: fakeDriver{conv: resumed, resumeCfg: &resumeCfg}},
					Log:     slog.New(slog.DiscardHandler),
					NewID:   func() string { return "resumed-generation" },
				})
				t.Cleanup(func() { _ = restarted.Stop(context.Background(), target.ID) })
				if _, err := restarted.Start(ctx, chatsvc.StartConfig{
					SessionID: target.ID, ProjectID: testProject, Kind: domain.KindOrchestrator,
					Harness: domain.HarnessCodex, WorkspacePath: t.TempDir(),
					ProviderConversationID: fresh.providerConversationID, HistoryMode: ports.ChatHistoryDeferred,
				}); err != nil {
					t.Fatalf("cold resume of replacement: %v", err)
				}
				if resumeCfg.ProviderConversationID != fresh.providerConversationID || resumeCfg.ProviderScopeID != boundary.ID {
					t.Fatalf("cold resume used incorrect identity: %+v", resumeCfg)
				}
			})
		}
	}
}
