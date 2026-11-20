package web

import (
	"fmt"
	"html"
	"html/template"
	"strings"

	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// Charts are inline SVG built on the server: the Content-Security-Policy
// forbids inline styles and third-party scripts, and SVG geometry attributes
// are neither. Colors come from CSS classes (app.css), so they follow the
// light/dark theme. Each mark has a <title>, which browsers show on hover.

// niceMax rounds a maximum up to 1, 2 or 5 times a power of ten.
func niceMax(v int) int {
	if v <= 4 {
		return 4
	}
	p := 1
	for p*10 <= v {
		p *= 10
	}
	for _, m := range []int{1, 2, 5, 10} {
		if m*p >= v {
			return m * p
		}
	}
	return 10 * p
}

// barTop draws a bar from the baseline with 4px rounded top corners.
func barTop(x, y, w, h float64) string {
	r := min(4, w/2, h)
	return fmt.Sprintf(`M%.1f %.1fV%.1fQ%.1f %.1f %.1f %.1fH%.1fQ%.1f %.1f %.1f %.1fV%.1fZ`,
		x, y+h, y+r, x, y, x+r, y, x+w-r, x+w, y, x+w, y+r, y+h)
}

// dayChart is a column chart of posted reviews per day.
func dayChart(days []store.DayStats) template.HTML {
	if len(days) == 0 {
		return ""
	}
	const (
		width, height       = 720.0, 180.0
		left, right, top    = 34.0, 8.0, 10.0
		bottom              = 24.0
		plotW, plotH        = width - left - right, height - top - bottom
		gridLines           = 2
		barShare            = 0.62
		minVisibleBarHeight = 2.0
	)
	peak := 0
	for _, d := range days {
		peak = max(peak, d.Posted)
	}
	ymax := niceMax(peak)
	slot := plotW / float64(len(days))
	labelEvery := 7 // a date a week, counted back from today
	if len(days) <= 14 {
		labelEvery = 1 // few enough to date every bar
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg class="viz" viewBox="0 0 %.0f %.0f" role="img" aria-label="Reviews posted per day, last %d days">`, width, height, len(days))
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
		tip := fmt.Sprintf("%s: %d reviews posted, %d findings (%d at P1–P2)", d.Day.Format("Mon 2 Jan"), d.Posted, d.Findings, d.Serious)
		if d.Errors > 0 {
			tip += fmt.Sprintf(", %d failed", d.Errors)
		}
		sb.WriteString(`<g class="viz-col">`)
		fmt.Fprintf(&sb, `<title>%s</title>`, html.EscapeString(tip))
		fmt.Fprintf(&sb, `<rect class="viz-hit" x="%.1f" y="%.0f" width="%.1f" height="%.0f"/>`, x, top, slot, plotH)
		if d.Posted > 0 {
			h := max(minVisibleBarHeight, plotH*float64(d.Posted)/float64(ymax))
			fmt.Fprintf(&sb, `<path class="viz-bar" d="%s"/>`, barTop(bx, top+plotH-h, bw, h))
		}
		sb.WriteString(`</g>`)
		if (len(days)-1-i)%labelEvery == 0 {
			fmt.Fprintf(&sb, `<text class="viz-axis" x="%.1f" y="%.0f" text-anchor="middle">%s</text>`, x+slot/2, height-6, d.Day.Format("2 Jan"))
		}
	}
	sb.WriteString(`</svg>`)
	return template.HTML(sb.String())
}

// sevChart is a horizontal bar chart of findings by level, P1 at the top.
func sevChart(sev [5]int) template.HTML {
	const (
		width        = 520.0
		rowH         = 26.0
		labelW       = 150.0
		valueW       = 44.0
		barH         = 14.0
		plotW        = width - labelW - valueW
		minBarLength = 2.0
	)
	peak := 0
	for _, n := range sev {
		peak = max(peak, n)
	}
	height := rowH * float64(len(sev))
	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg class="viz" viewBox="0 0 %.0f %.0f" role="img" aria-label="Findings posted by level">`, width, height)
	for i, n := range sev {
		lv := settings.Severities[i]
		y := rowH * float64(i)
		cy := y + rowH/2
		sb.WriteString(`<g class="viz-col">`)
		fmt.Fprintf(&sb, `<title>%s</title>`, html.EscapeString(fmt.Sprintf("%s: %d findings — %s", lv.Label, n, lv.Meaning)))
		fmt.Fprintf(&sb, `<rect class="viz-hit" x="0" y="%.0f" width="%.0f" height="%.0f"/>`, y, width, rowH)
		fmt.Fprintf(&sb, `<text class="viz-label" x="0" y="%.1f" dominant-baseline="middle">%s</text>`, cy, html.EscapeString(lv.Label))
		fmt.Fprintf(&sb, `<line class="viz-grid base" x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f"/>`, labelW, labelW, y+4, y+rowH-4)
		if n > 0 && peak > 0 {
			w := max(minBarLength, plotW*float64(n)/float64(peak))
			r := min(4, w/2)
			// Rounded at the data end only, square at the baseline.
			fmt.Fprintf(&sb, `<path class="viz-bar" d="M%.1f %.1fH%.1fQ%.1f %.1f %.1f %.1fV%.1fQ%.1f %.1f %.1f %.1fH%.1fZ"/>`,
				labelW, cy-barH/2, labelW+w-r, labelW+w, cy-barH/2, labelW+w, cy-barH/2+r,
				cy+barH/2-r, labelW+w, cy+barH/2, labelW+w-r, cy+barH/2, labelW)
		}
		fmt.Fprintf(&sb, `<text class="viz-value" x="%.0f" y="%.1f" text-anchor="end" dominant-baseline="middle">%d</text>`, width, cy, n)
		sb.WriteString(`</g>`)
	}
	sb.WriteString(`</svg>`)
	return template.HTML(sb.String())
}
