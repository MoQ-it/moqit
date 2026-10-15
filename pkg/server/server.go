// Package server runs a moqit node: one UDP socket serving MoQ over
// WebTransport (browsers, path /moq) and raw QUIC (native clients, other
// nodes), with a room.Manager authorizing everything.
package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/MoQ-it/moqit/pkg/auth"
	"github.com/MoQ-it/moqit/pkg/cluster"
	"github.com/MoQ-it/moqit/pkg/dvr"
	"github.com/MoQ-it/moqit/pkg/room"
	"github.com/MoQ-it/moqit/pkg/service"
	"github.com/MoQ-it/moqt"
	"github.com/MoQ-it/moqt/transport/rawquic"
	"github.com/MoQ-it/moqt/transport/wt"
	v22 "github.com/MoQ-it/moqt/wire/v22"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

// Config configures a node.
type Config struct {
	// UDPAddr to listen on. Listen dual-stack (":port"): Chrome resolves
	// localhost to ::1 for WebTransport.
	UDPAddr string
	Keys    auth.KeyProvider
	// Certificate for QUIC. SelfSignedCert makes one that browsers accept
	// through serverCertificateHashes.
	Certificate tls.Certificate
	// CertFile and KeyFile, if set, are used instead of Certificate and
	// re-read when they change, so a renewed certificate needs no restart.
	CertFile, KeyFile string

	DepartureTimeout time.Duration
	MaxParticipants  int
	Logger           *slog.Logger

	// Cluster, if set, joins this node to a cluster sharing the Redis
	// behind it. Other nodes reach this one at AdvertiseAddr (host:port of
	// the UDP socket); clients are sent to PublicURL.
	Cluster       *cluster.Cluster
	AdvertiseAddr string
	PublicURL     string
	Region        string
	// ExposeCertHash adds the chosen node's certificate hash to /v1/route
	// answers, for development clusters with self-signed certificates.
	ExposeCertHash bool
	// Webhook receives room and participant events; *webhook.Notifier.
	Webhook room.Notifier

	// Edge makes the node serve read-only viewers only, fetching every
	// track from the cluster's other nodes. It needs Cluster.
	Edge bool
	// Tier places an edge in a cascade: 1 (the default) fetches from the
	// nodes hosting tracks, 2 and up from an edge one tier up, preferring
	// one in Region. Viewers are routed to the deepest tier.
	Tier int
	// MaxViewers caps viewer sessions on this node; 0 = unlimited.
	MaxViewers int
	// ViewerInterval is how often viewer counts are shared (default 5s).
	ViewerInterval time.Duration

	// DVR, if set, records rooms with a dvr_window so viewers can rewind
	// them. DVRWindow is the window of rooms created without one (0 =
	// history off), capped at DVRMaxWindow. DVRMaxHeight picks the video
	// rendition recorded; DVRFetchLimit caps a viewer's open FETCHes, on
	// edges too.
	DVR           *dvr.Store
	DVRWindow     time.Duration
	DVRMaxWindow  time.Duration
	DVRMaxHeight  int
	DVRFetchLimit int
}

// Server is a running node.
type Server struct {
	Manager *room.Manager
	// Addr is the UDP address actually bound.
	Addr net.Addr
	// CertHash is the SHA-256 of the certificate at start, for serverCertificateHashes.
	CertHash [32]byte

	publicURL      string
	edge           bool
	exposeCertHash bool
	cluster        *cluster.Cluster
	keys           auth.KeyProvider
	ln             *quic.EarlyListener
	wts            *webtransport.Server
	cancel         context.CancelFunc
}

// A client that vanishes without closing (crashed tab, lost network) is
// dropped after idleTimeout of silence; keep-alives stop a quiet but live
// connection from reaching it. (quic-go's default is 30 s.)
const (
	idleTimeout = 15 * time.Second
	keepAlive   = 5 * time.Second
)

