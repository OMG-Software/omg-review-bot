package bot

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/llm"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
	"github.com/OMG-Software/omg-review-bot/internal/textutil"
)

func (b *Bot) handleReplies(ctx context.Context, j job) {
	pr, err := b.store.GetPR(j.repo, j.number)
	if err != nil || pr == nil {
		return
	}
	st := b.SettingsFor(pr.AccountID)
	if !conversationsOn(st) || !allowedRepo(st, pr.Repo) {
		return // off for this organization, or it's paused
	}
	app, err := b.getApp(ctx, st)
	if err != nil {
		return
	}
	login := b.BotLogin()
	if login == "" {
		return
	}
	cl := app.Installation(pr.InstallationID)
	mst, err := b.modelSettings(st, pr.AccountID)
	if err != nil {
		// Not answered with the instance's models instead: an organization
		// with its own uses only those.
		b.logf("error", pr.Repo, pr.Number, "not replying: %v", err)
	} else {
		st = mst
	}
	ctx = withCallTag(ctx, callTag{kind: store.CallReply, accountID: pr.AccountID})
	mc := &jobClient{b: b, st: st, pr: pr}
	if st.Replies.Enabled && err == nil {
		if err := b.replyThreads(ctx, st, mc, cl, pr, login); err != nil {
			b.logf("error", pr.Repo, pr.Number, "replying to review threads: %v", err)
		}
	}
	if st.Replies.Mentions && err == nil {
		if err := b.replyMentions(ctx, st, mc, cl, pr, login); err != nil {
			b.logf("error", pr.Repo, pr.Number, "replying to mentions: %v", err)
		}
	}
	if err := b.syncApproval(ctx, st, cl, pr, login); err != nil {
		b.logf("warn", pr.Repo, pr.Number, "updating approval: %v", err)
	}
}

// conversationsOn reports whether reviewed PRs need watching between pushes:
// for replies, mentions, or approving (or lifting a change request) once
// threads are resolved.
func conversationsOn(st settings.Settings) bool {
	return st.Replies.Enabled || st.Replies.Mentions || st.Review.ApprovalOn() || st.Review.RequestChangesOn()
}

func isBotUser(u github.User) bool {
	return u.Type == "Bot" || strings.HasSuffix(u.Login, "[bot]")
}

