package bot

import (
	"context"
	"strings"
	"sync"

	"github.com/OMG-Software/omg-review-bot/internal/diff"
	"github.com/OMG-Software/omg-review-bot/internal/github"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
	"github.com/OMG-Software/omg-review-bot/internal/store"
)

// GitHub stops sending patches part-way through a pull request whose diff is
// large (around a megabyte), and refuses its raw diff outright past 20,000
// lines. The files after the cut come back with no patch and no line counts,
// just as a binary file does, so a large pull request had its last files
// skipped on every review. fillPatches rebuilds those patches from the
// files' contents at the merge base and head.

const (
	// maxRebuildBytes is the largest file whose patch is rebuilt.
	maxRebuildBytes = 1 << 20
	// rebuildWorkers is how many files are fetched at once.
	rebuildWorkers = 4
	// patchContext is the context lines a rebuilt patch has, as GitHub's do.
	patchContext = 3
)

// fillPatches gives each file in files that GitHub sent no patch for a patch
// built from its contents, or, where that can't be done, a NoDiff saying why.
// Deleted and ignored files are left alone, and no more files are rebuilt
// than the review could take.
func (b *Bot) fillPatches(ctx context.Context, cl *github.Client, st settings.Settings, pr *store.PR, files []github.PRFile, base, head string) {
	ignore := compileGlobs(st.Review.IgnorePatterns)
	var todo []int
	have, needBase := 0, false
	for i, f := range files {
		switch {
		case f.Status == "removed" || matchAny(ignore, f.Filename):
		case f.Patch != "":
			have++
		default:
			todo = append(todo, i)
		}
	}
	if len(todo) == 0 {
		return
	}
	if room := max(st.Review.MaxFiles-have, 0); len(todo) > room {
		for _, i := range todo[room:] {
			files[i].NoDiff = "file limit reached"
		}
		todo = todo[:room]
	}
	for _, i := range todo {
		needBase = needBase || files[i].Status != "added"
	}
	mergeBase := ""
	if needBase {
		mb, err := cl.MergeBase(ctx, pr.Repo, base, head)
		if err != nil {
			b.logf("warn", pr.Repo, pr.Number, "could not find where %s branched from %s, so only added files GitHub sent no diff for are rebuilt: %v", short(head), short(base), err)
		}
		mergeBase = mb
	}

	work := make(chan int)
	var wg sync.WaitGroup
	for range min(rebuildWorkers, len(todo)) {
		wg.Go(func() {
			for i := range work {
				f := &files[i]
				f.Patch, f.Additions, f.Deletions, f.NoDiff = rebuildPatch(ctx, cl, pr.Repo, *f, mergeBase, head)
			}
		})
	}
	for _, i := range todo {
		work <- i
	}
	close(work)
	wg.Wait()

	rebuilt := 0
	for _, i := range todo {
		if files[i].Patch != "" {
			rebuilt++
		}
	}
	b.logf("info", pr.Repo, pr.Number, "GitHub sent no diff for %d file(s); rebuilt %d from their contents", len(todo), rebuilt)
}

// rebuildPatch diffs one file between the merge base (the previous name, if
// it was renamed) and head. An empty patch comes with the reason.
func rebuildPatch(ctx context.Context, cl *github.Client, repo string, f github.PRFile, mergeBase, head string) (patch string, added, removed int, noDiff string) {
	const unread = "GitHub sent no diff, and its contents couldn't be read"
	if f.Status != "added" && mergeBase == "" {
		return "", 0, 0, unread
	}
	newText, err := cl.FileContent(ctx, repo, f.Filename, head)
	if err != nil {
		return "", 0, 0, unread
	}
	oldText := ""
	if f.Status != "added" {
		name := f.Filename
		if f.PreviousFilename != "" {
			name = f.PreviousFilename
		}
		if oldText, err = cl.FileContent(ctx, repo, name, mergeBase); err != nil {
			return "", 0, 0, unread
		}
	}
	switch {
	case binary(newText) || binary(oldText):
		return "", 0, 0, "binary"
	case len(newText) > maxRebuildBytes || len(oldText) > maxRebuildBytes:
		return "", 0, 0, "too large for a diff"
	}
	patch, added, removed = diff.Unified(oldText, newText, patchContext)
	if patch == "" {
		return "", 0, 0, "unchanged"
	}
	return patch, added, removed, ""
}

// binary reports whether text looks like a binary file: a NUL byte near its
// start, as git decides.
func binary(text string) bool {
	return strings.IndexByte(text[:min(len(text), 8000)], 0) >= 0
}
