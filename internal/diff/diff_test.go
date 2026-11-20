package diff

import (
	"strings"
	"testing"
)

func TestParseAndRender(t *testing.T) {
	patch := "@@ -10,4 +10,5 @@ func f() {\n a\n-b\n+B\n+C\n c\n\\ No newline at end of file\n@@ -50 +51 @@\n-x\n+y"
	f := Parse("f.go", patch)
	for _, l := range []int{10, 11, 12, 13, 51} {
		if !f.Valid(l) {
			t.Errorf("line %d should be commentable", l)
		}
	}
	if f.Valid(14) || f.Valid(50) {
		t.Error("lines outside hunks must not be commentable")
	}
	if n, ok := f.Nearest(15, 3); !ok || n != 13 {
		t.Errorf("Nearest(15) = %d, %v", n, ok)
	}
	r := f.Hunks[0].Render()
	for _, want := range []string{"    10   a\n", "       - b\n", "    11 + B\n", "    13   c\n"} {
		if !strings.Contains(r, want) {
			t.Errorf("render missing %q:\n%s", want, r)
		}
	}
}