// replyThreads answers people who replied in review threads the bot started.
func (b *Bot) replyThreads(ctx context.Context, st settings.Settings, mc *jobClient, cl *github.Client, pr *store.PR, login string) error {
	comments, err := cl.ReviewComments(ctx, pr.Repo, pr.Number)
	if err != nil {
		return err
	}
	byID := map[int64]*github.ReviewComment{}
	threads := map[int64][]*github.ReviewComment{}
	for i := range comments {
		c := &comments[i]
		byID[c.ID] = c
		root := c.ID
		if c.InReplyToID != 0 {
			root = c.InReplyToID // GitHub points replies at the thread's first comment
		}
		threads[root] = append(threads[root], c)
	}
	roots := make([]int64, 0, len(threads))
	for id := range threads {
		roots = append(roots, id)
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i] < roots[j] })

	// A reply that comes with a push ("Fixed in …") waits for that push's
	// review: its fix check resolves the thread if the fix holds, without an
	// answer, and only otherwise is the reply answered, against the new code.
	// handleReview queues this job again once the review is done.
	waitForReview := reviewComing(st, pr)
	waiting := 0

	var threadStates map[int64]github.ReviewThread // fetched on first need
	resolveThread := func(root *github.ReviewComment) {
		if threadStates == nil {
			threadStates = map[int64]github.ReviewThread{}
			ts, err := cl.ReviewThreads(ctx, pr.Repo, pr.Number)
			if err != nil {
				b.logf("warn", pr.Repo, pr.Number, "could not read review thread states: %v", err)
			}
			for _, t := range ts {
				threadStates[t.RootCommentID] = t
			}
		}
		t, ok := threadStates[root.ID]
		if !ok || t.IsResolved {
			return
		}
		if err := cl.ResolveReviewThread(ctx, t.ID); err != nil {
			b.logf("warn", pr.Repo, pr.Number, "could not resolve the thread on %s:%d: %v", root.Path, root.AnyLine(), err)
			return
		}
		b.logf("info", pr.Repo, pr.Number, "resolved the thread on %s:%d after discussion", root.Path, root.AnyLine())
	}

	for _, rootID := range roots {
		root := byID[rootID]
		if root == nil || root.User.Login != login {
			continue // only threads the bot started
		}
		list := threads[rootID]
		sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })

		// Collect human messages since the bot last spoke in this thread.
		var pending []*github.ReviewComment
		botReplies := 0
		for _, c := range list {
			switch {
			case c.ID == rootID:
			case c.User.Login == login:
				botReplies++
				pending = nil
			case isBotUser(c.User):
				// never converse with other bots
			default:
				pending = append(pending, c)
			}
		}
		if len(pending) == 0 {
			continue
		}
		last := pending[len(pending)-1]
		if b.store.IsHandled(last.ID) {
			continue
		}
		if last.CreatedAt.Unix() < pr.TrackedAt || botReplies >= st.Replies.MaxPerThread {
			for _, c := range pending {
				_ = b.store.MarkHandled(c.ID, "thread-skipped", pr.Repo, pr.Number, 0)
			}
			if botReplies >= st.Replies.MaxPerThread {
				b.logf("info", pr.Repo, pr.Number, "thread on %s reached the reply limit (%d); not replying", root.Path, st.Replies.MaxPerThread)
			}
			continue
		}
		if waitForReview {
			waiting++
			continue
		}
		b.setCurrent(ctx, pr, "Replying to @%s on %s#%d", last.User.Login, pr.Repo, pr.Number)
		text, resolved, err := b.threadReply(ctx, st, mc, cl, pr, root, list, login, last)
		if err != nil {
			return err
		}
		posted, err := cl.ReplyToReviewComment(ctx, pr.Repo, pr.Number, rootID, sanitizeModelText(text))
		if err != nil {
			return err
		}
		for _, c := range pending {
			_ = b.store.MarkHandled(c.ID, "thread", pr.Repo, pr.Number, posted.ID)
		}
		b.logf("info", pr.Repo, pr.Number, "replied to @%s on %s:%d", last.User.Login, root.Path, root.AnyLine())
		if resolved {
			resolveThread(root) // only after the reply, so the thread says why
		}
	}
	if waiting > 0 {
		b.mu.Lock()
		b.repliesWaiting[prKey(job{repo: pr.Repo, number: pr.Number})] = true
		b.mu.Unlock()
		b.logf("info", pr.Repo, pr.Number, "%d thread repl(ies) wait for the review of %s, whose fix check may resolve them", waiting, short(pr.HeadSHA))
	}
	return nil
}

// releaseReplies queues the reply check for thread replies that waited for a
// review of j's PR (see replyThreads), so they're answered unless that
// review's fix check resolved their threads. While a review is still due or
// under way they go on waiting for it; one that a limit stops (the PR shows
// "limit" or "daily") lets them go, as no review is coming soon.
func (b *Bot) releaseReplies(j job) {
	k := prKey(j)
	b.mu.Lock()
	waited := b.repliesWaiting[k]
	b.mu.Unlock()
	if !waited {
		return
	}
	pr, err := b.store.GetPR(j.repo, j.number)
	if err != nil || pr == nil || pr.Status == "pending" || pr.Status == "reviewing" {
		return // unreadable now: the next review of the PR tries again
	}
	b.mu.Lock()
	delete(b.repliesWaiting, k)
	b.mu.Unlock()
	if pr.Open {
		// handleReplies checks whether conversations are on for the org.
		b.enqueue(job{kind: jobReplies, repo: pr.Repo, number: pr.Number, org: pr.AccountID})
	}
}

// reviewComing reports whether a review of a commit newer than the last one
// reviewed is due or under way.
func reviewComing(st settings.Settings, pr *store.PR) bool {
	return pr.HeadSHA != "" && pr.HeadSHA != pr.ReviewedSHA &&
		(pr.Status == "pending" || pr.Status == "reviewing") && (st.Review.Enabled || pr.ForceRetry)
}

var threadSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"reply", "resolved"},
	"properties": map[string]any{
		"reply":    map[string]any{"type": "string"},
		"resolved": map[string]any{"type": "boolean"},
	},
}

