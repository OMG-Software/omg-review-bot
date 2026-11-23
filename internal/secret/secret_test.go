package secret

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSealOpen(t *testing.T) {
	t.Setenv(EnvKey, "")
	dir := t.TempDir()
	b, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, FileName)); err != nil || fi.Size() == 0 {
		t.Fatalf("key file not created: %v", err)
	}
	sealed, err := b.Seal([]byte("sk-secret"), "org:1")
	if err != nil || strings.Contains(sealed, "sk-secret") {
		t.Fatalf("Seal = %q, %v", sealed, err)
	}
	// The same key is read back from the file.
	b2, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := b2.Open(sealed, "org:1"); err != nil || string(got) != "sk-secret" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	if _, err := b2.Open(sealed, "org:2"); !errors.Is(err, ErrSealed) {
		t.Errorf("opened with another organization's context: %v", err)
	}
	other, _ := Load(t.TempDir())
	if _, err := other.Open(sealed, "org:1"); !errors.Is(err, ErrSealed) {
		t.Errorf("opened with another key: %v", err)
	}
	if _, err := b2.Open("plain", "org:1"); !errors.Is(err, ErrSealed) {
		t.Errorf("opened an unsealed value: %v", err)
	}
}

func TestKeyFromEnv(t *testing.T) {
	t.Setenv(EnvKey, strings.Repeat("ab", 32))
	dir := t.TempDir()
	if _, err := Load(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a key file was written although the key came from the environment")
	}
	t.Setenv(EnvKey, "not-hex")
	if _, err := Load(dir); err == nil {
		t.Error("a malformed key was accepted")
	}
}

// TestLoadConcurrent: processes starting together on one data directory all
// end up with the same key; none sees a half-written key file.
func TestLoadConcurrent(t *testing.T) {
	t.Setenv(EnvKey, "")
	dir := t.TempDir()
	const n = 16
	boxes := make([]*Box, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() { defer wg.Done(); boxes[i], errs[i] = Load(dir) }()
	}
	wg.Wait()
	sealed, err := boxes[0].Seal([]byte("x"), "c")
	if errs[0] != nil || err != nil {
		t.Fatal(errs[0], err)
	}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("load %d: %v", i, errs[i])
		}
		if got, err := boxes[i].Open(sealed, "c"); err != nil || string(got) != "x" {
			t.Fatalf("load %d has another key: %v", i, err)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}
