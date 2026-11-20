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
	st := b.Settings()
	if !st.Replies.Enabled && !st.Replies.Mentions {
		return
	}
	pr, err := b.store.GetPR(j.repo, j.number)
	if err != nil || pr == nil {
		return
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
	if st.Replies.Enabled {
		if err := b.replyThreads(ctx, st, cl, pr, login); err != nil {
			b.logf("error", pr.Repo, pr.Number, "replying to review threads: %v", err)
		}
	}
	if st.Replies.Mentions {
		if err := b.replyMentions(ctx, st, cl, pr, login); err != nil {
			b.logf("error", pr.Repo, pr.Number, "replying to mentions: %v", err)
		}
	}
}

func isBotUser(u github.User) bool {
	return u.Type == "Bot" || strings.HasSuffix(u.Login, "[bot]")
}

// replyThreads answers people who replied in review threads the bot started.
func (b *Bot) replyThreads(ctx context.Context, st settings.Settings, cl *github.Client, pr *store.PR, login string) error {
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
		b.setCurrent("Replying to @%s on %s#%d", last.User.Login, pr.Repo, pr.Number)
		text, err := b.threadReply(ctx, st, cl, pr, root, list, login, last)
		if err != nil {
			return err
		}
		posted, err := cl.ReplyToReviewComment(ctx, pr.Repo, pr.Number, rootID, text)
		if err != nil {
			return err
		}
		for _, c := range pending {
			_ = b.store.MarkHandled(c.ID, "thread", pr.Repo, pr.Number, posted.ID)
		}
		b.logf("info", pr.Repo, pr.Number, "replied to @%s on %s:%d", last.User.Login, root.Path, root.AnyLine())
	}
	return nil
}

func (b *Bot) threadReply(ctx context.Context, st settings.Settings, cl *github.Client, pr *store.PR,
	root *github.ReviewComment, thread []*github.ReviewComment, login string, last *github.ReviewComment) (string, error) {
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
	if root.Line != nil && pr.HeadSHA != "" {
		if content, err := cl.FileContent(ctx, pr.Repo, root.Path, pr.HeadSHA); err == nil {
			if ex := numberedExcerpt(content, line, 30); ex != "" {
				sb.WriteString("\nCurrent code around that line (latest commit):\n```\n" + ex + "```\n")
			}
		}
	}
	sb.WriteString("\nThread so far (oldest first):\n")
	for _, c := range thread {
		who := "@" + c.User.Login
		if c.User.Login == login {
			who = "You (the reviewer bot)"
		}
		fmt.Fprintf(&sb, "\n--- %s:\n%s\n", who, textutil.Truncate(c.Body, 4000))
	}
	fmt.Fprintf(&sb, "\nWrite your reply to @%s's latest message.", last.User.Login)
	return b.freeText(ctx, st, threadSystemPrompt(sk.Name, skillText), sb.String())
}

func mentionRE(slug string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^\w-])@` + regexp.QuoteMeta(slug) + `(\[bot\])?($|[^\w-])`)
}

// replyMentions answers PR conversation comments that @mention the bot.
func (b *Bot) replyMentions(ctx context.Context, st settings.Settings, cl *github.Client, pr *store.PR, login string) error {
	slug := b.botSlug()
	if slug == "" {
		return nil
	}
	ics, err := cl.IssueComments(ctx, pr.Repo, pr.Number)
	if err != nil {
		return err
	}
	re := mentionRE(slug)
	for i, c := range ics {
		if c.User.Login == login || isBotUser(c.User) || !re.MatchString(c.Body) || b.store.IsHandled(c.ID) {
			continue
		}
		if c.CreatedAt.Unix() < pr.TrackedAt {
			_ = b.store.MarkHandled(c.ID, "mention-skipped", pr.Repo, pr.Number, 0)
			continue
		}
		b.setCurrent("Answering @%s on %s#%d", c.User.Login, pr.Repo, pr.Number)
		text, err := b.mentionReply(ctx, st, cl, pr, ics[:i+1], login)
		if err != nil {
			return err
		}
		posted, err := cl.CreateIssueComment(ctx, pr.Repo, pr.Number, "@"+c.User.Login+" "+text)
		if err != nil {
			return err
		}
		_ = b.store.MarkHandled(c.ID, "mention", pr.Repo, pr.Number, posted.ID)
		b.logf("info", pr.Repo, pr.Number, "answered mention from @%s", c.User.Login)
	}
	return nil
}

func (b *Bot) mentionReply(ctx context.Context, st settings.Settings, cl *github.Client, pr *store.PR, convo []github.IssueComment, login string) (string, error) {
	files, filesErr := cl.PullFiles(ctx, pr.Repo, pr.Number)
	var paths []string // nil (all references) if the file list is unavailable
	if filesErr == nil {
		paths = reviewablePaths(st, files)
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
	if rv, err := b.store.LatestPostedReview(pr.Repo, pr.Number); err == nil && rv != nil {
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
	return b.freeText(ctx, st, mentionSystemPrompt(sk.Name, skillText), sb.String())
}

func (b *Bot) freeText(ctx context.Context, st settings.Settings, system, user string) (string, error) {
	lc, err := llm.New(llmConfig(st))
	if err != nil {
		return "", err
	}
	resp, err := lc.Chat(ctx, llm.Request{
		Messages:    []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: user}},
		Temperature: st.LLM.Temperature,
		MaxTokens:   min(2048, st.LLM.MaxOutputTokens),
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
