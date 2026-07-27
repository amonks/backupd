package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"
	"monks.co/backupd/config"
	"monks.co/backupd/history"
	"monks.co/backupd/model"
)

// handler builds the full HTTP mux: the HTML dashboard, the long-poll
// endpoint, and the JSON control API.
func (b *Backupd) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/pause", b.handleSetPaused(true))
	mux.HandleFunc("POST /api/resume", b.handleSetPaused(false))
	mux.HandleFunc("POST /api/sync", b.handleSync)
	mux.HandleFunc("POST /api/snapshot", b.handleSnapshot)
	mux.HandleFunc("GET /api/config", b.handleConfigGet)
	mux.HandleFunc("POST /api/config/preview", b.handleConfigPreview)
	mux.HandleFunc("PUT /api/config", b.handleConfigPut)
	mux.HandleFunc("GET /api/state", b.handleState)
	mux.HandleFunc("GET /poll", b.handlePoll)
	mux.HandleFunc("/", b.handlePage)

	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// datasetParam returns the normalized dataset path from the request's
// ?dataset= parameter, and whether the parameter was present at all. The
// empty path with present=true addresses the root dataset.
func datasetParam(req *http.Request) (string, bool) {
	if !req.URL.Query().Has("dataset") {
		return "", false
	}
	ds := req.URL.Query().Get("dataset")
	if ds != "" && !strings.HasPrefix(ds, "/") {
		ds = "/" + ds
	}
	return ds, true
}

// saveAndApply validates a new raw config, persists it to the loaded
// config path, and swaps it in.
func (b *Backupd) saveAndApply(raw []byte) error {
	cur := b.conf.Deref()
	fresh, err := config.Parse(raw)
	if err != nil {
		return err
	}
	fresh.Path = cur.Path
	if cur.Path != "" {
		if err := config.Save(cur.Path, raw); err != nil {
			return fmt.Errorf("saving config: %w", err)
		}
	}
	b.applyConfig(fresh)
	return nil
}

// handleSetPaused toggles the pause flag — global without ?dataset=, per
// subtree with it — by editing and persisting the config file, which is
// where pause state lives.
func (b *Backupd) handleSetPaused(paused bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		dataset, _ := datasetParam(req)
		cur := b.conf.Deref()
		out, err := config.SetPaused(cur.Raw, dataset, paused)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := b.saveAndApply(out); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		verb := "paused"
		if !paused {
			verb = "resumed"
		}
		if dataset == "" {
			b.globalLogs.Printf("%s globally via api", verb)
		} else {
			b.globalLogs.Printf("%s '%s' via api", verb, dataset)
		}
		writeJSON(w, map[string]any{"ok": true})
	}
}

