package web

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
)

// pendingEnds is a small file in the data directory listing the people whose
// sessions have to end but the database couldn't record it (their access
// couldn't be updated at sign-in, and bumping their session epoch failed
// too). It's kept apart from the database because the database is what
// failed. At startup the listed people stay signed out until their sessions
// are ended in the database, so a restart doesn't bring the sessions back.
type pendingEnds struct {
	mu   sync.Mutex
	path string
}

func (p *pendingEnds) load() ([]int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.read()
}

func (p *pendingEnds) read() ([]int64, error) {
	b, err := os.ReadFile(p.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []int64
	if err := json.Unmarshal(b, &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

// write replaces the file whole (write then rename), so a crash mid-way
// leaves the old list rather than half of a new one. It returns once the new
// list is on disk: the file and the rename are synced, so a power cut after
// a successful add doesn't lose the note.
func (p *pendingEnds) write(ids []int64) error {
	if len(ids) == 0 {
		if err := os.Remove(p.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	return writeDurably(p.path, b)
}

// writeDurably replaces path with b (write then rename) and returns once it's
// on disk: the file and the rename are synced.
func writeDurably(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil // a directory can't be opened to sync it there
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (p *pendingEnds) add(id int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids, err := p.read()
	if err != nil {
		// Not replaced: it may list others whose sessions have to end. The
		// caller retries, and at startup an unreadable file ends every
		// session.
		return err
	}
	if slices.Contains(ids, id) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return err
	}
	return p.write(append(ids, id))
}

func (p *pendingEnds) remove(id int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids, err := p.read()
	if err != nil || !slices.Contains(ids, id) {
		return err
	}
	return p.write(slices.DeleteFunc(ids, func(x int64) bool { return x == id }))
}
