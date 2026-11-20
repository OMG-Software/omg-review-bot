// Package diff parses GitHub unified-diff patches and renders them with
// explicit new-file line numbers so a model can name valid comment lines.
package diff

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type Line struct {
	Kind byte // '+', '-' or ' '
	Old  int
	New  int
	Text string
}

type Hunk struct {
	Header string
	Lines  []Line
}

type File struct {
	Path  string
	Hunks []Hunk
	valid map[int]bool // new-file lines that a review comment may target (RIGHT side)
}

var hunkRE = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// Parse parses the "patch" field of a GitHub PR file.
func Parse(path, patch string) *File {
	f := &File{Path: path, valid: map[int]bool{}}
	var cur *Hunk
	oldN, newN := 0, 0
	for _, raw := range strings.Split(patch, "\n") {
		raw = strings.TrimSuffix(raw, "\r")
		if m := hunkRE.FindStringSubmatch(raw); m != nil {
			f.Hunks = append(f.Hunks, Hunk{Header: raw})
			cur = &f.Hunks[len(f.Hunks)-1]
			oldN, _ = strconv.Atoi(m[1])
			newN, _ = strconv.Atoi(m[2])
			continue
		}
		if cur == nil || raw == "" {
			continue
		}
		switch raw[0] {
		case '+':
			cur.Lines = append(cur.Lines, Line{Kind: '+', New: newN, Text: raw[1:]})
			f.valid[newN] = true
			newN++
		case '-':
			cur.Lines = append(cur.Lines, Line{Kind: '-', Old: oldN, Text: raw[1:]})
			oldN++
		case ' ':
			cur.Lines = append(cur.Lines, Line{Kind: ' ', Old: oldN, New: newN, Text: raw[1:]})
			f.valid[newN] = true
			oldN++
			newN++
		}
		// '\' ("\ No newline at end of file") carries no line.
	}
	return f
}

func (f *File) Empty() bool { return len(f.valid) == 0 }

// Valid reports whether a RIGHT-side review comment can be placed on line.
func (f *File) Valid(line int) bool { return f.valid[line] }

// Nearest finds the closest commentable line within maxDist of line.
func (f *File) Nearest(line, maxDist int) (int, bool) {
	for d := 1; d <= maxDist; d++ {
		if f.valid[line-d] {
			return line - d, true
		}
		if f.valid[line+d] {
			return line + d, true
		}
	}
	return 0, false
}

// Render prints the hunk with the new-file line number in a left gutter:
//
//	42 + added line
//	43   context line
//	   - removed line (no new-file number: can't be commented on)
func (h *Hunk) Render() string {
	var sb strings.Builder
	sb.WriteString(h.Header)
	sb.WriteByte('\n')
	for _, l := range h.Lines {
		switch l.Kind {
		case '-':
			fmt.Fprintf(&sb, "%6s - %s\n", "", l.Text)
		case '+':
			fmt.Fprintf(&sb, "%6d + %s\n", l.New, l.Text)
		default:
			fmt.Fprintf(&sb, "%6d   %s\n", l.New, l.Text)
		}
	}
	return sb.String()
}
