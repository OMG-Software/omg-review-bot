// Package skill loads a review "skill": a folder containing SKILL.md (YAML
// frontmatter + markdown instructions) plus optional reference files.
package skill

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/OMG-Software/omg-review-bot/internal/textutil"
)

type File struct {
	Path    string
	Content string
}

type Skill struct {
	Name        string
	Description string
	Body        string
	References  []File
	Dir         string
	Builtin     bool
}

const (
	maxRefFile    = 256 << 10
	maxZipTotal   = 20 << 20
	skillFileName = "SKILL.md"
)

var refExts = map[string]bool{".md": true, ".markdown": true, ".txt": true}

// Load reads the skill in dir.
func Load(dir string) (*Skill, error) {
	file, err := findSkillFile(dir)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	s := Parse(string(data))
	s.Dir = dir
	if s.Name == "" {
		s.Name = filepath.Base(dir)
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if p == file || !refExts[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > maxRefFile {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		s.References = append(s.References, File{Path: filepath.ToSlash(rel), Content: normalize(string(b))})
		return nil
	})
	sort.Slice(s.References, func(i, j int) bool { return s.References[i].Path < s.References[j].Path })
	return s, nil
}

// ReadRaw returns the raw SKILL.md text in dir.
func ReadRaw(dir string) (string, error) {
	file, err := findSkillFile(dir)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(file)
	return string(b), err
}

func findSkillFile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(e.Name(), skillFileName) {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("no %s in %s", skillFileName, dir)
}

func normalize(s string) string {
	s = strings.TrimPrefix(s, string(rune(0xFEFF))) // UTF-8 byte-order mark
	return strings.ReplaceAll(s, "\r\n", "\n")
}

// Parse splits SKILL.md text into frontmatter fields and body.
func Parse(text string) *Skill {
	s := &Skill{}
	text = normalize(text)
	if strings.HasPrefix(text, "---\n") {
		rest := text[4:]
		if end := strings.Index(rest, "\n---"); end >= 0 {
			var meta map[string]any
			if yaml.Unmarshal([]byte(rest[:end]), &meta) == nil {
				s.Name, _ = meta["name"].(string)
				s.Description, _ = meta["description"].(string)
			}
			// Unquoted YAML treats " #" as a comment start, which truncates
			// descriptions like `... "review PR #45" ...`. Like Claude Code,
			// prefer the whole raw line for single-line plain values.
			s.Name = rawScalar(rest[:end], "name", s.Name)
			s.Description = rawScalar(rest[:end], "description", s.Description)
			after := rest[end+4:]
			if i := strings.IndexByte(after, '\n'); i >= 0 {
				after = after[i+1:]
			} else {
				after = ""
			}
			text = after
		}
	}
	s.Body = strings.TrimSpace(text)
	return s
}

// rawScalar returns the full text of a single-line, unquoted `key: value`
// frontmatter entry, or parsed when the entry is quoted, multi-line or absent.
func rawScalar(frontmatter, key, parsed string) string {
	for _, line := range strings.Split(frontmatter, "\n") {
		v, ok := strings.CutPrefix(line, key+":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if v == "" || strings.ContainsAny(v[:1], `"'|>[{`) || !strings.HasPrefix(v, parsed) {
			return parsed
		}
		return v
	}
	return parsed
}

// Prompt renders the skill as system-prompt text within maxChars.
//
// Claude-style skills load reference files on demand; a plain chat model
// can't, so references are inlined. General references are always included;
// those under a languages/ folder only when one of paths (the files being
// reviewed) is in that language. A nil paths includes every reference.
// References that don't fit the budget are skipped.
func (s *Skill) Prompt(paths []string, maxChars int) (text string, included []string, truncated bool) {
	var tokens map[string]bool
	if paths != nil {
		tokens = LanguageTokens(paths)
	}
	var general, langs []File
	for _, r := range s.References {
		switch {
		case !IsLanguageRef(r.Path):
			general = append(general, r)
		case tokens == nil || refMatches(r.Path, tokens):
			langs = append(langs, r)
		}
	}
	var sb strings.Builder
	sb.WriteString(s.Body)
	for _, r := range append(general, langs...) {
		chunk := "\n\n---\n## Reference: " + r.Path + "\n\n" + strings.TrimSpace(r.Content)
		if maxChars > 0 && sb.Len()+len(chunk) > maxChars {
			truncated = true
			continue
		}
		sb.WriteString(chunk)
		included = append(included, r.Path)
	}
	out := sb.String()
	if maxChars > 0 && len(out) > maxChars {
		out = textutil.Truncate(out, maxChars) + "\n(guidelines truncated to fit the model context)"
		truncated = true
	}
	return out, included, truncated
}

func refSize(r File) int { return len(r.Content) + len(r.Path) + 20 }

// Size returns the character size of body + every reference.
func (s *Skill) Size() int {
	n := len(s.Body)
	for _, r := range s.References {
		n += refSize(r)
	}
	return n
}

// BaseSize returns the size always sent: body + non-language references.
func (s *Skill) BaseSize() int {
	n := len(s.Body)
	for _, r := range s.References {
		if !IsLanguageRef(r.Path) {
			n += refSize(r)
		}
	}
	return n
}

// RefSize is the prompt size of one reference (for display).
func (f File) Size() int { return refSize(f) }

// IsLanguage reports whether this reference is loaded per language.
func (f File) IsLanguage() bool { return IsLanguageRef(f.Path) }

// WriteSkillFile writes SKILL.md content into dir (creating it).
func WriteSkillFile(dir, content string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	target := filepath.Join(dir, skillFileName)
	if existing, err := findSkillFile(dir); err == nil {
		target = existing
	}
	return os.WriteFile(target, []byte(normalize(content)), 0o644)
}

// InstallZip replaces the skill in dest with the contents of a zip archive.
// The folder containing SKILL.md becomes the skill root, and paths that would
// escape dest (zip-slip) are rejected.
func InstallZip(data []byte, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("not a valid zip: %w", err)
	}
	root, found := "", false
	for _, f := range zr.File {
		name := path.Clean(strings.ReplaceAll(f.Name, "\\", "/"))
		if strings.EqualFold(path.Base(name), skillFileName) {
			if d := path.Dir(name); !found || len(d) < len(root) {
				root, found = d, true
			}
		}
	}
	if !found {
		return fmt.Errorf("zip does not contain a %s", skillFileName)
	}
	dest = filepath.Clean(dest)
	tmp := dest + ".new"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := path.Clean(strings.ReplaceAll(f.Name, "\\", "/"))
		rel := name
		if root != "." {
			if !strings.HasPrefix(name, root+"/") {
				continue
			}
			rel = name[len(root)+1:]
		}
		if rel == "" || rel == "." || strings.HasPrefix(rel, "../") || path.IsAbs(rel) || strings.Contains(rel, ":") {
			continue
		}
		target := filepath.Join(tmp, filepath.FromSlash(rel))
		if !strings.HasPrefix(target, tmp+string(os.PathSeparator)) {
			continue
		}
		total += int64(f.UncompressedSize64)
		if total > maxZipTotal {
			_ = os.RemoveAll(tmp)
			return errors.New("skill archive is larger than 20 MB uncompressed")
		}
		if err := extract(f, target); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
	}
	return swapDir(tmp, dest)
}

