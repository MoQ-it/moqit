//go:build browser

// End-to-end test of @moqit/client in real browsers against a moqit node.
// Run with: go test -tags browser ./test/e2e
package e2e

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/MoQ-it/moqit/pkg/auth"
	"github.com/MoQ-it/moqit/pkg/cluster"
	"github.com/MoQ-it/moqit/pkg/dvr"
	"github.com/MoQ-it/moqit/pkg/server"
	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	moqit "github.com/MoQ-it/moqit/sdk/go"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

const apiKey, apiSecret = "e2ekey", "e2e-secret-0123456789abcdef0123456789"

type result struct {
	UA      string   `json:"ua"`
	OK      bool     `json:"ok"`
	Error   string   `json:"error"`
	Steps   []string `json:"steps"`
	High    string   `json:"highSize"`
	Low     string   `json:"lowSize"`
	Latency string   `json:"latency"`
	Rewind  string   `json:"rewind"`
}

type launcher func(url, dir string) *exec.Cmd

func chrome(t *testing.T) launcher {
	bin, err := exec.LookPath("google-chrome")
	if err != nil {
		t.Skip("google-chrome not installed")
	}
	return func(url, dir string) *exec.Cmd {
		// Chrome's helpers can outlive the killed browser and keep writing
		// here, so the profile is not under t.TempDir, whose removal fails.
		prof, err := os.MkdirTemp("", "moqit-e2e-chrome-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			for range 10 {
				if os.RemoveAll(prof) == nil {
					return
				}
				time.Sleep(200 * time.Millisecond)
			}
		})
		return exec.Command(bin, "--headless=new", "--no-sandbox", "--user-data-dir="+prof,
			"--use-fake-device-for-media-stream", "--use-fake-ui-for-media-stream", "--autoplay-policy=no-user-gesture-required", url)
	}
}

func TestChrome(t *testing.T)  { run(t, chrome(t), false) }
func TestFirefox(t *testing.T) { run(t, firefox(t), false) }

// The cluster variants put Alice and Bob on different nodes, so all media
// and signalling crosses the node-to-node link, and the viewer Carol on an
// edge node.
func TestChromeCluster(t *testing.T)  { run(t, chrome(t), true) }
func TestFirefoxCluster(t *testing.T) { run(t, firefox(t), true) }

func firefox(t *testing.T) launcher {
	bin, err := exec.LookPath("firefox")
	if err != nil {
		t.Skip("firefox not installed")
	}
	return func(url, dir string) *exec.Cmd {
		// The snap build cannot read /tmp, so the profile lives here.
		prof, err := os.MkdirTemp(".", ".ff-profile-")
		if err != nil {
			t.Fatal(err)
		}
		abs, _ := filepath.Abs(prof)
		t.Cleanup(func() { os.RemoveAll(abs) })
		os.WriteFile(filepath.Join(abs, "user.js"), []byte(`user_pref("media.navigator.streams.fake", true);
user_pref("media.navigator.permission.disabled", true);
user_pref("media.autoplay.default", 0);
user_pref("dom.allow_scripts_to_close_windows", true);
user_pref("browser.shell.checkDefaultBrowser", false);
user_pref("datareporting.policy.dataSubmissionEnabled", false);
`), 0o644)
		return exec.Command(bin, "--headless", "--no-remote", "--profile", abs, url)
	}
}

func httptestServer(t *testing.T, h http.Handler) string {
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s.URL
}

