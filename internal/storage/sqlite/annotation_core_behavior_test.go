package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/zhanglei10281852-gif/ai/internal/domain"
	"github.com/zhanglei10281852-gif/ai/internal/repository"
)

func TestInferenceRunReadPreservesDistinctLifecycleTimes(t *testing.T) {
	store, ctx, now := testStore(t)
	workspace, origin, destination, pool, _ := seedCatalog(t, store, ctx, now)
	startedAt := now.Add(10 * time.Minute).UTC()
	completedAt := now.Add(40 * time.Minute).UTC()
	run := domain.InferenceRun{
		ID: "run_lifecycle_times", WorkspaceID: workspace.ID, SourceZoneID: origin.ID,
		TargetZoneID: destination.ID, ComputePoolID: pool.ID, Reference: "LIFECYCLE-TIMES",
		State: domain.InferenceRunCompleted, ScheduledStartAt: now,
		ExpectedFinishAt: now.Add(time.Hour), StartedAt: &startedAt, CompletedAt: &completedAt,
		TotalEstimatedRows: 100, Version: 1, CreatedAt: now, UpdatedAt: completedAt,
	}
	if err := store.WithTx(ctx, func(tx repository.Tx) error { return tx.InsertInferenceRun(ctx, run) }); err != nil {
		t.Fatal(err)
	}

	var restored domain.InferenceRun
	if err := store.Read(ctx, func(reader repository.Reader) error {
		var err error
		restored, err = reader.GetInferenceRun(ctx, run.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if restored.StartedAt == nil || restored.CompletedAt == nil {
		t.Fatalf("restored lifecycle timestamps = %+v", restored)
	}
	if !restored.StartedAt.Equal(startedAt) || !restored.CompletedAt.Equal(completedAt) {
		t.Fatalf("restored start/completion = %s / %s, want %s / %s", restored.StartedAt, restored.CompletedAt, startedAt, completedAt)
	}
	if !restored.CompletedAt.After(*restored.StartedAt) {
		t.Fatalf("restored lifecycle duration is not positive: %+v", restored)
	}
}

func TestTransactionRollsBackWorkspaceWhenAuditInsertFails(t *testing.T) {
	store, ctx, now := testStore(t)
	workspace, _, _, _, _ := seedCatalog(t, store, ctx, now)
	collision := domain.AuditEvent{
		ID: "audit_transaction_collision", RequestID: "request-transaction-collision", Actor: "system",
		Action: "workspace_created", EntityType: "workspace", EntityID: "workspace-transaction-collision",
		Outcome: "success", Metadata: map[string]string{"source": "fixture"}, CreatedAt: now,
	}
	if err := store.WithTx(ctx, func(tx repository.Tx) error {
		return tx.InsertAuditEvent(ctx, collision)
	}); err != nil {
		t.Fatal(err)
	}

	candidate := workspace
	candidate.ID = "workspace_transaction_rollback"
	candidate.Code = "TX-ROLLBACK"
	candidate.Name = "Transaction rollback workspace"
	candidate.Status = domain.WorkspaceDraft
	err := store.WithTx(ctx, func(tx repository.Tx) error {
		if err := tx.InsertWorkspace(ctx, candidate); err != nil {
			return err
		}
		return tx.InsertAuditEvent(ctx, collision)
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("transaction error = %v, want conflict", err)
	}

	if err := store.Read(ctx, func(reader repository.Reader) error {
		if _, err := reader.GetWorkspace(ctx, candidate.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("workspace after failed transaction error = %v, want not found", err)
		}
		page, err := reader.ListAuditEvents(ctx, repository.AuditFilter{
			Page: repository.PageRequest{Limit: 10}, EntityID: collision.EntityID,
		})
		if err != nil {
			return err
		}
		if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != collision.ID {
			t.Fatalf("audit rows after failed transaction = %+v", page)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
