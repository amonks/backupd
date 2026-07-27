package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"monks.co/backupd/config"
	"monks.co/backupd/history"
	"monks.co/backupd/model"
)

const apiTestConfig = `[local]
root = "data/tank"
[local.policy]
daily = 1

[remote]
root = "backup/tank"
[remote.policy]
daily = 1

[overrides."/foo"]
keep_baseline = false
`

func newAPITestBackupd(t *testing.T, local, remote *fakeExecutor) (*Backupd, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "backupd.toml")
	if err := os.WriteFile(path, []byte(apiTestConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	conf, err := config.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	return newTestBackupd(conf, local, remote), path
}

func do(t *testing.T, h http.Handler, method, target string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAPIPauseResumeGlobal(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, path := newAPITestBackupd(t, local, remote)
	h := b.handler()

	if w := do(t, h, "POST", "/api/pause", ""); w.Code != http.StatusOK {
		t.Fatalf("pause: expected 200, got %d: %s", w.Code, w.Body)
	}
	if !b.conf.Deref().Paused {
		t.Error("expected in-memory config to be paused")
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), "paused = true") {
		t.Errorf("expected pause to be persisted to config file:\n%s", onDisk)
	}

	if w := do(t, h, "POST", "/api/resume", ""); w.Code != http.StatusOK {
		t.Fatalf("resume: expected 200, got %d: %s", w.Code, w.Body)
	}
	if b.conf.Deref().Paused {
		t.Error("expected in-memory config to be unpaused")
	}
	onDisk, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), "paused") {
		t.Errorf("expected pause to be removed from config file:\n%s", onDisk)
	}
}

func TestAPIPauseDataset(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, _ := newAPITestBackupd(t, local, remote)
	h := b.handler()

	if w := do(t, h, "POST", "/api/pause?dataset=/foo", ""); w.Code != http.StatusOK {
		t.Fatalf("pause: expected 200, got %d: %s", w.Code, w.Body)
	}
	conf := b.conf.Deref()
	if !conf.PausedFor("/foo") {
		t.Error("expected /foo to be paused")
	}
	if conf.Paused || conf.PausedFor("/bar") {
		t.Error("expected only /foo to be paused")
	}
	// The existing override's retention must be intact.
	_, _, keepBaseline := conf.PolicyFor("/foo")
	if keepBaseline {
		t.Error("expected keep_baseline=false to survive pause toggle")
	}

	if w := do(t, h, "POST", "/api/resume?dataset=/foo", ""); w.Code != http.StatusOK {
		t.Fatalf("resume: expected 200, got %d: %s", w.Code, w.Body)
	}
	if b.conf.Deref().PausedFor("/foo") {
		t.Error("expected /foo to be unpaused")
	}
}

func TestAPISync(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, _ := newAPITestBackupd(t, local, remote)
	b.state.Swap(model.AddLocalDataset("/foo", []*model.Snapshot{snapA}, nil))
	h := b.handler()

	if w := do(t, h, "POST", "/api/sync", ""); w.Code != http.StatusOK {
		t.Fatalf("sync: expected 200, got %d: %s", w.Code, w.Body)
	}
	select {
	case req := <-b.syncNow:
		if !req.all {
			t.Errorf("expected a global sync request, got %+v", req)
		}
	default:
		t.Fatal("expected a sync request to be enqueued")
	}

	if w := do(t, h, "POST", "/api/sync?dataset=/foo", ""); w.Code != http.StatusOK {
		t.Fatalf("sync dataset: expected 200, got %d: %s", w.Code, w.Body)
	}
	select {
	case req := <-b.syncNow:
		if req.all || req.dataset != "/foo" {
			t.Errorf("expected a /foo sync request, got %+v", req)
		}
	default:
		t.Fatal("expected a sync request to be enqueued")
	}
}

