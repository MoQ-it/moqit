// Command moqit-server runs a moqit node: a MoQ relay with rooms, serving
// WebTransport (browsers) and raw QUIC (native clients, other nodes) on one
// UDP port, plus an HTTP port for health checks and, in -dev mode, tokens.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
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
	"strings"
	"syscall"
	"time"

	"github.com/MoQ-it/moqit/pkg/auth"
	"github.com/MoQ-it/moqit/pkg/cluster"
	"github.com/MoQ-it/moqit/pkg/dvr"
	"github.com/MoQ-it/moqit/pkg/server"
	"github.com/MoQ-it/moqit/pkg/webhook"
	"github.com/redis/go-redis/v9"
)

const (
	devKey    = "devkey"
	devSecret = "secret-for-development-only-0123456789"
)

func main() {
	dev := flag.Bool("dev", false, "development mode: dev API key, self-signed certificate, /v1/dev/token")
	udpAddr := flag.String("udp", ":4443", "UDP address for MoQ (WebTransport path /moq and raw QUIC)")
	httpAddr := flag.String("http", ":7880", "HTTP address for health checks (and dev tokens)")
	keysFlag := flag.String("keys", os.Getenv("MOQIT_KEYS"), "API keys as key:secret,key2:secret2 (env MOQIT_KEYS)")
	certFile := flag.String("cert", "", "TLS certificate file")
	keyFile := flag.String("key", "", "TLS key file")
	hosts := flag.String("hosts", "localhost,127.0.0.1,::1", "names for the self-signed certificate")
	departure := flag.Duration("departure-timeout", 20*time.Second, "how long an empty room lives on")
	verbose := flag.Bool("v", false, "debug logging")
	redisURL := flag.String("redis", os.Getenv("MOQIT_REDIS"), "Redis URL to join a cluster, e.g. redis://redis:6379/0 (env MOQIT_REDIS)")
	nodeID := flag.String("node-id", os.Getenv("MOQIT_NODE_ID"), "cluster node ID (default: random)")
	advertise := flag.String("advertise", os.Getenv("MOQIT_ADVERTISE"), "host:port other nodes use to reach this node's UDP port")
	publicURL := flag.String("public-url", os.Getenv("MOQIT_PUBLIC_URL"), "WebTransport URL clients are routed to, e.g. https://node1.example.com:4443/moq")
	region := flag.String("region", os.Getenv("MOQIT_REGION"), "region label for routing")
	edge := flag.Bool("edge", os.Getenv("MOQIT_EDGE") == "true", "edge node: serve read-only viewers only, fetching tracks from the cluster (needs -redis; env MOQIT_EDGE=true)")
	maxViewers := flag.Int("max-viewers", 0, "maximum viewer sessions on this node; 0 = unlimited")
	tier := flag.Int("tier", 0, "edge tier in a cascade: 1 (default) fetches from the nodes hosting tracks, 2 and up from an edge one tier up in the same -region")
	dvrDir := flag.String("dvr-dir", os.Getenv("MOQIT_DVR_DIR"), "directory recording rooms that keep history, so viewers can rewind; empty = off (env MOQIT_DVR_DIR)")
	dvrWindow := flag.Duration("dvr-window", 0, "history window of rooms created without one; 0 = only rooms created with a dvr_window keep history")
	dvrMaxWindow := flag.Duration("dvr-max-window", 6*time.Hour, "longest history window a room may keep")
	dvrMaxBytes := flag.Int64("dvr-max-bytes", 0, "disk the recordings may use; the oldest are deleted first; 0 = no cap")
	dvrMaxHeight := flag.Int("dvr-max-height", 720, "record the largest video rendition at most this high")
	dvrFetchLimit := flag.Int("dvr-fetch-limit", 4, "concurrent rewind FETCHes per viewer; 0 = no cap")
	qlogDir := flag.String("qlog-dir", "", "write a qlog file per QUIC connection into this directory, for troubleshooting; empty = off")
	webhooks := flag.String("webhook", os.Getenv("MOQIT_WEBHOOK"), "comma-separated URLs receiving webhook events, signed with the first API key (env MOQIT_WEBHOOK)")
	flag.Parse()
	webhookURLs = *webhooks
	clusterCfg = clusterFlags{*redisURL, *nodeID, *advertise, *publicURL, *region}
	edgeNode, viewerCap, edgeTier = *edge, *maxViewers, *tier
	qlogPath = *qlogDir
	dvrCfg = dvrFlags{*dvrDir, *dvrWindow, *dvrMaxWindow, *dvrMaxBytes, *dvrMaxHeight, *dvrFetchLimit}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if err := run(log, *dev, *udpAddr, *httpAddr, *keysFlag, *certFile, *keyFile, *hosts, *departure); err != nil {
		log.Error("moqit-server", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, dev bool, udpAddr, httpAddr, keysFlag, certFile, keyFile, hosts string, departure time.Duration) error {
	keys := auth.KeyProvider{}
	for kv := range strings.SplitSeq(keysFlag, ",") {
		if k, s, ok := strings.Cut(strings.TrimSpace(kv), ":"); ok {
			keys[k] = s
		}
	}
	if dev {
		keys[devKey] = devSecret
	}
	if len(keys) == 0 {
		return errors.New("no API keys: pass -keys key:secret or -dev")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var cert tls.Certificate
	var err error
	switch {
	case certFile != "":
		// The certificate may still be on its way (an ACME client's first run).
		err = waitForFiles(ctx, log, certFile, keyFile)
	case dev:
		cert, err = server.SelfSignedCert(hosts)
	default:
		err = errors.New("a TLS certificate is required outside -dev")
	}
	if err != nil {
		return err
	}

	cfg := server.Config{UDPAddr: udpAddr, Keys: keys, Certificate: cert, CertFile: certFile, KeyFile: keyFile, DepartureTimeout: departure, Logger: log,
		AdvertiseAddr: clusterCfg.advertise, PublicURL: clusterCfg.publicURL, Region: clusterCfg.region, ExposeCertHash: dev,
		Edge: edgeNode, Tier: edgeTier, MaxViewers: viewerCap, QlogDir: qlogPath}
	if edgeNode && clusterCfg.redis == "" {
		return errors.New("-edge needs -redis")
	}
	if clusterCfg.redis != "" {
		opts, err := redis.ParseURL(clusterCfg.redis)
		if err != nil {
			return fmt.Errorf("-redis: %w", err)
		}
		if cfg.AdvertiseAddr == "" || cfg.PublicURL == "" {
			return errors.New("a cluster node needs -advertise and -public-url")
		}
		id := clusterCfg.nodeID
		if id == "" {
			id = "ND_" + randomHex(5)
		}
		rdb := redis.NewClient(opts)
		defer rdb.Close()
		if err := rdb.Ping(ctx).Err(); err != nil {
			return fmt.Errorf("redis: %w", err)
		}
		cfg.Cluster = cluster.New(rdb, id, 10*time.Second, log)
		log.Info("joining cluster", "node", id, "advertise", cfg.AdvertiseAddr, "publicUrl", cfg.PublicURL, "edge", edgeNode)
	}
	cfg.DVRFetchLimit = dvrCfg.fetchLimit // edges forward viewers' FETCHes too
	if dvrCfg.dir != "" {
		store, err := dvr.Open(dvr.Config{Dir: dvrCfg.dir, MaxBytes: dvrCfg.maxBytes, Logger: log})
		if err != nil {
			return fmt.Errorf("-dvr-dir: %w", err)
		}
		cfg.DVR, cfg.DVRWindow, cfg.DVRMaxWindow, cfg.DVRMaxHeight = store, dvrCfg.window, dvrCfg.maxWindow, dvrCfg.maxHeight
		log.Info("rewind enabled", "dir", dvrCfg.dir, "window", dvrCfg.window, "maxWindow", dvrCfg.maxWindow, "stored", store.Size())
	}
	if webhookURLs != "" {
		key := firstKey(keysFlag, dev)
		cfg.Webhook = webhook.NewNotifier(strings.Split(webhookURLs, ","), key, keys[key], log)
		log.Info("webhooks enabled", "urls", webhookURLs, "key", key)
	}
	srv, err := server.Start(ctx, cfg)
	if err != nil {
		return err
	}
	defer srv.Close()
	certHash := hex.EncodeToString(srv.CertHash[:])

	api := http.NewServeMux()
	api.Handle("/", srv.APIHandler())
	if dev {
		api.HandleFunc("GET /v1/dev/token", func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			tok, err := auth.NewAccessToken(devKey, devSecret).SetIdentity(q.Get("identity")).SetName(q.Get("name")).
				SetRoomGrant(&auth.RoomGrant{Join: true, Room: q.Get("room"), UpdateOwnMetadata: true}).ToJWT()
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", "*")
			json.NewEncoder(w).Encode(map[string]string{"token": tok, "url": "https://" + hostOf(r) + ":" + srv.Port() + "/moq", "certHash": certHash})
		})
	}
	httpSrv := &http.Server{Addr: httpAddr, Handler: api}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http", "err", err)
		}
	}()
	defer httpSrv.Close()

	log.Info("moqit-server ready", "udp", srv.Addr.String(), "http", httpAddr, "dev", dev, "certSha256", certHash)
	if dev {
		log.Info("dev token", "example", "curl 'http://localhost"+httpAddr+"/v1/dev/token?room=demo&identity=alice'")
	}
	<-ctx.Done()
	log.Info("shutting down")
	return nil
}

type clusterFlags struct{ redis, nodeID, advertise, publicURL, region string }

var webhookURLs string

// firstKey is the key webhooks are signed with: the first -keys entry, or
// the dev key.
func firstKey(keysFlag string, dev bool) string {
	for kv := range strings.SplitSeq(keysFlag, ",") {
		if k, _, ok := strings.Cut(strings.TrimSpace(kv), ":"); ok {
			return k
		}
	}
	if dev {
		return devKey
	}
	return ""
}

var clusterCfg clusterFlags

type dvrFlags struct {
	dir                   string
	window, maxWindow     time.Duration
	maxBytes              int64
	maxHeight, fetchLimit int
}

var dvrCfg dvrFlags

var qlogPath string

var (
	edgeNode  bool
	viewerCap int
	edgeTier  int
)

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hostOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		return r.Host
	}
	return host
}

func waitForFiles(ctx context.Context, log *slog.Logger, files ...string) error {
	for logged := false; ; {
		missing := ""
		for _, f := range files {
			if _, err := os.Stat(f); err != nil {
				missing = f
				break
			}
		}
		if missing == "" {
			return nil
		}
		if !logged {
			log.Info("waiting for certificate", "file", missing)
			logged = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