// Start starts a node; it runs until Close or ctx ends.
func Start(ctx context.Context, cfg Config) (*Server, error) {
	if len(cfg.Keys) == 0 {
		return nil, errors.New("server: no API keys")
	}
	if cfg.Edge && cfg.Cluster == nil {
		return nil, errors.New("server: an edge node needs a cluster")
	}
	if cfg.Tier != 0 && !cfg.Edge {
		return nil, errors.New("server: only edge nodes have a tier")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	log := cfg.Logger
	var serverKey string
	for k := range cfg.Keys {
		serverKey = k
		break
	}
	var certs *certSource
	switch {
	case cfg.CertFile != "":
		var err error
		if certs, err = fileCert(cfg.CertFile, cfg.KeyFile); err != nil {
			return nil, err
		}
	case len(cfg.Certificate.Certificate) > 0:
		certs = fixedCert(cfg.Certificate)
	default:
		return nil, errors.New("server: no certificate")
	}
	certHash := certs.sum()
	ctx, cancel := context.WithCancel(ctx)
	m := room.New(room.Config{
		Keys: cfg.Keys, ServerKey: serverKey, DepartureTimeout: cfg.DepartureTimeout, MaxParticipants: cfg.MaxParticipants, Logger: log,
		Cluster:  cfg.Cluster,
		Webhook:  cfg.Webhook,
		Node:     cluster.NodeInfo{QUICAddr: cfg.AdvertiseAddr, PublicURL: cfg.PublicURL, CertSHA256: hex.EncodeToString(certHash[:]), Region: cfg.Region},
		DialPeer: dialPeer,
		Edge:     cfg.Edge, Tier: cfg.Tier, MaxViewers: cfg.MaxViewers, ViewerInterval: cfg.ViewerInterval,
		DVR: cfg.DVR, DVRWindow: cfg.DVRWindow, DVRMaxWindow: cfg.DVRMaxWindow, DVRMaxHeight: cfg.DVRMaxHeight, DVRFetchLimit: cfg.DVRFetchLimit,
	})

	tlsConf := &tls.Config{GetCertificate: certs.get, NextProtos: []string{v22.ALPN, http3.NextProtoH3}}
	qconf := &quic.Config{
		EnableDatagrams:                  true,
		EnableStreamResetPartialDelivery: true,
		MaxIncomingStreams:               1 << 12,
		MaxIncomingUniStreams:            1 << 14,
		MaxIdleTimeout:                   idleTimeout,
		KeepAlivePeriod:                  keepAlive,
	}
	h3 := &http3.Server{TLSConfig: tlsConf, QUICConfig: qconf}
	webtransport.ConfigureHTTP3Server(h3)
	wts := &webtransport.Server{H3: h3, ApplicationProtocols: []string{v22.ALPN}, CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/moq", func(w http.ResponseWriter, req *http.Request) {
		sess, err := wt.Upgrade(wts, w, req)
		if err != nil {
			log.Info("webtransport upgrade failed", "remote", req.RemoteAddr, "user_agent", req.UserAgent(), "err", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if err := m.Serve(ctx, wt.New(sess), req.URL.Query().Get("token")); err != nil {
			log.Debug("session rejected", "remote", req.RemoteAddr, "err", err)
		}
	})
	h3.Handler = mux

	ln, err := quic.ListenAddrEarly(cfg.UDPAddr, tlsConf, qconf)
	if err != nil {
		cancel()
		return nil, err
	}
	go func() {
		for {
			conn, err := ln.Accept(ctx)
			if err != nil {
				return
			}
			switch conn.ConnectionState().TLS.NegotiatedProtocol {
			case v22.ALPN:
				go func() {
					if err := m.Serve(ctx, rawquic.New(conn), ""); err != nil {
						log.Debug("session rejected", "remote", conn.RemoteAddr(), "err", err)
					}
				}()
			case http3.NextProtoH3:
				go wts.ServeQUICConn(conn)
			default:
				conn.CloseWithError(0x10, "unsupported ALPN")
			}
		}
	}()

	// The manager's own session dials this node over loopback, pinning our
	// certificate.
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	err = m.Start(ctx, func(ctx context.Context) (moqt.Conn, error) {
		h := certs.sum()
		return dialPeer(ctx, net.JoinHostPort("127.0.0.1", port), hex.EncodeToString(h[:]))
	})
	if err != nil {
		ln.Close()
		cancel()
		return nil, err
	}
	go certs.watch(ctx, log, func(h [32]byte) { m.SetCertSHA256(hex.EncodeToString(h[:])) })
	return &Server{Manager: m, Addr: ln.Addr(), CertHash: certHash, publicURL: cfg.PublicURL, edge: cfg.Edge, exposeCertHash: cfg.ExposeCertHash, cluster: cfg.Cluster, keys: cfg.Keys, ln: ln, wts: wts, cancel: cancel}, nil
}

// dialPeer opens a raw QUIC MoQ connection to a node, accepting only the
// certificate with the given SHA-256: node certificates are often
// self-signed, and the cluster registry vouches for them.
func dialPeer(ctx context.Context, addr, certSHA256 string) (moqt.Conn, error) {
	want, err := hex.DecodeString(certSHA256)
	if err != nil {
		return nil, err
	}
	c, err := quic.DialAddr(ctx, addr, &tls.Config{
		InsecureSkipVerify: true, // replaced by the pin below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no certificate")
			}
			if sum := sha256.Sum256(raw[0]); !bytes.Equal(sum[:], want) {
				return errors.New("node certificate does not match the cluster registry")
			}
			return nil
		},
		NextProtos: []string{v22.ALPN},
	}, &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true, MaxIncomingUniStreams: 1 << 14, MaxIdleTimeout: idleTimeout, KeepAlivePeriod: keepAlive})
	if err != nil {
		return nil, err
	}
	return rawquic.New(c), nil
}

