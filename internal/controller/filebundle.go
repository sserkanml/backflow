package controller

import (
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultFileTTL is how long a file bundle is trusted without looking at the file
// again. A ConfigMap that is mounted changes under the pod without a restart; the
// next call after this time sees it.
const DefaultFileTTL = 10 * time.Second

// fileBundle is a PEM file of certificate authorities, read again when it changes.
type fileBundle struct {
	path string
	ttl  time.Duration
	now  func() time.Time

	mu      sync.Mutex
	data    []byte
	mod     time.Time
	size    int64
	checked time.Time
}

func newFileBundle(path string) *fileBundle {
	return &fileBundle{path: path, ttl: DefaultFileTTL, now: time.Now}
}

// get returns the content of the file. Within ttl of the last look the cached
// content is returned; after it the file is looked at (its modification time and
// size), and read again only when one of them changed. A file that cannot be read,
// or holds no certificate, is an error, and the earlier content is not served
// instead: a CA that was withdrawn must stop being trusted.
func (b *fileBundle) get() ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if b.data != nil && now.Sub(b.checked) < b.ttl {
		return b.data, nil
	}
	info, err := os.Stat(b.path)
	if err != nil {
		b.data = nil
		return nil, fmt.Errorf("CA file: %w", err)
	}
	if b.data != nil && info.ModTime().Equal(b.mod) && info.Size() == b.size {
		b.checked = now
		return b.data, nil
	}
	data, err := os.ReadFile(b.path)
	if err != nil {
		b.data = nil
		return nil, fmt.Errorf("CA file: %w", err)
	}
	if !x509.NewCertPool().AppendCertsFromPEM(data) {
		b.data = nil
		return nil, fmt.Errorf("CA file %s holds no PEM certificate", b.path)
	}
	b.data, b.mod, b.size, b.checked = data, info.ModTime(), info.Size(), now
	return data, nil
}

// trustedCA is the file of extra roots for the connections to Git hosts that name no
// CA of their own. It is set once at start (UseTrustedCAFile) from a flag.
var trustedCA atomic.Pointer[fileBundle]

// UseTrustedCAFile makes the PEM bundle in path (read again when it changes) the extra
// roots of every ScmConnection that has no caSecretRef. An empty path switches it off.
func UseTrustedCAFile(path string) {
	if path == "" {
		trustedCA.Store(nil)
		return
	}
	trustedCA.Store(newFileBundle(path))
}

// trustedCABundle returns the bundle, or nil when there is none.
func trustedCABundle() ([]byte, error) {
	b := trustedCA.Load()
	if b == nil {
		return nil, nil
	}
	return b.get()
}
