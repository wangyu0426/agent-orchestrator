package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

var upstreamSyncMigrationVersions = [...]int64{
	20261028180000000, 20261028224250364, 20261028233910979, 20261030230920365,
	20261030234657125, 20261031173315209, 20261031173316629, 20261101102149555,
	20261103000000000, 20261103030401452, 20261105105237146, 20261109072438921,
	20261109094334974, 20261109120000000, 20261109153109361, 20261110172911382,
	20261111145612313, 20261113111818788, 20261113173435236, 20261114072016988,
	20261114182101000, 20261115050423095, 20261115071147951, 20261115072651599,
}

// forkMainMigrationSnapshot recreates the old fork-main migration directory by
// excluding exactly the migrations introduced since the common base. Building
// from the merged migration directory would pre-apply any newly added
// out-of-order timestamps and would not model an existing fork database.
func forkMainMigrationSnapshot(t *testing.T, version int64) []byte {
	t.Helper()
	excluded := make(map[int64]struct{}, len(upstreamSyncMigrationVersions))
	for _, migrationVersion := range upstreamSyncMigrationVersions {
		excluded[migrationVersion] = struct{}{}
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migration directory: %v", err)
	}
	legacyFS := fstest.MapFS{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		migrationVersion, err := goose.NumericComponent(entry.Name())
		if err != nil {
			t.Fatalf("parse migration version for %s: %v", entry.Name(), err)
		}
		if _, isUpstreamSyncMigration := excluded[migrationVersion]; isUpstreamSyncMigration {
			continue
		}
		contents, err := migrationsFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatalf("read migration %s: %v", entry.Name(), err)
		}
		legacyFS["migrations/"+entry.Name()] = &fstest.MapFile{Data: contents}
	}

	// buildMigrationSnapshots has a package-level cache keyed by version. Isolate
	// it so this legacy image cannot reuse a checkpoint built from the merged set.
	migrationSnapshots.Lock()
	defer migrationSnapshots.Unlock()
	images, highest := migrationSnapshots.images, migrationSnapshots.highest
	migrationSnapshots.images, migrationSnapshots.highest = nil, 0
	defer func() { migrationSnapshots.images, migrationSnapshots.highest = images, highest }()
	return append([]byte(nil), buildMigrationSnapshots(t, version, legacyFS)...)
}

