package bot

import (
	"regexp"
	"strings"
)

// compileGlobs turns gitignore-style patterns into regexps:
//
//	*.lock      any file ending in .lock, in any directory
//	vendor/     anything under a "vendor" directory anywhere
//	docs/*.md   anchored at the repo root because it contains a slash
//	**/gen/**   "**" matches across directories
func compileGlobs(patterns []string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, p := range patterns {
		if re := globToRegexp(p); re != nil {
			out = append(out, re)
		}
	}
	return out
}

func globToRegexp(p string) *regexp.Regexp {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" || strings.HasPrefix(p, "#") {
		return nil
	}
	dirOnly := strings.HasSuffix(p, "/")
	p = strings.TrimSuffix(p, "/")
	anchored := strings.Contains(p, "/")
	p = strings.TrimPrefix(p, "/")

	var sb strings.Builder
	sb.WriteString("^")
	if !anchored || strings.HasPrefix(p, "**/") {
		sb.WriteString("(?:.*/)?")
		p = strings.TrimPrefix(p, "**/")
	}
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				if i+2 < len(p) && p[i+2] == '/' {
					sb.WriteString("(?:.*/)?")
					i += 2
				} else {
					sb.WriteString(".*")
					i++
				}
			} else {
				sb.WriteString("[^/]*")
			}
		case '?':
			sb.WriteString("[^/]")
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	if dirOnly {
		sb.WriteString("/.*")
	}
	sb.WriteString("$")
	re, err := regexp.Compile(sb.String())
	if err != nil {
		return nil
	}
	return re
}

func matchAny(res []*regexp.Regexp, path string) bool {
	for _, re := range res {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}
