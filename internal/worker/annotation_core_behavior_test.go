package worker

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/zhanglei10281852-gif/ai/internal/domain"
	"github.com/zhanglei10281852-gif/ai/internal/repository"
	"github.com/zhanglei10281852-gif/ai/internal/requestmeta"
	"github.com/zhanglei10281852-gif/ai/internal/service"
)

func TestPlannedOutboxPayloadMatchesPersistedRun(t *testing.T) {
	worker, store, ctx, now := workerFixture(t)
	minimum, _ := domain.ScoreFromFloat(2)
	maximum, _ := domain.ScoreFromFloat(8)
	rangeValue, _ := domain.NewQualityRange(minimum, maximum)
	workspace := domain.Workspace{
		ID: "workspace_outbox", Code: "OUTBOX", Name: "Outbox contract", Status: domain.WorkspaceActive,
		Score: rangeValue, MaxExecution: 4 * time.Hour, ReviewDeadline: time.Hour,
		BusinessTimezone: "UTC", Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	origin := domain.DataZone{ID: "origin_outbox", Code: "ORIGIN-O", Name: "Origin", Timezone: "UTC", Status: domain.DataZoneActive, DailyLimit: 10, CutoffHour: 0, Version: 1, CreatedAt: now, UpdatedAt: now}
	destination := domain.DataZone{ID: "destination_outbox", Code: "DEST-O", Name: "Destination", Timezone: "UTC", Status: domain.DataZoneActive, DailyLimit: 10, CutoffHour: 0, Version: 1, CreatedAt: now, UpdatedAt: now}
	pool := domain.ComputePool{ID: "pool_outbox", SerialNumber: "POOL-O", State: domain.ComputePoolAvailable, CapacityRows: 1000, AttestationDueAt: now.Add(48 * time.Hour), LastReconciledAt: now, Version: 1, CreatedAt: now, UpdatedAt: now}
	snapshot := domain.DatasetSnapshot{ID: "snapshot_outbox", WorkspaceID: workspace.ID, SourceZoneID: origin.ID, SourceRevision: "REV-O", SchemaFamily: "ranking-v6", PartitionCount: 2, EstimatedRows: 240, State: domain.SnapshotValidated, ExpiresAt: now.Add(48 * time.Hour), Version: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.WithTx(ctx, func(tx repository.Tx) error {
		if err := tx.InsertWorkspace(ctx, workspace); err != nil {
			return err
		}
		if err := tx.InsertDataZone(ctx, origin); err != nil {
			return err
		}
		if err := tx.InsertDataZone(ctx, destination); err != nil {
			return err
		}
		if err := tx.InsertComputePool(ctx, pool); err != nil {
			return err
		}
		return tx.InsertDatasetSnapshot(ctx, snapshot)
	}); err != nil {
		t.Fatal(err)
	}

	services := service.New(store, worker.clock, 4*time.Hour, 30*time.Minute)
	principalCtx := requestmeta.WithPrincipal(ctx, domain.Principal{UserID: "ml_outbox", Role: domain.RoleMLEngineer})
	created, err := services.Inference.PlanInferenceRun(principalCtx, service.PlanInferenceRunInput{
		WorkspaceID: workspace.ID, SourceZoneID: origin.ID, TargetZoneID: destination.ID,
		ComputePoolID: pool.ID, Reference: "OUTBOX-RUN", SnapshotIDs: []string{snapshot.ID},
		ScheduledStartAt: now.Add(time.Hour), ExpectedFinishAt: now.Add(2 * time.Hour),
		IdempotencyKey: "outbox-contract-key",
	})
	if err != nil {
		t.Fatal(err)
	}

	var persisted domain.InferenceRun
	if err := store.Read(ctx, func(reader repository.Reader) error {
		var err error
		persisted, err = reader.GetInferenceRun(ctx, created.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WithTx(ctx, func(tx repository.Tx) error {
		jobs, err := tx.ClaimJobs(ctx, now, 10)
		if err != nil {
			return err
		}
		if len(jobs) != 1 {
			t.Fatalf("planned jobs = %+v", jobs)
		}
		job := jobs[0]
		if job.Kind != "inference_run_planned" || job.AggregateID != persisted.ID {
			t.Fatalf("planned job = %+v", job)
		}
		var event domain.InferenceRun
		if err := json.Unmarshal(job.Payload, &event); err != nil {
			t.Fatalf("decode planned run payload: %v", err)
		}
		if event.ID != persisted.ID || event.Reference != persisted.Reference || event.TotalEstimatedRows != persisted.TotalEstimatedRows {
			t.Fatalf("planned payload = %+v, persisted run = %+v", event, persisted)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
