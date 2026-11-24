package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/llm"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/textutil"
)

// The model's read-only view of the repository during a review, so the
// skill's context steps (read around each change, open definitions, find
// callers) and its verification step can run. Every call reads the PR's own
// repository through the installation's API: nothing runs, nothing is
// written, and nothing leaves for another host.
const (
	maxToolSteps      = 16      // model turns per question before it must answer
	maxSearchFiles    = 300     // files one search_code call may read
	maxSearchHits     = 80      // matching lines one search_code call returns
	maxListPaths      = 400     // paths one list_files call returns
	maxToolFileBytes  = 1 << 20 // larger files aren't searched
	searchConcurrency = 8
)

type repoTools struct {
	cl         *github.Client
	repo       string
	head, base string
	ignore     []*regexp.Regexp // the review's ignore patterns: search skips them

	mu       sync.Mutex
	disabled bool // the model or server refused tools: not offered again
	cache    *toolCache
}

// toolCache is what the tools have read, shared by every model in a review.
type toolCache struct {
	mu    sync.Mutex
	files map[string]toolFile
	trees map[string]toolTree
}

// fork returns tools for another model in the same review: they share what
// has been read, but each model's server can refuse tools on its own.
func (rt *repoTools) fork() *repoTools {
	return &repoTools{cl: rt.cl, repo: rt.repo, head: rt.head, base: rt.base, ignore: rt.ignore,
		disabled: !rt.on(), cache: rt.cache}
}

type toolFile struct {
	content string
	err     error
}

type toolTree struct {
	paths     []string
	truncated bool
	err       error
}

func newRepoTools(cl *github.Client, st settings.Settings, repo, head, base string) *repoTools {
	if base == "" {
		base = head
	}
	return &repoTools{cl: cl, repo: repo, head: head, base: base, ignore: compileGlobs(st.Review.IgnorePatterns),
		cache: &toolCache{files: map[string]toolFile{}, trees: map[string]toolTree{}}}
}

// on reports whether tools are offered (rt may be nil: none).
func (rt *repoTools) on() bool {
	if rt == nil {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return !rt.disabled
}

func (rt *repoTools) disable() {
	rt.mu.Lock()
	rt.disabled = true
	rt.mu.Unlock()
}

var versionParam = map[string]any{"type": "string", "enum": []string{"head", "base"},
	"description": `"head" (default) is the pull request's latest commit; "base" is the commit it branches from, before the change.`}

func (rt *repoTools) defs() []llm.Tool {
	return []llm.Tool{
		{Name: "read_file", Description: "Read a file of the repository, with line numbers. Use it to see the code around a change, the definitions the changed code uses, and the code a finding depends on. Large files are cut short: ask for a line range.",
			Parameters: map[string]any{"type": "object", "required": []string{"path"}, "properties": map[string]any{
				"path":       map[string]any{"type": "string", "description": "File path from the repository root."},
				"start_line": map[string]any{"type": "integer", "description": "First line to show (default 1)."},
				"end_line":   map[string]any{"type": "integer", "description": "Last line to show (default: as much as fits)."},
				"version":    versionParam,
			}}},
		{Name: "search_code", Description: "Search the repository's files for a regular expression (RE2 syntax), like grep. Use it to find callers and other uses of changed functions, types, config keys and endpoints. Returns matching lines as path:line: text.",
			Parameters: map[string]any{"type": "object", "required": []string{"pattern"}, "properties": map[string]any{
				"pattern":   map[string]any{"type": "string", "description": "RE2 regular expression, matched against each line."},
				"path_glob": map[string]any{"type": "string", "description": `Optional gitignore-style pattern limiting which files are searched, e.g. "*.go" or "internal/**". Narrow it in a large repository.`},
				"version":   versionParam,
			}}},
		{Name: "list_files", Description: "List the repository's file paths, optionally only those under a directory.",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{
				"dir":     map[string]any{"type": "string", "description": "Directory to list, from the repository root (default: the whole repository)."},
				"version": versionParam,
			}}},
	}
}