func extract(f *zip.File, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(target)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(rc, maxZipTotal)); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func swapDir(tmp, dest string) error {
	old := dest + ".old"
	_ = os.RemoveAll(old)
	if _, err := os.Stat(dest); err == nil {
		if err := os.Rename(dest, old); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Rename(old, dest)
		return err
	}
	_ = os.RemoveAll(old)
	return nil
}

// Default is used until a skill has been installed.
func Default() *Skill {
	s := Parse(defaultSkill)
	s.Builtin = true
	return s
}

// DefaultText is the built-in SKILL.md, shown as a starting point in the editor.
func DefaultText() string { return defaultSkill }

const defaultSkill = `---
name: default-code-review
description: Built-in general-purpose code review guidelines (used until you install your own skill).
---

# Code review guidelines

Review the change like a careful senior engineer on this team. Focus on what matters:

1. **Correctness**: logic errors, off-by-one, nil/null handling, wrong conditions, broken edge cases, race conditions.
2. **Security**: injection (SQL, shell, path), missing authz/authn checks, secrets in code, unsafe deserialization, SSRF, XSS.
3. **Reliability**: unhandled errors, resource leaks (files, connections, goroutines), missing timeouts, retries without backoff.
4. **Performance**: N+1 queries, work inside hot loops, unbounded memory growth, needless allocations on hot paths.
5. **API & compatibility**: breaking changes to public interfaces, schema or config changes without migration.
6. **Tests**: new behaviour without tests, tests that don't actually assert anything.

Do not comment on formatting or style that a linter or formatter would handle.
Prefer fewer, high-confidence comments over many speculative ones.
When you suggest a fix, show the corrected code.
`
