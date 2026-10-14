package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoQ-it/moqit/pkg/auth"
	"github.com/MoQ-it/moqit/pkg/cluster"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func writeCert(t *testing.T, dir string, mod time.Time) string {
	t.Helper()
	cert, err := SelfSignedCert("localhost,127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalECPrivateKey(cert.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"cert.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}),
		"key.pem":  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: key}),
	}
	for name, b := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, mod, mod)
	}
	h := sha256.Sum256(cert.Certificate[0])
	return hex.EncodeToString(h[:])
}

// A renewed certificate is served without a restart, and advertised to the
// cluster so other nodes' pins follow it.
func TestCertificateReload(t *testing.T) {
	defer func(d time.Duration) { certPollInterval = d }(certPollInterval)
	certPollInterval = 50 * time.Millisecond

	dir := t.TempDir()
	first := writeCert(t, dir, time.Now().Add(-time.Hour))
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	c := cluster.New(rdb, "A", time.Second, nil)
	srv, err := Start(context.Background(), Config{
		UDPAddr: "127.0.0.1:0", Keys: auth.KeyProvider{"k": "secret-secret-secret-secret-secret-0"},
		CertFile: filepath.Join(dir, "cert.pem"), KeyFile: filepath.Join(dir, "key.pem"),
		Cluster: c, AdvertiseAddr: "127.0.0.1:1", PublicURL: "https://localhost/moq",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Addr.String())
	addr := net.JoinHostPort("127.0.0.1", port)
	dial := func(hash string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		conn, err := dialPeer(ctx, addr, hash)
		if err == nil {
			conn.CloseWithError(0, "")
		}
		return err
	}
	if err := dial(first); err != nil {
		t.Fatalf("first certificate: %v", err)
	}

	second := writeCert(t, dir, time.Now())
	deadline := time.Now().Add(5 * time.Second)
	for dial(second) != nil {
		if time.Now().After(deadline) {
			t.Fatal("renewed certificate never served")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if dial(first) == nil {
		t.Error("old certificate still served")
	}
	for {
		nodes, _ := c.Nodes(context.Background())
		if len(nodes) == 1 && nodes[0].CertSHA256 == second {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cluster still advertises %v", nodes)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