// run executes one tool call and returns its result for the model, at most
// limit characters. Failures are results too: the model can try another way.
func (rt *repoTools) run(ctx context.Context, call llm.ToolCall, limit int) string {
	var a struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
		Pattern   string `json:"pattern"`
		PathGlob  string `json:"path_glob"`
		Dir       string `json:"dir"`
		Version   string `json:"version"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &a); err != nil {
		return "Error: the arguments are not a JSON object: " + err.Error()
	}
	ref := rt.head
	if a.Version == "base" {
		ref = rt.base
	}
	var out string
	switch call.Name {
	case "read_file":
		out = rt.readFile(ctx, ref, a.Path, a.StartLine, a.EndLine, limit)
	case "search_code":
		out = rt.search(ctx, ref, a.Pattern, a.PathGlob)
	case "list_files":
		out = rt.list(ctx, ref, a.Dir)
	default:
		out = fmt.Sprintf("Error: there is no tool %q. The tools are read_file, search_code and list_files.", call.Name)
	}
	if len(out) > limit {
		out = textutil.Truncate(out, limit-80) + "\n(output cut short: narrow the request)"
	}
	return out
}

func cleanRepoPath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	p = strings.TrimPrefix(strings.TrimPrefix(p, "./"), "/")
	return strings.TrimSuffix(p, "/")
}

func (rt *repoTools) file(ctx context.Context, ref, path string) (string, error) {
	key := ref + "\x00" + path
	c := rt.cache
	c.mu.Lock()
	f, ok := c.files[key]
	c.mu.Unlock()
	if ok {
		return f.content, f.err
	}
	content, err := rt.cl.FileContent(ctx, rt.repo, path, ref)
	if err != nil && ctx.Err() != nil {
		return "", err // not cached: the review is ending anyway
	}
	c.mu.Lock()
	c.files[key] = toolFile{content, err}
	c.mu.Unlock()
	return content, err
}

func (rt *repoTools) tree(ctx context.Context, ref string) toolTree {
	c := rt.cache
	c.mu.Lock()
	t, ok := c.trees[ref]
	c.mu.Unlock()
	if ok {
		return t
	}
	t.paths, t.truncated, t.err = rt.cl.TreePaths(ctx, rt.repo, ref)
	if t.err == nil || ctx.Err() == nil {
		c.mu.Lock()
		c.trees[ref] = t
		c.mu.Unlock()
	}
	return t
}

func (rt *repoTools) readFile(ctx context.Context, ref, path string, from, to, limit int) string {
	path = cleanRepoPath(path)
	if path == "" {
		return "Error: give a file path."
	}
	content, err := rt.file(ctx, ref, path)
	if err != nil {
		if github.IsStatus(err, 404) {
			return fmt.Sprintf("Error: %s does not exist at this version. list_files shows the paths that do.", path)
		}
		return fmt.Sprintf("Error: couldn't read %s.", path)
	}
	if strings.ContainsRune(textutil.Truncate(content, 8000), 0) {
		return fmt.Sprintf("%s is a binary file.", path)
	}
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	from = max(from, 1)
	if to <= 0 || to > len(lines) {
		to = len(lines)
	}
	if from > to {
		return fmt.Sprintf("%s has %d lines.", path, len(lines))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (%d lines)\n", path, len(lines))
	for i := from; i <= to; i++ {
		l := fmt.Sprintf("%5d | %s\n", i, lines[i-1])
		if sb.Len()+len(l) > limit-120 {
			fmt.Fprintf(&sb, "(stopped at line %d of %d to fit: ask for start_line %d to read on)\n", i-1, len(lines), i)
			break
		}
		sb.WriteString(l)
	}
	return sb.String()
}

func (rt *repoTools) list(ctx context.Context, ref, dir string) string {
	t := rt.tree(ctx, ref)
	if t.err != nil {
		return "Error: couldn't list the repository's files."
	}
	dir = cleanRepoPath(dir)
	var out []string
	for _, p := range t.paths {
		if dir == "" || strings.HasPrefix(p, dir+"/") {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return fmt.Sprintf("No files under %q.", dir)
	}
	var sb strings.Builder
	for i, p := range out {
		if i == maxListPaths {
			fmt.Fprintf(&sb, "...and %d more: list a subdirectory.\n", len(out)-maxListPaths)
			break
		}
		sb.WriteString(p + "\n")
	}
	if t.truncated {
		sb.WriteString("(GitHub cut the listing short: the repository is very large.)\n")
	}
	return sb.String()
}

// binaryExt are file types search_code doesn't read.
var binaryExt = regexp.MustCompile(`(?i)\.(png|jpe?g|gif|bmp|ico|webp|pdf|zip|gz|tgz|bz2|xz|7z|rar|jar|war|class|exe|dll|so|dylib|a|o|obj|bin|woff2?|ttf|otf|eot|mp[34]|mov|avi|wav|ogg|flac|psd|sqlite|db)$`)

func (rt *repoTools) search(ctx context.Context, ref, pattern, glob string) string {
	if strings.TrimSpace(pattern) == "" {
		return "Error: give a pattern."
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "Error: the pattern is not a valid RE2 regular expression: " + err.Error()
	}
	t := rt.tree(ctx, ref)
	if t.err != nil {
		return "Error: couldn't list the repository's files."
	}
	var only *regexp.Regexp
	if strings.TrimSpace(glob) != "" {
		if only = globToRegexp(glob); only == nil {
			return "Error: path_glob is not a usable pattern."
		}
	}
	var cands []string
	for _, p := range t.paths {
		if binaryExt.MatchString(p) || (only == nil && matchAny(rt.ignore, p)) || (only != nil && !only.MatchString(p)) {
			continue
		}
		cands = append(cands, p)
	}
	total := len(cands)
	if total > maxSearchFiles {
		cands = cands[:maxSearchFiles]
	}

	type hit struct {
		path  string
		lines []string
	}
	results := make([]hit, len(cands))
	sem := make(chan struct{}, searchConcurrency)
	var wg sync.WaitGroup
	for i, p := range cands {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			content, err := rt.file(ctx, ref, p)
			if err != nil || len(content) > maxToolFileBytes || strings.ContainsRune(textutil.Truncate(content, 8000), 0) {
				return
			}
			for n, l := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
				if re.MatchString(l) {
					results[i].lines = append(results[i].lines, fmt.Sprintf("%s:%d: %s", p, n+1, textutil.Truncate(strings.TrimSpace(l), 240)))
				}
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return "Error: the review was stopped."
	}

	var sb strings.Builder
	hits := 0
	for _, r := range results {
		for _, l := range r.lines {
			if hits == maxSearchHits {
				break
			}
			sb.WriteString(l + "\n")
			hits++
		}
	}
	all := 0
	for _, r := range results {
		all += len(r.lines)
	}
	switch {
	case all == 0:
		sb.WriteString("No matches.\n")
	case all > hits:
		fmt.Fprintf(&sb, "...and %d more matches: narrow the pattern or path_glob.\n", all-hits)
	}
	if total > len(cands) {
		fmt.Fprintf(&sb, "(Searched the first %d of %d files: give a path_glob to search the rest.)\n", len(cands), total)
	}
	if t.truncated {
		sb.WriteString("(The repository is very large and GitHub listed only part of it.)\n")
	}
	return sb.String()
}

// ---------- the tool-using conversation ----------

// chatTools asks for structured output like chatJSON, but first lets the
// model call the repository tools. room caps the characters of tool results
// added to the conversation; once it's used up the model must answer. The
// answer is asked for with the schema and without tools (servers can't do
// both at once), with the investigation flattened into plain messages.
func (b *Bot) chatTools(ctx context.Context, lc llm.Client, st settings.Settings, rt *repoTools, schema map[string]any, schemaName, system, user string, room int, out any) (llm.Response, string, error) {
	msgs := []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: user}}
	if !rt.on() || room < 2000 {
		return b.chatJSONMsgs(ctx, lc, st, schema, schemaName, msgs, out)
	}
	var total llm.Response
	var raw strings.Builder
	used := 0
	perCall := min(24000, max(room/3, 2000))
	for step := 0; ; step++ {
		if step == maxToolSteps || used >= room {
			msgs = append(msgs, llm.Message{Role: "user", Content: "You have used all the tool calls this review allows. Don't call any more tools: give your final answer now."})
			break
		}
		// MaxTokens 0: the answering model's own output cap (see tuned).
		resp, err := lc.Chat(ctx, llm.Request{Messages: msgs, Tools: rt.defs(), Temperature: st.LLM.Temperature})
		if errors.Is(err, llm.ErrToolsUnsupported) {
			rt.disable()
			fmt.Fprintf(&raw, "(tools refused by the model server, so the review goes on without them: %v)\n", err)
			u, r, err := b.chatJSONMsgs(ctx, lc, st, schema, schemaName, msgs[:2], out)
			total.PromptTokens += u.PromptTokens
			total.CompletionTokens += u.CompletionTokens
			return total, raw.String() + r, err
		}
		if err != nil {
			return total, raw.String(), err
		}
		total.PromptTokens += resp.PromptTokens
		total.CompletionTokens += resp.CompletionTokens
		if len(resp.ToolCalls) == 0 {
			raw.WriteString(resp.Content)
			if !resp.Truncated && llm.DecodeJSON(resp.Content, out) == nil {
				return total, raw.String(), nil
			}
			// An answer, but not the JSON asked for: ask again with the schema.
			msgs = append(msgs, llm.Message{Role: "assistant", Content: textutil.Truncate(llm.StripThinking(resp.Content), 4000)},
				llm.Message{Role: "user", Content: "That was not the JSON object asked for."})
			raw.WriteString("\n----- asking for the JSON answer -----\n")
			break
		}
		content := textutil.Truncate(llm.StripThinking(resp.Content), 2000)
		msgs = append(msgs, llm.Message{Role: "assistant", Content: content, ToolCalls: resp.ToolCalls})
		used += len(content)
		for _, c := range resp.ToolCalls {
			res := rt.run(ctx, c, min(perCall, max(room-used, 200)))
			used += len(res) + len(c.Arguments)
			fmt.Fprintf(&raw, "[tool] %s %s -> %d chars\n", c.Name, textutil.Truncate(c.Arguments, 300), len(res))
			msgs = append(msgs, llm.Message{Role: "tool", Content: res, ToolCallID: c.ID, ToolName: c.Name})
		}
	}
	msgs = append(flattenTools(msgs), llm.Message{Role: "user", Content: "Now reply with ONLY the JSON object in the required format, nothing else."})
	u, r, err := b.chatJSONMsgs(ctx, lc, st, schema, schemaName, msgs, out)
	total.PromptTokens += u.PromptTokens
	total.CompletionTokens += u.CompletionTokens
	return total, raw.String() + r, err
}

// flattenTools rewrites tool calls and results as plain assistant and user
// messages, which every server takes in a request without tools.
func flattenTools(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		switch {
		case len(m.ToolCalls) > 0:
			var sb strings.Builder
			if m.Content != "" {
				sb.WriteString(m.Content + "\n\n")
			}
			for _, c := range m.ToolCalls {
				fmt.Fprintf(&sb, "(I called %s %s)\n", c.Name, c.Arguments)
			}
			out = append(out, llm.Message{Role: "assistant", Content: sb.String()})
		case m.Role == "tool":
			out = append(out, llm.Message{Role: "user", Content: fmt.Sprintf("Result of %s:\n%s", m.ToolName, m.Content)})
		default:
			out = append(out, m)
		}
	}
	// Consecutive user messages (several results) are merged: some chat
	// templates require the roles to alternate.
	merged := out[:0]
	for _, m := range out {
		if n := len(merged); n > 0 && merged[n-1].Role == m.Role && m.Role != "system" {
			merged[n-1].Content += "\n\n" + m.Content
			continue
		}
		merged = append(merged, m)
	}
	return merged
}
