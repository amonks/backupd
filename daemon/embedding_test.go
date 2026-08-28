package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
	"monks.co/backupd/history"
)

// The tests here cover the seam between the daemon and a host page. A
// host embeds the dashboard body inside chrome the daemon knows nothing
// about, and mounts it under a path the daemon never sees in a URL. Two
// things have to hold for that to work: the daemon's CSS must not reach
// outside its own subtree, and every link and fetch it emits must be
// built from the mount point rather than assuming a root.

var (
	customPropRE = regexp.MustCompile(`(--[a-z0-9-]+)\s*:`)
	varUseRE     = regexp.MustCompile(`var\((--[a-z0-9-]+)`)
	styleBlockRE = regexp.MustCompile(`(?s)<style([^>]*)>(.*?)</style>`)
	urlAttrRE    = regexp.MustCompile(`(?:href|action|data-api)="([^"]*)"`)
	keyframesRE  = regexp.MustCompile(`@keyframes\s+([A-Za-z0-9_-]+)`)
	// The at-rules whose bodies hold ordinary rules, as opposed to
	// @keyframes, whose body holds keyframe selectors.
	conditionalAtRuleRE = regexp.MustCompile(`^@(media|supports|container|layer|scope)\b`)
	commentRE           = regexp.MustCompile(`(?s)/\*.*?\*/`)
	scriptRE            = regexp.MustCompile(`(?s)<script[^>]*>.*?</script>`)
)

// ownStyles returns the daemon's own style blocks from a rendered page,
// dropping the shared datagrid asset — that one scopes itself to its
// custom element, a contract datagrid's own tests hold.
func ownStyles(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, block := range styleBlockRE.FindAllStringSubmatch(body, -1) {
		if strings.Contains(block[1], "monks-datagrid") {
			continue
		}
		out = append(out, block[2])
	}
	if len(out) == 0 {
		t.Fatal("no <style> block of the daemon's own found; the extraction regex is stale")
	}
	return out
}

// pagePaths are the daemon's own GET pages. The dataset page is
// addressed by the one dataset the steady-state fixture holds.
var pagePaths = []string{"/global", "/config", "/root", "/foo"}

// renderPage fetches one dashboard page, optionally behind a mount
// prefix, and returns the response body.
func renderPage(t *testing.T, b *Daemon, path, prefix string) string {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if prefix != "" {
		req.Header.Set("X-Forwarded-Prefix", prefix)
	}
	w := httptest.NewRecorder()
	b.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s (prefix %q): status %d", path, prefix, w.Code)
	}
	return w.Body.String()
}

// embeddedDaemon returns a daemon whose layout captures the Page it is
// handed and writes the body straight through, which is what a host
// layout does around its own chrome.
func embeddedDaemon(t *testing.T, page *Page) *Daemon {
	t.Helper()
	local, remote := steadyStateExecutors()
	b, _ := newAPITestDaemon(t, local, remote)
	b.layout = func(w http.ResponseWriter, r *http.Request, p Page) error {
		*page = p
		_, err := w.Write([]byte(p.Body))
		return err
	}
	return b
}

// TestEmbeddedBodyIsOneScopedElement holds the contract Page documents:
// the body handed to a host layout is a single element the host can
// drop anywhere in its document. A stray <html> or <body> tag in there
// would be dropped or would restart the host's document.
func TestEmbeddedBodyIsOneScopedElement(t *testing.T) {
	var page Page
	b := embeddedDaemon(t, &page)
	primeState(t, b)

	for _, path := range pagePaths {
		renderPage(t, b, path, "")
		body := strings.TrimSpace(string(page.Body))
		if !strings.HasPrefix(body, `<div class="backupd`) {
			t.Errorf("GET %s: embedded body should be one .backupd element, got %.80s", path, body)
		}
		for _, tag := range []string{"<html", "<body", "<!DOCTYPE", "</html>", "</body>"} {
			if strings.Contains(body, tag) {
				t.Errorf("GET %s: embedded body contains %q, which belongs to the host's document", path, tag)
			}
		}
		if page.Title == "" {
			t.Errorf("GET %s: no page title for the host layout to render", path)
		}
	}
}