func TestAPIConfigGet(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, _ := newAPITestBackupd(t, local, remote)
	h := b.handler()

	w := do(t, h, "GET", "/api/config", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != apiTestConfig {
		t.Errorf("expected raw config, got:\n%s", w.Body)
	}
}

func TestAPIConfigPreview(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, _ := newAPITestBackupd(t, local, remote)
	b.state.Swap(model.AddLocalDataset("/foo", []*model.Snapshot{snapA, snapB, snapC}, nil))
	b.state.Swap(model.AddRemoteDataset("/foo", []*model.Snapshot{snapA}, nil))
	h := b.handler()

	// Raise /foo's local retention from the global daily=1 to daily=3.
	edited := apiTestConfig + "[overrides.\"/foo\".local.policy]\ndaily = 3\n"
	w := do(t, h, "POST", "/api/config/preview", edited)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body)
	}
	var resp struct {
		Impacts []configImpact `json:"impacts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v\n%s", err, w.Body)
	}
	if len(resp.Impacts) != 1 {
		t.Fatalf("expected 1 impact, got %+v", resp.Impacts)
	}
	imp := resp.Impacts[0]
	if imp.Dataset != "/foo" {
		t.Errorf("expected impact on /foo, got %q", imp.Dataset)
	}
	if imp.RetentionOld == imp.RetentionNew {
		t.Errorf("expected retention change to be reported, got %q → %q", imp.RetentionOld, imp.RetentionNew)
	}
	// With daily=3 local, nothing is deleted locally and C still
	// transfers to the remote.
	if imp.LocalDeletions != 0 {
		t.Errorf("expected 0 local deletions, got %d", imp.LocalDeletions)
	}
	if imp.Transfers != 1 {
		t.Errorf("expected 1 transfer, got %d", imp.Transfers)
	}

	// Invalid TOML is a 400 with a useful message, and changes nothing.
	w = do(t, h, "POST", "/api/config/preview", "not [ valid")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid config, got %d", w.Code)
	}
}

func TestAPIConfigPut(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, path := newAPITestBackupd(t, local, remote)
	h := b.handler()

	edited := strings.Replace(apiTestConfig, "daily = 1", "daily = 7", 1)
	w := do(t, h, "PUT", "/api/config", edited)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != edited {
		t.Errorf("expected config file to be replaced:\n%s", onDisk)
	}
	if got := b.conf.Deref().Local.Policy["daily"]; got != 7 {
		t.Errorf("expected in-memory config to be applied, got daily=%d", got)
	}
	if b.conf.Deref().Path != path {
		t.Errorf("expected applied config to keep its path, got %q", b.conf.Deref().Path)
	}

	// An invalid config is rejected and nothing changes.
	w = do(t, h, "PUT", "/api/config", "[local]\nroot = 3\n")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid config, got %d: %s", w.Code, w.Body)
	}
	onDisk, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != edited {
		t.Errorf("expected config file to be unchanged after invalid PUT:\n%s", onDisk)
	}
}

func TestAPISnapshot(t *testing.T) {
	local, remote := steadyStateExecutors()
	local.handlers = append(local.handlers, fakeHandler{match: "zfs snapshot"})
	b, _ := newAPITestBackupd(t, local, remote)
	h := b.handler()

	if w := do(t, h, "POST", "/api/snapshot?periodicity=daily", ""); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body)
	}
	if !local.calledMatching("zfs snapshot") {
		t.Errorf("expected a snapshot call, got:\n%s", strings.Join(local.calls, "\n"))
	}

	if w := do(t, h, "POST", "/api/snapshot", ""); w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 without periodicity, got %d", w.Code)
	}
}

// TestCLIRoundTrip: the CLI subcommands are CallAPI wrappers; drive one
// against a live handler end to end.
func TestCLIRoundTrip(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, _ := newAPITestBackupd(t, local, remote)
	srv := httptest.NewServer(b.handler())
	defer srv.Close()
	b.addr = strings.TrimPrefix(srv.URL, "http://")

	if err := b.CallAPI(t.Context(), "POST", "/api/pause"); err != nil {
		t.Fatalf("CallAPI pause: %v", err)
	}
	if !b.conf.Deref().Paused {
		t.Error("expected pause via CLI round trip")
	}
	if err := b.CallAPI(t.Context(), "POST", "/api/resume"); err != nil {
		t.Fatalf("CallAPI resume: %v", err)
	}
	if b.conf.Deref().Paused {
		t.Error("expected resume via CLI round trip")
	}
	if err := b.CallAPI(t.Context(), "POST", "/api/nonexistent"); err == nil {
		t.Error("expected error for unknown endpoint")
	}
}

func TestPageRendering(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, _ := newAPITestBackupd(t, local, remote)
	b.state.Swap(model.AddLocalDataset("/foo", []*model.Snapshot{snapA}, nil))
	b.state.Swap(model.AddRemoteDataset("/foo", []*model.Snapshot{snapA}, nil))
	b.history.RecordCycle(history.Cycle{OK: true, Datasets: 1})
	h := b.handler()

	w := do(t, h, "GET", "/global", "")
	if w.Code != http.StatusOK {
		t.Fatalf("global: expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Cycle History") {
		t.Error("expected global page to show cycle history")
	}
	if !strings.Contains(w.Body.String(), "Pause all") {
		t.Error("expected global page to show the pause control")
	}

	w = do(t, h, "GET", "/config", "")
	if w.Code != http.StatusOK {
		t.Fatalf("config: expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "config-editor") {
		t.Error("expected config page to render the editor")
	}
	if !strings.Contains(w.Body.String(), "[local.policy]") {
		t.Error("expected config page to include the raw config")
	}

	w = do(t, h, "GET", "/foo", "")
	if w.Code != http.StatusOK {
		t.Fatalf("dataset: expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Pause dataset") {
		t.Error("expected dataset page to show a pause button")
	}
	if !strings.Contains(w.Body.String(), "/api/sync?dataset=/foo") {
		t.Error("expected dataset page to show a sync-now button")
	}

	// A paused dataset shows Resume instead, and the global pause shows
	// the banner.
	b.conf.Swap(func(c *config.Config) *config.Config {
		out, err := config.SetPaused(c.Raw, "/foo", true)
		if err != nil {
			t.Fatal(err)
		}
		out, err = config.SetPaused(out, "", true)
		if err != nil {
			t.Fatal(err)
		}
		fresh, err := config.Parse(out)
		if err != nil {
			t.Fatal(err)
		}
		return fresh
	})
	w = do(t, h, "GET", "/foo", "")
	if !strings.Contains(w.Body.String(), "Resume dataset") {
		t.Error("expected paused dataset page to show a resume button")
	}
	if !strings.Contains(w.Body.String(), "PAUSED") {
		t.Error("expected globally-paused page to show the PAUSED verdict in the status strip")
	}
	if !strings.Contains(w.Body.String(), "Resume all") {
		t.Error("expected globally-paused page to offer Resume all")
	}
}

// TestPageShowsVerdictsAndActivity: the dashboard surfaces — status
// strip, issue cards, fleet health, recent activity feed — render from
// real state. Snapshots are given fresh timestamps because health is
// derived from inventory ages against the wall clock.
func TestPageShowsVerdictsAndActivity(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, _ := newAPITestBackupd(t, local, remote)
	freshA := testSnap("daily-a", time.Now().Add(-26*time.Hour).Unix())
	freshB := testSnap("daily-b", time.Now().Add(-time.Hour).Unix())
	b.state.Swap(model.AddLocalDataset("/foo", []*model.Snapshot{freshA, freshB}, nil))
	b.state.Swap(model.AddRemoteDataset("/foo", []*model.Snapshot{freshA}, nil))
	b.history.RecordCycle(history.Cycle{OK: true, Datasets: 1})
	b.history.RecordDatasetFailure("/foo", time.Now(), "remote out of space")
	b.history.RecordOp(history.Op{At: time.Now(), Dataset: "/foo", Operation: "transfer range x", Duration: time.Second})
	h := b.handler()

	w := do(t, h, "GET", "/global", "")
	if w.Code != http.StatusOK {
		t.Fatalf("global: expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"FAILING",             // system verdict (failing dataset) — rendered upper-case
		"remote out of space", // the failure error, in the issue card and fleet reason
		"issue-critical",      // the issue card, severity-styled
		"Recent Activity",     // op feed
		"transfer range x",    // the recorded op
		"chip-failing",        // fleet health chip
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected global page to contain %q", want)
		}
	}

	// After a newer success, the dataset (and system) settle back to ok,
	// and the overview says all clear.
	b.history.RecordDatasetSuccess("/foo", time.Now().Add(time.Minute))
	body = do(t, h, "GET", "/global", "").Body.String()
	if strings.Contains(body, "FAILING") {
		t.Error("expected system verdict to recover after a newer success")
	}
	if !strings.Contains(body, "All clear") {
		t.Error("expected the all-clear banner with no issues")
	}

	// The dataset page shows the recovery sentence, assurance facts,
	// and the run record.
	w = do(t, h, "GET", "/foo", "")
	if w.Code != http.StatusOK {
		t.Fatalf("dataset: expected 200, got %d", w.Code)
	}
	body = w.Body.String()
	for _, want := range []string{
		"could be restored as of", // recovery sentence
		"Snapshotted",             // assurance facts
		"Backed up",
		"behind local)",       // lag as derived detail
		"Last sync run",       // run record
		"remote out of space", // the last failure's error stays visible
		"Retention Fulfillment",
		"Snapshots",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected dataset page to contain %q", want)
		}
	}
}

// TestPageShowsStaleAndUnreplicated: the assurance-driven states render
// as issues even though no run has ever failed — the dashboard derives
// them from the inventory alone.
func TestPageShowsStaleAndUnreplicated(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, _ := newAPITestBackupd(t, local, remote)
	// /stale: fresh local snapshots, remote 5 days behind (policy is
	// daily, so the 2×24h grace is blown).
	staleOld := testSnap("daily-old", time.Now().Add(-5*24*time.Hour).Unix())
	staleNew := testSnap("daily-new", time.Now().Add(-time.Hour).Unix())
	b.state.Swap(model.AddLocalDataset("/stale", []*model.Snapshot{staleOld, staleNew}, nil))
	b.state.Swap(model.AddRemoteDataset("/stale", []*model.Snapshot{staleOld}, nil))
	// /new: never replicated.
	b.state.Swap(model.AddLocalDataset("/new", []*model.Snapshot{staleNew}, nil))
	h := b.handler()

	body := do(t, h, "GET", "/global", "").Body.String()
	for _, want := range []string{
		"FAILING",            // both conditions are critical
		"never replicated",   // the unreplicated issue
		"backup 5d old",      // the stale-backup issue
		"chip-atrisk",        // fleet health chip
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected global page to contain %q", want)
		}
	}

	// The unreplicated dataset's page leads with the warning sentence.
	body = do(t, h, "GET", "/new", "").Body.String()
	if !strings.Contains(body, "never been replicated") {
		t.Error("expected the unreplicated recovery sentence")
	}
}

func TestAPIState(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, _ := newAPITestBackupd(t, local, remote)
	b.state.Swap(model.AddLocalDataset("/foo", []*model.Snapshot{snapA}, nil))
	b.state.Swap(model.AddRemoteDataset("/foo", []*model.Snapshot{snapA}, nil))
	h := b.handler()

	w := do(t, h, "GET", "/api/state", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Paused   bool `json:"paused"`
		Datasets []struct {
			Name           string `json:"name"`
			Paused         bool   `json:"paused"`
			LocalSnapshots int    `json:"localSnapshots"`
		} `json:"datasets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v\n%s", err, w.Body)
	}
	if len(resp.Datasets) != 1 || resp.Datasets[0].Name != "/foo" {
		t.Fatalf("expected /foo in state, got %s", w.Body)
	}
	if resp.Datasets[0].LocalSnapshots != 1 {
		t.Errorf("expected 1 local snapshot, got %d", resp.Datasets[0].LocalSnapshots)
	}
}
