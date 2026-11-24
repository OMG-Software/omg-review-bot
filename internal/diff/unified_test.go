package diff

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func TestUnifiedMatchesDiff(t *testing.T) {
	old := "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\nm\n"
	new := "a\nb\nC\nd\ne\nf\ng\nh\ni\nj\nk\nl\nm\nn\n"
	patch, added, removed := Unified(old, new, 3)
	want := "@@ -1,6 +1,6 @@\n a\n b\n-c\n+C\n d\n e\n f\n" +
		"@@ -11,3 +11,4 @@\n k\n l\n m\n+n"
	if patch != want || added != 2 || removed != 1 {
		t.Fatalf("got +%d -%d:\n%s\nwant +2 -1:\n%s", added, removed, patch, want)
	}
}

func TestUnifiedEdges(t *testing.T) {
	for _, c := range []struct{ name, old, new, want string }{
		{"added file", "", "x\ny\n", "@@ -0,0 +1,2 @@\n+x\n+y"},
		{"emptied file", "x\ny\n", "", "@@ -1,2 +0,0 @@\n-x\n-y"},
		{"unchanged", "x\ny\n", "x\ny\n", ""},
		{"line endings only", "x\r\ny\r\n", "x\ny\n", ""},
		{"insert at top", "b\nc\n", "a\nb\nc\n", "@@ -1,2 +1,3 @@\n+a\n b\n c"},
	} {
		if got, _, _ := Unified(c.old, c.new, 3); got != c.want {
			t.Errorf("%s: got\n%s\nwant\n%s", c.name, got, c.want)
		}
	}
}

// Applying the patch to the old text gives the new, its new-file line
// numbers are right, and for small inputs it's as short as a diff can be.
func TestUnifiedRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	gen := func(n, alphabet int) []string {
		ls := make([]string, n)
		for i := range ls {
			ls[i] = fmt.Sprint(r.Intn(alphabet))
		}
		return ls
	}
	for i := 0; i < 2000; i++ {
		a, b := gen(r.Intn(30), 4), gen(r.Intn(30), 4)
		patch, added, removed := Unified(join(a), join(b), r.Intn(4))
		checkApply(t, a, b, patch)
		if want := len(a) + len(b) - 2*lcs(a, b); added+removed != want {
			t.Fatalf("%v -> %v: %d edits, want %d\n%s", a, b, added+removed, want, patch)
		}
	}
	// Too many edits for a shortest diff: still a right one.
	a, b := gen(3000, 1000), gen(3000, 1000)
	patch, _, _ := Unified(join(a), join(b), 3)
	checkApply(t, a, b, patch)
}

// The common case for GitHub's cut: one big file with a few changes.
func TestUnifiedLargeFileFewChanges(t *testing.T) {
	a := make([]string, 20000)
	for i := range a {
		a[i] = fmt.Sprintf("line %d", i)
	}
	b := append([]string(nil), a...)
	b[100], b[15000] = "changed", "also changed"
	b = append(b[:9000], append([]string{"inserted"}, b[9000:]...)...)
	patch, added, removed := Unified(join(a), join(b), 3)
	checkApply(t, a, b, patch)
	if added != 3 || removed != 2 || strings.Count(patch, "@@ -") != 3 {
		t.Fatalf("+%d -%d in %d hunks, want +3 -2 in 3:\n%s", added, removed, strings.Count(patch, "@@ -"), patch)
	}
}

func join(ls []string) string {
	if len(ls) == 0 {
		return ""
	}
	return strings.Join(ls, "\n") + "\n"
}

func checkApply(t *testing.T, old, new []string, patch string) {
	t.Helper()
	var out []string
	i := 0
	for _, h := range Parse("f", patch).Hunks {
		var oldStart, oldN int
		if _, err := fmt.Sscanf(h.Header, "@@ -%d,%d", &oldStart, &oldN); err != nil {
			t.Fatalf("bad header %q", h.Header)
		}
		target := oldStart
		if oldN > 0 {
			target--
		}
		if target < i {
			t.Fatalf("hunk %q overlaps the one before", h.Header)
		}
		out, i = append(out, old[i:target]...), target
		for _, l := range h.Lines {
			if l.Kind != '-' && l.New != len(out)+1 {
				t.Fatalf("line %q numbered %d, want %d\n%s", l.Text, l.New, len(out)+1, patch)
			}
			switch l.Kind {
			case '+':
				out = append(out, l.Text)
			default:
				if i >= len(old) || old[i] != l.Text {
					t.Fatalf("hunk line %q doesn't match old line %d\n%s", l.Text, i+1, patch)
				}
				if l.Kind == ' ' {
					out = append(out, old[i])
				}
				i++
			}
		}
	}
	out = append(out, old[i:]...)
	if strings.Join(out, "\n") != strings.Join(new, "\n") || len(out) != len(new) {
		t.Fatalf("patch applied to %v gives %v, want %v\n%s", old, out, new, patch)
	}
}

func lcs(a, b []string) int {
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	return dp[0][0]
}
