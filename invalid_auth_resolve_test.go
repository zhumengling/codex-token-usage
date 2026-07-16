package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordInvalidAuthRequiresExactHostIdentity(t *testing.T) {
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return nil, os.ErrNotExist
		}
		return json.Marshal(hostAuthListResponse{Files: []hostAuthFileEntry{
			{ID: "id-a", AuthIndex: "index-a", Name: "a.json", Provider: "codex", Email: "same@example.com"},
			{ID: "id-b", AuthIndex: "index-b", Name: "b.json", Provider: "codex", Email: "same@example.com"},
		}})
	})
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if err := recordInvalidAuthIfNeeded(context.Background(), db, usageRecord{
		Provider: "codex", Source: "same@example.com", RequestedAt: time.Now(),
	}, http.StatusUnauthorized); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM invalid_auths`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("email-only 401 rows=%d, want 0", count)
	}

	if err := recordInvalidAuthIfNeeded(context.Background(), db, usageRecord{
		Provider: "codex", AuthID: "id-b", AuthIndex: "index-b", Source: "same@example.com", RequestedAt: time.Now(),
	}, http.StatusUnauthorized); err != nil {
		t.Fatal(err)
	}
	var authID, authFile, sourceKind string
	if err := db.QueryRow(`SELECT auth_id, auth_file, auth_source_kind FROM invalid_auths WHERE active=1`).Scan(&authID, &authFile, &sourceKind); err != nil {
		t.Fatal(err)
	}
	if authID != "id-b" || authFile != "b.json" || sourceKind != authSourceKindFile {
		t.Fatalf("stored 401 = %q %q %q, want exact id-b/b.json/file", authID, authFile, sourceKind)
	}
}

func TestRecordInvalidAuthUsesRuntimeCallbackWhenListHidesCredential(t *testing.T) {
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			return json.Marshal(hostAuthListResponse{})
		case "host.auth.get_runtime":
			return json.Marshal(hostAuthRuntimeResponse{Auth: hostAuthFileEntry{
				ID: "runtime-id", AuthIndex: "runtime-index", Provider: "codex", Source: "memory", RuntimeOnly: true, Disabled: true,
			}})
		default:
			return nil, os.ErrNotExist
		}
	})
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := recordInvalidAuthIfNeeded(context.Background(), db, usageRecord{
		Provider: "codex", AuthID: "runtime-id", AuthIndex: "runtime-index", RequestedAt: time.Now(),
	}, http.StatusUnauthorized); err != nil {
		t.Fatal(err)
	}
	var sourceKind string
	if err := db.QueryRow(`SELECT auth_source_kind FROM invalid_auths WHERE auth_id='runtime-id' AND active=1`).Scan(&sourceKind); err != nil {
		t.Fatal(err)
	}
	if sourceKind != authSourceKindRuntimeOnly {
		t.Fatalf("runtime source kind=%q, want %q", sourceKind, authSourceKindRuntimeOnly)
	}
}

func TestSuccessfulAuthRecoveryDoesNotClearSameEmailSibling(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO invalid_auths (
  auth_id,auth_index,source,provider,reason,invalidated_at,active,last_status_code,auth_source_kind
) VALUES
 ('id-a','index-a','same@example.com','codex','401',1,1,401,'runtime_only'),
 ('id-b','index-b','same@example.com','codex','401',1,1,401,'runtime_only')`); err != nil {
		t.Fatal(err)
	}
	if err := clearRecoveredInvalidAuthForRecord(context.Background(), db, usageRecord{
		Provider: "codex", AuthID: "id-b", AuthIndex: "index-b", Source: "same@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	var activeA, activeB int
	if err := db.QueryRow(`SELECT active FROM invalid_auths WHERE auth_id='id-a'`).Scan(&activeA); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT active FROM invalid_auths WHERE auth_id='id-b'`).Scan(&activeB); err != nil {
		t.Fatal(err)
	}
	if activeA != 1 || activeB != 0 {
		t.Fatalf("same-email recovery active states a=%d b=%d, want 1/0", activeA, activeB)
	}
}

func TestResolveInvalidAuthFileStates(t *testing.T) {
	for _, test := range []struct {
		name       string
		createFile bool
		fileMTime  int64
		fileNanos  int64
		baseline   int64
		wantStatus string
		wantActive int
		wantExists bool
	}{
		{name: "already absent", wantStatus: "resolved", wantActive: 0},
		{name: "original still present", createFile: true, fileMTime: 100, baseline: 100, wantStatus: "still_present", wantActive: 1, wantExists: true},
		{name: "replacement kept", createFile: true, fileMTime: 200, baseline: 100, wantStatus: "replacement_kept", wantActive: 0, wantExists: true},
		{name: "same second replacement kept", createFile: true, fileMTime: 1700000000, fileNanos: int64(200 * time.Millisecond), baseline: 1700000000100, wantStatus: "replacement_kept", wantActive: 0, wantExists: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
				if method != "host.auth.list" {
					return nil, os.ErrNotExist
				}
				return json.Marshal(hostAuthListResponse{})
			})
			resetSchedulerStateForTest()
			t.Cleanup(resetSchedulerStateForTest)
			s := newTestStore(t)
			authDir := configuredAuthDir()
			if err := os.MkdirAll(authDir, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(authDir, "account.json")
			if test.createFile {
				if err := os.WriteFile(path, []byte(`{"type":"codex"}`), 0o600); err != nil {
					t.Fatal(err)
				}
				stamp := time.Unix(test.fileMTime, test.fileNanos)
				if err := os.Chtimes(path, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
			db, _, err := s.open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`
INSERT INTO invalid_auths (
  auth_id, auth_index, source, provider, reason, invalidated_at, active,
  last_status_code, auth_file, auth_file_mtime, auth_source_kind
) VALUES ('file-id','file-index','user@example.com','codex','401',?,1,401,'account.json',?,'file')`, test.baseline, test.baseline); err != nil {
				t.Fatal(err)
			}
			if test.wantActive == 0 {
				globalSchedulerState.setRestricted("codex", true)
			}

			result, err := s.resolveInvalidAuths(context.Background(), invalidAuthResolveRequest{Items: []invalidAuthResolveRequestItem{{
				AuthID: "file-id", Action: "file_deleted",
			}}})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Items) != 1 || result.Items[0].Status != test.wantStatus {
				t.Fatalf("result=%+v, want status %q", result, test.wantStatus)
			}
			var active int
			if err := db.QueryRow(`SELECT active FROM invalid_auths WHERE auth_id='file-id'`).Scan(&active); err != nil {
				t.Fatal(err)
			}
			if active != test.wantActive {
				t.Fatalf("active=%d, want %d", active, test.wantActive)
			}
			if test.wantActive == 0 && globalSchedulerState.needsDatabase("codex", false) {
				t.Fatal("resolved 401 remained in scheduler restriction state")
			}
			_, statErr := os.Stat(path)
			if (statErr == nil) != test.wantExists {
				t.Fatalf("file exists=%v, want %v (err=%v)", statErr == nil, test.wantExists, statErr)
			}
		})
	}
}

func TestResolveRuntimeInvalidAuthRequiresDisabledOrAbsent(t *testing.T) {
	listCalls := 0
	files := []hostAuthFileEntry{{
		ID: "runtime-id", AuthIndex: "runtime-index", Provider: "codex", Source: "memory", RuntimeOnly: true,
	}}
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return nil, os.ErrNotExist
		}
		listCalls++
		return json.Marshal(hostAuthListResponse{Files: files})
	})
	resetSchedulerStateForTest()
	t.Cleanup(resetSchedulerStateForTest)
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO invalid_auths (
  auth_id, auth_index, source, provider, reason, invalidated_at, active,
  last_status_code, auth_source_kind
) VALUES ('runtime-id','runtime-index','runtime@example.com','codex','401',1,1,401,'runtime_only')`); err != nil {
		t.Fatal(err)
	}

	request := invalidAuthResolveRequest{Items: []invalidAuthResolveRequestItem{{AuthID: "runtime-id", Action: "runtime_disabled"}}}
	result, err := s.resolveInvalidAuths(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Items[0].Status != "still_present" {
		t.Fatalf("enabled runtime result=%+v, want still_present", result)
	}

	files = nil
	globalSchedulerState.setRestricted("codex", true)
	result, err = s.resolveInvalidAuths(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Items[0].Status != "resolved" {
		t.Fatalf("absent runtime result=%+v, want resolved", result)
	}
	var active int
	if err := db.QueryRow(`SELECT active FROM invalid_auths WHERE auth_id='runtime-id'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("runtime active=%d, want 0", active)
	}
	if globalSchedulerState.needsDatabase("codex", false) {
		t.Fatal("resolved runtime 401 remained in scheduler restriction state")
	}
	if listCalls < 2 {
		t.Fatalf("host.auth.list calls=%d, want a fresh inventory for each resolution attempt", listCalls)
	}
}

func TestResolveRuntimeInvalidAuthClearsStaleStateWhenAuthIndexWasReused(t *testing.T) {
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return nil, os.ErrNotExist
		}
		return json.Marshal(hostAuthListResponse{Files: []hostAuthFileEntry{{
			ID: "new-runtime-id", AuthIndex: "reused-index", Provider: "codex", Source: "memory", RuntimeOnly: true,
		}}})
	})
	resetSchedulerStateForTest()
	t.Cleanup(resetSchedulerStateForTest)
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO invalid_auths (auth_id,auth_index,source,provider,reason,invalidated_at,active,last_status_code,auth_source_kind)
VALUES ('old-runtime-id','reused-index','same@example.com','codex','401',1,1,401,'runtime_only')`); err != nil {
		t.Fatal(err)
	}
	result, err := s.resolveInvalidAuths(context.Background(), invalidAuthResolveRequest{Items: []invalidAuthResolveRequestItem{{
		AuthID: "old-runtime-id", Action: "runtime_disabled",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].Status != "already_resolved" {
		t.Fatalf("stale runtime resolution = %+v, want already_resolved", result)
	}
	var active int
	if err := db.QueryRow(`SELECT active FROM invalid_auths WHERE auth_id='old-runtime-id'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("stale runtime active=%d, want 0", active)
	}
}

func TestReconcileInvalidAuthSourceKindsKeepsExactRuntimeAndClearsMissingLegacy(t *testing.T) {
	files := []hostAuthFileEntry{{ID: "runtime-id", AuthIndex: "runtime-index", Provider: "codex", Source: "memory", RuntimeOnly: true}}
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return nil, os.ErrNotExist
		}
		return json.Marshal(hostAuthListResponse{Files: files})
	})
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO invalid_auths (auth_id,auth_index,source,provider,reason,invalidated_at,active,last_status_code,auth_source_kind)
VALUES
 ('runtime-id','runtime-index','runtime@example.com','codex','401',1,1,401,'legacy'),
 ('missing-id','missing-index','missing@example.com','codex','401',1,1,401,'legacy'),
 ('old-memory-id','old-memory-index','memory','codex','401',1,1,401,'legacy')`); err != nil {
		t.Fatal(err)
	}
	if err := reconcileInvalidAuthSourceKinds(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var runtimeActive, missingActive, oldMemoryActive int
	var kind string
	if err := db.QueryRow(`SELECT active,auth_source_kind FROM invalid_auths WHERE auth_id='runtime-id'`).Scan(&runtimeActive, &kind); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT active FROM invalid_auths WHERE auth_id='missing-id'`).Scan(&missingActive); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT active FROM invalid_auths WHERE auth_id='old-memory-id'`).Scan(&oldMemoryActive); err != nil {
		t.Fatal(err)
	}
	if runtimeActive != 1 || kind != authSourceKindRuntimeOnly || missingActive != 0 || oldMemoryActive != 0 {
		t.Fatalf("runtime active/kind=%d/%q missing active=%d old-memory active=%d", runtimeActive, kind, missingActive, oldMemoryActive)
	}
}

func TestReconcileInvalidAuthSourceKindsUpgradesExactLegacyFile(t *testing.T) {
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return nil, os.ErrNotExist
		}
		return json.Marshal(hostAuthListResponse{Files: []hostAuthFileEntry{{
			ID: "file-id", AuthIndex: "file-index", Name: "account.json", Path: filepath.Join(configuredAuthDir(), "account.json"), Provider: "codex", Source: "file",
		}}})
	})
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO invalid_auths (auth_id,auth_index,source,provider,reason,invalidated_at,active,last_status_code,auth_file,auth_source_kind)
VALUES ('file-id','file-index','file@example.com','codex','401',1,1,401,'account.json','legacy')`); err != nil {
		t.Fatal(err)
	}
	if err := reconcileInvalidAuthSourceKinds(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var active int
	var kind string
	if err := db.QueryRow(`SELECT active,auth_source_kind FROM invalid_auths WHERE auth_id='file-id'`).Scan(&active, &kind); err != nil {
		t.Fatal(err)
	}
	if active != 1 || kind != authSourceKindFile {
		t.Fatalf("legacy file active/kind=%d/%q, want 1/%q", active, kind, authSourceKindFile)
	}
}

func TestReconcileInvalidAuthSourceKindsPreservesRuntimeWhenHostInventoryFails(t *testing.T) {
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		return nil, os.ErrNotExist
	})
	s := newTestStore(t)
	if err := os.MkdirAll(configuredAuthDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO invalid_auths (auth_id,auth_index,source,provider,reason,invalidated_at,active,last_status_code,auth_source_kind)
VALUES ('runtime-id','runtime-index','runtime@example.com','codex','401',1,1,401,'runtime_only')`); err != nil {
		t.Fatal(err)
	}
	if err := reconcileInvalidAuthSourceKinds(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := db.QueryRow(`SELECT active FROM invalid_auths WHERE auth_id='runtime-id'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("runtime active=%d, want preserved while host inventory is unavailable", active)
	}
}

func TestReconcileInvalidAuthSourceKindsIgnoresFilesystemNameOverlapForRuntime(t *testing.T) {
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		return nil, os.ErrNotExist
	})
	s := newTestStore(t)
	authDir := configuredAuthDir()
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "account.json"), []byte(`{"type":"codex","email":"file@example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO invalid_auths (auth_id,auth_index,source,provider,reason,invalidated_at,active,last_status_code,auth_source_kind)
VALUES ('account.json','runtime-index','runtime@example.com','codex','401',1,1,401,'runtime_only')`); err != nil {
		t.Fatal(err)
	}
	if err := reconcileInvalidAuthSourceKinds(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := db.QueryRow(`SELECT active FROM invalid_auths WHERE auth_id='account.json'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("runtime/file name overlap active=%d, want preserved without authoritative host inventory", active)
	}
}

func TestReconcileInvalidAuthSourceKindsPreservesLegacyJSONIdentityWhenHostInventoryFails(t *testing.T) {
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		return nil, os.ErrNotExist
	})
	s := newTestStore(t)
	if err := os.MkdirAll(configuredAuthDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO invalid_auths (auth_id,auth_index,source,provider,reason,invalidated_at,active,last_status_code,auth_source_kind)
VALUES ('runtime.json','runtime-index','runtime@example.com','codex','401',1,1,401,'legacy')`); err != nil {
		t.Fatal(err)
	}
	if err := reconcileInvalidAuthSourceKinds(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := db.QueryRow(`SELECT active FROM invalid_auths WHERE auth_id='runtime.json'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("legacy .json identity active=%d, want preserved while host inventory is unavailable", active)
	}
}

func TestReconcileInvalidAuthSourceKindsDoesNotInferLegacyJSONFromFilesystem(t *testing.T) {
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		return nil, os.ErrNotExist
	})
	s := newTestStore(t)
	authDir := configuredAuthDir()
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "runtime.json"), []byte(`{"type":"codex","email":"file@example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO invalid_auths (auth_id,auth_index,source,provider,reason,invalidated_at,active,last_status_code,auth_source_kind)
VALUES ('runtime.json','runtime-index','runtime@example.com','codex','401',1,1,401,'legacy')`); err != nil {
		t.Fatal(err)
	}
	if err := reconcileInvalidAuthSourceKinds(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var active int
	var kind string
	if err := db.QueryRow(`SELECT active,auth_source_kind FROM invalid_auths WHERE auth_id='runtime.json'`).Scan(&active, &kind); err != nil {
		t.Fatal(err)
	}
	if active != 1 || kind != authSourceKindLegacy {
		t.Fatalf("legacy runtime-looking identity active/kind=%d/%q, want 1/%q", active, kind, authSourceKindLegacy)
	}
}

