package service

import (
	"errors"
	"testing"
	"time"

	"github.com/zhanglei10281852-gif/ai/internal/domain"
	"github.com/zhanglei10281852-gif/ai/internal/repository"
)

func TestLogoutRevokesOnlyCurrentSession(t *testing.T) {
	f := newServiceFixture(t)
	first, err := f.services.Auth.Login(f.ctx, LoginInput{Email: f.ml_engineer.Email, Password: "very-secure-password"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.services.Auth.Login(f.ctx, LoginInput{Email: f.ml_engineer.Email, Password: "very-secure-password"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == second.Token || first.Principal.SessionID == second.Principal.SessionID {
		t.Fatal("independent logins returned the same session")
	}

	if err := f.services.Auth.Logout(f.as(first.Principal), first.Principal); err != nil {
		t.Fatal(err)
	}
	if _, err := f.services.Auth.Authenticate(f.ctx, first.Token); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("logged-out session authentication error = %v", err)
	}
	principal, err := f.services.Auth.Authenticate(f.ctx, second.Token)
	if err != nil {
		t.Fatalf("other active session authentication: %v", err)
	}
	if principal.UserID != f.ml_engineer.UserID || principal.SessionID != second.Principal.SessionID {
		t.Fatalf("other session principal = %+v", principal)
	}
}

func TestLoginSessionTTLStartsAtLogin(t *testing.T) {
	f := newServiceFixture(t)
	f.clock.Advance(24 * time.Hour)
	loggedInAt := f.clock.Now()

	login, err := f.services.Auth.Login(f.ctx, LoginInput{Email: f.ml_engineer.Email, Password: "very-secure-password"})
	if err != nil {
		t.Fatal(err)
	}
	wantExpiry := loggedInAt.Add(4 * time.Hour)
	if !login.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("session expiry = %s, want %s", login.ExpiresAt, wantExpiry)
	}
	if _, err := f.services.Auth.Authenticate(f.ctx, login.Token); err != nil {
		t.Fatalf("new session is not immediately usable: %v", err)
	}
}

func TestDuplicateSnapshotRegistrationDoesNotReturnGhostRecord(t *testing.T) {
	f := newServiceFixture(t)
	ctx := f.as(f.ml_engineer)
	_, err := f.services.Catalog.RegisterSnapshot(ctx, domain.DatasetSnapshot{
		WorkspaceID: f.workspace.ID, SourceZoneID: f.origin.ID,
		SourceRevision: f.batch.SourceRevision, SchemaFamily: "plasma-v2",
		PartitionCount: 3, EstimatedRows: 150, ExpiresAt: f.clock.Now().Add(72 * time.Hour),
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate registration error = %v", err)
	}

	page, err := f.services.Query.Snapshots(ctx, repository.SnapshotFilter{
		Page: repository.PageRequest{Limit: 20}, WorkspaceID: f.workspace.ID, DataZoneID: f.origin.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != f.batch.ID {
		t.Fatalf("snapshots after duplicate registration = %+v", page)
	}
}

func TestDailyLimitUsesScheduledBusinessDay(t *testing.T) {
	f := newServiceFixture(t)
	ctx := f.as(f.ml_engineer)
	source, err := f.services.Catalog.CreateDataZone(ctx, domain.DataZone{
		Code: "SCHEDULED-DAY", Name: "Scheduled day source", Timezone: "UTC", DailyLimit: 1, CutoffHour: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := f.services.Catalog.CreateComputePool(ctx, domain.ComputePool{
		SerialNumber: "SCHEDULED-POOL", CapacityRows: 1000,
		AttestationDueAt: f.clock.Now().Add(72 * time.Hour), LastReconciledAt: f.clock.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.services.Catalog.RegisterSnapshot(ctx, domain.DatasetSnapshot{
		WorkspaceID: f.workspace.ID, SourceZoneID: source.ID, SourceRevision: "SCHEDULED-REV",
		SchemaFamily: "agent-policy", PartitionCount: 1, EstimatedRows: 100,
		ExpiresAt: f.clock.Now().Add(72 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = f.services.Catalog.ValidateSnapshot(ctx, snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}

	todayStart := f.clock.Now().Add(time.Hour)
	existing := domain.InferenceRun{
		ID: "run_today_quota", WorkspaceID: f.workspace.ID, SourceZoneID: source.ID,
		TargetZoneID: f.destination.ID, ComputePoolID: pool.ID, Reference: "TODAY-QUOTA",
		State: domain.InferenceRunQueued, ScheduledStartAt: todayStart,
		ExpectedFinishAt: todayStart.Add(time.Hour), TotalEstimatedRows: 10,
		Version: 1, CreatedAt: f.clock.Now(), UpdatedAt: f.clock.Now(),
	}
	if err := f.store.WithTx(ctx, func(tx repository.Tx) error { return tx.InsertInferenceRun(ctx, existing) }); err != nil {
		t.Fatal(err)
	}

	tomorrowStart := f.clock.Now().Add(25 * time.Hour)
	created, err := f.services.Inference.PlanInferenceRun(ctx, PlanInferenceRunInput{
		WorkspaceID: f.workspace.ID, SourceZoneID: source.ID, TargetZoneID: f.destination.ID,
		ComputePoolID: pool.ID, Reference: "TOMORROW-QUOTA", SnapshotIDs: []string{snapshot.ID},
		ScheduledStartAt: tomorrowStart, ExpectedFinishAt: tomorrowStart.Add(time.Hour),
		IdempotencyKey: "tomorrow-quota-key",
	})
	if err != nil {
		t.Fatalf("plan for the next business day: %v", err)
	}
	if created.ScheduledStartAt.UTC().Format("2006-01-02") == existing.ScheduledStartAt.UTC().Format("2006-01-02") {
		t.Fatalf("test setup did not cross a business-day boundary: %+v / %+v", existing, created)
	}
}

func TestPlanRejectsSnapshotExpiringBeforeExpectedFinish(t *testing.T) {
	f := newServiceFixture(t)
	ctx := f.as(f.ml_engineer)
	snapshot, err := f.services.Catalog.RegisterSnapshot(ctx, domain.DatasetSnapshot{
		WorkspaceID: f.workspace.ID, SourceZoneID: f.origin.ID, SourceRevision: "SHORT-LIVED-REV",
		SchemaFamily: "ranking-v4", PartitionCount: 1, EstimatedRows: 100,
		ExpiresAt: f.clock.Now().Add(90 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = f.services.Catalog.ValidateSnapshot(ctx, snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}

	_, err = f.services.Inference.PlanInferenceRun(ctx, PlanInferenceRunInput{
		WorkspaceID: f.workspace.ID, SourceZoneID: f.origin.ID, TargetZoneID: f.destination.ID,
		ComputePoolID: f.compute_pool.ID, Reference: "SHORT-LIVED-RUN", SnapshotIDs: []string{snapshot.ID},
		ScheduledStartAt: f.clock.Now().Add(time.Hour), ExpectedFinishAt: f.clock.Now().Add(2 * time.Hour),
		IdempotencyKey: "short-lived-run-key",
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("plan with an input expiring during execution error = %v", err)
	}
}

func TestStartingRunDoesNotAdvanceSiblingRunSnapshots(t *testing.T) {
	f := newServiceFixture(t)
	ctx := f.as(f.ml_engineer)
	first, err := f.services.Inference.PlanInferenceRun(ctx, PlanInferenceRunInput{
		WorkspaceID: f.workspace.ID, SourceZoneID: f.origin.ID, TargetZoneID: f.destination.ID,
		ComputePoolID: f.compute_pool.ID, Reference: "RUN-FIRST", SnapshotIDs: []string{f.batch.ID},
		ScheduledStartAt: f.clock.Now().Add(time.Hour), ExpectedFinishAt: f.clock.Now().Add(2 * time.Hour),
		IdempotencyKey: "run-first-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.services.Inference.StageInferenceRun(ctx, first.ID); err != nil {
		t.Fatal(err)
	}

	secondPool, err := f.services.Catalog.CreateComputePool(ctx, domain.ComputePool{
		SerialNumber: "SIBLING-POOL", CapacityRows: 1000,
		AttestationDueAt: f.clock.Now().Add(48 * time.Hour), LastReconciledAt: f.clock.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	secondSnapshot, err := f.services.Catalog.RegisterSnapshot(ctx, domain.DatasetSnapshot{
		WorkspaceID: f.workspace.ID, SourceZoneID: f.origin.ID, SourceRevision: "SIBLING-REV",
		SchemaFamily: "ranking-v5", PartitionCount: 1, EstimatedRows: 100,
		ExpiresAt: f.clock.Now().Add(48 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	secondSnapshot, err = f.services.Catalog.ValidateSnapshot(ctx, secondSnapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.services.Inference.PlanInferenceRun(ctx, PlanInferenceRunInput{
		WorkspaceID: f.workspace.ID, SourceZoneID: f.origin.ID, TargetZoneID: f.destination.ID,
		ComputePoolID: secondPool.ID, Reference: "RUN-SECOND", SnapshotIDs: []string{secondSnapshot.ID},
		ScheduledStartAt: f.clock.Now().Add(3 * time.Hour), ExpectedFinishAt: f.clock.Now().Add(4 * time.Hour),
		IdempotencyKey: "run-second-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.services.Inference.StageInferenceRun(ctx, second.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := f.services.Inference.StartInferenceRun(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	_, siblingInputs, err := f.services.Query.InferenceRun(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(siblingInputs) != 1 || siblingInputs[0].ID != secondSnapshot.ID {
		t.Fatalf("sibling run inputs = %+v", siblingInputs)
	}
	if siblingInputs[0].State != domain.SnapshotReserved || siblingInputs[0].InferenceRunID != second.ID {
		t.Fatalf("sibling snapshot changed when another run started: %+v", siblingInputs[0])
	}
}

func TestAuthenticationCacheIsScopedByToken(t *testing.T) {
	f := newServiceFixture(t)
	mlLogin, err := f.services.Auth.Login(f.ctx, LoginInput{Email: f.ml_engineer.Email, Password: "very-secure-password"})
	if err != nil {
		t.Fatal(err)
	}
	dataLogin, err := f.services.Auth.Login(f.ctx, LoginInput{Email: f.data_engineer.Email, Password: "very-secure-password"})
	if err != nil {
		t.Fatal(err)
	}
	if mlLogin.Token == dataLogin.Token {
		t.Fatal("different users received the same token")
	}

	mlPrincipal, err := f.services.Auth.Authenticate(f.ctx, mlLogin.Token)
	if err != nil {
		t.Fatal(err)
	}
	dataPrincipal, err := f.services.Auth.Authenticate(f.ctx, dataLogin.Token)
	if err != nil {
		t.Fatal(err)
	}
	if mlPrincipal.UserID != f.ml_engineer.UserID || mlPrincipal.Role != domain.RoleMLEngineer {
		t.Fatalf("ML principal = %+v", mlPrincipal)
	}
	if dataPrincipal.UserID != f.data_engineer.UserID || dataPrincipal.Role != domain.RoleDataEngineer {
		t.Fatalf("data principal = %+v", dataPrincipal)
	}
}
