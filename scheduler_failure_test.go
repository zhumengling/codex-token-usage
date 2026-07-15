package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestSchedulerPickFailsClosedWhenRestrictedDatabaseIsUnavailable(t *testing.T) {
	resetSchedulerStateForTest()
	t.Cleanup(resetSchedulerStateForTest)
	globalSchedulerState.setRestricted("codex", true)

	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CPA_TOKEN_USAGE_DIR", blocker)

	previousStore := globalStore
	globalStore = &store{}
	t.Cleanup(func() {
		globalStore.close()
		globalStore = previousStore
	})

	request, err := json.Marshal(schedulerPickRequest{
		Provider: "codex",
		Candidates: []schedulerAuthCandidate{
			{ID: "account", Provider: "codex", Priority: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleMethod("scheduler.pick", request)
	if err != nil {
		t.Fatal(err)
	}
	var response envelope
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Error == nil {
		t.Fatalf("response=%s, want scheduler error envelope", raw)
	}
	if response.Error.Code != "scheduler_unavailable" || response.Error.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("scheduler error=%+v, want scheduler_unavailable/503", response.Error)
	}
}
