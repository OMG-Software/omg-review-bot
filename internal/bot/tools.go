package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	maxToolSteps     = 16      // model turns per question before it must answer
	maxCallsPerStep  = 8       // tool calls run per model turn; the rest are refused
	maxSearchFiles   = 300     // files one search_code call may read
	maxSearchHits    = 80      // matching lines one search_code call returns
	maxListPaths     = 400     // paths one list_files call returns
	maxToolFileBytes = 1 << 20 // larger files aren't searched
	maxCacheBytes    = 64 << 20
	// GitHub reads of files one review may make: the installation's hourly
	// quota (5,000 calls) is shared with every other review and with posting.
	maxReviewFetches  = 1000
	searchConcurrency = 8
)

// errReadBudget is a read refused because the review has made all the GitHub
// reads it may (maxReviewFetches).
var errReadBudget = errors.New("this review has used up its repository reads")

type repoTools struct {
	cl         *github.Client
	repo       string
	head, base string
	// The review's ignore patterns: a search without a path_glob skips them.
	// They keep files out of the review, not out of reach: read_file opens
	// them, and a path_glob searches them, for context the review needs.
	ignore []*regexp.Regexp

	mu       sync.Mutex
	disabled bool // the model or server refused tools: not offered again
	cache    *toolCache
}

// toolCache is what the tools have read, shared by every model in a review.
type toolCache struct {
	mu    sync.Mutex
	files map[string]toolFile
	bytes int // size of the cached files
	trees map[string]toolTree
	// loading holds the files and listings being fetched, closed when the
	// fetch ends: models share the cache and read in parallel, and a second
	// reader waits for the first rather than fetching it again (see claim).
	loading map[string]chan struct{}
	// fetches counts the files read from GitHub; past maxFetches, reads fail
	// with errReadBudget and only what's cached can be read.
	fetches, maxFetches int
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
	sizes     map[string]int // each file's size in bytes, from the listing
	truncated bool
	err       error
}

func newRepoTools(cl *github.Client, st settings.Settings, repo, head, base string) *repoTools {
	if base == "" {
		base = head
	}
	return &repoTools{cl: cl, repo: repo, head: head, base: base, ignore: compileGlobs(st.Review.IgnorePatterns),
		cache: &toolCache{files: map[string]toolFile{}, trees: map[string]toolTree{}, maxFetches: maxReviewFetches}}
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
				"path_glob": map[string]any{"type": "string", "description": `Optional gitignore-style pattern limiting which files are searched, e.g. "*.go" or "internal/**". Narrow it in a large repository. Without it, files the review ignores (generated, vendored, lock files) are skipped; with it, they're searched too.`},
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
	p = strings.TrimSuffix(p, "/")
	if p == "." { // the repository's root, like ""
		return ""
	}
	return p
}

func (rt *repoTools) file(ctx context.Context, ref, path string) (string, error) {
	key := ref + "\x00" + path
	c := rt.cache
	var f toolFile
	done, err := c.claim(ctx, key, func() (ok bool) { f, ok = c.files[key]; return ok })
	if err != nil {
		return "", err
	}
	if done == nil {
		return f.content, f.err
	}
	c.mu.Lock()
	if c.fetches >= c.maxFetches {
		c.release(key, done)
		c.mu.Unlock()
		return "", errReadBudget
	}
	c.fetches++
	c.mu.Unlock()
	content, err := rt.cl.FileContent(ctx, rt.repo, path, ref)
	c.mu.Lock()
	// What's cached in a review is capped: past it, files are read again.
	// Files too large to search aren't cached, so they can't crowd out the
	// many smaller ones a search reads.
	if settled(err) && len(content) <= maxToolFileBytes && c.bytes+len(content) <= maxCacheBytes {
		c.files[key] = toolFile{content, err}
		c.bytes += len(content)
	}
	c.release(key, done)
	c.mu.Unlock()
	return content, err
}

