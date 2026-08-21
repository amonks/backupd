package daemon

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"monks.co/backupd/config"
	"monks.co/backupd/sim"
)

// The tests here cover the dashboard's narrow and touch layouts. Both
// are expressed entirely in CSS against markup that looks arbitrary
// from the markup's side, so the coupling between the two is the thing
// worth holding: a rule that no longer finds its element, or an element
// that no longer carries the class a rule orders by, does not fail
// loudly — it just lays the page out wrong on the one device nobody
// is looking at while they work.

var (
	// The classes the narrow block orders around the content. Loose
	// about whitespace on purpose: a reformatted block that still orders
	// the same four things should not fail, and should certainly not
	// fail claiming it orders none of them.
	orderRE = regexp.MustCompile(`\.backupd \.([a-z-]+)\s*\{[^{}]*\border:\s*\d+`)
	// The daemon's own <style> blocks always carry these two at-rules.
	narrowQuery = "@media (max-width: 820px)"
	touchQuery  = "@media (hover: none)"
)

// atRuleBody returns the body of the at-rule whose prelude is prelude.
func atRuleBody(t *testing.T, css, prelude string) string {
	t.Helper()
	css = commentRE.ReplaceAllString(css, "")
	i := strings.Index(css, prelude)
	if i < 0 {
		t.Fatalf("stylesheet has no %s block", prelude)
	}
	open := strings.Index(css[i:], "{")
	if open < 0 {
		t.Fatalf("%s has no body", prelude)
	}
	depth := 0
	for j, c := range css[i+open:] {
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return css[i+open+1 : i+open+j]
			}
		}
	}
	t.Fatalf("%s block is unterminated", prelude)
	return ""
}

// daemonStyles returns one page's own stylesheet as a single string.
func daemonStyles(t *testing.T, b *Daemon, path string) string {
	t.Helper()
	return strings.Join(ownStyles(t, renderPage(t, b, path, "")), "\n")
}

// TestNarrowLayoutOrdersGroupsThatExist is the coupling between the
// sidebar's markup and the narrow block that rearranges it. Below the
// breakpoint the sidebar's own box dissolves, so its groups become
// siblings of the content and are ordered around it one at a time —
// the page links ahead of it, the controls and the dataset list after.
// Flatten the sidebar back into a run of children and nothing errors:
// the phone layout just silently loses its order, dropping the page
// links to the bottom and spreading the controls one to a row.
func TestNarrowLayoutOrdersGroupsThatExist(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, _ := newAPITestDaemon(t, local, remote)
	primeState(t, b)

	page := renderPage(t, b, "/global", "")
	narrow := atRuleBody(t, strings.Join(ownStyles(t, page), "\n"), narrowQuery)

	ordered := orderRE.FindAllStringSubmatch(narrow, -1)
	if len(ordered) < 4 {
		t.Fatalf("the narrow block orders %d elements; it takes at least the "+
			"three sidebar groups and the content to lay the page out", len(ordered))
	}
	// Strip the script as well as the styles: it is 4KB of string
	// literals that build markup (renderImpacts emits a
	// class="table-scroll" of its own), so a class found only in there
	// would pass this for the wrong reason. embedding_test.go strips it
	// for the same hazard.
	markup := scriptRE.ReplaceAllString(styleBlockRE.ReplaceAllString(page, ""), "")
	for _, m := range ordered {
		if !strings.Contains(markup, `class="`+m[1]+`"`) {
			t.Errorf("the narrow block orders .%s, which no element on the page carries", m[1])
		}
	}

	// The ordering only reaches the sidebar's groups because the
	// sidebar itself stops being a box. Left as a pane, the whole
	// 270px column stays beside the content — which on a phone is
	// most of the screen — and every order above applies to nothing.
	if !strings.Contains(narrow, ".backupd .sidebar { display: contents; }") {
		t.Error("the narrow block leaves the sidebar a box, so its groups cannot be ordered around the content")
	}
}

// TestNarrowLayoutReleasesTheStandaloneAppShell: standalone, the
// dashboard is a viewport-height shell whose two panes scroll
// independently under a fixed strip. Stacked, that second scroller is
// a trap — a pane of its own height inside a page that also scrolls —
// so the narrow layout hands the scrolling back to the document.
func TestNarrowLayoutReleasesTheStandaloneAppShell(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, _ := newAPITestDaemon(t, local, remote)
	primeState(t, b)

	narrow := atRuleBody(t, daemonStyles(t, b, "/global"), narrowQuery)
	for _, want := range []string{
		".backupd.standalone { height: auto; }",
		".backupd.standalone .main-content { overflow-y: visible; }",
	} {
		if !strings.Contains(narrow, want) {
			t.Errorf("narrow block missing %q; the standalone shell keeps its own scrollers when stacked", want)
		}
	}
}

