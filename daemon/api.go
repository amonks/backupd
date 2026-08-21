package daemon

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"
	"monks.co/backupd/config"
	"monks.co/backupd/history"
	"monks.co/backupd/model"
	"monks.co/backupd/status"
	"monks.co/backupd/view"
)

// Handler is the dashboard and control API: the HTML pages, the
// long-poll endpoint, and the JSON API, on the paths documented in the
// README. It is safe to mount under a prefix — every link and fetch is
// built from the request's X-Forwarded-Prefix — and it authorizes
// nothing, so a host serving it anywhere but a trusted network is
// expected to wrap it.
func (b *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/pause", b.handleSetPaused(true))
	mux.HandleFunc("POST /api/resume", b.handleSetPaused(false))
	mux.HandleFunc("POST /api/sync", b.handleSync)
	mux.HandleFunc("POST /api/snapshot", b.handleSnapshot)
	mux.HandleFunc("GET /api/config", b.handleConfigGet)
	mux.HandleFunc("POST /api/config/preview", b.handleConfigPreview)
	mux.HandleFunc("PUT /api/config", b.handleConfigPut)
	mux.Handle("GET /api/state", maybeGzip(http.HandlerFunc(b.handleState)))
	mux.HandleFunc("GET /poll", b.handlePoll)
	mux.Handle("/", maybeGzip(http.HandlerFunc(b.handlePage)))

	return mux
}

// maybeGzip compresses a response when the client accepts it. The
// dashboard pages ship complete datagrid data sets and are re-fetched
// on every state change, and nothing sits in front of backupd to
// compress for it, so this is the difference between ~1 MB and tens
// of KB per refresh on a loaded system. /poll stays uncompressed: its
// 204s must not grow a body.
func maybeGzip(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.Contains(req.Header.Get("Accept-Encoding"), "gzip") {
			h.ServeHTTP(w, req)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		h.ServeHTTP(gzipResponseWriter{ResponseWriter: w, gz: gz}, req)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (g gzipResponseWriter) Write(bs []byte) (int, error) { return g.gz.Write(bs) }

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
func (b *Daemon) saveAndApply(raw []byte) error {
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
func (b *Daemon) handleSetPaused(paused bool) http.HandlerFunc {
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
			b.event(history.Info, nil, "%s globally via api", verb)
		} else {
			name := model.DatasetName(dataset)
			b.event(history.Info, &name, "%s via api", verb)
		}
		writeJSON(w, map[string]any{"ok": true})
	}
}

func (b *Daemon) handleSync(w http.ResponseWriter, req *http.Request) {
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

func (b *Daemon) handleSnapshot(w http.ResponseWriter, req *http.Request) {
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
		Kind:      "snapshot",
	})
	b.notifyStateChange()
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Created %s snapshot for root %s\n", periodicity, root)
}

