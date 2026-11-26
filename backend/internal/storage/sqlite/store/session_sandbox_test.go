package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestSessionStoreSandboxPreservedAcrossUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := sqlitetest.MustOpen(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := s.UpsertProject(ctx, domain.ProjectRecord{
		ID:           "proj-1",
		Path:         t.TempDir(),
		RegisteredAt: now,
	}); err != nil {
		t.Fatalf("UpsertProject() error = %v", err)
	}

	rec := domain.SessionRecord{
		ProjectID: "proj-1",
		Kind:      domain.KindWorker,
		Harness:   domain.HarnessAgy,
		Mode:      domain.SessionModeChat,
		Activity:  domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
		CreatedAt: now,
		UpdatedAt: now,
		Metadata: domain.SessionMetadata{
			Permissions: domain.PermissionModeBypassPermissions,
			Sandbox:     domain.SandboxModeRestricted,
		},
	}
	created, err := s.CreateSession(ctx, rec)
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	rec = created

	// Generic UpdateSession leaves immutable session_sandbox intact even if a
	// stale caller record omits Metadata.Sandbox.
	stale := rec
	stale.Metadata.Sandbox = ""
	stale.UpdatedAt = now.Add(time.Second)
	if err := s.UpdateSession(ctx, stale); err != nil {
		t.Fatalf("UpdateSession() error = %v", err)
	}

	got, ok, err := s.GetSession(ctx, rec.ID)
	if err != nil || !ok {
		t.Fatalf("GetSession() ok=%v err=%v", ok, err)
	}
	if got.Metadata.Permissions != domain.PermissionModeBypassPermissions || got.Metadata.Sandbox != domain.SandboxModeRestricted {
		t.Fatalf("reloaded metadata = %+v, want bypass-permissions + restricted", got.Metadata)
	}
}