// TestTouchFloorsTypedControls: 16px is the size below which a phone
// browser zooms the page in to meet a field taking focus, and it never
// zooms back out — so one tap on the periodicity picker leaves the
// operator panning a magnified dashboard around. Every control here is
// deliberately smaller than that, because this is a dense page read at
// a desk, which is exactly why the floor has to be its own rule.
//
// The assertion is on the whole declaration, !important included,
// because a floor that loses the cascade is not a floor: #config-editor
// sets its font-size from an id, which outranks every selector the
// touch block can write, and the textarea it names is the one control
// on these pages anybody types into. A rule that merely existed left
// that one at 14.4px.
//
// monks.co/pkg/opsui floors its own controls the same way and this
// daemon is embedded in it, but it is published to hosts that are not
// opsui, so it carries its own.
func TestTouchFloorsTypedControls(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, _ := newAPITestDaemon(t, local, remote)
	primeState(t, b)

	// The config page carries the textarea and the sidebar's select;
	// the search fields belong to the grids on every other page.
	touch := atRuleBody(t, daemonStyles(t, b, "/config"), touchQuery)
	for _, control := range []string{"select", "textarea", "input"} {
		floored := false
		for rule := range strings.SplitSeq(touch, "}") {
			head, body, ok := strings.Cut(rule, "{")
			if !ok || !strings.Contains(body, "font-size: 16px") || !strings.Contains(body, "!important") {
				continue
			}
			if strings.Contains(head, ".backupd "+control) {
				floored = true
			}
		}
		if !floored {
			t.Errorf("no touch rule floors <%s> at 16px !important, so a rule of higher "+
				"specificity — an id, or a host's — leaves the zoom trap open", control)
		}
	}
}

// busyDatasetPage renders a dataset page in the state the layout is
// hardest in: a dataset several transfers behind its remote, so the
// planner has produced a plan and the page carries the plan table
// under the facts. The steady-state fixture the other tests use is
// converged by construction, which leaves `planSection` emitting
// nothing — and the plan table is the widest thing on the page.
func busyDatasetPage(t *testing.T) string {
	t.Helper()
	return renderPage(t, demoDaemon(t), busyDataset, "")
}

// busyDataset is the demo environment's backlogged, persistently
// failing dataset: a plan that never drains, a long ZFS error in the
// facts and in the overview's issue list, and dated snapshot names in
// the plan's operation column. The hardest page the layout has.
const busyDataset = "/db"

// demoDaemon is the fixture behind the responsive tests and the
// browser one. It is sim.Demo — the same environment `backupd -sim`
// runs, and the reason to use it here rather than a hand-seeded pair:
// its dataset names are paths, its snapshot names carry a timestamp,
// and one dataset fails with a real ZFS message. A fixture of "/foo"
// and "daily-1" fits any width and so proves nothing about a phone.
func demoDaemon(t *testing.T) *Daemon {
	t.Helper()
	conf := simConf(map[string]int{"hourly": 6, "daily": 7}, map[string]int{"daily": 14}, true)
	// The demo's paused subtree, which is where the overview's info
	// issue and the sidebar's paused dots come from.
	conf.Overrides = map[string]*config.Override{"/tm": {Paused: true}}
	b := newSimDaemon(conf, sim.Demo(time.Now()))
	primeState(t, b)
	return b
}

// tableTagRE finds every table on a page along with its attributes.
var tableTagRE = regexp.MustCompile(`<table([^>]*)>`)

// TestEveryPlainTableStacksOrScrolls holds the rule the narrow layout
// splits tables by: one whose cells are a label and a value stacks
// (.facts), and one whose cells only mean anything in their column
// scrolls inside its own box (.table-scroll). A table with neither
// treatment is the third case — it widens the page, and the phone
// pans sideways again for the sake of one table.
//
// The datagrids are exempt: they bring their own scroller, and this
// sheet stands aside for anything inside a monks-datagrid.
func TestEveryPlainTableStacksOrScrolls(t *testing.T) {
	page := busyDatasetPage(t)

	// Without a plan the interesting half of this page is missing and
	// the assertion below passes vacuously.
	if !strings.Contains(page, `class="step-status`) {
		t.Fatal("the busy fixture rendered no plan steps, so the plan table is not on this page")
	}

	var plain int
	for _, m := range tableTagRE.FindAllStringSubmatchIndex(page, -1) {
		attrs := page[m[2]:m[3]]
		if strings.Contains(attrs, "datagrid") {
			continue
		}
		plain++
		if strings.Contains(attrs, `class="facts"`) {
			continue
		}
		before := strings.TrimRight(page[:m[0]], " \t\n")
		if !strings.HasSuffix(before, `<div class="table-scroll">`) {
			t.Errorf("<table%s> neither stacks nor scrolls: it is not .facts and nothing "+
				"wraps it in a .table-scroll, so at a phone's width it widens the page", attrs)
		}
	}
	if plain < 3 {
		t.Errorf("found %d plain tables on the dataset page; expected the facts, "+
			"fulfillment and plan tables at least", plain)
	}
}