func (b *Backupd) handleSync(w http.ResponseWriter, req *http.Request) {
	dataset, hasDataset := datasetParam(req)
	if hasDataset {
		name := model.DatasetName(dataset)
		if b.state.Deref().GetDataset(name) == nil {
			http.Error(w, fmt.Sprintf("no such dataset '%s'", name), http.StatusNotFound)
			return
		}
		if !b.TriggerSync(false, name) {
			http.Error(w, "sync queue is full", http.StatusServiceUnavailable)
			return
		}
	} else {
		if !b.TriggerSync(true, "") {
			http.Error(w, "sync queue is full", http.StatusServiceUnavailable)
			return
		}
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (b *Backupd) handleSnapshot(w http.ResponseWriter, req *http.Request) {
	periodicity := req.URL.Query().Get("periodicity")
	if periodicity == "" {
		http.Error(w, "Missing periodicity parameter", http.StatusBadRequest)
		return
	}

	root := b.conf.Deref().Local.Root

	if err := b.env.Snapshot(req.Context(), b.globalLogs, root, periodicity); err != nil {
		http.Error(w, fmt.Sprintf("Error creating snapshot: %v", err), http.StatusInternalServerError)
		return
	}

	if err := b.RefreshLocalSnapshots(req.Context(), b.globalLogs); err != nil {
		http.Error(w, fmt.Sprintf("Error refreshing state: %v", err), http.StatusInternalServerError)
		return
	}

	b.globalLogs.Printf("Created %s snapshot for root %s", periodicity, root)
	b.history.RecordOp(history.Op{
		At:        time.Now(),
		Operation: fmt.Sprintf("recursive %s snapshot", periodicity),
	})
	b.notifyStateChange()
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Created %s snapshot for root %s\n", periodicity, root)
}

func (b *Backupd) handleConfigGet(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(b.conf.Deref().Raw)
}

// configImpact describes what saving an edited config would change for
// one dataset, computed against the in-memory inventories without
// touching ZFS.
type configImpact struct {
	Dataset         string `json:"dataset"`
	RetentionOld    string `json:"retentionOld"`
	RetentionNew    string `json:"retentionNew"`
	PausedOld       bool   `json:"pausedOld"`
	PausedNew       bool   `json:"pausedNew"`
	LocalDeletions  int    `json:"localDeletions"`
	RemoteDeletions int    `json:"remoteDeletions"`
	Transfers       int    `json:"transfers"`
}

// previewConfig computes the per-dataset impact of switching to a new
// config: which retention descriptions change, what gets paused, and how
// many snapshots the new target would delete or transfer.
func (b *Backupd) previewConfig(fresh *config.Config) []configImpact {
	state := b.state.Deref()
	cur := b.conf.Deref()
	var impacts []configImpact
	if state == nil {
		return impacts
	}
	for _, name := range state.ListDatasets() {
		ds := state.GetDataset(name)
		if ds == nil || ds.Current == nil {
			continue
		}
		oldL, oldR, oldKB := cur.PolicyFor(name.Path())
		newL, newR, newKB := fresh.PolicyFor(name.Path())
		oldTarget := model.CalculateTargetInventory(ds.Current, oldL, oldR, oldKB)
		newTarget := model.CalculateTargetInventory(ds.Current, newL, newR, newKB)

		imp := configImpact{
			Dataset:         name.String(),
			RetentionOld:    cur.RetentionDescription(name.Path()),
			RetentionNew:    fresh.RetentionDescription(name.Path()),
			PausedOld:       cur.PausedFor(name.Path()),
			PausedNew:       fresh.PausedFor(name.Path()),
			LocalDeletions:  ds.Current.Local.Difference(newTarget.Local).Len(),
			RemoteDeletions: ds.Current.Remote.Difference(newTarget.Remote).Len(),
			Transfers:       newTarget.Remote.Difference(ds.Current.Remote).Len(),
		}
		if imp.RetentionOld != imp.RetentionNew || imp.PausedOld != imp.PausedNew || !oldTarget.Eq(newTarget) {
			impacts = append(impacts, imp)
		}
	}
	return impacts
}

func readBody(w http.ResponseWriter, req *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}
	return raw, true
}

func (b *Backupd) handleConfigPreview(w http.ResponseWriter, req *http.Request) {
	raw, ok := readBody(w, req)
	if !ok {
		return
	}
	fresh, err := config.Parse(raw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	impacts := b.previewConfig(fresh)
	if impacts == nil {
		impacts = []configImpact{}
	}
	writeJSON(w, map[string]any{"impacts": impacts})
}

func (b *Backupd) handleConfigPut(w http.ResponseWriter, req *http.Request) {
	raw, ok := readBody(w, req)
	if !ok {
		return
	}
	if _, err := config.Parse(raw); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := b.saveAndApply(raw); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Regenerate targets and plans so the dashboard shows the new
	// config's effect immediately; execution happens on the next cycle
	// (or a sync-now).
	b.generatePlansForAllDatasets(req.Context())
	b.notifyStateChange()
	b.globalLogs.Printf("config updated via api")
	writeJSON(w, map[string]any{"ok": true})
}

type apiDatasetState struct {
	Name             string     `json:"name"`
	Verdict          string     `json:"verdict"`
	Reason           string     `json:"reason,omitempty"`
	Paused           bool       `json:"paused"`
	Syncing          bool       `json:"syncing"`
	StalenessSeconds int64      `json:"stalenessSeconds"`
	LocalSnapshots   int        `json:"localSnapshots"`
	RemoteSnapshots  int        `json:"remoteSnapshots"`
	PlanSteps        int        `json:"planSteps"`
	PlanCompleted    int        `json:"planCompleted"`
	PlanFailed       int        `json:"planFailed"`
	PendingDeletions int        `json:"pendingDeletions"`
	PendingTransfers int        `json:"pendingTransfers"`
	LastSuccess      *time.Time `json:"lastSuccess,omitempty"`
	LastFailure      *time.Time `json:"lastFailure,omitempty"`
	LastError        string     `json:"lastError,omitempty"`
}

type apiActivity struct {
	Phase               string     `json:"phase"`
	Dataset             string     `json:"dataset,omitempty"`
	Step                int        `json:"step,omitempty"`
	Steps               int        `json:"steps,omitempty"`
	Operation           string     `json:"operation,omitempty"`
	Until               *time.Time `json:"until,omitempty"`
	ConsecutiveFailures int        `json:"consecutiveFailures,omitempty"`
	TransferPercent     *float64   `json:"transferPercent,omitempty"`
	TransferRate        *float64   `json:"transferRateBytesPerSec,omitempty"`
}

func (b *Backupd) handleState(w http.ResponseWriter, req *http.Request) {
	d := b.pageData("")

	act := apiActivity{
		Phase:               d.Activity.Phase.String(),
		Dataset:             d.Activity.Dataset.Path(),
		Step:                d.Activity.Step,
		Steps:               d.Activity.Steps,
		Operation:           d.Activity.Operation,
		ConsecutiveFailures: d.Activity.ConsecutiveFailures,
	}
	if !d.Activity.Until.IsZero() {
		act.Until = &d.Activity.Until
	}
	if x := d.Activity.Transfer; x != nil {
		pct := x.Percent()
		act.TransferPercent = &pct
		act.TransferRate = &x.Rate
	}

	resp := struct {
		Verdict  string            `json:"verdict"`
		Reason   string            `json:"reason,omitempty"`
		Paused   bool              `json:"paused"`
		Dryrun   bool              `json:"dryrun"`
		Activity apiActivity       `json:"activity"`
		Datasets []apiDatasetState `json:"datasets"`
		Cycles   []history.Cycle   `json:"cycles"`
	}{
		Verdict:  d.System.Verdict.String(),
		Reason:   d.System.Reason,
		Paused:   d.Conf.Paused,
		Dryrun:   d.Dryrun,
		Activity: act,
		Datasets: []apiDatasetState{},
		Cycles:   d.Cycles,
	}
	if resp.Cycles == nil {
		resp.Cycles = []history.Cycle{}
	}

	for _, dv := range d.Views {
		ds := d.State.GetDataset(dv.Name)
		out := apiDatasetState{
			Name:             dv.Name.String(),
			Verdict:          dv.Verdict.String(),
			Reason:           dv.Reason,
			Paused:           d.Conf.PausedFor(dv.Name.Path()),
			Syncing:          dv.Verdict == VerdictSyncing,
			StalenessSeconds: int64(dv.Lag.Seconds()),
			PendingDeletions: dv.PendingDeletions,
			PendingTransfers: dv.PendingTransfers,
		}
		if ds != nil && ds.Current != nil {
			out.LocalSnapshots = ds.Current.Local.Len()
			out.RemoteSnapshots = ds.Current.Remote.Len()
		}
		if ds != nil && ds.Plan != nil {
			out.PlanSteps = len(ds.Plan.Steps)
			for _, step := range ds.Plan.Steps {
				switch step.Status {
				case model.StepCompleted:
					out.PlanCompleted++
				case model.StepFailed:
					out.PlanFailed++
				}
			}
		}
		if !dv.LastSuccess.IsZero() {
			at := dv.LastSuccess
			out.LastSuccess = &at
		}
		if dv.LastFailure != nil {
			at := dv.LastFailure.At
			out.LastFailure = &at
			out.LastError = dv.LastFailure.Error
		}
		resp.Datasets = append(resp.Datasets, out)
	}

	writeJSON(w, resp)
}

func (b *Backupd) handlePoll(w http.ResponseWriter, req *http.Request) {
	select {
	case <-b.versionCh:
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "refresh")
	case <-time.After(5 * time.Minute):
		w.WriteHeader(http.StatusNoContent)
	case <-req.Context().Done():
		w.WriteHeader(http.StatusNoContent)
	}
}

// pageData assembles one consistent snapshot of everything the
// dashboard shows: raw state plus the derived per-dataset and system
// verdicts.
func (b *Backupd) pageData(page string) pageData {
	state := b.state.Deref()
	conf := b.conf.Deref()

	var views []DatasetView
	byName := map[model.DatasetName]DatasetView{}
	if state != nil {
		for _, name := range state.ListDatasets() {
			dv := datasetView(name, state.GetDataset(name), conf, b.history, b.activity)
			views = append(views, dv)
			byName[name] = dv
		}
	}

	return pageData{
		Page:       page,
		State:      state,
		Conf:       conf,
		Activity:   b.activity.Get(),
		Views:      views,
		ViewByName: byName,
		System:     systemView(state, conf, b.history, b.activity, views),
		Cycles:     b.history.Cycles(),
		Ops:        b.history.Ops(),
		GlobalLogs: b.globalLogs.GetLogs(),
		Dryrun:     b.dryrun,
	}
}

func (b *Backupd) handlePage(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := req.URL.Path
	if path == "/" {
		http.Redirect(w, req, "/global", http.StatusFound)
		return
	}

	trimmedPath := strings.TrimPrefix(path, "/")

	var pageName string
	switch trimmedPath {
	case "global", "config":
		pageName = trimmedPath
	case "root":
		// The empty string is the root dataset's name.
		pageName = ""
	default:
		pageName = "/" + trimmedPath
	}

	templ.Handler(page(b.pageData(pageName))).ServeHTTP(w, req)
}
