//go:build browser

package e2e

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/MoQ-it/moqit/pkg/auth"
	"github.com/MoQ-it/moqit/pkg/server"
)

type congestionResult struct {
	UA        string            `json:"ua"`
	OK        bool              `json:"ok"`
	Error     string            `json:"error"`
	Limited   string            `json:"limited"`
	Recovered string            `json:"recovered"`
	Samples   []json.RawMessage `json:"samples"`
}

func TestCongestionChrome(t *testing.T)  { runCongestion(t, chrome(t)) }
func TestCongestionFirefox(t *testing.T) { runCongestion(t, firefox(t)) }

// runCongestion puts Alice's uplink behind a shaper whose rate the page
// changes, while Bob watches her video at the top quality: send-side
// adaptation must keep the delay bounded and recover.
func runCongestion(t *testing.T, browser launcher) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	cert, err := server.SelfSignedCert("localhost,127.0.0.1,::1")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.Start(ctx, server.Config{UDPAddr: "127.0.0.1:0", Keys: auth.KeyProvider{apiKey: apiSecret}, Certificate: cert})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	shaper, err := NewShaper("127.0.0.1:"+srv.Port(), 0, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	defer shaper.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"aliceUrl":   "https://127.0.0.1:" + strconv.Itoa(shaper.Addr().Port) + "/moq",
			"bobUrl":     "https://127.0.0.1:" + srv.Port() + "/moq",
			"certHash":   hex.EncodeToString(srv.CertHash[:]),
			"aliceToken": token(t, "alice"),
			"bobToken":   token(t, "bob"),
		})
	})
	mux.HandleFunc("/rate", func(w http.ResponseWriter, r *http.Request) {
		bps, _ := strconv.ParseInt(r.URL.Query().Get("bps"), 10, 64)
		shaper.SetRate(bps)
	})
	var res congestionResult
	browse(ctx, t, browser, "congestion.ts", mux, &res)
	t.Logf("browser: %s", res.UA)
	if !res.OK {
		for _, s := range res.Samples {
			t.Logf("  %s", s)
		}
		t.Fatalf("browser failed: %s", res.Error)
	}
	t.Logf("%s; %s", res.Limited, res.Recovered)
}
