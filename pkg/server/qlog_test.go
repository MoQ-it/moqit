package server_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoQ-it/moqit/internal/testclient"
	"github.com/MoQ-it/moqit/pkg/server"
)

// With QlogDir, each QUIC connection gets a qlog file of its frames.
func TestQlogDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "qlog")
	a := startNodeWith(t, nil, "A", func(c *server.Config) { c.QlogDir = dir })
	alice := testclient.Join(t, a.addr, "r1", "alice", token(t, "alice", "r1"))
	alice.Send(testclient.MicTracks())
	deadline := time.Now().Add(5 * time.Second)
	for {
		files, _ := filepath.Glob(filepath.Join(dir, "*_server.sqlog"))
		for _, f := range files {
			if b, _ := os.ReadFile(f); strings.Contains(string(b), "parameters_set") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no qlog with the connection's transport parameters in %s: %v", dir, files)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
