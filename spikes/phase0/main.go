// Command phase0 is the Phase 0 de-risking harness: a WebTransport echo
// server plus a browser test page that checks transport stability, WebCodecs
// capture/encode/decode/render paths, stream-rate limits and echo
// cancellation in each target browser.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

//go:embed index.html
var indexHTML []byte

var echoEmpty *bool

func main() {
	wtAddr := flag.String("wt-addr", ":4443", "WebTransport (UDP) listen address")
	httpAddr := flag.String("http-addr", ":8080", "plain HTTP address serving the test page (secure context on localhost only)")
	httpsAddr := flag.String("https-addr", ":8443", "HTTPS address serving the test page with the same certificate (for LAN/iOS testing)")
	hosts := flag.String("hosts", "", "extra comma-separated DNS names / IPs for the self-signed certificate")
	certFile := flag.String("cert", "", "TLS certificate file (disables self-signed mode and cert hashes)")
	keyFile := flag.String("key", "", "TLS key file")
	echoEmpty = flag.Bool("echo-empty", false, "echo empty unidirectional streams (an empty server-opened stream kills Firefox 157 sessions)")
	wtFlowControl := flag.String("wt-flow-control", "full", "WebTransport session flow control: off | streams (stream limits only) | full (stream + data limits)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	var (
		tlsConf  *tls.Config
		certHash string
	)
	if *certFile != "" {
		cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			log.Error("loading certificate", "err", err)
			os.Exit(1)
		}
		tlsConf = &tls.Config{Certificates: []tls.Certificate{cert}}
	} else {
		cert, hash, err := selfSignedCert(*hosts)
		if err != nil {
			log.Error("creating certificate", "err", err)
			os.Exit(1)
		}
		tlsConf = &tls.Config{Certificates: []tls.Certificate{cert}}
		certHash = hash
		log.Info("self-signed certificate", "sha256", certHash)
	}

	_, wtPort, err := net.SplitHostPort(*wtAddr)
	if err != nil {
		log.Error("bad -wt-addr", "err", err)
		os.Exit(1)
	}

	page := http.NewServeMux()
	page.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	page.HandleFunc("/config.json", func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"url":      "https://" + net.JoinHostPort(host, wtPort) + "/wt",
			"certHash": certHash,
		})
	})
	page.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		log.Info("browser report", "ua", r.UserAgent(), "body", string(body))
	})

	go func() {
		log.Info("test page", "url", "http://localhost"+*httpAddr)
		if err := http.ListenAndServe(*httpAddr, page); err != nil {
			log.Error("http server", "err", err)
		}
	}()
	go func() {
		srv := &http.Server{Addr: *httpsAddr, Handler: page, TLSConfig: tlsConf.Clone()}
		log.Info("test page (TLS)", "addr", *httpsAddr)
		if err := srv.ListenAndServeTLS("", ""); err != nil {
			log.Error("https server", "err", err)
		}
	}()

	h3 := &http3.Server{
		Addr:      *wtAddr,
		TLSConfig: http3.ConfigureTLSConfig(tlsConf),
		QUICConfig: &quic.Config{
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
			MaxIncomingStreams:               1000,
			MaxIncomingUniStreams:            10000,
			KeepAlivePeriod:                  5 * time.Second,
			MaxIdleTimeout:                   30 * time.Second,
		},
	}
	webtransport.ConfigureHTTP3Server(h3)
	wt := &webtransport.Server{
		H3:                   h3,
		ApplicationProtocols: []string{"moqt-22"},
		CheckOrigin:          func(*http.Request) bool { return true },
	}
	switch *wtFlowControl {
	case "off":
	case "streams":
		wt.Config = &webtransport.Config{MaxIncomingStreams: 1000, MaxIncomingUniStreams: 10000}
	case "full":
		wt.Config = &webtransport.Config{MaxIncomingStreams: 1000, MaxIncomingUniStreams: 10000, MaxIncomingData: 64 << 20}
	default:
		log.Error("bad -wt-flow-control", "value", *wtFlowControl)
		os.Exit(1)
	}
	mux := http.NewServeMux()
	var sessionIDs atomic.Uint64
	mux.HandleFunc("/wt", func(w http.ResponseWriter, r *http.Request) {
		sess, err := wt.Upgrade(w, r)
		if err != nil {
			log.Warn("upgrade failed", "err", err, "ua", r.UserAgent())
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		l := log.With("session", sessionIDs.Add(1), "remote", sess.RemoteAddr(), "ua", r.UserAgent())
		go echo(sess, l)
	})
	h3.Handler = mux

	log.Info("webtransport", "addr", *wtAddr, "path", "/wt")
	if err := wt.ListenAndServe(); err != nil {
		log.Error("webtransport server", "err", err)
		os.Exit(1)
	}
}

