// Package secret encrypts values stored in the database, such as an
// organization's own model API keys, with a key kept outside it: a copy of
// the database alone doesn't give them away.
//
// The key is PRBOT_SECRET_KEY (64 hex characters) if set, else the file
// secret.key in the data directory, created on first use. Losing it makes
// what it sealed unreadable: those organizations then have to enter their
// keys again.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvKey names the environment variable that can hold the key.
const EnvKey = "PRBOT_SECRET_KEY"

// FileName is the key file in the data directory.
const FileName = "secret.key"

const prefix = "v1:"

// Box seals and opens values with one key.
type Box struct{ aead cipher.AEAD }

// New returns a Box using key (32 bytes).
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("the secret key must be 32 bytes, not %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Load returns the Box for dataDir's key (see the package comment), creating
// the key file if there's no key yet.
func Load(dataDir string) (*Box, error) {
	if v := strings.TrimSpace(os.Getenv(EnvKey)); v != "" {
		key, err := hex.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("%s isn't hex: %w", EnvKey, err)
		}
		return New(key)
	}
	path := filepath.Join(dataDir, FileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		lerr := publish(path, []byte(hex.EncodeToString(key)+"\n"))
		if lerr == nil {
			return New(key)
		}
		if !errors.Is(lerr, os.ErrExist) {
			return nil, lerr
		}
		data, err = os.ReadFile(path) // another process made one meanwhile: that one is used
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("%s isn't a hex key: %w", path, err)
	}
	return New(key)
}

// publish writes data to path only if path doesn't exist yet (an error
// matching os.ErrExist if it does). The file appears complete or not at
// all: it's written in full under another name first, then linked into
// place, so another process never reads it half written.
func publish(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o600)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return err
		}
		return fmt.Errorf("create %s: %w", path, err)
	}
	return nil
}

// Seal encrypts plain. context binds it to where it's stored (an
// organization's ID, say): it opens only with the same context, so a sealed
// value copied to another row is useless there.
func (b *Box) Seal(plain []byte, context string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := b.aead.Seal(nonce, nonce, plain, []byte(context))
	return prefix + base64.StdEncoding.EncodeToString(out), nil
}

// ErrSealed means a value can't be opened: it was sealed with another key
// or context, or was changed.
var ErrSealed = errors.New("the stored value can't be decrypted (was the secret key changed?)")

// Open decrypts a value from Seal with the same context.
func (b *Box) Open(sealed, context string) ([]byte, error) {
	raw, ok := strings.CutPrefix(sealed, prefix)
	if !ok {
		return nil, ErrSealed
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(data) < b.aead.NonceSize() {
		return nil, ErrSealed
	}
	n := b.aead.NonceSize()
	plain, err := b.aead.Open(nil, data[:n], data[n:], []byte(context))
	if err != nil {
		return nil, ErrSealed
	}
	return plain, nil
}
