// Command meet serves the moqit Meet web app and mints access tokens for
// it. With -dev it also runs a moqit node in-process, so
//
//	(cd web && npm run build -w meet) && go run ./cmd/meet -dev
//
// is a complete local deployment at http://localhost:8080.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/MoQ-it/moqit/pkg/auth"
	"github.com/MoQ-it/moqit/pkg/server"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP address of the web app")
	static := flag.String("static", "web/apps/meet/dist", "built web app (npm run build -w meet)")
	dev := flag.Bool("dev", false, "run a moqit node in-process with a self-signed certificate")
	udp := flag.String("udp", ":4443", "-dev: UDP address of the in-process node")
	nodeURL := flag.String("node-url", os.Getenv("MOQIT_URL"), "WebTransport URL of the moqit node, e.g. https://moq.example.com:4443/moq (env MOQIT_URL)")
	apiKey := flag.String("key", envOr("MOQIT_API_KEY", "devkey"), "API key (env MOQIT_API_KEY)")
	apiSecret := flag.String("secret", os.Getenv("MOQIT_API_SECRET"), "API secret (env MOQIT_API_SECRET)")
	routeURL = flag.String("route-url", os.Getenv("MOQIT_ROUTE_URL"), "a node's /v1/route endpoint; when set, each meeting goes to the node the cluster picks (env MOQIT_ROUTE_URL)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log, *addr, *static, *dev, *udp, *nodeURL, *apiKey, *apiSecret); err != nil {
		log.Error("meet", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, addr, static string, dev bool, udp, nodeURL, apiKey, apiSecret string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var certHash, nodePort string
	if dev {
		if apiSecret == "" {
			apiSecret = "secret-for-development-only-0123456789"
		}
		cert, err := server.SelfSignedCert("localhost,127.0.0.1,::1")
		if err != nil {
			return err
		}
		srv, err := server.Start(ctx, server.Config{UDPAddr: udp, Keys: auth.KeyProvider{apiKey: apiSecret}, Certificate: cert, Logger: log})
		if err != nil {
			return err
		}
		defer srv.Close()
		certHash = hex.EncodeToString(srv.CertHash[:])
		nodePort = srv.Port()
		log.Info("in-process moqit node", "udp", srv.Addr.String())
	} else if (nodeURL == "" && *routeURL == "") || apiSecret == "" {
		return errors.New("pass -node-url (or -route-url) and -secret, or -dev")
	}
	if _, err := os.Stat(filepath.Join(static, "index.html")); err != nil {
		return errors.New("web app not built: run `npm run build -w meet` in web/ (or pass -static)")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/token", func(w http.ResponseWriter, r *http.Request) {
		// Rejoin is the random part of the identity this tab had before a
		// reload: reusing it makes the server replace the old session at
		// once instead of showing both until the old one times out. Viewer
		// asks for a read-only viewer token, which needs no name.
		var req struct {
			Room, Name, Rejoin string
			Viewer             bool
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Viewer && req.Name == "" {
			req.Name = "viewer"
		}
		if !roomCode.MatchString(req.Room) || req.Name == "" || utf8.RuneCountInString(req.Name) > 64 {
			http.Error(w, "invalid room or name", http.StatusBadRequest)
			return
		}
		// Identities are unique per join, so two people with the same name
		// (or one person in two tabs) do not replace each other.
		suffix := randomSuffix()
		if rejoinSuffix.MatchString(req.Rejoin) {
			suffix = req.Rejoin
		}
		identity := slug(req.Name) + "-" + suffix
		grant := &auth.RoomGrant{Join: !req.Viewer, Viewer: req.Viewer, Room: req.Room}
		tok, err := auth.NewAccessToken(apiKey, apiSecret).SetIdentity(identity).SetName(req.Name).
			SetRoomGrant(grant).SetValidFor(6 * time.Hour).ToJWT()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		url, hash := nodeURL, certHash
		if *routeURL != "" {
			u, h, err := route(r.Context(), *routeURL, tok)
			if err != nil {
				http.Error(w, "no node available: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
			url, hash = u, h
		} else if dev {
			// The node is on this machine; reach it at the host the page was loaded from.
			url = "https://" + hostOf(r) + ":" + nodePort + "/moq"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"url": url, "token": tok, "certHash": hash})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	files := http.FileServer(http.Dir(static))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Single-page app: anything that is not a file is the app.
		// index.html is always revalidated, so a rebuilt app shows up on the
		// next load; the assets it names are content-hashed, so they never change.
		if _, err := os.Stat(filepath.Join(static, filepath.Clean("/"+r.URL.Path))); err != nil || r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeFile(w, r, filepath.Join(static, "index.html"))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})

	hs := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		hs.Close()
	}()
	url := "http://" + addr
	if strings.HasPrefix(addr, ":") {
		url = "http://localhost" + addr
	}
	log.Info("meet ready", "url", url, "dev", dev)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

var roomCode = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

var routeURL *string

// route asks a node which node should host the token's room. Development
// clusters also return that node's certificate hash.
func route(ctx context.Context, endpoint, token string) (url, certHash string, err error) {
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	var out struct{ URL, CertHash string }
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&out) != nil || out.URL == "" {
		return "", "", fmt.Errorf("route: status %d", res.StatusCode)
	}
	return out.URL, out.CertHash, nil
}

func slug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
		if b.Len() >= 24 {
			break
		}
	}
	if s := strings.Trim(b.String(), "-"); s != "" {
		return s
	}
	return "guest"
}

// 48 random bits: a rejoining tab may present its old suffix, so it must
// not be guessable.
var rejoinSuffix = regexp.MustCompile(`^[0-9a-f]{12}$`)

func randomSuffix() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hostOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		return r.Host
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