// claim is how a cache read starts: it returns nil if cached (hit reports
// that, under c.mu), and otherwise a channel the caller, now the one fetching
// key, passes to release once it has stored what it fetched. Meanwhile other
// readers of key wait, then look again: cached by then, unless it couldn't be
// (too large, or a failed read), and then they fetch it themselves.
func (c *toolCache) claim(ctx context.Context, key string, hit func() bool) (chan struct{}, error) {
	for {
		c.mu.Lock()
		if hit() {
			c.mu.Unlock()
			return nil, nil
		}
		wait, busy := c.loading[key]
		if !busy {
			if c.loading == nil {
				c.loading = map[string]chan struct{}{}
			}
			done := make(chan struct{})
			c.loading[key] = done
			c.mu.Unlock()
			return done, nil
		}
		c.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// release ends a claim, with c.mu held.
func (c *toolCache) release(key string, done chan struct{}) {
	delete(c.loading, key)
	close(done)
}

// settled reports whether a GitHub read's outcome can be cached for the rest
// of the review: contents, or a definite miss. Anything else (rate limits,
// server errors, a cancelled review) might go away, so the next read retries.
func settled(err error) bool {
	return err == nil || github.IsStatus(err, http.StatusNotFound)
}

func (rt *repoTools) tree(ctx context.Context, ref string) toolTree {
	c := rt.cache
	key := "\x00tree\x00" + ref // apart from the files' keys, which start with a ref
	var t toolTree
	done, err := c.claim(ctx, key, func() (ok bool) { t, ok = c.trees[ref]; return ok })
	if err != nil {
		return toolTree{err: err}
	}
	if done == nil {
		return t
	}
	var files []github.TreeFile
	files, t.truncated, t.err = rt.cl.TreeFiles(ctx, rt.repo, ref)
	t.sizes = make(map[string]int, len(files))
	for _, f := range files {
		t.paths = append(t.paths, f.Path)
		t.sizes[f.Path] = f.Size
	}
	c.mu.Lock()
	if settled(t.err) {
		c.trees[ref] = t
	}
	c.release(key, done)
	c.mu.Unlock()
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
		if errors.Is(err, errReadBudget) {
			return fmt.Sprintf("Error: this review has read as many files as it may (%d), so %s can't be read. Files already read can still be read again: work from those.", maxReviewFetches, path)
		}
		return fmt.Sprintf("Error: couldn't read %s.", path)
	}
	if isBinary(content) {
		return fmt.Sprintf("%s is a binary file.", path)
	}
	lines := fileLines(content)
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
		if i == from && sb.Len()+len(l) > limit-120 {
			// One line too long to show whole: show its start, and go on
			// from the next.
			fmt.Fprintf(&sb, "%s\n(line %d is too long to show whole", textutil.Truncate(strings.TrimSuffix(l, "\n"), max(limit-sb.Len()-200, 40)), i)
			if i < len(lines) {
				fmt.Fprintf(&sb, ": ask for start_line %d to read on", i+1)
			}
			sb.WriteString(")\n")
			break
		}
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
		if t.truncated {
			return fmt.Sprintf("No files under %q in the part of the listing GitHub returned: the repository is very large, so the listing is incomplete. Try read_file with a path.", dir)
		}
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
	ignored := 0 // skipped by the review's ignore patterns
	for _, p := range t.paths {
		if only == nil && matchAny(rt.ignore, p) {
			ignored++
			continue
		}
		if binaryExt.MatchString(p) || (only != nil && !only.MatchString(p)) {
			continue
		}
		cands = append(cands, p)
	}
	total := len(cands)
	if total > maxSearchFiles {
		cands = cands[:maxSearchFiles]
	}

	// Each file keeps at most the lines that could be shown, and counts the rest.
	type hit struct {
		lines []string
		count int
		read  bool // false: the file couldn't be read
		large bool // too large to search
		spent bool // not read: the review's reads are used up
	}
	results := make([]hit, len(cands))
	sem := make(chan struct{}, searchConcurrency)
	var wg sync.WaitGroup
	for i, p := range cands {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			// The listing gives sizes, so a file too large to search isn't
			// downloaded (again, on every search) just to find that out.
			if t.sizes[p] > maxToolFileBytes {
				results[i].read, results[i].large = true, true
				return
			}
			content, err := rt.file(ctx, ref, p)
			results[i].read = err == nil
			results[i].spent = errors.Is(err, errReadBudget)
			if len(content) > maxToolFileBytes {
				results[i].large = true
				return
			}
			if err != nil || isBinary(content) {
				return
			}
			for n, l := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
				if re.MatchString(l) {
					if results[i].count < maxSearchHits {
						results[i].lines = append(results[i].lines, fmt.Sprintf("%s:%d: %s", p, n+1, textutil.Truncate(strings.TrimSpace(l), 240)))
					}
					results[i].count++
				}
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return "Error: the review was stopped."
	}

	var sb strings.Builder
	hits, all, unread, large, spent := 0, 0, 0, 0, 0
	for _, r := range results {
		all += r.count
		switch {
		case r.spent:
			spent++
		case !r.read:
			unread++
		}
		if r.large {
			large++
		}
		for _, l := range r.lines {
			if hits == maxSearchHits {
				break
			}
			sb.WriteString(l + "\n")
			hits++
		}
	}
	switch {
	case all == 0 && unread+large+ignored+spent > 0:
		sb.WriteString("No matches in the files searched.\n")
	case all == 0:
		sb.WriteString("No matches.\n")
	case all > hits:
		fmt.Fprintf(&sb, "...and %d more matches: narrow the pattern or path_glob.\n", all-hits)
	}
	if unread > 0 {
		fmt.Fprintf(&sb, "(%d of the %d files couldn't be read just now, so they weren't searched: try again, or read_file them.)\n", unread, len(cands))
	}
	if spent > 0 {
		fmt.Fprintf(&sb, "(%d of the %d files weren't searched: this review has read as many files as it may (%d). Only files already read can be searched or read now.)\n", spent, len(cands), maxReviewFetches)
	}
	if all == 0 && ignored > 0 {
		fmt.Fprintf(&sb, "(%d files the review ignores (generated, vendored and the like) weren't searched: give a path_glob to search them.)\n", ignored)
	}
	if large > 0 {
		fmt.Fprintf(&sb, "(%d of the %d files are too large to search (over %d MB), so they weren't searched: read_file them if they matter.)\n", large, len(cands), maxToolFileBytes>>20)
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
// system renders the system prompt with or without the tools mentioned.
func (b *Bot) chatTools(ctx context.Context, lc llm.Client, st settings.Settings, rt *repoTools, schema map[string]any, schemaName string, system func(tools bool) string, user string, room int, out any) (llm.Response, string, error) {
	plain := func() []llm.Message {
		return []llm.Message{{Role: "system", Content: system(false)}, {Role: "user", Content: user}}
	}
	if !rt.on() || room < 2000 {
		return b.chatJSONMsgs(ctx, lc, st, schema, schemaName, plain(), out)
	}
	msgs := []llm.Message{{Role: "system", Content: system(true)}, {Role: "user", Content: user}}
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
			// Ask again with a prompt that doesn't promise the tools.
			u, r, err := b.chatJSONMsgs(ctx, lc, st, schema, schemaName, plain(), out)
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
			// Without the schema enforced, the answer must at least have
			// the fields it requires: {} would read as "nothing found".
			if !resp.Truncated && decodeRequired(schema, resp.Content, out) {
				return total, raw.String(), nil
			}
			// An answer, but not the JSON asked for: ask again with the schema.
			msgs = append(msgs, llm.Message{Role: "assistant", Content: textutil.Truncate(llm.StripThinking(resp.Content), 4000)},
				llm.Message{Role: "user", Content: "That was not the JSON object asked for."})
			raw.WriteString("\n----- asking for the JSON answer -----\n")
			break
		}
		content := textutil.Truncate(llm.StripThinking(resp.Content), 2000)
		// Reasoning is sent back with the calls: some servers (DeepSeek's
		// thinking mode) require it on a continued tool conversation.
		msgs = append(msgs, llm.Message{Role: "assistant", Content: content, ToolCalls: resp.ToolCalls, Reasoning: resp.Reasoning})
		used += len(content) + len(resp.Reasoning)
		for _, c := range resp.ToolCalls {
			used += len(c.Arguments) // in the history from now on, run or not
		}
		for i, c := range resp.ToolCalls {
			// Every call gets a result, but only so many run per turn (each
			// search can read hundreds of files through the API), and none
			// once the room for tool results is used up.
			var res string
			switch {
			case i >= maxCallsPerStep:
				res = fmt.Sprintf("Not run: at most %d tool calls run at once. Ask for this again in your next turn if you still need it.", maxCallsPerStep)
				fmt.Fprintf(&raw, "[tool] %s skipped (more than %d calls in one turn)\n", c.Name, maxCallsPerStep)
			case used >= room:
				res = "Not run: the room for tool results in this review is used up."
				fmt.Fprintf(&raw, "[tool] %s skipped (no room left)\n", c.Name)
			default:
				res = rt.run(ctx, c, min(perCall, max(room-used, 200)))
				fmt.Fprintf(&raw, "[tool] %s %s -> %d chars\n", c.Name, textutil.Truncate(c.Arguments, 300), len(res))
			}
			used += len(res)
			msgs = append(msgs, llm.Message{Role: "tool", Content: res, ToolCallID: c.ID, ToolName: c.Name})
		}
	}
	// Added before flattening so it merges into any user message that ends
	// the conversation: the roles must still alternate.
	msgs = flattenTools(append(msgs, llm.Message{Role: "user", Content: "Now reply with ONLY the JSON object in the required format, nothing else."}))
	u, r, err := b.chatJSONMsgs(ctx, lc, st, schema, schemaName, msgs, out)
	total.PromptTokens += u.PromptTokens
	total.CompletionTokens += u.CompletionTokens
	return total, raw.String() + r, err
}

// decodeRequired decodes the JSON object in content into out if it has every
// field the schema requires. The object checked is the one decoded: one that
// has the fields but doesn't fit out (a single finding where a list belongs)
// is refused, not searched for another object inside it.
func decodeRequired(schema map[string]any, content string, out any) bool {
	var raw json.RawMessage
	if llm.DecodeJSON(content, &raw) != nil {
		return false
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return false
	}
	req, _ := schema["required"].([]string)
	for _, k := range req {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	return json.Unmarshal(raw, out) == nil
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

// isBinary reports whether content holds a NUL byte anywhere: binary data
// can start past any prefix (a generated file, a fixture), and the whole of
// a file read here is at most maxToolFileBytes, so all of it is checked.
func isBinary(content string) bool { return strings.IndexByte(content, 0) >= 0 }
