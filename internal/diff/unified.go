package diff

import (
	"fmt"
	"strings"
)

// maxEdits is how many line edits Unified searches for a shortest diff
// before settling for one that replaces everything between the first and
// last changed lines. Memory grows with its square.
const maxEdits = 1000

type op struct {
	kind byte // '+', '-' or ' '
	a, b int  // old and new line index: the line's own, or where it would go
}

// Unified returns a patch of the change from old to new in the form of
// GitHub's "patch" field (hunks only, no file header), with context lines of
// context around each change, and how many lines it adds and removes. It is
// for files whose patch GitHub leaves out of a large pull request.
func Unified(old, new string, context int) (patch string, added, removed int) {
	a, b := lines(old), lines(new)
	ops := editScript(a, b)
	for _, o := range ops {
		switch o.kind {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	var sb strings.Builder
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(i-context, 0)
		end := i
		for {
			for end < len(ops) && ops[end].kind != ' ' {
				end++
			}
			same := end
			for same < len(ops) && ops[same].kind == ' ' {
				same++
			}
			// Changes this close share a hunk, as in diff -U.
			if same < len(ops) && same-end <= 2*context {
				end = same
				continue
			}
			end = min(end+context, len(ops))
			break
		}
		writeHunk(&sb, ops[start:end], a, b)
		i = end
	}
	return strings.TrimSuffix(sb.String(), "\n"), added, removed
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	ls := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i := range ls {
		ls[i] = strings.TrimSuffix(ls[i], "\r")
	}
	return ls
}

func writeHunk(sb *strings.Builder, ops []op, a, b []string) {
	oldN, newN := 0, 0
	for _, o := range ops {
		if o.kind != '+' {
			oldN++
		}
		if o.kind != '-' {
			newN++
		}
	}
	// A side with no lines in the hunk is numbered by the line before it.
	oldStart, newStart := ops[0].a, ops[0].b
	if oldN > 0 {
		oldStart++
	}
	if newN > 0 {
		newStart++
	}
	fmt.Fprintf(sb, "@@ -%d,%d +%d,%d @@\n", oldStart, oldN, newStart, newN)
	for _, o := range ops {
		switch o.kind {
		case '-':
			sb.WriteString("-" + a[o.a] + "\n")
		case '+':
			sb.WriteString("+" + b[o.b] + "\n")
		default:
			sb.WriteString(" " + b[o.b] + "\n")
		}
	}
}

// editScript returns every line of old and new as kept, removed or added,
// in order. Lines common to both ends are matched first; the rest is a
// shortest diff (Myers), or, when that would take more than maxEdits edits,
// all of old's rest removed and new's added.
func editScript(a, b []string) []op {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	ops := make([]op, 0, len(a)+len(b))
	for i := 0; i < pre; i++ {
		ops = append(ops, op{' ', i, i})
	}
	am, bm := a[pre:len(a)-suf], b[pre:len(b)-suf]
	mid, ok := myers(am, bm)
	if !ok {
		mid = mid[:0]
		for i := range am {
			mid = append(mid, op{'-', i, 0})
		}
		for j := range bm {
			mid = append(mid, op{'+', len(am), j})
		}
	}
	for _, o := range mid {
		o.a += pre
		o.b += pre
		ops = append(ops, o)
	}
	for i := 0; i < suf; i++ {
		ops = append(ops, op{' ', len(a) - suf + i, len(b) - suf + i})
	}
	return ops
}

// myers finds a shortest edit script from a to b, or reports false when it
// takes more than maxEdits edits.
func myers(a, b []string) ([]op, bool) {
	n, m := len(a), len(b)
	if n == 0 || m == 0 {
		ops := make([]op, 0, n+m)
		for i := range a {
			ops = append(ops, op{'-', i, 0})
		}
		for j := range b {
			ops = append(ops, op{'+', 0, j})
		}
		return ops, true
	}
	limit := n + m
	off := limit + 1
	v := make([]int, 2*limit+3) // v[off+k]: the furthest x reached on diagonal k
	// trace[d] holds v[off-d-1 .. off+d+1] as it was before step d.
	var trace [][]int
	for d := 0; d <= limit; d++ {
		if d > maxEdits {
			return nil, false
		}
		trace = append(trace, append([]int(nil), v[off-d-1:off+d+2]...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1] // down: a line of b added
			} else {
				x = v[off+k-1] + 1 // right: a line of a removed
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				return backtrack(trace, n, m), true
			}
		}
	}
	return nil, false // not reached: d = n+m always reaches the end
}

func backtrack(trace [][]int, n, m int) []op {
	var rev []op
	x, y := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		vd := trace[d]
		at := func(k int) int { return vd[k+d+1] }
		k := x - y
		prevK := k - 1
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			prevK = k + 1
		}
		prevX := at(prevK)
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			rev = append(rev, op{' ', x, y})
		}
		if d > 0 {
			if x == prevX {
				y--
				rev = append(rev, op{'+', x, y})
			} else {
				x--
				rev = append(rev, op{'-', x, y})
			}
		}
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}
