package skill

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeZip(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestInstallZipAndLoad(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "skill")
	data := makeZip(t, map[string]string{
		"my-skill/SKILL.md":            "---\r\nname: go-review\r\ndescription: Go rules\r\n---\r\n# Rules\r\nCheck errors.",
		"my-skill/references/style.md": "Prefer early returns.",
		"my-skill/scripts/run.sh":      "echo not a reference",
		"my-skill/../../escape.md":     "zip slip!",
		"other/README.md":              "outside the skill root",
	})
	if err := InstallZip(data, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "escape.md")); err == nil {
		t.Fatal("zip-slip path was extracted outside the destination")
	}
	s, err := Load(dest)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "go-review" || s.Description != "Go rules" || !strings.HasPrefix(s.Body, "# Rules") {
		t.Errorf("parsed skill = %+v", s)
	}
	if len(s.References) != 1 || s.References[0].Path != "references/style.md" {
		t.Errorf("references = %+v", s.References)
	}
	p, _, truncated := s.Prompt(nil, 10_000)
	if truncated || !strings.Contains(p, "Prefer early returns.") {
		t.Errorf("prompt missing reference: %q", p)
	}
	if p, _, truncated := s.Prompt(nil, 30); !truncated || strings.Contains(p, "Prefer early returns") {
		t.Errorf("small budget should drop references: %q", p)
	}
}

func TestInstallZipRequiresSkillFile(t *testing.T) {
	if err := InstallZip(makeZip(t, map[string]string{"README.md": "x"}), filepath.Join(t.TempDir(), "s")); err == nil {
		t.Fatal("expected error without SKILL.md")
	}
}

func TestDescriptionWithHash(t *testing.T) {
	s := Parse("---\nname: r\ndescription: Use for \"review PR #45\" and more\n---\nbody")
	if s.Description != `Use for "review PR #45" and more` {
		t.Errorf("description = %q", s.Description)
	}
	q := Parse("---\nname: r\ndescription: \"quoted # kept\"\n---\nbody")
	if q.Description != "quoted # kept" {
		t.Errorf("quoted description = %q", q.Description)
	}
}

func TestLanguageReferenceSelection(t *testing.T) {
	s := &Skill{Body: "rules", References: []File{
		{Path: "references/languages/c-cpp.md", Content: "CPP"},
		{Path: "references/languages/csharp-dotnet.md", Content: "CS"},
		{Path: "references/languages/infrastructure.md", Content: "INFRA"},
		{Path: "references/languages/java-kotlin.md", Content: "JAVA"},
		{Path: "references/languages/javascript-typescript.md", Content: "JS"},
		{Path: "references/languages/shell.md", Content: "SH"},
		{Path: "references/review-angles.md", Content: "ANGLES"},
	}}
	cases := map[string]struct {
		paths []string
		want  []string
	}{
		"cpp+ci+shell": {[]string{"src/ranges.cpp", "include/x.h", ".github/workflows/rpm.yml", "scripts/reject-unsafe.sh"},
			[]string{"references/review-angles.md", "references/languages/c-cpp.md", "references/languages/infrastructure.md", "references/languages/shell.md"}},
		"java is not javascript": {[]string{"App.java"}, []string{"references/review-angles.md", "references/languages/java-kotlin.md"}},
		"tsx":                    {[]string{"ui/App.tsx"}, []string{"references/review-angles.md", "references/languages/javascript-typescript.md"}},
		"csharp is not c":        {[]string{"Program.cs"}, []string{"references/review-angles.md", "references/languages/csharp-dotnet.md"}},
		"dockerfile":             {[]string{"build/Dockerfile.rpm"}, []string{"references/review-angles.md", "references/languages/infrastructure.md"}},
		"no known language":      {[]string{"README.md"}, []string{"references/review-angles.md"}},
	}
	for name, c := range cases {
		_, got, _ := s.Prompt(c.paths, 0)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: got %v want %v", name, got, c.want)
		}
	}
	if _, all, _ := s.Prompt(nil, 0); len(all) != len(s.References) {
		t.Errorf("nil paths should include every reference, got %v", all)
	}
}

// TestInstallGitHubZip installs a skill the way the README describes: the
// repository's "Download ZIP" from GitHub, uploaded as is.
func TestInstallGitHubZip(t *testing.T) {
	root := "code-change-review-skill-main/"
	skill := root + "skills/code-change-review/"
	data := makeZip(t, map[string]string{
		root + "README.md":                    "repo readme, not part of the skill",
		root + "LICENSE":                      "MIT",
		skill + "SKILL.md":                    "---\nname: code-change-review\ndescription: Review a change\n---\n# Code Change Review",
		skill + "references/reporting.md":     "Tags first.",
		skill + "references/languages/sql.md": "Migrations need a down step.",
		skill + "scripts/detect_languages.py": "print('not a reference')",
	})
	dest := filepath.Join(t.TempDir(), "skill")
	if err := InstallZip(data, dest); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dest)
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, r := range s.References {
		refs = append(refs, r.Path)
	}
	if s.Name != "code-change-review" || strings.Join(refs, ",") != "references/languages/sql.md,references/reporting.md" {
		t.Errorf("installed %q with references %v", s.Name, refs)
	}
	// A migration in any language gets the SQL guidance.
	if _, sent, _ := s.Prompt([]string{"db/migrations/20260101_add_users.rb"}, 0); strings.Join(sent, ",") != "references/reporting.md,references/languages/sql.md" {
		t.Errorf("references sent for a migration = %v", sent)
	}
}

func TestZipSkillRoot(t *testing.T) {
	root := func(files map[string]string) (string, error) {
		zr, err := zip.NewReader(bytes.NewReader(makeZip(t, files)), int64(len(makeZip(t, files))))
		if err != nil {
			t.Fatal(err)
		}
		return zipSkillRoot(zr.File)
	}
	// Shallowest by folder depth, not by the length of the path string.
	if r, err := root(map[string]string{
		"a-very-long-repository-name/SKILL.md": "x",
		"x/y/SKILL.md":                         "nested example",
	}); err != nil || r != "a-very-long-repository-name" {
		t.Errorf("root = %q, %v", r, err)
	}
	if r, err := root(map[string]string{"SKILL.md": "x", "__MACOSX/._SKILL.md": "junk"}); err != nil || r != "." {
		t.Errorf("top-level root = %q, %v", r, err)
	}
	if _, err := root(map[string]string{"repo/skills/a/SKILL.md": "a", "repo/skills/b/SKILL.md": "b"}); err == nil ||
		!strings.Contains(err.Error(), "2 skills (repo/skills/a, repo/skills/b)") {
		t.Errorf("two skills at the same depth: %v", err)
	}
	if _, err := root(map[string]string{"README.md": "no skill"}); err == nil {
		t.Error("zip without SKILL.md accepted")
	}
}