func TestReconcileInvalidAuthSourceKindsPreservesRuntimeHiddenFromAuthoritativeList(t *testing.T) {
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return nil, os.ErrNotExist
		}
		return json.Marshal(hostAuthListResponse{})
	})
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO invalid_auths (auth_id,auth_index,source,provider,reason,invalidated_at,active,last_status_code,auth_source_kind)
VALUES ('runtime-id','runtime-index','runtime@example.com','codex','401',1,1,401,'runtime_only')`); err != nil {
		t.Fatal(err)
	}
	if err := reconcileInvalidAuthSourceKinds(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := db.QueryRow(`SELECT active FROM invalid_auths WHERE auth_id='runtime-id'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("runtime active=%d, want preserved when host list hides disabled runtime credentials", active)
	}
}

func TestReconcileInvalidAuthSourceKindsClearsRuntimeIdentityConflict(t *testing.T) {
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return nil, os.ErrNotExist
		}
		return json.Marshal(hostAuthListResponse{Files: []hostAuthFileEntry{{
			ID: "new-runtime-id", AuthIndex: "reused-index", Provider: "codex", Source: "memory", RuntimeOnly: true,
		}}})
	})
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO invalid_auths (auth_id,auth_index,source,provider,reason,invalidated_at,active,last_status_code,auth_source_kind)
VALUES ('old-runtime-id','reused-index','same@example.com','codex','401',1,1,401,'runtime_only')`); err != nil {
		t.Fatal(err)
	}
	if err := reconcileInvalidAuthSourceKinds(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := db.QueryRow(`SELECT active FROM invalid_auths WHERE auth_id='old-runtime-id'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("conflicting runtime active=%d, want 0", active)
	}
}