func (b *Bot) threadReply(ctx context.Context, st settings.Settings, mc *jobClient, cl *github.Client, pr *store.PR,
	root *github.ReviewComment, thread []*github.ReviewComment, login string, last *github.ReviewComment) (text string, resolved bool, err error) {
	sk := b.loadSkill(st)
	totalChars := (st.LLM.ContextTokens - st.LLM.MaxOutputTokens - 256) * charsPerToken
	skillText, _, _ := sk.Prompt([]string{root.Path}, totalChars*25/100)

	line := root.AnyLine()
	var sb strings.Builder
	fmt.Fprintf(&sb, "Pull request %s#%d: %s\n", pr.Repo, pr.Number, pr.Title)
	fmt.Fprintf(&sb, "File: %s, line %d", root.Path, line)
	if root.Line == nil {
		sb.WriteString(" (this comment is now outdated: the code has changed since)")
	}
	sb.WriteString("\n\nDiff hunk your comment was attached to:\n```diff\n")
	sb.WriteString(textutil.Truncate(root.DiffHunk, 6000))
	sb.WriteString("\n```\n")
	if pr.HeadSHA != "" {
		// Show the current code so the model can confirm a claimed fix. An
		// outdated comment's line has moved, so show a wider window.
		radius, note := 30, ""
		if root.Line == nil {
			radius, note = 60, "; line numbers may have shifted since the comment"
		}
		if content, err := cl.FileContent(ctx, pr.Repo, root.Path, pr.HeadSHA); err == nil {
			if ex := numberedExcerpt(content, line, radius); ex != "" {
				fmt.Fprintf(&sb, "\nCurrent code around that line (latest commit %s%s):\n```\n%s```\n", short(pr.HeadSHA), note, ex)
			}
		} else if github.IsStatus(err, 404) {
			sb.WriteString("\nThis file no longer exists in the latest commit.\n")
		}
	}
	sb.WriteString("\nThread so far (oldest first):\n")
	for _, c := range thread {
		who := "@" + c.User.Login
		if c.User.Login == login {
			who = "You (the reviewer bot)"
		}
		fmt.Fprintf(&sb, "\n--- %s:\n%s\n", who, textutil.Truncate(stripFooter(c.Body), 4000))
	}
	fmt.Fprintf(&sb, "\nWrite your reply to @%s's latest message. Respond with the JSON object only.", last.User.Login)
	guide, _ := b.guidelinesFor(ctx, st, cl, pr.Repo, pr.Number, pr.BaseRef, pr.BaseRef, []string{root.Path}, 10)
	system := threadSystemPrompt(sk.Name, skillText, guide)

	lc, err := mc.get()
	if err != nil {
		return "", false, err
	}
	var out struct {
		Reply    string `json:"reply"`
		Resolved bool   `json:"resolved"`
	}
	if _, _, err := b.chatJSON(ctx, lc, st, threadSchema, "thread_reply", system, sb.String(), &out); err == nil {
		if r := strings.TrimSpace(out.Reply); r != "" {
			return r, out.Resolved, nil
		}
	}
	// The model couldn't produce the JSON: still answer, but don't resolve.
	b.logf("warn", pr.Repo, pr.Number, "model gave no structured thread reply; answering in plain text")
	text, err = b.freeText(ctx, lc, st, system+"\n\nIgnore the JSON format: output only the reply text.", sb.String())
	return text, false, err
}