type counters struct {
	bidi, uni, datagrams, bytes atomic.Uint64
}

// echo reflects everything the browser sends: bidirectional streams are
// echoed in place, each unidirectional stream is echoed on a new
// unidirectional stream, and datagrams are sent straight back.
func echo(sess *webtransport.Session, log *slog.Logger) {
	start := time.Now()
	log.Info("session started", "protocol", sess.SessionState().ApplicationProtocol)
	var c counters
	ctx := sess.Context()
	// The session context is cancelled without a cause; the actual close
	// error only surfaces from stream operations, so keep the first one.
	var firstErr atomic.Pointer[string]
	noteErr := func(where string, err error) {
		s := where + ": " + err.Error()
		firstErr.CompareAndSwap(nil, &s)
	}

	go func() {
		for {
			str, err := sess.AcceptStream(ctx)
			if err != nil {
				noteErr("accept bidi", err)
				return
			}
			c.bidi.Add(1)
			go func() {
				n, _ := io.Copy(str, str)
				c.bytes.Add(uint64(n))
				str.Close()
			}()
		}
	}()
	go func() {
		for {
			str, err := sess.AcceptUniStream(ctx)
			if err != nil {
				noteErr("accept uni", err)
				return
			}
			c.uni.Add(1)
			go func() {
				b, err := io.ReadAll(str)
				if err != nil {
					noteErr("read uni", err)
					return
				}
				c.bytes.Add(uint64(len(b)))
				if len(b) == 0 && !*echoEmpty {
					return
				}
				if len(b) > 13 && b[0] == kindResetProbe {
					resetProbe(sess, b[13], noteErr)
					return
				}
				out, err := sess.OpenUniStreamSync(ctx)
				if err != nil {
					noteErr("open uni", err)
					return
				}
				out.Write(b)
				out.Close()
			}()
		}
	}()
	go func() {
		for {
			b, err := sess.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			c.datagrams.Add(1)
			sess.SendDatagram(b)
		}
	}()

	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			log.Info("session alive", "elapsed", time.Since(start).Round(time.Second),
				"bidi", c.bidi.Load(), "uni", c.uni.Load(), "datagrams", c.datagrams.Load(), "bytes", c.bytes.Load())
		case <-ctx.Done():
			cause := context.Cause(ctx)
			var sessErr *webtransport.SessionError
			level := slog.LevelWarn
			if errors.As(cause, &sessErr) && sessErr.ErrorCode == 0 {
				level = slog.LevelInfo
			}
			var streamErr string
			if p := firstErr.Load(); p != nil {
				streamErr = *p
			}
			log.Log(context.Background(), level, "session ended", "elapsed", time.Since(start).Round(time.Second),
				"cause", cause, "streamErr", streamErr, "bidi", c.bidi.Load(), "uni", c.uni.Load(), "datagrams", c.datagrams.Load())
			return
		}
	}
}

const kindResetProbe = 4

// resetProbe opens a server-initiated unidirectional stream and resets it,
// as a MoQT relay does when a subgroup's delivery timeout expires.
// Variant 0 resets before any payload (header only), variant 1 after a
// partial write.
func resetProbe(sess *webtransport.Session, variant byte, noteErr func(string, error)) {
	out, err := sess.OpenUniStreamSync(sess.Context())
	if err != nil {
		noteErr("open reset probe", err)
		return
	}
	if variant == 1 {
		out.Write([]byte("partial subgroup payload"))
		time.Sleep(20 * time.Millisecond)
	}
	out.CancelWrite(1)
}

// selfSignedCert creates an ECDSA P-256 certificate valid for 13 days, as
// required for WebTransport serverCertificateHashes, and returns it with the
// base64 SHA-256 hash of its DER encoding.
func selfSignedCert(extraHosts string) (tls.Certificate, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "moqit-phase0"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(13 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	for h := range strings.SplitSeq(extraHosts, ",") {
		if h = strings.TrimSpace(h); h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	sum := sha256.Sum256(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, fmt.Sprintf("%x", sum), nil
}
