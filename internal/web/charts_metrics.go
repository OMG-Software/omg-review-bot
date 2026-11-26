package web

import (
	"fmt"
	"html"
	"html/template"
	"sort"
	"strings"

	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// The metrics pages' charts: ranked bars (most active repositories, models)
// and findings stacked by level. Levels are an ordered scale, so they're one
// hue from light (P5) to dark (P1) — the classes viz-s1..viz-s5 in app.css,
// with their own steps in dark mode — not five unrelated colors.

// rankRow is one row of a horizontal bar chart.
type rankRow struct {
	Label string
	Value int
	Tip   string // the hover text
}

// chartLabel shortens a row label to fit the label column; the full text
// is in the row's hover title.
func chartLabel(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// hbarEnd draws a horizontal bar from x with 4px rounded corners at its
// data end only, square at the baseline.
func hbarEnd(x, cy, w, h float64) string {
	r := min(4, w/2, h/2)
	return fmt.Sprintf(`M%.1f %.1fH%.1fQ%.1f %.1f %.1f %.1fV%.1fQ%.1f %.1f %.1f %.1fH%.1fZ`,
		x, cy-h/2, x+w-r, x+w, cy-h/2, x+w, cy-h/2+r, cy+h/2-r, x+w, cy+h/2, x+w-r, cy+h/2, x)
}

// Row chart geometry shared by rankChart and sevStackChart.
const (
	rowChartW      = 720.0
	rowChartRowH   = 26.0
	rowChartLabelW = 230.0
	rowChartValueW = 56.0
	rowChartBarH   = 14.0
	rowChartPlotW  = rowChartW - rowChartLabelW - rowChartValueW
	rowLabelRunes  = 32
)

// rowStart opens one row's group: its hover title, hit area, label and baseline.
func rowStart(sb *strings.Builder, i int, label, tip string) (cy float64) {
	y := rowChartRowH * float64(i)
	cy = y + rowChartRowH/2
	sb.WriteString(`<g class="viz-col">`)
	fmt.Fprintf(sb, `<title>%s</title>`, html.EscapeString(tip))
	fmt.Fprintf(sb, `<rect class="viz-hit" x="0" y="%.0f" width="%.0f" height="%.0f"/>`, y, rowChartW, rowChartRowH)
	fmt.Fprintf(sb, `<text class="viz-label" x="0" y="%.1f" dominant-baseline="middle">%s</text>`, cy, html.EscapeString(chartLabel(label, rowLabelRunes)))
	fmt.Fprintf(sb, `<line class="viz-grid base" x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f"/>`, rowChartLabelW, rowChartLabelW, y+4, y+rowChartRowH-4)
	return cy
}

// rowEnd writes the row's value at the right and closes its group.
func rowEnd(sb *strings.Builder, cy float64, v int) {
	fmt.Fprintf(sb, `<text class="viz-value" x="%.0f" y="%.1f" text-anchor="end" dominant-baseline="middle">%s</text>`, rowChartW, cy, humanNum(v))
	sb.WriteString(`</g>`)
}

// rankChart is a horizontal bar chart of rows, in the order given.
func rankChart(label string, rows []rankRow) template.HTML {
	if len(rows) == 0 {
		return ""
	}
	peak := 0
	for _, r := range rows {
		peak = max(peak, r.Value)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg class="viz" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, rowChartW, rowChartRowH*float64(len(rows)), html.EscapeString(label))
	for i, r := range rows {
		cy := rowStart(&sb, i, r.Label, r.Tip)
		if r.Value > 0 && peak > 0 {
			w := max(2, rowChartPlotW*float64(r.Value)/float64(peak))
			fmt.Fprintf(&sb, `<path class="viz-bar" d="%s"/>`, hbarEnd(rowChartLabelW, cy, w, rowChartBarH))
		}
		rowEnd(&sb, cy, r.Value)
	}
	sb.WriteString(`</svg>`)
	return template.HTML(sb.String())
}

// stackRow is one row of a chart of findings stacked by level.
type stackRow struct {
	Label string
	Sev   [5]int
}

func sevTotal(sev [5]int) int {
	t := 0
	for _, n := range sev {
		t += n
	}
	return t
}

// sevTip describes counts by level, for a hover title.
func sevTip(head string, sev [5]int) string {
	parts := make([]string, len(sev))
	for i, n := range sev {
		parts[i] = fmt.Sprintf("%s %d", settings.Severities[i].ID, n)
	}
	return fmt.Sprintf("%s: %d findings (%s)", head, sevTotal(sev), strings.Join(parts, " · "))
}

// lastLevel is the index of the last level with findings (-1 if none): the
// segment at the bar's data end, which gets the rounded corners.
func lastLevel(sev [5]int) int {
	last := -1
	for k, n := range sev {
		if n > 0 {
			last = k
		}
	}
	return last
}

// sevLegend names the severity ramp's colors. Identity is never by color
// alone: each mark's hover title and the table view name the levels too.
func sevLegend() string {
	var sb strings.Builder
	sb.WriteString(`<ul class="viz-legend">`)
	for i, s := range settings.Severities {
		fmt.Fprintf(&sb, `<li><span class="viz-swatch %s"></span>%s</li>`, sevColor(i), html.EscapeString(s.Label))
	}
	sb.WriteString(`</ul>`)
	return sb.String()
}

// sevStackChart is a horizontal bar per row with its findings stacked by
// level, P1 at the baseline.
func sevStackChart(label string, rows []stackRow) template.HTML {
	if len(rows) == 0 {
		return ""
	}
	peak := 0
	for _, r := range rows {
		peak = max(peak, sevTotal(r.Sev))
	}
	var sb strings.Builder
	sb.WriteString(sevLegend())
	fmt.Fprintf(&sb, `<svg class="viz" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, rowChartW, rowChartRowH*float64(len(rows)), html.EscapeString(label))
	for i, r := range rows {
		cy := rowStart(&sb, i, r.Label, sevTip(r.Label, r.Sev))
		last := lastLevel(r.Sev)
		x := rowChartLabelW
		for k, n := range r.Sev {
			if n == 0 || peak == 0 {
				continue
			}
			w := max(2, rowChartPlotW*float64(n)/float64(peak))
			cls := segClass(sevColor(k), w)
			if k == last {
				fmt.Fprintf(&sb, `<path class="%s" d="%s"/>`, cls, hbarEnd(x, cy, w, rowChartBarH))
			} else {
				fmt.Fprintf(&sb, `<rect class="%s" x="%.1f" y="%.1f" width="%.1f" height="%.0f"/>`, cls, x, cy-rowChartBarH/2, w, rowChartBarH)
			}
			x += w
		}
		rowEnd(&sb, cy, sevTotal(r.Sev))
	}
	sb.WriteString(`</svg>`)
	return template.HTML(sb.String())
}

// sevColor is a finding level's color class (level 0 is P1).
func sevColor(level int) string { return fmt.Sprintf("viz-s%d", level+1) }

// segClass is a stacked segment's classes: its color, and "short" when its
// length along the stack is under 4 units. The 2-unit gap stroke, centred on
// the edge, would then cover the whole segment, so a short one goes without.
func segClass(color string, length float64) string {
	cls := "viz-seg " + color
	if length < 4 {
		cls += " short"
	}
	return cls
}

// colSeg is one segment of a stacked column: a count and its color class.
type colSeg struct {
	N     int
	Color string
}

// sevDayChart is a column per day of the findings posted, stacked by level
// from P1 at the baseline.
func sevDayChart(days []store.DayStats) template.HTML {
	aria := fmt.Sprintf("Findings posted per %s by level, last %d days", periodUnit(days), daysCovered(days))
	return stackedDayChart(days, aria, sevLegend(), func(i int) ([]colSeg, string) {
		d := days[i]
		segs := make([]colSeg, len(d.Sev))
		for k, n := range d.Sev {
			segs[k] = colSeg{N: n, Color: sevColor(k)}
		}
		tip := sevTip(periodLabel(d), d.Sev)
		if d.Fixed > 0 {
			tip += fmt.Sprintf("; %d earlier findings confirmed fixed", d.Fixed)
		}
		return segs, tip
	})
}

// orgDayChart is a column per day (or week) of the reviews posted, stacked
// by organization in the order of its series.
func orgDayChart(o orgDays) template.HTML {
	aria := fmt.Sprintf("Reviews posted per %s by organization, last %d days", periodUnit(o.Days), daysCovered(o.Days))
	return stackedDayChart(o.Days, aria, orgLegend(o.Series), func(i int) ([]colSeg, string) {
		d := o.Days[i]
		segs := make([]colSeg, len(o.Series))
		var parts []string
		for k, s := range o.Series {
			segs[k] = colSeg{N: s.Posted[i], Color: s.Color()}
			if s.Posted[i] > 0 {
				parts = append(parts, fmt.Sprintf("%s: %d", s.Label, s.Posted[i]))
			}
		}
		tip := fmt.Sprintf("%s: %d reviews posted", periodLabel(d), d.Posted)
		if len(parts) > 0 {
			tip += " (" + strings.Join(parts, " · ") + ")"
		}
		tip += fmt.Sprintf("; %d findings (%d at P1–P2)", d.Findings, d.Serious)
		if d.Errors > 0 {
			tip += fmt.Sprintf(", %d failed", d.Errors)
		}
		return segs, tip
	})
}

// orgLegend names the organizations' colors. As with the levels, identity
// is never by color alone: the hover titles and the table name them too.
func orgLegend(series []orgSeries) string {
	var sb strings.Builder
	sb.WriteString(`<ul class="viz-legend">`)
	for _, s := range series {
		fmt.Fprintf(&sb, `<li><span class="viz-swatch %s"></span>%s</li>`, s.Color(), html.EscapeString(s.Label))
	}
	sb.WriteString(`</ul>`)
	return sb.String()
}

// stackedDayChart is a column per day (or week) of segments stacked from
// the baseline in the order col gives them, under legend. col gives one
// column's segments and its hover text; aria is the chart's accessible name.
func stackedDayChart(days []store.DayStats, aria, legend string, col func(int) ([]colSeg, string)) template.HTML {
	if len(days) == 0 {
		return ""
	}
	const (
		width, height    = 720.0, 180.0
		left, right, top = 34.0, 8.0, 10.0
		bottom           = 24.0
		plotW, plotH     = width - left - right, height - top - bottom
		gridLines        = 2
		barShare         = 0.62
	)
	segs := make([][]colSeg, len(days))
	tips := make([]string, len(days))
	peak := 0
	for i := range days {
		segs[i], tips[i] = col(i)
		total := 0
		for _, s := range segs[i] {
			total += s.N
		}
		peak = max(peak, total)
	}
	ymax := niceMax(peak)
	slot := plotW / float64(len(days))
	labelEvery := dayLabelEvery(len(days))
	var sb strings.Builder
	sb.WriteString(legend)
	thin := ""
	if slot*barShare < 4 {
		thin = " thin" // too narrow for the gap between segments
	}
	fmt.Fprintf(&sb, `<svg class="viz%s" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, thin, width, height, html.EscapeString(aria))
	for i := 0; i <= gridLines; i++ {
		v := ymax * i / gridLines
		y := top + plotH - plotH*float64(v)/float64(ymax)
		fmt.Fprintf(&sb, `<line class="viz-grid%s" x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f"/>`, map[bool]string{true: " base"}[i == 0], left, width-right, y, y)
		fmt.Fprintf(&sb, `<text class="viz-axis" x="%.0f" y="%.1f" text-anchor="end" dominant-baseline="middle">%d</text>`, left-6, y, v)
	}
	for i, d := range days {
		x := left + slot*float64(i)
		bw := slot * barShare
		bx := x + (slot-bw)/2
		sb.WriteString(`<g class="viz-col">`)
		fmt.Fprintf(&sb, `<title>%s</title>`, html.EscapeString(tips[i]))
		fmt.Fprintf(&sb, `<rect class="viz-hit" x="%.1f" y="%.0f" width="%.1f" height="%.0f"/>`, x, top, slot, plotH)
		last := -1 // the top segment, which gets the rounded corners
		for k, s := range segs[i] {
			if s.N > 0 {
				last = k
			}
		}
		base := top + plotH
		for k, s := range segs[i] {
			if s.N == 0 {
				continue
			}
			h := max(1.5, plotH*float64(s.N)/float64(ymax))
			cls := segClass(s.Color, h)
			if k == last {
				fmt.Fprintf(&sb, `<path class="%s" d="%s"/>`, cls, barTop(bx, base-h, bw, h))
			} else {
				fmt.Fprintf(&sb, `<rect class="%s" x="%.1f" y="%.1f" width="%.1f" height="%.1f"/>`, cls, bx, base-h, bw, h)
			}
			base -= h
		}
		sb.WriteString(`</g>`)
		if (len(days)-1-i)%labelEvery == 0 {
			fmt.Fprintf(&sb, `<text class="viz-axis" x="%.1f" y="%.0f" text-anchor="middle">%s</text>`, x+slot/2, height-6, d.Day.Format("2 Jan"))
		}
	}
	sb.WriteString(`</svg>`)
	return template.HTML(sb.String())
}

// periodLabel names a day chart's column: a day, or the week it starts.
func periodLabel(d store.DayStats) string {
	if d.Span > 1 {
		return "Week of " + d.Day.Format("Mon 2 Jan")
	}
	return d.Day.Format("Mon 2 Jan")
}

// periodUnit is what a day chart's columns are: days, or weeks. The last
// column is looked at, as the first may be a part week a day long.
func periodUnit(days []store.DayStats) string {
	if len(days) > 0 && days[len(days)-1].Span > 1 {
		return "week"
	}
	return "day"
}

// dayLabelEvery is how many columns apart a day chart's dates are, counted
// back from today: every day for two weeks or less, weekly up to four
// months, else monthly.
func dayLabelEvery(days int) int {
	switch {
	case days <= 14:
		return 1
	case days > 120:
		return 30
	}
	return 7
}

// ---------- chart rows for templates ----------

// repoNames names repositories in charts. Across organizations a name can
// be on two accounts' rows (a repository that moved, or an organization
// installed again), so a row whose organization isn't the name's owner, or
// whose name is on another row too, says whose it is.
func repoNames(repos []store.RepoStats) map[int]string {
	seen := map[string]int{}
	for _, r := range repos {
		seen[r.Repo]++
	}
	names := make(map[int]string, len(repos))
	for i, r := range repos {
		owner, _, _ := strings.Cut(r.Repo, "/")
		names[i] = r.Repo
		switch {
		case r.Org == "":
			names[i] += " (no organization)"
		case seen[r.Repo] > 1 || !strings.EqualFold(owner, r.Org):
			names[i] += " (" + r.Org + ")"
		}
	}
	return names
}

// repoReviewRows ranks repositories by reviews posted.
func repoReviewRows(repos []store.RepoStats, limit int) []rankRow {
	names := repoNames(repos)
	out := make([]rankRow, 0, min(limit, len(repos)))
	for i, r := range repos {
		if len(out) == limit {
			break
		}
		if r.Reviews == 0 {
			continue
		}
		out = append(out, rankRow{Label: names[i], Value: r.Reviews,
			Tip: fmt.Sprintf("%s: %d reviews posted on %d pull requests; %d findings (%d at P1–P2), %d confirmed fixed",
				names[i], r.Reviews, r.PRs, r.Findings, r.Serious, r.Fixed)})
	}
	return out
}

// repoSevRows are repositories' findings by level, the most found first.
func repoSevRows(repos []store.RepoStats, limit int) []stackRow {
	names := repoNames(repos)
	var idx []int
	for i, r := range repos {
		if r.Findings > 0 {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool { return repos[idx[a]].Findings > repos[idx[b]].Findings })
	out := make([]stackRow, 0, min(limit, len(idx)))
	for _, i := range idx[:min(limit, len(idx))] {
		out = append(out, stackRow{Label: names[i], Sev: repos[i].Sev})
	}
	return out
}

// modelName names a model on the metrics pages: an organization's own
// model, on the administrators' page, says whose it is.
func modelName(m store.ModelUsage, withSet bool) string {
	if withSet && m.ModelSet != 0 && m.SetOrg != "" {
		return m.Label + " (" + m.SetOrg + ")"
	}
	return m.Label
}

// modelRows rank models by the reviews sent to them.
func modelRows(ms []store.ModelUsage, withSet bool) []rankRow {
	out := make([]rankRow, 0, len(ms))
	for _, m := range ms {
		if m.Reviews == 0 {
			continue
		}
		name := modelName(m, withSet)
		out = append(out, rankRow{Label: name, Value: m.Reviews,
			Tip: fmt.Sprintf("%s: %d reviews sent, %d answered; %d replies; %d calls, %d failed", name, m.Reviews, m.Answered, m.Replies, m.Calls, m.Failed)})
	}
	return out
}

// orgSevRows are organizations' findings by level, the most found first, at
// most limit of them.
func orgSevRows(os []store.OrgMetrics, limit int) []stackRow {
	var idx []int
	for i, o := range os {
		if o.Findings > 0 {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool { return os[idx[a]].Findings > os[idx[b]].Findings })
	out := make([]stackRow, 0, min(limit, len(idx)))
	for _, i := range idx[:min(limit, len(idx))] {
		out = append(out, stackRow{Label: orgLabel(os[i].Login), Sev: os[i].Sev})
	}
	return out
}

// orgLabel names an organization, or the reviews linked to none.
func orgLabel(login string) string {
	if login == "" {
		return "(no organization)"
	}
	return login
}

// maxOrgSeries is how many organizations the reviews-per-day chart colors
// apart: the categorical palette's eight hues (viz-c1..viz-c8 in app.css).
const maxOrgSeries = 8

// orgSeries is one organization's part of the reviews-per-day chart, or the
// organizations past maxOrgSeries folded into one.
type orgSeries struct {
	Label  string
	Slot   int   // its color: 1..maxOrgSeries, or 0 for the folded organizations
	Posted []int // reviews posted in each of the chart's columns
}

// Color is the series' color class.
func (s orgSeries) Color() string { return fmt.Sprintf("viz-c%d", s.Slot) }

// orgDays is the reviews posted per day (or week) split by organization,
// for the charts of every organization. Each series' Posted lines up with Days.
type orgDays struct {
	Days   []store.DayStats
	Series []orgSeries
}

// splitByOrg splits each day's reviews posted by organization. days are
// ReviewsPerDay's entries for every organization and counts are
// PostedPerDayByOrg's for the same days; with weekly, both are summed by
// week, as byWeek does.
//
// Up to maxOrgSeries organizations get a series each; past that, the most
// active maxOrgSeries-1 do and the rest share the last. A login on two
// accounts (an organization installed again) is one series, named as the
// latest account spells it, as are the reviews linked to no stored
// organization. The series are in a fixed
// order, by account ID (when the organization was first seen), with no
// organization and then the folded ones last. An organization so keeps its
// color, and its place in each column, from one period to the next while
// the same organizations are charted, and the colors still go in palette
// order with none skipped, which is what keeps neighbors in a stack apart.
func splitByOrg(days []store.DayStats, counts []store.OrgDayPosted, weekly bool) orgDays {
	at := make(map[int64]int, len(days)) // each day's column
	for i, d := range days {
		at[d.Day.Unix()] = i
	}
	type group struct {
		login  string // as the latest account with it spells it
		first  int64  // the lowest account ID with the login
		latest int64  // the highest
		total  int
		posted []int
	}
	groups := map[string]*group{}
	var gs []*group
	for _, c := range counts {
		i, ok := at[c.Day.Unix()]
		if !ok || c.Posted == 0 {
			continue
		}
		key := strings.ToLower(c.Login)
		g := groups[key]
		if g == nil {
			g = &group{login: c.Login, first: c.AccountID, latest: c.AccountID, posted: make([]int, len(days))}
			groups[key] = g
			gs = append(gs, g)
		}
		if c.AccountID > g.latest {
			g.login, g.latest = c.Login, c.AccountID
		}
		g.first = min(g.first, c.AccountID)
		g.total += c.Posted
		g.posted[i] += c.Posted
	}
	// The most active first, to choose which get a series of their own.
	sort.SliceStable(gs, func(a, b int) bool {
		if gs[a].total != gs[b].total {
			return gs[a].total > gs[b].total
		}
		return gs[a].first < gs[b].first
	})
	var rest []*group
	if len(gs) > maxOrgSeries {
		gs, rest = gs[:maxOrgSeries-1], gs[maxOrgSeries-1:]
	}
	sort.SliceStable(gs, func(a, b int) bool {
		if (gs[a].login == "") != (gs[b].login == "") {
			return gs[b].login == ""
		}
		return gs[a].first < gs[b].first
	})
	out := orgDays{Days: days}
	for k, g := range gs {
		out.Series = append(out.Series, orgSeries{Label: orgLabel(g.login), Slot: k + 1, Posted: g.posted})
	}
	if len(rest) > 0 {
		other := orgSeries{Label: fmt.Sprintf("%d other organizations", len(rest)), Posted: make([]int, len(days))}
		for _, g := range rest {
			for i, n := range g.posted {
				other.Posted[i] += n
			}
		}
		out.Series = append(out.Series, other)
	}
	if weekly {
		spans := weekSpans(len(days))
		out.Days = byWeek(days)
		for k := range out.Series {
			out.Series[k].Posted = sumSpans(out.Series[k].Posted, spans)
		}
	}
	return out
}