func (b *Daemon) handleConfigGet(w http.ResponseWriter, req *http.Request) {
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
func (b *Daemon) previewConfig(fresh *config.Config) []configImpact {
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

func (b *Daemon) handleConfigPreview(w http.ResponseWriter, req *http.Request) {
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

func (b *Daemon) handleConfigPut(w http.ResponseWriter, req *http.Request) {
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
	b.event(history.Info, nil, "config updated via api")
	writeJSON(w, map[string]any{"ok": true})
}

// The JSON state summary serializes the same view.System the HTML
// renders from, so scripts and humans can never disagree about what
// the system says.

type apiIssue struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	// Dataset is the affected dataset's path (empty string is the root
	// dataset); absent for system-level issues.
	Dataset *string    `json:"dataset,omitempty"`
	Summary string     `json:"summary"`
	Detail  string     `json:"detail,omitempty"`
	Since   *time.Time `json:"since,omitempty"`
}

type apiFulfillment struct {
	Periodicity string `json:"periodicity"`
	LocalHave   int    `json:"localHave"`
	LocalWant   int    `json:"localWant"`
	RemoteHave  int    `json:"remoteHave"`
	RemoteWant  int    `json:"remoteWant"`
}

type apiDatasetState struct {
	Name               string           `json:"name"`
	Path               string           `json:"path"`
	Health             string           `json:"health"`
	Reason             string           `json:"reason,omitempty"`
	Syncing            bool             `json:"syncing"`
	Paused             bool             `json:"paused"`
	SnapshottedSeconds *int64           `json:"snapshottedSeconds,omitempty"`
	BackedUpSeconds    *int64           `json:"backedUpSeconds,omitempty"`
	DepthSeconds       *int64           `json:"depthSeconds,omitempty"`
	LagSeconds         int64            `json:"lagSeconds"`
	SnapshotsStale     bool             `json:"snapshotsStale,omitempty"`
	BackupStale        bool             `json:"backupStale,omitempty"`
	Unreplicated       bool             `json:"unreplicated,omitempty"`
	LocalSnapshots     int              `json:"localSnapshots"`
	RemoteSnapshots    int              `json:"remoteSnapshots"`
	LocalUsedBytes     int64            `json:"localUsedBytes,omitempty"`
	RemoteUsedBytes    int64            `json:"remoteUsedBytes,omitempty"`
	PendingDeletions   int              `json:"pendingDeletions"`
	PendingTransfers   int              `json:"pendingTransfers"`
	PlanSteps          int              `json:"planSteps"`
	PlanCompleted      int              `json:"planCompleted"`
	Retention          string           `json:"retention"`
	Fulfillment        []apiFulfillment `json:"fulfillment,omitempty"`
	LastSuccess        *time.Time       `json:"lastSuccess,omitempty"`
	LastFailure        *time.Time       `json:"lastFailure,omitempty"`
	LastError          string           `json:"lastError,omitempty"`
}

type apiCycleProgress struct {
	Total    int    `json:"total"`
	Position int    `json:"position"`
	Done     int    `json:"done"`
	Failed   int    `json:"failed"`
	Skipped  int    `json:"skipped"`
	Active   string `json:"active,omitempty"`
}

type apiActivity struct {
	Phase               string           `json:"phase"`
	Dataset             string           `json:"dataset,omitempty"`
	Step                int              `json:"step,omitempty"`
	Steps               int              `json:"steps,omitempty"`
	Operation           string           `json:"operation,omitempty"`
	Until               *time.Time       `json:"until,omitempty"`
	ConsecutiveFailures int              `json:"consecutiveFailures,omitempty"`
	TransferPercent     *float64         `json:"transferPercent,omitempty"`
	TransferRate        *float64         `json:"transferRateBytesPerSec,omitempty"`
	Cycle               apiCycleProgress `json:"cycle"`
}

// apiCycleRun serializes one collapsed run of consecutive
// same-outcome cycles.
type apiCycleRun struct {
	Outcome    string    `json:"outcome"`
	Count      int       `json:"count"`
	First      time.Time `json:"first"`
	Last       time.Time `json:"last"`
	AvgSeconds int64     `json:"avgSeconds"`
	Detail     string    `json:"detail,omitempty"`
}

// apiEvent serializes one journal entry.
type apiEvent struct {
	At    time.Time `json:"at"`
	Level string    `json:"level"`
	// Dataset is the affected dataset's path (empty string is the root
	// dataset); absent for daemon-level events.
	Dataset *string `json:"dataset,omitempty"`
	Message string  `json:"message"`
}

func secondsPtr(has bool, d time.Duration) *int64 {
	if !has {
		return nil
	}
	s := int64(d.Seconds())
	return &s
}

func (b *Daemon) handleState(w http.ResponseWriter, req *http.Request) {
	d := b.pageData(req, "")
	sys := d.Sys

	act := apiActivity{
		Phase:               d.Activity.Phase.String(),
		Dataset:             d.Activity.Dataset.Path(),
		Step:                d.Activity.Step,
		Steps:               d.Activity.Steps,
		Operation:           d.Activity.Operation,
		ConsecutiveFailures: d.Activity.ConsecutiveFailures,
		Cycle: apiCycleProgress{
			Total:    sys.Cycle.Total,
			Position: sys.Cycle.Position,
			Done:     sys.Cycle.Done,
			Failed:   sys.Cycle.Failed,
			Skipped:  sys.Cycle.Skipped,
		},
	}
	if sys.Cycle.HasActive {
		act.Cycle.Active = sys.Cycle.Active.String()
	}
	if !d.Activity.Until.IsZero() {
		act.Until = &d.Activity.Until
	}
	if x := d.Activity.Transfer; x != nil {
		pct := x.Percent()
		act.TransferPercent = &pct
		act.TransferRate = &x.Rate
	}

	issues := []apiIssue{}
	for _, issue := range sys.Issues {
		out := apiIssue{
			Kind:     string(issue.Kind),
			Severity: issue.Severity.String(),
			Summary:  issue.Summary,
			Detail:   issue.Detail,
		}
		if issue.Dataset != nil {
			path := issue.Dataset.Path()
			out.Dataset = &path
		}
		if !issue.Since.IsZero() {
			since := issue.Since
			out.Since = &since
		}
		issues = append(issues, out)
	}

	// Raw cycles are capped: a long-running daemon holds weeks of them,
	// and the collapsed runs are the long-horizon serialization.
	cycles := d.Cycles
	if len(cycles) > 50 {
		cycles = cycles[:50]
	}

	runs := []apiCycleRun{}
	for _, r := range sys.Runs {
		runs = append(runs, apiCycleRun{
			Outcome:    r.Outcome.String(),
			Count:      r.Count,
			First:      r.First,
			Last:       r.Last,
			AvgSeconds: int64(r.AvgDuration.Seconds()),
			Detail:     r.Detail,
		})
	}

	events := []apiEvent{}
	for _, e := range d.Events {
		out := apiEvent{
			At:      e.At,
			Level:   string(e.Level),
			Message: e.Message,
		}
		if e.Dataset != nil {
			path := e.Dataset.Path()
			out.Dataset = &path
		}
		events = append(events, out)
	}

	resp := struct {
		Verdict  string            `json:"verdict"`
		Reason   string            `json:"reason,omitempty"`
		Paused   bool              `json:"paused"`
		Dryrun   bool              `json:"dryrun"`
		Issues   []apiIssue        `json:"issues"`
		Activity apiActivity       `json:"activity"`
		Datasets []apiDatasetState `json:"datasets"`
		Cycles   []history.Cycle   `json:"cycles"`
		Runs     []apiCycleRun     `json:"cycleRuns"`
		Events   []apiEvent        `json:"events"`
	}{
		Verdict:  sys.Verdict.String(),
		Reason:   sys.Reason,
		Paused:   d.Conf.Paused,
		Dryrun:   d.Dryrun,
		Issues:   issues,
		Activity: act,
		Datasets: []apiDatasetState{},
		Cycles:   cycles,
		Runs:     runs,
		Events:   events,
	}
	if resp.Cycles == nil {
		resp.Cycles = []history.Cycle{}
	}

	for _, dv := range sys.Datasets {
		out := apiDatasetState{
			Name:               dv.Name.String(),
			Path:               dv.Name.Path(),
			Health:             dv.Health.String(),
			Reason:             dv.Reason,
			Syncing:            dv.Syncing,
			Paused:             dv.Paused,
			SnapshottedSeconds: secondsPtr(dv.HasLocal, dv.Snapshotted),
			BackedUpSeconds:    secondsPtr(dv.HasRemote, dv.BackedUp),
			DepthSeconds:       secondsPtr(dv.HasRemote, dv.Depth),
			LagSeconds:         int64(dv.Lag.Seconds()),
			SnapshotsStale:     dv.SnapshotsStale,
			BackupStale:        dv.BackupStale,
			Unreplicated:       dv.Unreplicated,
			LocalSnapshots:     dv.LocalCount,
			RemoteSnapshots:    dv.RemoteCount,
			LocalUsedBytes:     dv.LocalUsed,
			RemoteUsedBytes:    dv.RemoteUsed,
			PendingDeletions:   dv.PendingDeletions,
			PendingTransfers:   dv.PendingTransfers,
			PlanSteps:          dv.StepsTotal,
			PlanCompleted:      dv.StepsDone,
			Retention:          dv.Retention,
		}
		for _, row := range dv.Fulfillment {
			out.Fulfillment = append(out.Fulfillment, apiFulfillment{
				Periodicity: row.Periodicity,
				LocalHave:   row.LocalHave,
				LocalWant:   row.LocalWant,
				RemoteHave:  row.RemoteHave,
				RemoteWant:  row.RemoteWant,
			})
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

func (b *Daemon) handlePoll(w http.ResponseWriter, req *http.Request) {
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

// View is the daemon's current judgment of itself: the system verdict
// and its reason, the typed issue list, per-dataset health and ages,
// what it is doing right now, and the cycle history — the same
// derivation the dashboard renders and /api/state serializes, so a host
// exporting metrics or health checks cannot disagree with the page.
//
// It is a snapshot, computed on call, and safe to call concurrently
// with the sync loop.
func (b *Daemon) View() view.System {
	return b.viewOf(b.state.Deref(), b.conf.Deref(), b.activity.Get())
}

// viewOf is the one place the derivation is assembled. Both View and
// the page render go through it, so "an exported metric cannot disagree
// with the dashboard" is structural rather than two literals somebody
// keeps in step. It takes its inputs rather than reading them because
// the page needs the same activity snapshot it lays out elsewhere.
func (b *Daemon) viewOf(state *model.Model, conf *config.Config, activity status.Activity) view.System {
	return view.Compute(view.Input{
		State:    state,
		Conf:     conf,
		History:  b.history,
		Activity: activity,
		Now:      time.Now(),
		Boot:     b.boot,
	})
}

// pageData assembles one consistent snapshot of everything the
// dashboard shows: the raw state plus the derived view.System.
func (b *Daemon) pageData(req *http.Request, page string) pageData {
	state := b.state.Deref()
	conf := b.conf.Deref()
	activity := b.activity.Get()

	return pageData{
		Page:       page,
		Base:       basePath(req),
		Standalone: b.layout == nil,
		OmitNav:    b.omitNav,
		State:      state,
		Conf:       conf,
		Activity:   activity,
		Sys:        b.viewOf(state, conf, activity),
		Cycles:     b.history.Cycles(),
		Ops:        b.history.Ops(),
		Events:     b.history.Events(),
		Dryrun:     b.dryrun,
	}
}

func (b *Daemon) handlePage(w http.ResponseWriter, req *http.Request) {
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

	d := b.pageData(req, pageName)
	if b.layout == nil {
		templ.Handler(document(d)).ServeHTTP(w, req)
		return
	}

	var buf bytes.Buffer
	if err := pageBody(d).Render(req.Context(), &buf); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := b.layout(w, req, Page{Title: pageTitle(d), Body: template.HTML(buf.String())}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
