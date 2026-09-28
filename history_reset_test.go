package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	thingscloud "github.com/arthursoares/things-cloud-sdk"
	memory "github.com/arthursoares/things-cloud-sdk/state/memory"
)

// resetCloud simulates a Things Cloud history whose head has dropped to
// headIndex while the server's cursor still points further along.
func resetCloud(t *testing.T, headIndex int, staleStatus int) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var starts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/version/1/history/history" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"latest-server-index":   headIndex,
				"latest-schema-version": 301,
			})
			return
		}
		start := r.URL.Query().Get("start-index")
		mu.Lock()
		starts = append(starts, start)
		mu.Unlock()
		switch start {
		case "0":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []any{map[string]any{"fresh-task": map[string]any{
					"e": "Task6", "t": 0, "p": map[string]any{"tt": "after reset"},
				}}},
				"current-item-index": 1,
				"schema":             301,
			})
		case "868":
			w.Header().Set("Things-Response", "AccountIssue")
			http.Error(w, "not found", staleStatus)
		default:
			t.Errorf("unexpected request at start-index %s", start)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), starts...)
	}
}

func staleThingsMCP(serverURL string) *ThingsMCP {
	client := thingscloud.New(serverURL, "test@example.com", "password", thingscloud.WithRequestInterval(0))
	state := memory.NewState()
	return &ThingsMCP{
		client:  client,
		history: &thingscloud.History{Client: client, ID: "history", LoadedServerIndex: 868, LatestServerIndex: 868, LatestSchemaVersion: 301},
		state:   state,
	}
}

func TestSyncRecoversFromServerHistoryReset(t *testing.T) {
	server, starts := resetCloud(t, 27, http.StatusNotFound)
	defer server.Close()

	tmcp := staleThingsMCP(server.URL)
	if err := tmcp.syncAndRebuild(); err != nil {
		t.Fatalf("sync did not recover from history reset: %v", err)
	}
	if task := tmcp.state.Tasks["fresh-task"]; task == nil || task.Title != "after reset" {
		t.Fatalf("state was not rebuilt from index 0: %#v", tmcp.state.Tasks)
	}
	if tmcp.history.LoadedServerIndex != 1 {
		t.Fatalf("cursor = %d after rebuild, want 1", tmcp.history.LoadedServerIndex)
	}
	got := starts()
	if len(got) != 2 || got[0] != "868" || got[1] != "0" {
		t.Fatalf("request sequence = %v, want [868 0]", got)
	}
}

func TestSync404WithoutResetDoesNotRebuild(t *testing.T) {
	// Head is still ahead of the cursor, so the 404 is not a reset.
	server, starts := resetCloud(t, 900, http.StatusNotFound)
	defer server.Close()

	tmcp := staleThingsMCP(server.URL)
	if err := tmcp.syncAndRebuild(); err == nil {
		t.Fatal("expected sync error when history head is not below cursor")
	}
	for _, s := range starts() {
		if s == "0" {
			t.Fatal("404 without a history reset triggered a full rebuild")
		}
	}
	if tmcp.history.LoadedServerIndex != 868 {
		t.Fatalf("cursor moved to %d", tmcp.history.LoadedServerIndex)
	}
}

func TestSyncServerErrorDoesNotProbeOrRebuild(t *testing.T) {
	// A 500 is transient: no reset check, no rebuild, even if the head looks low.
	server, starts := resetCloud(t, 27, http.StatusInternalServerError)
	defer server.Close()

	tmcp := staleThingsMCP(server.URL)
	if err := tmcp.syncAndRebuild(); err == nil {
		t.Fatal("expected sync error on 500")
	}
	for _, s := range starts() {
		if s == "0" {
			t.Fatal("transient 500 triggered a full rebuild")
		}
	}
}