// TestEmbeddedStylesStayInsideTheSubtree is the rule that makes the body
// safe to embed: a bare `body { font-family: … }` rule restyles the
// host's masthead, and bare `table` / `h1` / `button` rules restyle
// everything else the host renders on the page.
func TestEmbeddedStylesStayInsideTheSubtree(t *testing.T) {
	var page Page
	b := embeddedDaemon(t, &page)
	primeState(t, b)

	for _, path := range pagePaths {
		renderPage(t, b, path, "")
		for _, css := range ownStyles(t, string(page.Body)) {
			for _, sel := range selectors(css) {
				switch {
				case sel == ".backupd",
					strings.HasPrefix(sel, ".backupd "),
					strings.HasPrefix(sel, ".backupd."),
					strings.HasPrefix(sel, ".backupd:"),
					strings.HasPrefix(sel, ".backupd>"),
					strings.HasPrefix(sel, ".backupd*"),
					// The standalone document's own body, which
					// exists only when the daemon owns the page.
					sel == "body.backupd-standalone-body":
				default:
					t.Errorf("GET %s: unscoped selector %q — every rule in the embedded body must sit under .backupd", path, sel)
				}
			}
		}
	}
}

// TestKeyframesAreNamespaced guards the one thing scoping cannot reach.
// A @keyframes name is global wherever the rule is written, so an
// animation called `spin` or `pulse` silently redefines the host's
// animation of the same name — and utility frameworks define both.
func TestKeyframesAreNamespaced(t *testing.T) {
	var page Page
	b := embeddedDaemon(t, &page)
	primeState(t, b)
	renderPage(t, b, "/global", "")

	found := 0
	for _, css := range ownStyles(t, string(page.Body)) {
		for _, m := range keyframesRE.FindAllStringSubmatch(css, -1) {
			found++
			if !strings.HasPrefix(m[1], "backupd-") {
				t.Errorf("@keyframes %q is a global name; prefix it", m[1])
			}
		}
	}
	if found == 0 {
		t.Fatal("no @keyframes found; the extraction regex is stale")
	}
}

// TestLinksCarryTheMountPoint: mounted under a prefix, every link the
// daemon renders has to point back inside the mount. The daemon never
// sees the prefix in a URL — the proxy strips it before forwarding — so
// the only source for it is the X-Forwarded-Prefix header.
func TestLinksCarryTheMountPoint(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, _ := newAPITestDaemon(t, local, remote)
	primeState(t, b)

	for _, prefix := range []string{"", "/backupd"} {
		for _, path := range pagePaths {
			// Scripts are stripped first: the datagrid asset builds
			// selectors like `[action="${…}"]` in template literals,
			// which are not links.
			body := scriptRE.ReplaceAllString(renderPage(t, b, path, prefix), "")

			urls := urlAttrRE.FindAllStringSubmatch(body, -1)
			if len(urls) == 0 {
				t.Fatalf("GET %s (prefix %q): no links found; the extraction regex is stale", path, prefix)
			}
			for _, m := range urls {
				u := m[1]
				if strings.Contains(u, "://") {
					continue
				}
				if strings.HasPrefix(u, "//") {
					t.Errorf("GET %s (prefix %q): scheme-relative URL %q — the browser reads this as another host", path, prefix, u)
				}
				if strings.Contains(u, "//") {
					t.Errorf("GET %s (prefix %q): doubled slash in URL %q", path, prefix, u)
				}
				// data-api paths are joined onto the mount point by
				// the script, so they stay root-relative in markup.
				if m[0][0] == 'd' {
					continue
				}
				if prefix != "" && !strings.HasPrefix(u, prefix+"/") {
					t.Errorf("GET %s (prefix %q): link %q escapes the mount", path, prefix, u)
				}
			}

			// The script joins every fetch onto this, so a wrong
			// value points the control API at the wrong host path.
			if want := `data-base="` + prefix + `"`; !strings.Contains(body, want) {
				t.Errorf("GET %s (prefix %q): page does not carry %s", path, prefix, want)
			}
		}
	}
}