func mentionRE(slug string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^\w-])@` + regexp.QuoteMeta(slug) + `(\[bot\])?($|[^\w-])`)
}

// replyMentions answers PR conversation comments that @mention the bot, and
// acts on the ones that ask it for a review ("@<app-name> review").
func (b *Bot) replyMentions(ctx context.Context, st settings.Settings, mc *jobClient, cl *github.Client, pr *store.PR, login string) error {
	slug := b.botSlug()
	if slug == "" {
		return nil
	}
	ics, err := cl.IssueComments(ctx, pr.Repo, pr.Number)
	if err != nil {
		return err
	}
	re, cmd := mentionRE(slug), reviewCommandRE(slug)
	for i, c := range ics {
		if c.User.Login == login || isBotUser(c.User) || !re.MatchString(c.Body) || b.store.IsHandled(c.ID) {
			continue
		}
		if c.CreatedAt.Unix() < pr.TrackedAt {
			_ = b.store.MarkHandled(c.ID, "mention-skipped", pr.Repo, pr.Number, 0)
			continue
		}
		if cmd.MatchString(c.Body) {
			if err := b.reviewCommand(ctx, cl, pr, c); err != nil {
				return err
			}
			continue
		}
		b.setCurrent(ctx, pr, "Answering @%s on %s#%d", c.User.Login, pr.Repo, pr.Number)
		text, err := b.mentionReply(ctx, st, mc, cl, pr, ics[:i+1], login)
		if err != nil {
			return err
		}
		posted, err := cl.CreateIssueComment(ctx, pr.Repo, pr.Number, afterPrefix("@"+c.User.Login, sanitizeModelText(text)))
		if err != nil {
			return err
		}
		_ = b.store.MarkHandled(c.ID, "mention", pr.Repo, pr.Number, posted.ID)
		b.logf("info", pr.Repo, pr.Number, "answered mention from @%s", c.User.Login)
	}
	return nil
}

// mentionPaths lists the PR files a mention reply's skill references are
// chosen by: every changed file it may be asked about, as its prompt lists
// them. Unlike reviewablePaths, a file GitHub sent no patch for counts: past
// the first megabyte or so of a large PR's diff, it sends none.
func mentionPaths(st settings.Settings, files []github.PRFile) []string {
	ignore := compileGlobs(st.Review.IgnorePatterns)
	paths := []string{} // non-nil: an empty list means "no languages", not "all"
	for _, f := range files {
		if f.Status != "removed" && !matchAny(ignore, f.Filename) {
			paths = append(paths, f.Filename)
		}
	}
	return paths
}

func (b *Bot) mentionReply(ctx context.Context, st settings.Settings, mc *jobClient, cl *github.Client, pr *store.PR, convo []github.IssueComment, login string) (string, error) {
	files, filesErr := cl.PullFiles(ctx, pr.Repo, pr.Number)
	var paths []string // nil (all references) if the file list is unavailable
	if filesErr == nil {
		paths = mentionPaths(st, files)
	}
	sk := b.loadSkill(st)
	totalChars := (st.LLM.ContextTokens - st.LLM.MaxOutputTokens - 256) * charsPerToken
	skillText, _, _ := sk.Prompt(paths, totalChars*25/100)

	var sb strings.Builder
	if gp, err := cl.Pull(ctx, pr.Repo, pr.Number); err == nil {
		sb.WriteString(prHeader(gp))
	} else {
		fmt.Fprintf(&sb, "Pull request %s#%d: %s\n", pr.Repo, pr.Number, pr.Title)
	}
	if filesErr == nil {
		sb.WriteString("\nChanged files:\n")
		for i, f := range files {
			if i == 100 {
				fmt.Fprintf(&sb, "- …and %d more\n", len(files)-100)
				break
			}
			fmt.Fprintf(&sb, "- %s (%s, +%d -%d)\n", f.Filename, f.Status, f.Additions, f.Deletions)
		}
	}
	if rv, err := b.store.LatestPostedReview(pr.AccountID, pr.Repo, pr.Number); err == nil && rv != nil {
		fmt.Fprintf(&sb, "\nYour latest review summary (commit %s):\n%s\n", short(rv.HeadSHA), rv.Summary)
	}
	sb.WriteString("\nConversation (oldest first):\n")
	start := max(0, len(convo)-10)
	for _, c := range convo[start:] {
		who := "@" + c.User.Login
		if c.User.Login == login {
			who = "You (the reviewer bot)"
		}
		fmt.Fprintf(&sb, "\n--- %s:\n%s\n", who, textutil.Truncate(c.Body, 3000))
	}
	fmt.Fprintf(&sb, "\nReply to @%s's latest message, which mentions you.", convo[len(convo)-1].User.Login)
	var changed []string
	for _, f := range files {
		changed = append(changed, f.Filename)
	}
	guide, _ := b.guidelinesFor(ctx, st, cl, pr.Repo, pr.Number, pr.BaseRef, pr.BaseRef, changed, 10)
	lc, err := mc.get()
	if err != nil {
		return "", err
	}
	return b.freeText(ctx, lc, st, mentionSystemPrompt(sk.Name, skillText, guide), sb.String())
}

func (b *Bot) freeText(ctx context.Context, lc llm.Client, st settings.Settings, system, user string) (string, error) {
	resp, err := lc.Chat(ctx, llm.Request{
		Messages:    []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: user}},
		Temperature: st.LLM.Temperature,
		MaxTokens:   2048, // or less: the answering model's own cap (see tuned)
	})
	if err != nil {
		return "", err
	}
	text := llm.StripThinking(resp.Content)
	if text == "" {
		return "", errors.New("model returned an empty reply")
	}
	return text, nil
}
