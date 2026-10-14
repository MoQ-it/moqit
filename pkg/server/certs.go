package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"log/slog"
	"os"
	"sync"
	"time"
)

// certPollInterval is how often certificate files are checked for changes.
var certPollInterval = 30 * time.Second

// certSource holds the node's certificate, re-reading it from files when
// they change, e.g. after an ACME client renews it.
type certSource struct {
	certFile, keyFile string

	mu   sync.Mutex
	cert *tls.Certificate
	hash [32]byte
	mod  [2]time.Time
}

func fixedCert(cert tls.Certificate) *certSource {
	return &certSource{cert: &cert, hash: sha256.Sum256(cert.Certificate[0])}
}

func fileCert(certFile, keyFile string) (*certSource, error) {
	s := &certSource{certFile: certFile, keyFile: keyFile}
	if _, err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *certSource) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cert, nil
}

func (s *certSource) sum() [32]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hash
}

// reload loads the files if they changed since the last load.
func (s *certSource) reload() (bool, error) {
	var mod [2]time.Time
	for i, f := range []string{s.certFile, s.keyFile} {
		st, err := os.Stat(f)
		if err != nil {
			return false, err
		}
		mod[i] = st.ModTime()
	}
	s.mu.Lock()
	unchanged := mod == s.mod
	s.mu.Unlock()
	if unchanged {
		return false, nil
	}
	cert, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		return false, err // e.g. the key written but not yet its certificate; next poll retries
	}
	s.mu.Lock()
	s.cert, s.hash, s.mod = &cert, sha256.Sum256(cert.Certificate[0]), mod
	s.mu.Unlock()
	return true, nil
}

// watch reloads changed files until ctx ends, calling changed with the new
// certificate's hash.
func (s *certSource) watch(ctx context.Context, log *slog.Logger, changed func([32]byte)) {
	if s.certFile == "" {
		return
	}
	t := time.NewTicker(certPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ok, err := s.reload()
		if err != nil {
			log.Warn("reloading certificate", "err", err)
			continue
		}
		if ok {
			log.Info("certificate reloaded", "file", s.certFile)
			changed(s.sum())
		}
	}
}