func token(t *testing.T, identity string) string {
	tok, err := auth.NewAccessToken(apiKey, apiSecret).SetIdentity(identity).
		SetRoomGrant(&auth.RoomGrant{Join: true, Room: "e2e", UpdateOwnMetadata: true}).ToJWT()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func viewerToken(t *testing.T, identity string) string {
	tok, err := auth.NewAccessToken(apiKey, apiSecret).SetIdentity(identity).
		SetRoomGrant(&auth.RoomGrant{Viewer: true, Room: "e2e"}).ToJWT()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// node starts a moqit node, joined to a cluster when rdb is set; an edge
// node serves viewers only.
func node(t *testing.T, ctx context.Context, rdb *redis.Client, id string, edge bool) *server.Server {
	t.Helper()
	cert, err := server.SelfSignedCert("localhost,127.0.0.1,::1")
	if err != nil {
		t.Fatal(err)
	}
	cfg := server.Config{UDPAddr: "127.0.0.1:0", Keys: auth.KeyProvider{apiKey: apiSecret}, Certificate: cert,
		ViewerInterval: 500 * time.Millisecond, Edge: edge}
	if !edge {
		store, err := dvr.Open(dvr.Config{Dir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		cfg.DVR, cfg.DVRWindow, cfg.DVRMaxHeight, cfg.DVRFetchLimit = store, 10*time.Minute, 720, 8
	}
	if rdb != nil {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		cfg.UDPAddr = pc.LocalAddr().String()
		pc.Close()
		cfg.Cluster = cluster.New(rdb, id, 3*time.Second, nil)
		cfg.AdvertiseAddr = cfg.UDPAddr
		cfg.PublicURL = "https://" + cfg.UDPAddr + "/moq"
	}
	srv, err := server.Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

func run(t *testing.T, browser launcher, clustered bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var aliceNode, bobNode, carolNode *server.Server
	if clustered {
		rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
		defer rdb.Close()
		aliceNode, bobNode, carolNode = node(t, ctx, rdb, "A", false), node(t, ctx, rdb, "B", false), node(t, ctx, rdb, "E", true)
	} else {
		aliceNode = node(t, ctx, nil, "", false)
		bobNode, carolNode = aliceNode, aliceNode
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"aliceUrl":      "https://127.0.0.1:" + aliceNode.Port() + "/moq",
			"aliceCertHash": hex.EncodeToString(aliceNode.CertHash[:]),
			"bobUrl":        "https://127.0.0.1:" + bobNode.Port() + "/moq",
			"bobCertHash":   hex.EncodeToString(bobNode.CertHash[:]),
			"carolUrl":      "https://127.0.0.1:" + carolNode.Port() + "/moq",
			"carolCertHash": hex.EncodeToString(carolNode.CertHash[:]),
			"aliceToken":    token(t, "alice"),
			"bobToken":      token(t, "bob"),
			"carolToken":    viewerToken(t, "carol"),
		})
	})
	// The page asks for a server-side mute through the RoomService API.
	admin := moqit.NewRoomServiceClient(httptestServer(t, aliceNode.APIHandler()), apiKey, apiSecret)
	mux.HandleFunc("/admin/mute", func(w http.ResponseWriter, r *http.Request) {
		_, err := admin.MuteTrack(r.Context(), connect.NewRequest(&moqitv1.MuteTrackRequest{
			Room: "e2e", Identity: r.URL.Query().Get("identity"), TrackSid: r.URL.Query().Get("track"), Muted: true,
		}))
		if err != nil {
			http.Error(w, err.Error(), 500)
		}
	})
	var res result
	browse(ctx, t, browser, "page.ts", mux, &res)
	t.Logf("browser: %s", res.UA)
	for _, s := range res.Steps {
		t.Logf("  %s", s)
	}
	if !res.OK {
		t.Fatalf("browser failed: %s", res.Error)
	}
	t.Logf("video high %s, low %s; %s", res.High, res.Low, res.Latency)
	t.Logf("rewind: %s", res.Rewind)
}

// browse bundles a page from this directory, serves it with mux's
// handlers, opens it in the browser and decodes what it posts to /result.
func browse(ctx context.Context, t *testing.T, browser launcher, page string, mux *http.ServeMux, res any) {
	t.Helper()
	dir := t.TempDir()
	bundle := filepath.Join(dir, "page.js")
	esbuild := exec.Command("npx", "esbuild", "../test/e2e/"+page, "--bundle", "--format=esm", "--outfile="+bundle)
	esbuild.Dir = "../../web"
	if out, err := esbuild.CombinedOutput(); err != nil {
		t.Fatalf("esbuild: %v\n%s", err, out)
	}
	results := make(chan []byte, 1)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><body><script type="module" src="/page.js"></script>`)
	})
	mux.HandleFunc("/page.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		http.ServeFile(w, r, bundle)
	})
	mux.HandleFunc("/result", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		results <- b
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: mux}
	go hs.Serve(ln)
	defer hs.Close()

	cmd := browser("http://localhost:"+ln.Addr().(*net.TCPAddr).AddrPort().String()[len("127.0.0.1:"):]+"/", dir)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	select {
	case b := <-results:
		if err := json.Unmarshal(b, res); err != nil {
			t.Fatalf("result %s: %v", b, err)
		}
	case <-ctx.Done():
		t.Fatal("no result from the browser")
	}
}
