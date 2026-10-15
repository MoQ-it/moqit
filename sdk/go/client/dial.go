package client

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/MoQ-it/moqt"
	"github.com/MoQ-it/moqt/transport/rawquic"
	v22 "github.com/MoQ-it/moqt/wire/v22"
	"github.com/quic-go/quic-go"
)

// Config says where sessions go.
type Config struct {
	// RouteURL is a node's /v1/route endpoint, asked for each session.
	// Without it every session goes to URL.
	RouteURL string
	// URL is a node's WebTransport URL (https://host:port/moq); sessions use
	// raw QUIC on the same host and port.
	URL string
	// CertHash pins the node's certificate (hex SHA-256) when URL is used;
	// routes from development clusters carry their own.
	CertHash string
	// Insecure skips certificate verification.
	Insecure bool
	// Log receives a participant's warnings, and which tracks are
	// subscribed and sent; nil discards them.
	Log *slog.Logger
}

// Target returns the address and pinned certificate hash of the node a
// session with token should connect to.
func (c Config) Target(ctx context.Context, token string) (addr, certHash string, err error) {
	u, hash := c.URL, c.CertHash
	if c.RouteURL != "" {
		if u, hash, err = route(ctx, c.RouteURL, token); err != nil {
			return "", "", err
		}
	}
	p, err := url.Parse(u)
	if err != nil {
		return "", "", err
	}
	if p.Host == "" {
		return "", "", fmt.Errorf("dial: no host in %q", u)
	}
	return p.Host, hash, nil
}

func route(ctx context.Context, endpoint, token string) (string, string, error) {
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

// Dial opens a MoQ session with token, sending it in SETUP. With a nil
// transport the session gets its own UDP socket, closed when it ends.
func Dial(ctx context.Context, tr *quic.Transport, cfg Config, token string, h moqt.Handler) (*moqt.Session, error) {
	addr, certHash, err := cfg.Target(ctx, token)
	if err != nil {
		return nil, err
	}
	raddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	own := tr == nil
	if own {
		udp, err := net.ListenUDP("udp", nil)
		if err != nil {
			return nil, err
		}
		tr = &quic.Transport{Conn: udp}
	}
	sess, err := dial(ctx, tr, raddr, addr, certHash, cfg.Insecure, token, h)
	if own {
		if err != nil {
			tr.Close()
		} else {
			go func() {
				<-sess.Context().Done()
				tr.Close()
			}()
		}
	}
	return sess, err
}

func dial(ctx context.Context, tr *quic.Transport, raddr *net.UDPAddr, addr, certHash string, insecure bool, token string, h moqt.Handler) (*moqt.Session, error) {
	host, _, _ := net.SplitHostPort(addr)
	tc := &tls.Config{ServerName: host, NextProtos: []string{v22.ALPN}, InsecureSkipVerify: insecure}
	if certHash != "" {
		tc.InsecureSkipVerify = true
		tc.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no certificate")
			}
			if sum := sha256.Sum256(raw[0]); hex.EncodeToString(sum[:]) != certHash {
				return errors.New("certificate hash mismatch")
			}
			return nil
		}
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	qc, err := tr.Dial(dctx, raddr, tc, &quic.Config{EnableDatagrams: true, MaxIncomingUniStreams: 1 << 14, KeepAlivePeriod: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	opt := v22.Token{AliasType: v22.AliasUseValue, Value: []byte(token)}
	return moqt.NewSession(dctx, rawquic.New(qc), moqt.Client, moqt.Config{
		Handler:      h,
		SetupOptions: v22.KeyValues{{Type: v22.OptAuthorizationToken, Bytes: opt.Append(nil)}},
	})
}
