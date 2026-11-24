package sqlite

import "testing"

func TestMigrateTerminateOnPRMergeDefaultPreservesSessions(t *testing.T) {
	const migrationVersion int64 = 20261124061652631
	db := openMigratedDatabaseCopy(t, migrationVersion-1)
	for _, enabled := range []bool{false, true} {
		id := "worker-archive-" + map[bool]string{false: "off", true: "on"}[enabled]
		if _, err := db.Exec(`INSERT INTO sessions (
			id, num, kind, terminate_on_pr_merge, activity_last_at, created_at, updated_at
		) VALUES (?, 1, 'worker', ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, id, enabled); err != nil {
			t.Fatalf("seed %s session: %v", id, err)
		}
	}

	upTo(t, db, migrationVersion)

	var globalDefault bool
	if err := db.QueryRow(`SELECT terminate_on_pr_merge_default FROM app_settings WHERE id = 1`).Scan(&globalDefault); err != nil {
		t.Fatalf("read global archive default: %v", err)
	}
	if !globalDefault {
		t.Fatal("global archive default = false, want enabled for newly created worker sessions")
	}

	rows, err := db.Query(`SELECT id, terminate_on_pr_merge FROM sessions WHERE id LIKE 'worker-archive-%' ORDER BY id`)
	if err != nil {
		t.Fatalf("read existing session policies: %v", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var id string
		var enabled bool
		if err := rows.Scan(&id, &enabled); err != nil {
			t.Fatalf("scan session policy: %v", err)
		}
		seen[id] = enabled
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate session policies: %v", err)
	}
	if len(seen) != 2 || seen["worker-archive-off"] || !seen["worker-archive-on"] {
		t.Fatalf("existing session policies after upgrade = %#v, want explicit false and true preserved", seen)
	}
}