// TestOmitNavDropsTheDaemonsOwnLinks: a host rendering Nav() itself asks
// for OmitNav so the two navs don't both appear. The dataset list is not
// part of that bargain — no host can render it, since it comes from the
// daemon's derived state.
func TestOmitNavDropsTheDaemonsOwnLinks(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, _ := newAPITestDaemon(t, local, remote)
	primeState(t, b)

	with := renderPage(t, b, "/global", "")
	for _, link := range Nav() {
		if !strings.Contains(with, `href="`+link.Path+`" class="nav-link`) {
			t.Fatalf("nav link %q missing from the default rendering", link.Label)
		}
	}

	// The stylesheet still carries the .nav-link rules — a host using
	// OmitNav may well reuse the class — so the check is on markup.
	b.omitNav = true
	without := styleBlockRE.ReplaceAllString(renderPage(t, b, "/global", ""), "")
	if strings.Contains(without, `class="nav-link`) {
		t.Error("the daemon rendered its own nav under OmitNav")
	}
	if !strings.Contains(without, `class="dataset-link"`) && !strings.Contains(without, "dataset-link") {
		t.Error("OmitNav dropped the dataset list, which no host can render")
	}
}

// TestStandaloneRendersAWholeDocument: with no layout installed the
// daemon owns the page, which is what licenses the viewport-height app
// shell and the color-scheme declaration.
func TestStandaloneRendersAWholeDocument(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, _ := newAPITestDaemon(t, local, remote)
	primeState(t, b)

	body := renderPage(t, b, "/global", "")
	for _, want := range []string{"<!doctype html>", "color-scheme", `class="backupd standalone"`} {
		if !strings.Contains(strings.ToLower(body), strings.ToLower(want)) {
			t.Errorf("standalone page missing %q", want)
		}
	}
}

// TestEmbeddedPageDeclaresNoColorScheme: the daemon's colors are
// light-dark() pairs, which resolve against the scheme in force. Naming
// a scheme inside the embedded subtree would override the host's choice
// and leave a light dashboard on a dark page.
func TestEmbeddedPageDeclaresNoColorScheme(t *testing.T) {
	var page Page
	b := embeddedDaemon(t, &page)
	primeState(t, b)
	renderPage(t, b, "/global", "")

	styles := strings.Join(ownStyles(t, string(page.Body)), "\n")
	if strings.Contains(styles, "color-scheme") {
		t.Error("embedded body declares a color-scheme; it must follow the host's")
	}
	if !strings.Contains(styles, "light-dark(") {
		t.Error("embedded body has no light-dark() colors; it cannot follow the host's scheme")
	}
}

// selectors returns the selector of every rule in a stylesheet,
// splitting selector lists on commas and descending into conditional
// at-rules. The descent is the point: the sheet's responsive rules live
// inside @media blocks, and a rule that reaches outside .backupd reaches
// just as far for being wrapped in one. @keyframes is the at-rule that
// is not descended into — its `from` / `to` / `50%` heads are not
// selectors and match no element at all.
func selectors(css string) []string {
	css = commentRE.ReplaceAllString(css, "")
	var out []string
	var head string
	start, blockStart, depth := 0, 0, 0
	for i, c := range css {
		switch c {
		case '{':
			if depth == 0 {
				head = strings.TrimSpace(css[start:i])
				blockStart = i + 1
				if head != "" && !strings.HasPrefix(head, "@") {
					for sel := range strings.SplitSeq(head, ",") {
						if sel = strings.TrimSpace(sel); sel != "" {
							out = append(out, sel)
						}
					}
				}
			}
			depth++
		case '}':
			depth--
			if depth == 0 {
				if conditionalAtRuleRE.MatchString(head) {
					out = append(out, selectors(css[blockStart:i])...)
				}
				start = i + 1
			}
		}
	}
	return out
}

