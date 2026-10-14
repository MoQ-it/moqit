// Package bench load-tests moqit nodes with headless viewers over raw QUIC,
// optionally fed by a synthetic publisher, and reports what they receive.
package bench

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/MoQ-it/moqit/pkg/auth"
	"github.com/MoQ-it/moqt"
	"github.com/MoQ-it/moqt/transport/rawquic"
	v22 "github.com/MoQ-it/moqt/wire/v22"
	"github.com/quic-go/quic-go"
)

// Config describes a run.
type Config struct {
	// RouteURL is a node's /v1/route endpoint, asked once per session so
	// viewers spread over edges as browsers would. Without it every session
	// goes to URL.
	RouteURL string
	// URL is a node's WebTransport URL (https://host:port/moq); sessions use
	// raw QUIC on the same host and port.
	URL string
	// CertHash pins the node's certificate (hex SHA-256) when URL is used;
	// routes from development clusters carry their own.
	CertHash string
	// Insecure skips certificate verification.
	Insecure bool

	APIKey, APISecret string
	Room              string
	// Publisher is the identity watched. With Publish, a synthetic
	// publisher joins under it.
	Publisher string
	Publish   bool

	Viewers int
	// Rate is how many viewers connect per second (default 100).
	Rate     float64
	Duration time.Duration
	// Video is the camera rendition viewers receive: high, medium, low or
	// none.
	Video string
	// Every is how often a progress line is written (default 5s).
	Every time.Duration
}

var renditions = map[string]string{"high": "camera-h", "medium": "camera-m", "low": "camera-l"}

// Run runs a benchmark until cfg.Duration has passed or ctx ends, writing
// progress to out, and returns the totals.
func Run(ctx context.Context, cfg Config, out io.Writer) (Totals, error) {
	if cfg.Rate <= 0 {
		cfg.Rate = 100
	}
	if cfg.Every <= 0 {
		cfg.Every = 5 * time.Second
	}
	if cfg.Video == "" {
		cfg.Video = "medium"
	}
	if _, ok := renditions[cfg.Video]; !ok && cfg.Video != "none" {
		return Totals{}, fmt.Errorf("bench: unknown video quality %q", cfg.Video)
	}
	if cfg.Publisher == "" {
		return Totals{}, errors.New("bench: no publisher identity")
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Duration)
	defer cancel()
	b := &bench{cfg: cfg, stats: newStats()}

	if cfg.Publish {
		pub, err := b.publish(ctx)
		if err != nil {
			return Totals{}, err
		}
		defer pub.CloseWithError(v22.ErrNoError, "bench done")
	}

	var wg sync.WaitGroup
	go b.report(ctx, out)
	tick := time.NewTicker(time.Duration(float64(time.Second) / cfg.Rate))
	defer tick.Stop()
	for i := 0; i < cfg.Viewers; i++ {
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.watch(ctx, fmt.Sprintf("bench-viewer-%d", i)); err != nil && ctx.Err() == nil {
				b.stats.failed(err)
			}
		}()
	}
	<-ctx.Done()
	wg.Wait()
	t := b.stats.totals(cfg.Duration)
	fmt.Fprintln(out, t)
	return t, nil
}

type bench struct {
	cfg   Config
	stats *stats

	mu         sync.Mutex
	transports []*quic.Transport
	dialed     int
}

func (b *bench) token(identity string, viewer bool) (string, error) {
	g := &auth.RoomGrant{Join: !viewer, Viewer: viewer, Room: b.cfg.Room}
	return auth.NewAccessToken(b.cfg.APIKey, b.cfg.APISecret).SetIdentity(identity).SetRoomGrant(g).ToJWT()
}

// target returns where a session with token should connect.
func (b *bench) target(ctx context.Context, token string) (addr, certHash string, err error) {
	u, hash := b.cfg.URL, b.cfg.CertHash
	if b.cfg.RouteURL != "" {
		if u, hash, err = route(ctx, b.cfg.RouteURL, token); err != nil {
			return "", "", err
		}
	}
	p, err := url.Parse(u)
	if err != nil {
		return "", "", err
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

// sessionsPerSocket bounds how many connections share one UDP socket.
const sessionsPerSocket = 256

func (b *bench) transport() (*quic.Transport, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dialed%sessionsPerSocket == 0 {
		udp, err := net.ListenUDP("udp", nil)
		if err != nil {
			return nil, err
		}
		b.transports = append(b.transports, &quic.Transport{Conn: udp})
	}
	b.dialed++
	return b.transports[len(b.transports)-1], nil
}

// dial opens a MoQ session over raw QUIC with token.
func (b *bench) dial(ctx context.Context, token string, h moqt.Handler) (*moqt.Session, error) {
	addr, certHash, err := b.target(ctx, token)
	if err != nil {
		return nil, err
	}
	raddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	tr, err := b.transport()
	if err != nil {
		return nil, err
	}
	host, _, _ := net.SplitHostPort(addr)
	tc := &tls.Config{ServerName: host, NextProtos: []string{v22.ALPN}, InsecureSkipVerify: b.cfg.Insecure}
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