// TestOpenUpgradesForkMainFixture keeps representative project, tracker,
// reviewer, and provider recovery state when the v0.15.5 migration set opens a
// database at the fork main schema level.
func TestOpenUpgradesForkMainFixture(t *testing.T) {
	const previousSchema = int64(20261107181854835)
	const projectID = "sync-fixture"
	const sessionID = "sync-fixture-1"
	const configJSON = `{"trackerIntake":{"enabled":true,"provider":"github","repo":"wangyu0426/agent-orchestrator","assignee":"wangyu0426"}}`
	now := time.Date(2026, time.November, 7, 12, 0, 0, 0, time.UTC)

	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "ao.db"), forkMainMigrationSnapshot(t, previousSchema), 0o600); err != nil {
		t.Fatalf("write fork-main migration snapshot: %v", err)
	}
	db, err := sql.Open("sqlite", databaseURI(dataDir)+pragmas)
	if err != nil {
		t.Fatalf("open fork-main fixture: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`INSERT INTO projects (id, path, repo_origin_url, registered_at, config)
		VALUES (?, ?, ?, ?, ?)`, projectID, "/tmp/ao-sync-fixture", "https://github.com/wangyu0426/agent-orchestrator.git", now, configJSON); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (
		id, project_id, num, issue_id, kind, harness, reviewer_harness,
		reviewer_agent_config, auto_review_enabled, activity_state, activity_last_at,
		is_terminated, runtime_handle_id, agent_session_id,
		provider_conversation_id, session_mode, created_at, updated_at
	) VALUES (?, ?, 1, ?, 'worker', 'codex', 'codex', ?, 1, 'idle', ?, 0,
		'ptyhost-v1:sync-fixture-1', 'codex-native-session-17',
		'provider-conversation-17', 'chat', ?, ?)`,
		sessionID, projectID, "wangyu0426/agent-orchestrator#17",
		`{"model":"gpt-5-codex","effort":"high"}`, now, now, now); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close pre-upgrade fixture: %v", err)
	}
	databasePath := filepath.Join(dataDir, "ao.db")
	preUpgradeSnapshot, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatalf("read pre-upgrade snapshot: %v", err)
	}

	store, err := Open(dataDir)
	if err != nil {
		t.Fatalf("open and migrate fork-main fixture: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	project, ok, err := store.GetProject(context.Background(), projectID)
	if err != nil || !ok {
		t.Fatalf("GetProject() = ok %t, err %v", ok, err)
	}
	if tracker := project.Config.TrackerIntake; !tracker.Enabled || tracker.Provider != domain.TrackerProviderGitHub || tracker.Repo != "wangyu0426/agent-orchestrator" || tracker.Assignee != "wangyu0426" {
		t.Fatalf("tracker intake config after upgrade = %#v", tracker)
	}

	session, ok, err := store.GetSession(context.Background(), domain.SessionID(sessionID))
	if err != nil || !ok {
		t.Fatalf("GetSession() = ok %t, err %v", ok, err)
	}
	if session.IssueID != domain.IssueID("wangyu0426/agent-orchestrator#17") {
		t.Errorf("tracker issue identity after upgrade = %q", session.IssueID)
	}
	if session.Activity.State != domain.ActivityIdle || session.IsTerminated {
		t.Errorf("session lifecycle after upgrade = state %q, terminated %t", session.Activity.State, session.IsTerminated)
	}
	if session.ReviewerHarness != domain.ReviewerCodex || session.ReviewerConfig.Model != "gpt-5-codex" || session.ReviewerConfig.Effort != "high" {
		t.Errorf("reviewer config after upgrade = harness %q config %#v", session.ReviewerHarness, session.ReviewerConfig)
	}
	if session.Mode != domain.SessionModeChat || session.Metadata.RuntimeHandleID != "ptyhost-v1:sync-fixture-1" || session.Metadata.AgentSessionID != "codex-native-session-17" || session.Metadata.ProviderConversationID != "provider-conversation-17" {
		t.Errorf("session recovery identity after upgrade = mode %q metadata %#v", session.Mode, session.Metadata)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close upgraded store: %v", err)
	}

	upgradedDB, err := sql.Open("sqlite", databaseURI(dataDir)+pragmas)
	if err != nil {
		t.Fatalf("inspect upgraded database: %v", err)
	}
	upgradedDB.SetMaxOpenConns(1)
	defer upgradedDB.Close()
	for _, migrationVersion := range upstreamSyncMigrationVersions {
		var applied int
		if err := upgradedDB.QueryRow(`SELECT COUNT(*) FROM goose_db_version WHERE version_id = ? AND is_applied = 1`, migrationVersion).Scan(&applied); err != nil {
			t.Fatalf("read migration %d receipt: %v", migrationVersion, err)
		}
		if applied != 1 {
			t.Fatalf("migration %d applied receipt count = %d, want 1", migrationVersion, applied)
		}
	}
	var sessionPinsTable int
	if err := upgradedDB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'session_account_pins'`).Scan(&sessionPinsTable); err != nil {
		t.Fatalf("inspect dropped session pins table: %v", err)
	}
	if sessionPinsTable != 0 {
		t.Fatalf("session_account_pins exists after upgrade, want migration 20261114072016988 applied")
	}
	if err := upgradedDB.Close(); err != nil {
		t.Fatalf("close upgraded database inspector: %v", err)
	}

	for _, sidecar := range []string{databasePath + "-wal", databasePath + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove database sidecar %s before restore: %v", sidecar, err)
		}
	}
	if err := os.WriteFile(databasePath, preUpgradeSnapshot, 0o600); err != nil {
		t.Fatalf("restore pre-upgrade snapshot: %v", err)
	}
	restoredDB, err := sql.Open("sqlite", databaseURI(dataDir)+pragmas)
	if err != nil {
		t.Fatalf("inspect restored database: %v", err)
	}
	defer restoredDB.Close()
	var restoredVersion int64
	if err := restoredDB.QueryRow(`SELECT MAX(version_id) FROM goose_db_version WHERE is_applied = 1`).Scan(&restoredVersion); err != nil {
		t.Fatalf("read restored schema version: %v", err)
	}
	if restoredVersion != previousSchema {
		t.Fatalf("restored schema version = %d, want %d", restoredVersion, previousSchema)
	}
	var restoredProject, restoredSession int
	if err := restoredDB.QueryRow(`SELECT COUNT(*) FROM projects WHERE id = ?`, projectID).Scan(&restoredProject); err != nil {
		t.Fatalf("read restored project: %v", err)
	}
	if err := restoredDB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, sessionID).Scan(&restoredSession); err != nil {
		t.Fatalf("read restored session: %v", err)
	}
	if restoredProject != 1 || restoredSession != 1 {
		t.Fatalf("restored project/session rows = %d/%d, want 1/1", restoredProject, restoredSession)
	}
}