// APIHandler serves the node's HTTP API: the RoomService (Connect, at
// /moqit.v1.RoomService/), GET /v1/route and GET /healthz.
func (s *Server) APIHandler() http.Handler {
	mux := http.NewServeMux()
	path, h := service.NewRoomService(s.Manager, s.keys)
	mux.Handle(path, h)
	mux.Handle("GET /v1/route", s.RouteHandler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	return mux
}

// RouteHandler serves GET /v1/route?token=... (or Authorization: Bearer):
// it returns {"url": ...}, the WebTransport URL of the node the client
// should join. Participants go to a node already hosting the token's room,
// otherwise the least loaded, never an edge. Viewers go to an edge with
// spare capacity, preferring ?region=, or else where a participant would.
// Without a cluster it returns this node.
func (s *Server) RouteHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		token := r.URL.Query().Get("token")
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token = strings.TrimPrefix(h, "Bearer ")
		}
		claims, err := s.keys.Verify(token)
		if err != nil || claims.Grants.Room == nil || (!claims.Grants.Room.Join && !claims.IsViewer()) {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		out := map[string]string{"url": s.publicURL}
		if s.exposeCertHash {
			out["certHash"] = hex.EncodeToString(s.CertHash[:])
		}
		if s.cluster != nil {
			n, err := s.route(r.Context(), claims, r.URL.Query().Get("region"))
			switch {
			case n != nil:
				out["url"] = n.PublicURL
				if s.exposeCertHash {
					out["certHash"] = n.CertSHA256
				}
			case s.edge && !claims.IsViewer():
				if err == nil {
					err = errors.New("no node hosts participants")
				}
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
}

func (s *Server) route(ctx context.Context, claims *auth.Claims, region string) (*cluster.NodeInfo, error) {
	nodes, err := s.cluster.Nodes(ctx)
	if err != nil || len(nodes) == 0 {
		return nil, err
	}
	if claims.IsViewer() {
		if n := pickEdge(nodes, region); n != nil {
			return n, nil
		}
	}
	return s.pickNode(ctx, nodes, claims.Grants.Room.Room)
}

// unlimitedEdge is the capacity routing assumes for an edge without
// MaxViewers.
const unlimitedEdge = 10000

// pickEdge picks an edge of the deepest tier at random, weighted by its
// spare capacity, so a burst of viewers spreads out even though the load
// figures lag.
func pickEdge(nodes []cluster.NodeInfo, region string) *cluster.NodeInfo {
	var edges, local []cluster.NodeInfo
	for _, n := range nodes {
		if n.Role != cluster.RoleEdge || (n.MaxViewers > 0 && n.Viewers >= n.MaxViewers) {
			continue
		}
		edges = append(edges, n)
		if region != "" && n.Region == region {
			local = append(local, n)
		}
	}
	if len(local) > 0 {
		edges = local
	}
	deepest := 0
	for _, n := range edges {
		deepest = max(deepest, n.Tier)
	}
	edges = slices.DeleteFunc(edges, func(n cluster.NodeInfo) bool { return n.Tier != deepest })
	spare := func(n cluster.NodeInfo) int {
		c := n.MaxViewers
		if c == 0 {
			c = unlimitedEdge
		}
		return max(c-n.Viewers, 1)
	}
	total := 0
	for _, n := range edges {
		total += spare(n)
	}
	if total == 0 {
		return nil
	}
	x := mrand.IntN(total)
	for i, n := range edges {
		if x -= spare(n); x < 0 {
			return &edges[i]
		}
	}
	return nil
}

func (s *Server) pickNode(ctx context.Context, nodes []cluster.NodeInfo, room string) (*cluster.NodeInfo, error) {
	nodes = slices.DeleteFunc(nodes, func(n cluster.NodeInfo) bool { return n.Role == cluster.RoleEdge })
	if len(nodes) == 0 {
		return nil, nil
	}
	_, ps, err := s.cluster.Participants(ctx, room)
	if err != nil {
		return nil, err
	}
	hosting := map[string]bool{}
	for _, p := range ps {
		hosting[p.NodeId] = true
	}
	slices.SortFunc(nodes, func(a, b cluster.NodeInfo) int {
		if hosting[a.ID] != hosting[b.ID] {
			if hosting[a.ID] {
				return -1
			}
			return 1
		}
		return a.Participants - b.Participants
	})
	return &nodes[0], nil
}

// Port returns the UDP port.
func (s *Server) Port() string {
	_, port, _ := net.SplitHostPort(s.Addr.String())
	return port
}

func (s *Server) Close() error {
	s.cancel()
	s.wts.Close()
	return s.ln.Close()
}

// SelfSignedCert makes a certificate browsers accept through WebTransport
// serverCertificateHashes: ECDSA P-256, valid for under 14 days.
func SelfSignedCert(hosts string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "moqit-dev"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(13 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for h := range strings.SplitSeq(hosts, ",") {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
