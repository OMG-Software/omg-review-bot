package bot

import (
	"context"
	"path"
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
	// maxFailedRebuilds is how many files a review fetches and fails to
	// rebuild (binary, too large, unreadable) before it stops trying: a
	// failure leaves its slot for the next file, so without a cap a pull
	// request full of assets would cost a fetch of each on every push.
	maxFailedRebuilds = 20
)

// binaryExts are the extensions of files taken as binary without fetching
// them: images, fonts, archives, media, documents and compiled code.
var binaryExts = map[string]bool{}

func init() {
	for _, e := range strings.Fields(`
		png jpg jpeg gif webp bmp ico icns tif tiff avif heic psd
		woff woff2 ttf otf eot
		zip gz tgz bz2 xz zst 7z rar tar jar war
		mp3 mp4 m4a mov avi wav ogg flac webm mkv
		pdf doc docx xls xlsx ppt pptx
		exe dll so dylib a o lib obj class pyc wasm`) {
		binaryExts["."+e] = true
	}
}

// fillPatches gives each file in files that GitHub sent no patch for a patch
// built from its contents, or, where that can't be done, a NoDiff saying why.
// Deleted and ignored files are left alone. Files are rebuilt until there
// are as many patches as the review could take; one that can't be (a binary
// file, say) doesn't count, so the next is tried in its place.
func (b *Bot) fillPatches(ctx context.Context, cl *github.Client, st settings.Settings, pr *store.PR, files []github.PRFile, base, head string) {
	ignore := compileGlobs(st.Review.IgnorePatterns)
	var todo []int
	have, needBase := 0, false
	for i, f := range files {
		switch {
		case f.Status == "removed" || matchAny(ignore, f.Filename):
		case f.Patch != "":
			have++
		case binaryExts[strings.ToLower(path.Ext(f.Filename))]:
			files[i].NoDiff = "binary"
		default:
			todo = append(todo, i)
			needBase = needBase || f.Status != "added"
		}
	}
	if len(todo) == 0 {
		return
	}
	room := max(st.Review.MaxFiles-have, 0)
	if room == 0 {
		for _, i := range todo {
			files[i].NoDiff = "file limit reached"
		}
		return
	}
	mergeBase := ""
	if needBase {
		mb, err := cl.MergeBase(ctx, pr.Repo, base, head)
		if err != nil {
			b.logf("warn", pr.Repo, pr.Number, "could not find where %s branched from %s, so only added files GitHub sent no diff for are rebuilt: %v", short(head), short(base), err)
		}
		mergeBase = mb
	}

	// Each file is handed out only while fewer patches than room have been
	// rebuilt and fewer than maxFailedRebuilds have failed, so the workers in
	// flight overshoot by at most a few files, which the review's own file
	// limit then leaves out. stop says why the rest aren't tried.
	var mu sync.Mutex
	rebuilt, failed := 0, 0
	stop := func() string {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case rebuilt >= room:
			return "file limit reached"
		case failed >= maxFailedRebuilds:
			return "GitHub sent no diff, and too many others couldn't be rebuilt to try it"
		}
		return ""
	}
	work := make(chan int)
	var wg sync.WaitGroup
	for range min(rebuildWorkers, len(todo)) {
		wg.Go(func() {
			for i := range work {
				f := &files[i]
				f.Patch, f.Additions, f.Deletions, f.NoDiff = rebuildPatch(ctx, cl, pr.Repo, *f, mergeBase, head)
				mu.Lock()
				if f.Patch != "" {
					rebuilt++
				} else {
					failed++
				}
				mu.Unlock()
			}
		})
	}
	for _, i := range todo {
		if why := stop(); why != "" {
			files[i].NoDiff = why
			continue
		}
		work <- i
	}
	close(work)
	wg.Wait()
	b.logf("info", pr.Repo, pr.Number, "GitHub sent no diff for %d file(s); rebuilt %d from their contents", len(todo), rebuilt)
}

// rebuildPatch diffs one file between the merge base (the previous name, if
// it was renamed) and head. An empty patch comes with the reason.
func rebuildPatch(ctx context.Context, cl *github.Client, repo string, f github.PRFile, mergeBase, head string) (patch string, added, removed int, noDiff string) {
	const unread = "GitHub sent no diff, and its contents couldn't be read"
	if f.Status != "added" && mergeBase == "" {
		return "", 0, 0, "GitHub sent no diff, and the commit it branched from couldn't be found"
	}
	// One byte past the limit is enough to tell a file is too large, and
	// head is checked before the old version is fetched at all.
	read := func(file, ref string) (text, noDiff string) {
		text, err := cl.FileContentUpTo(ctx, repo, file, ref, maxRebuildBytes+1)
		switch {
		case err != nil:
			return "", unread
		case gitBinary(text):
			return "", "binary"
		case len(text) > maxRebuildBytes:
			return "", "too large for a diff"
		}
		return text, ""
	}
	newText, why := read(f.Filename, head)
	if why != "" {
		return "", 0, 0, why
	}
	oldText := ""
	if f.Status != "added" {
		name := f.Filename
		if f.PreviousFilename != "" {
			name = f.PreviousFilename
		}
		if oldText, why = read(name, mergeBase); why != "" {
			return "", 0, 0, why
		}
	}
	patch, added, removed = diff.Unified(oldText, newText, patchContext)
	if patch == "" {
		return "", 0, 0, "unchanged"
	}
	return patch, added, removed, ""
}

// gitBinary reports whether text looks like a binary file as git decides:
// a NUL byte in its first 8000 bytes. Unlike isBinary (a NUL anywhere, for
// the size-capped files the model reads through its tools), it allows a
// NUL further in, as git's own diff of the file would.
func gitBinary(text string) bool {
	return strings.IndexByte(text[:min(len(text), 8000)], 0) >= 0
}