// TestSelectorsDescendsIntoConditionalAtRules guards the guard: the
// scoping test is only worth what its selector walk covers, and a walk
// that stopped at a @media block would wave every responsive rule
// through.
func TestSelectorsDescendsIntoConditionalAtRules(t *testing.T) {
	got := selectors(`
		.a { color: red }
		@media (max-width: 45rem) { .b, table { color: red } }
		@keyframes spin { from { opacity: 0 } to { opacity: 1 } }
		.c { color: red }
	`)
	want := []string{".a", ".b", "table", ".c"}
	if len(got) != len(want) {
		t.Fatalf("selectors() = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("selectors()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// primeState refreshes the daemon from its fake environment, so pages
// render with the fixture's dataset in the sidebar rather than against
// an empty model.
func primeState(t *testing.T, b *Daemon) {
	t.Helper()
	if err := b.refreshAllDatasetsAndPlans(t.Context()); err != nil {
		t.Fatalf("priming state: %v", err)
	}
}

// TestMountServesUnderASubPath: a host without a prefix-setting proxy
// puts the dashboard under a path of its own mux, and everything the
// page emits has to stay inside it — including the redirect off the
// mount root, which points at a backend-absolute path.
func TestMountServesUnderASubPath(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, _ := newAPITestDaemon(t, local, remote)
	primeState(t, b)

	mux := http.NewServeMux()
	mux.Handle("/backups/", b.Mount("/backups"))

	req := httptest.NewRequest("GET", "/backups/global", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /backups/global: status %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `data-base="/backups"`) {
		t.Error("mounted page does not carry its mount point")
	}
	if !strings.Contains(w.Body.String(), `href="/backups/config"`) {
		t.Error("mounted page links escape the mount")
	}

	req = httptest.NewRequest("GET", "/backups/", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if got := w.Header().Get("Location"); got != "/backups/global" {
		t.Errorf("redirect Location = %q, want /backups/global", got)
	}
}

// TestViewMatchesTheDashboard: a host exporting health or metrics reads
// View rather than scraping the page, and the two must be the same
// derivation — a second opinion is a bug, not a feature.
func TestViewMatchesTheDashboard(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, _ := newAPITestDaemon(t, local, remote)
	primeState(t, b)

	sys := b.View()
	if sys.DatasetCount == 0 {
		t.Fatal("View reports no datasets after a refresh")
	}
	body := renderPage(t, b, "/global", "")
	if !strings.Contains(body, sys.Verdict.String()) {
		t.Errorf("View's verdict %q does not appear on the page", sys.Verdict)
	}
}

// TestPageTitlesNameThePageNotTheProgram: a host layout composes the
// title with its own site's name, so a title of "backupd" reads as
// "backupd — backups · ss.cx" on the page it labels.
func TestPageTitlesNameThePageNotTheProgram(t *testing.T) {
	var page Page
	b := embeddedDaemon(t, &page)
	primeState(t, b)

	for path, want := range map[string]string{
		"/global": "Overview",
		"/config": "Config",
		"/foo":    "foo",
	} {
		renderPage(t, b, path, "")
		if page.Title != want {
			t.Errorf("GET %s: title %q, want %q", path, page.Title, want)
		}
	}

	// Standalone, nothing else names the program, so the document does.
	local, remote := steadyStateExecutors()
	standalone, _ := newAPITestDaemon(t, local, remote)
	if body := renderPage(t, standalone, "/global", ""); !strings.Contains(body, "<title>backupd — Overview</title>") {
		t.Error("the standalone document's title does not name the program")
	}
}

// Every custom property the stylesheet reads must be one it defines: a
// typo'd var() is not an error, it is a rule that silently does
// nothing, and embedded there is a host stylesheet nearby whose
// property of that name might even resolve.
func TestEveryCustomPropertyIsDefined(t *testing.T) {
	var page Page
	b := embeddedDaemon(t, &page)
	primeState(t, b)
	renderPage(t, b, "/global", "")

	css := strings.Join(ownStyles(t, string(page.Body)), "\n")
	defined := map[string]bool{}
	for _, m := range customPropRE.FindAllStringSubmatch(css, -1) {
		defined[m[1]] = true
	}
	used := map[string]bool{}
	for _, m := range varUseRE.FindAllStringSubmatch(css, -1) {
		used[m[1]] = true
	}
	if len(used) == 0 {
		t.Fatal("no var() uses found; the extraction regex is stale")
	}
	for name := range used {
		if !defined[name] {
			t.Errorf("stylesheet reads %s but never defines it", name)
		}
	}
}

// The dashboard's script carries the request's CSP nonce when the
// context has one (serve.Mux stamps the proxy's), so the deployed app
// isn't on the proxy's inline work-list and keeps its behavior under
// enforcement.
func TestScriptCarriesTheNonce(t *testing.T) {
	var b strings.Builder
	if err := script().Render(templ.WithNonce(context.Background(), "n0nce"), &b); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.String(), `<script nonce="n0nce">`) {
		t.Errorf("script lacks the nonce: %.60s", b.String())
	}
}

// Every "when" on the dashboard is a localtime stamp — the instant in
// the element for the viewer's browser to rewrite into their zone, the
// marked UTC reading as the text a reader without the script gets. The
// demo fixture is used because it has every surface at once: a cycle
// strip, issues, a failing dataset with a last failure, a busy one with
// a plan and log lines, snapshots. Each surface is asserted by its own
// markup, so one converted stamp cannot stand in for another.
func TestStampsReadInTheViewersZone(t *testing.T) {
	b := demoDaemon(t)
	// The demo primes state without running a cycle; the strip needs one.
	b.history.RecordCycle(history.Cycle{StartedAt: time.Now().Add(-2 * time.Minute), StoppedAt: time.Now().Add(-time.Minute), OK: true, Datasets: 1})
	global := renderPage(t, b, "/global", "")
	busy := renderPage(t, b, busyDataset, "")

	if !strings.Contains(global, `id="monks-localtime-script"`) {
		t.Errorf("standalone document lacks the localtime script")
	}
	stamp := regexp.MustCompile(`<time datetime="\d{4}-\d\d-\d\dT[^"]*Z" data-localtime="(minute|second|day)">[^<]*</time>`)
	titled := regexp.MustCompile(`<time datetime="\d{4}-\d\d-\d\dT[^"]*Z" data-localtime-title="second" title="\d{4}-\d\d-\d\d \d\d:\d\d:\d\d UTC">`)
	dot := regexp.MustCompile(`class="qdot qdot-[a-z]+" data-at="\d{4}-[^"]*Z" data-outcome="[^"]+" title="\d{4}-\d\d-\d\d \d\d:\d\d:\d\d UTC · [^"]+"`)

	for _, c := range []struct{ page, what, surface string }{
		{global, "global", "the cycle strip's dots carry the instant and the outcome as data"},
		{global, "global", "an issue's since"},
		{global, "global", "the sidebar's backed-up age"},
		{busy, busyDataset, "the recovery sentence's restore point"},
		{busy, busyDataset, "the recovery sentence's oldest date"},
		{busy, busyDataset, "a plan step's start"},
	} {
		var ok bool
		switch c.surface {
		case "the cycle strip's dots carry the instant and the outcome as data":
			ok = dot.MatchString(c.page)
			if !ok {
				t.Logf("cycle strip on %s: %q", c.what, regexp.MustCompile(`<div class="cycle-strip">.{0,300}`).FindString(c.page))
			}
		case "an issue's since":
			ok = strings.Contains(c.page, `class="issue-since">`) && titled.MatchString(c.page)
		case "the sidebar's backed-up age":
			ok = regexp.MustCompile(`class="dataset-age[^"]*">\s*<time datetime="[^"]*" data-localtime-title="second"`).MatchString(c.page)
		case "the recovery sentence's restore point":
			ok = regexp.MustCompile(`<b>\s*<time datetime="[^"]*" data-localtime="minute">`).MatchString(c.page)
		case "the recovery sentence's oldest date":
			ok = strings.Contains(c.page, `data-localtime="day">`)
		case "a plan step's start":
			ok = stamp.MatchString(c.page) && strings.Contains(c.page, `class="step-status`)
		}
		if !ok {
			t.Errorf("%s: %s is not a localtime stamp", c.what, c.surface)
		}
	}

	// No stamp anywhere is written the old way. Every wall-clock time on
	// either page is a marked UTC fallback — a stamp's text, a title the
	// script rewrites or composes, a grid's search value — so a bare one
	// is a Format call the sweep missed.
	wallClock := regexp.MustCompile(`\d{4}-\d\d-\d\d \d\d:\d\d(?::\d\d)?( UTC)?`)
	for _, page := range []string{global, busy} {
		for _, m := range wallClock.FindAllStringSubmatchIndex(page, -1) {
			if m[2] < 0 {
				t.Errorf("a wall-clock time outside a stamp: %q in …%s…", page[m[0]:m[1]], page[max(0, m[0]-120):min(len(page), m[1]+40)])
			}
		}
	}

	// The embedded body renders the same stamps and leaves the script
	// to the host's head.
	var page Page
	e := embeddedDaemon(t, &page)
	primeState(t, e)
	renderPage(t, e, "/global", "")
	if strings.Contains(string(page.Body), `id="monks-localtime-script"`) {
		t.Errorf("the embedded body carries the script; that is the host's head to render")
	}
	if !titled.MatchString(string(page.Body)) {
		t.Errorf("the embedded body's stamps are not localtime's")
	}
}