func TestReconcileInvalidAuthSourceKindsUpgradesLegacyRuntimeViaRuntimeCallback(t *testing.T) {
	withCodexHostAuthSource(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			return json.Marshal(hostAuthListResponse{})
		case "host.auth.get_runtime":
			return json.Marshal(hostAuthRuntimeResponse{Auth: hostAuthFileEntry{
				ID: "runtime.json", AuthIndex: "runtime-index", Provider: "codex", Source: "memory", RuntimeOnly: true,
			}})
		default:
			return nil, os.ErrNotExist
		}
	})
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO invalid_auths (auth_id,auth_index,source,provider,reason,invalidated_at,active,last_status_code,auth_source_kind)
VALUES ('runtime.json','runtime-index','runtime@example.com','codex','401',1,1,401,'legacy')`); err != nil {
		t.Fatal(err)
	}
	if err := reconcileInvalidAuthSourceKinds(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var active int
	var kind string
	if err := db.QueryRow(`SELECT active,auth_source_kind FROM invalid_auths WHERE auth_id='runtime.json'`).Scan(&active, &kind); err != nil {
		t.Fatal(err)
	}
	if active != 1 || kind != authSourceKindRuntimeOnly {
		t.Fatalf("legacy runtime active/kind=%d/%q, want 1/%q", active, kind, authSourceKindRuntimeOnly)
	}
}

func TestClearMissingInvalidAuthsKeepsRuntimeOnlyJSONIdentity(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO invalid_auths (
  auth_id,auth_index,source,provider,reason,invalidated_at,active,last_status_code,auth_source_kind
) VALUES ('runtime.json','runtime-index','runtime@example.com','codex','401',1,1,401,'runtime_only')`); err != nil {
		t.Fatal(err)
	}
	if err := clearMissingInvalidAuths(context.Background(), db, map[string]struct{}{}, map[string]struct{}{}); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := db.QueryRow(`SELECT active FROM invalid_auths WHERE auth_id='runtime.json'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("runtime-only .json identity active=%d, want 1", active)
	}
}
