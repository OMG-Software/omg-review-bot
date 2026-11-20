package skill

import (
	"path"
	"strings"
	"unicode"
)

// Reference files inside a folder with one of these names are language
// specific: they are only sent to the model when the PR touches that language.
var langDirs = map[string]bool{"languages": true, "language": true, "langs": true, "lang": true}

// IsLanguageRef reports whether a reference file (slash path relative to the
// skill root) lives under a languages/ folder.
func IsLanguageRef(p string) bool {
	parts := strings.Split(strings.ToLower(p), "/")
	for _, d := range parts[:len(parts)-1] {
		if langDirs[d] {
			return true
		}
	}
	return false
}

var (
	infra = []string{"infrastructure", "infra", "iac"}

	extTokens = map[string][]string{
		".c": {"c", "cpp"}, ".h": {"c", "cpp"},
		".cc": {"cpp", "c"}, ".cpp": {"cpp", "c"}, ".cxx": {"cpp", "c"}, ".c++": {"cpp", "c"},
		".hpp": {"cpp", "c"}, ".hh": {"cpp", "c"}, ".hxx": {"cpp", "c"}, ".ipp": {"cpp", "c"}, ".inl": {"cpp", "c"}, ".cppm": {"cpp", "c"},
		".cs": {"csharp", "cs", "dotnet"}, ".csx": {"csharp", "cs", "dotnet"}, ".csproj": {"csharp", "dotnet"},
		".sln": {"csharp", "dotnet"}, ".razor": {"csharp", "dotnet"}, ".cshtml": {"csharp", "dotnet"},
		".fs": {"fsharp", "dotnet"}, ".vb": {"vb", "dotnet"},
		".go":   {"go", "golang"},
		".java": {"java"}, ".kt": {"kotlin", "kt"}, ".kts": {"kotlin", "kt"}, ".gradle": {"java", "kotlin", "gradle"},
		".groovy": {"groovy", "java"}, ".scala": {"scala"},
		".js": {"javascript", "js"}, ".mjs": {"javascript", "js"}, ".cjs": {"javascript", "js"}, ".jsx": {"javascript", "js", "react"},
		".ts": {"typescript", "ts"}, ".mts": {"typescript", "ts"}, ".cts": {"typescript", "ts"}, ".tsx": {"typescript", "ts", "react"},
		".vue": {"javascript", "typescript", "vue"}, ".svelte": {"javascript", "typescript", "svelte"},
		".php": {"php"},
		".py":  {"python", "py"}, ".pyi": {"python", "py"},
		".rb": {"ruby", "rb"}, ".rake": {"ruby"}, ".gemspec": {"ruby"}, ".erb": {"ruby"},
		".rs": {"rust", "rs"},
		".sh": {"shell", "bash", "sh"}, ".bash": {"shell", "bash", "sh"}, ".zsh": {"shell", "zsh"}, ".ksh": {"shell"},
		".ps1": {"powershell", "shell"}, ".psm1": {"powershell", "shell"}, ".bat": {"batch", "shell"}, ".cmd": {"batch", "shell"},
		".sql": {"sql"}, ".psql": {"sql", "postgres"},
		".swift": {"swift"}, ".m": {"objc", "objective"}, ".mm": {"objc", "objective", "cpp"},
		".dart": {"dart", "flutter"}, ".lua": {"lua"}, ".pl": {"perl"}, ".pm": {"perl"},
		".ex": {"elixir"}, ".exs": {"elixir"}, ".erl": {"erlang"}, ".hs": {"haskell"}, ".clj": {"clojure"},
		".html": {"html", "web"}, ".css": {"css", "web"}, ".scss": {"css", "web"},
		".yml": append([]string{"yaml"}, infra...), ".yaml": append([]string{"yaml"}, infra...),
		".tf": append([]string{"terraform"}, infra...), ".tfvars": append([]string{"terraform"}, infra...),
		".hcl": append([]string{"terraform", "hcl"}, infra...), ".bicep": append([]string{"bicep", "azure"}, infra...),
		".dockerfile": append([]string{"docker"}, infra...),
		".cmake":      {"cmake", "build"}, ".mk": {"make", "build"}, ".spec": {"rpm", "packaging"},
		".proto": {"protobuf", "proto"},
	}

	nameTokens = map[string][]string{
		"dockerfile":         append([]string{"docker"}, infra...),
		"containerfile":      append([]string{"docker"}, infra...),
		"docker-compose.yml": append([]string{"docker"}, infra...),
		"makefile":           {"make", "build"},
		"cmakelists.txt":     {"cmake", "build", "cpp", "c"},
		"go.mod":             {"go", "golang"},
		"gemfile":            {"ruby"},
		"rakefile":           {"ruby"},
		"cargo.toml":         {"rust"},
		"package.json":       {"javascript", "typescript", "node"},
		"pom.xml":            {"java"},
		"pyproject.toml":     {"python"},
		"requirements.txt":   {"python"},
		"jenkinsfile":        append([]string{"ci", "groovy"}, infra...),
		"vagrantfile":        append([]string{"ruby"}, infra...),
	}
)

// LanguageTokens describes the languages of the given file paths as a set of
// lowercase tokens ("cpp", "shell", "infrastructure", ...).
func LanguageTokens(paths []string) map[string]bool {
	out := map[string]bool{}
	add := func(ts []string) {
		for _, t := range ts {
			out[t] = true
		}
	}
	for _, p := range paths {
		lp := strings.ToLower(strings.ReplaceAll(p, "\\", "/"))
		base := path.Base(lp)
		add(nameTokens[base])
		add(extTokens[path.Ext(base)])
		if strings.HasPrefix(base, "dockerfile") || strings.HasPrefix(base, "containerfile") {
			add(nameTokens["dockerfile"])
		}
		for _, ci := range []string{".github/workflows/", ".gitlab-ci", ".circleci/", "azure-pipelines", ".buildkite/", ".drone"} {
			if strings.Contains("/"+lp, "/"+ci) {
				add(append([]string{"ci"}, infra...))
			}
		}
		for _, k := range []string{"k8s/", "kubernetes/", "helm/", "charts/", "terraform/", "ansible/", "deploy/"} {
			if strings.Contains("/"+lp, "/"+k) {
				add(infra)
			}
		}
	}
	return out
}

// refMatches reports whether a language reference's file name names one of
// the tokens: "references/languages/c-cpp.md" matches "c" or "cpp".
func refMatches(refPath string, tokens map[string]bool) bool {
	base := path.Base(strings.ToLower(refPath))
	stem := strings.TrimSuffix(base, path.Ext(base))
	for _, w := range strings.FieldsFunc(stem, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if tokens[w] {
			return true
		}
	}
	return false
}
