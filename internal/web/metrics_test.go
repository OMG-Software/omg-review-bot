package web

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/bot"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// TestMetricsPages: an organization's members see its metrics (and only
// its own); every organization's together are for administrators.
func TestMetricsPages(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	for _, c := range []store.ModelCall{
		// A name that contains the model name: members see neither.
		{AccountID: acmeID, ReviewID: 1, Kind: store.CallReview, ConfigID: "m1", Label: "Production shared-7b", Model: "shared-7b", OK: true, PromptTokens: 900},
		{AccountID: otherID, ReviewID: 2, Kind: store.CallReview, ModelSet: otherID, ConfigID: "m1", Label: "Secret own model", Model: "secret-70b", OK: true},
		// An instance model with no name of its own is labelled with its
		// model name: members see neither.
		{AccountID: acmeID, ReviewID: 1, Kind: store.CallReview, ConfigID: "m2", Label: "plain-13b", Model: "plain-13b", OK: true},
	} {
		if err := s.st.RecordModelCall(c); err != nil {
			t.Fatal(err)
		}
	}
	// A repository whose only review failed still shows, with its failure.
	id, err := s.st.CreateReview(&store.Review{Repo: "acme/broken", Number: 1, AccountID: acmeID, Status: "running", StartedAt: time.Now().Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.UpdateReview(&store.Review{ID: id, Repo: "acme/broken", Number: 1, Status: "error", FinishedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	s.signIn() // alice: an owner of acme only

	code, body, _ := s.get("/orgs/acme/metrics?days=90")
	if code != http.StatusOK {
		t.Fatalf("acme's metrics = %d", code)
	}
	for _, want := range []string{"Most active repositories", "acme/web", "Findings by repository and level", ">Instance model", "Removed instance model", // m1 is configured, m2 isn't
		"Reviews sent to each model", "Time to first review", `class="active">90 days`, "acme/broken"} {
		if !strings.Contains(body, want) {
			t.Errorf("acme's metrics lack %q", want)
		}
	}
	// The page says every chart has a table: the day, level, per-period and
	// findings-by-repository charts carry one each (the rest sit above
	// tables of exactly their rows).
	if n := strings.Count(body, `class="table-view"`); n != 4 {
		t.Errorf("acme's metrics have %d table views, want 4", n)
	}
	// Another organization's repositories, models and reviews stay out of
	// it, and an instance model's server isn't named to members.
	for _, leak := range []string{"other/secret", "Secret own model", "secret-70b", "shared-7b", "Production", "plain-13b", "by organization"} {
		if strings.Contains(body, leak) {
			t.Errorf("acme's metrics show %q", leak)
		}
	}
	if code, _, _ := s.get("/orgs/other/metrics"); code != http.StatusNotFound {
		t.Errorf("another organization's metrics = %d, want 404", code)
	}
	if code, _, _ := s.get("/metrics"); code != http.StatusForbidden {
		t.Errorf("every organization's metrics for an owner = %d, want 403", code)
	}
	if _, body, _ := s.get("/orgs/acme"); !strings.Contains(body, "Most active repositories") {
		t.Error("the overview has no chart of the most active repositories")
	}

	if err := s.st.SetUserAdmin(aliceID, true); err != nil {
		t.Fatal(err)
	}
	code, body, _ = s.get("/metrics")
	if code != http.StatusOK {
		t.Fatalf("every organization's metrics for an administrator = %d", code)
	}
	for _, want := range []string{"acme/web", "other/secret", "Findings by organization and level", "Secret own model",
		"Reviews posted per day by organization", `<span class="viz-swatch viz-c2"></span>other</li>`,
		"secret-70b", "other's own", "plain-13b", "Production shared-7b", "/orgs/acme/reviews?repo="} {
		if !strings.Contains(body, want) {
			t.Errorf("the administrators' metrics lack %q", want)
		}
	}
	if _, body, _ := s.get("/"); !strings.Contains(body, "Most active repositories") || !strings.Contains(body, `href="/metrics"`) {
		t.Error("the dashboard has no chart of the most active repositories, or no link to the metrics")
	} else if !strings.Contains(body, "Reviews posted per day by organization") || !strings.Contains(body, `<span class="viz-swatch viz-c1"></span>acme</li>`) {
		t.Error("the dashboard doesn't split the reviews per day by organization")
	}
}

// TestHideInstanceModelNames: an instance model's number is its place in the
// configured list, the same whichever models a period used; a removed one,
// or an earlier model of a configuration, says so; own models keep names.
func TestHideInstanceModelNames(t *testing.T) {
	cfgs := []settings.LLM{{ID: "a", Model: "small"}, {ID: "b", Model: "mid"}, {ID: "c", Model: "large"}}
	names := func(ms []store.ModelUsage) []string {
		hideInstanceModelNames(ms, cfgs)
		var out []string
		for _, m := range ms {
			if m.Model != "" && m.ModelSet == 0 {
				t.Errorf("%s still names its model %q", m.Label, m.Model)
			}
			out = append(out, m.Label)
		}
		return out
	}
	// Seven days: "b" unused. Ninety: all three, plus history.
	week := names([]store.ModelUsage{{ConfigID: "c", Label: "Large one", Model: "large"}, {ConfigID: "a", Label: "small", Model: "small"}})
	quarter := names([]store.ModelUsage{
		{ConfigID: "c", Label: "Large one", Model: "large"}, {ConfigID: "b", Label: "mid", Model: "mid"},
		{ConfigID: "a", Label: "small", Model: "small"}, {ConfigID: "a", Label: "tiny", Model: "tiny"},
		{ConfigID: "z", Label: "gone", Model: "gone"}, {ModelSet: 5, ConfigID: "a", Label: "Ours", Model: "ours-7b"},
	})
	if fmt.Sprint(week) != "[Instance model 3 Instance model 1]" {
		t.Errorf("7 days = %v", week)
	}
	want := "[Instance model 3 Instance model 2 Instance model 1 Instance model 1 (earlier model) Removed instance model Ours]"
	if fmt.Sprint(quarter) != want {
		t.Errorf("90 days = %v, want %s", quarter, want)
	}
}

// TestRepoChartCaptions: a repository chart cut to its top rows says so,
// and the table's hint doesn't claim the charts cover every repository.
func TestRepoChartCaptions(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	now := time.Now().Unix()
	for i := range repoChartRows + 2 {
		repo := fmt.Sprintf("acme/r%02d", i)
		id, err := s.st.CreateReview(&store.Review{Repo: repo, Number: 1, AccountID: acmeID, Status: "running", StartedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.st.UpdateReview(&store.Review{ID: id, Repo: repo, Number: 1, Status: "posted", Comments: 1, Sev: [5]int{0, 0, 1, 0, 0}, FinishedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	s.signIn()
	code, body, _ := s.get("/orgs/acme/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics = %d", code)
	}
	top := fmt.Sprintf("the top %d of", repoChartRows)
	if strings.Count(body, top) != 2 || strings.Contains(body, "cover them all") {
		t.Errorf("captions: %d with %q, want 2 (one per chart), and no claim the charts cover all", strings.Count(body, top), top)
	}
}

// TestOrgSevRows: the organizations' chart shows the ones with the most
// findings, most first, at most the cap.
func TestOrgSevRows(t *testing.T) {
	var orgs []store.OrgMetrics
	for i := range repoChartRows + 3 {
		orgs = append(orgs, store.OrgMetrics{Login: fmt.Sprintf("org%02d", i), Findings: i})
	}
	if rows := orgSevRows(orgs, repoChartRows); len(rows) != repoChartRows || rows[0].Label != fmt.Sprintf("org%02d", repoChartRows+2) {
		t.Errorf("organizations' chart: %d rows, first %q; want the %d with most findings, most first", len(rows), rows[0].Label, repoChartRows)
	}
}

// TestRepoNames: across organizations, a repository on two accounts' rows,
// or not under its organization's name, says whose row it is.
func TestRepoNames(t *testing.T) {
	names := repoNames([]store.RepoStats{
		{Org: "acme", Repo: "acme/web"},
		{Org: "acme", Repo: "acme/api"},
		{Org: "Acme", Repo: "acme/api"}, // installed again: a new account
		{Org: "globex", Repo: "acme/moved"},
		{Org: "", Repo: "x/y"},
	})
	want := []string{"acme/web", "acme/api (acme)", "acme/api (Acme)", "acme/moved (globex)", "x/y (no organization)"}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("name %d = %q, want %q", i, names[i], w)
		}
	}
	if a, b := segClass(sevColor(0), 3.9), segClass(sevColor(4), 4); a != "viz-seg viz-s1 short" || b != "viz-seg viz-s5" {
		t.Errorf("segment classes: %q %q", a, b)
	}
}

// TestSplitByOrg: each organization is a series in a fixed order, by
// account ID, with no organization and then the folded ones last; past the
// palette, the least active share one series.
func TestSplitByOrg(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	days := make([]store.DayStats, 10)
	for i := range days {
		days[i].Day = start.AddDate(0, 0, i)
	}
	labels := func(o orgDays) string {
		var out []string
		for _, s := range o.Series {
			out = append(out, fmt.Sprintf("%s=c%d", s.Label, s.Slot))
		}
		return strings.Join(out, " ")
	}

	// A login on two accounts is one series; reviews linked to no stored
	// organization come after every organization, though account 0 is the
	// lowest; a day outside the chart's is left out.
	o := splitByOrg(days, []store.OrgDayPosted{
		{Day: start, AccountID: 0, Posted: 1},
		{Day: start, AccountID: 7, Login: "globex", Posted: 4},
		{Day: start, AccountID: 12, Login: "Acme", Posted: 2}, // installed again
		{Day: start.AddDate(0, 0, 1), AccountID: 5, Login: "acme", Posted: 3},
		{Day: start.AddDate(0, 0, -1), AccountID: 9, Login: "earlier", Posted: 50},
	}, false)
	if got := labels(o); got != "Acme=c1 globex=c2 (no organization)=c3" {
		t.Errorf("series = %s", got)
	}
	if fmt.Sprint(o.Series[0].Posted[:3]) != "[2 3 0]" || len(o.Series[0].Posted) != len(days) {
		t.Errorf("acme's two accounts = %v", o.Series[0].Posted)
	}

	// Nine organizations: the seven most active keep a color each, in
	// account order; the two least share the "other" series.
	var counts []store.OrgDayPosted
	for id := int64(1); id <= 9; id++ {
		counts = append(counts, store.OrgDayPosted{Day: start, AccountID: id, Login: fmt.Sprintf("org%d", id), Posted: int(id)})
	}
	o = splitByOrg(days, counts, false)
	if got := labels(o); got != "org3=c1 org4=c2 org5=c3 org6=c4 org7=c5 org8=c6 org9=c7 2 other organizations=c0" {
		t.Errorf("series = %s", got)
	}
	if other := o.Series[len(o.Series)-1]; other.Posted[0] != 1+2 || other.Color() != "viz-c0" {
		t.Errorf("other organizations = %+v", other)
	}
	if o = splitByOrg(days, counts[1:], false); len(o.Series) != maxOrgSeries || o.Series[0].Label != "org2" {
		t.Errorf("eight organizations aren't folded: %s", labels(o))
	}

	// By week, each series is summed over the same weeks as the totals.
	counts = counts[:0]
	for i := range days {
		days[i].Posted = 2
		counts = append(counts, store.OrgDayPosted{Day: days[i].Day, AccountID: 1, Login: "acme", Posted: 1},
			store.OrgDayPosted{Day: days[i].Day, AccountID: 2, Login: "globex", Posted: 1})
	}
	o = splitByOrg(days, counts, true)
	if len(o.Days) != 2 || o.Days[0].Posted != 6 || o.Days[1].Posted != 14 ||
		fmt.Sprint(o.Series[0].Posted) != "[3 7]" || fmt.Sprint(o.Series[1].Posted) != "[3 7]" {
		t.Errorf("by week: days %+v, series %+v", o.Days, o.Series)
	}
	chart := string(orgDayChart(o))
	for _, want := range []string{`aria-label="Reviews posted per week by organization, last 10 days"`,
		`<span class="viz-swatch viz-c1"></span>acme</li>`, `<span class="viz-swatch viz-c2"></span>globex</li>`,
		"Week of Sun 4 Jan: 14 reviews posted (acme: 7 · globex: 7)", "viz-seg viz-c2"} {
		if !strings.Contains(chart, want) {
			t.Errorf("the chart lacks %q", want)
		}
	}
}

// TestByWeek: a long period is charted by week, the last week ending today
// and a part week, if any, first.
func TestByWeek(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	days := make([]store.DayStats, 10)
	for i := range days {
		days[i] = store.DayStats{Day: start.AddDate(0, 0, i), Posted: 1, Sev: [5]int{i, 0, 0, 0, 1}}
	}
	w := byWeek(days)
	if len(w) != 2 || w[0].Span != 3 || w[1].Span != 7 || !w[0].Day.Equal(start) || !w[1].Day.Equal(start.AddDate(0, 0, 3)) {
		t.Fatalf("weeks = %+v", w)
	}
	if w[0].Posted != 3 || w[1].Posted != 7 || w[0].Sev != [5]int{0 + 1 + 2, 0, 0, 0, 3} || w[1].Sev[4] != 7 {
		t.Errorf("weeks' sums = %+v", w)
	}
	if periodUnit(w) != "week" || periodUnit(days) != "day" || periodLabel(w[1]) != "Week of Sun 4 Jan" {
		t.Errorf("labels: %q %q %q", periodUnit(w), periodUnit(days), periodLabel(w[1]))
	}
	// The charts' names say the period they cover, by day or by week.
	for _, got := range []string{string(dayChart(w)), string(sevDayChart(w)), string(dayChart(days)), string(orgDayChart(splitByOrg(days, nil, true)))} {
		if !strings.Contains(got, `, last 10 days"`) {
			t.Errorf("a chart's name doesn't say it covers 10 days: %.160s", got)
		}
	}
	// A year is 52 weeks and a day: the part week first is a day long, and
	// the chart is still by week.
	if y := byWeek(make([]store.DayStats, 365)); len(y) != 53 || y[0].Span != 1 || periodUnit(y) != "week" {
		t.Errorf("a year's weeks: %d, first %d days, unit %q", len(y), y[0].Span, periodUnit(y))
	}
}

// An administrator can stop new reviews from the dashboard (to restart the
// bot safely) and resume them; nobody else can.
func TestStopReviewsButton(t *testing.T) {
	s := newTestSite(t)
	s.seedOrgs()
	s.signIn() // alice: an owner of acme only
	if code, _ := s.post("/reviews/stop", nil); s.b.Stopped() {
		t.Fatalf("a non-administrator stopped reviews (HTTP %d)", code)
	}
	if err := s.st.SetUserAdmin(aliceID, true); err != nil {
		t.Fatal(err)
	}
	if _, body, _ := s.get("/"); !strings.Contains(body, `action="/reviews/stop"`) {
		t.Fatal("the dashboard has no Stop reviews button")
	}
	s.post("/reviews/stop", nil)
	if !s.b.Stopped() {
		t.Fatal("reviews not stopped")
	}
	_, body, _ := s.get("/")
	for _, want := range []string{"New reviews are stopped", "safe to upgrade and restart", `action="/reviews/resume"`} {
		if !strings.Contains(body, want) {
			t.Errorf("stopped dashboard lacks %q", want)
		}
	}
	if strings.Contains(body, `action="/reviews/stop"`) {
		t.Error("stopped dashboard still offers Stop reviews")
	}
	s.post("/reviews/resume", nil)
	if s.b.Stopped() {
		t.Fatal("reviews not resumed")
	}
}

// While reviews are stopped with jobs still running, the dashboard says how
// many (it isn't yet safe to restart) and still offers Resume.
func TestStoppedDashboardWithJobRunning(t *testing.T) {
	s := newTestSite(t)
	job := bot.Activity{What: "Reviewing", Repo: "acme/web", Number: 7}
	for _, c := range []struct {
		working []bot.Activity
		says    string
	}{
		{[]bot.Activity{job}, "1 job is still running"},
		{[]bot.Activity{job, job}, "2 jobs are still running"},
	} {
		var buf bytes.Buffer
		err := s.srv.pages["dashboard.html"].ExecuteTemplate(&buf, "layout", page{Title: "Dashboard", Nav: "dash", Auth: true, V: &viewer{Admin: true},
			Status: bot.Status{Stopped: true, Working: c.working},
			D:      s.srv.dashboardData()})
		if err != nil {
			t.Fatal(err)
		}
		body := buf.String()
		for _, want := range []string{"New reviews are stopped", c.says, `action="/reviews/resume"`, "Resume reviews"} {
			if !strings.Contains(body, want) {
				t.Errorf("dashboard with %d jobs lacks %q", len(c.working), want)
			}
		}
		if strings.Contains(body, "safe to upgrade and restart") {
			t.Errorf("says it's safe to restart with %d jobs still running", len(c.working))
		}
	}
}
