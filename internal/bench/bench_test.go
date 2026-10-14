package bench_test

import (
	"bytes"
	"context"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MoQ-it/moqit/internal/bench"
	"github.com/MoQ-it/moqit/pkg/auth"
	"github.com/MoQ-it/moqit/pkg/cluster"
	"github.com/MoQ-it/moqit/pkg/server"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

const apiKey, apiSecret = "benchkey", "bench-secret-0123456789abcdef0123456"

func startNode(t *testing.T, mr *miniredis.Miniredis, id string, edge bool) *server.Server {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	cert, err := server.SelfSignedCert("localhost,127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	pc.Close()
	srv, err := server.Start(context.Background(), server.Config{
		UDPAddr: addr, Keys: auth.KeyProvider{apiKey: apiSecret}, Certificate: cert, ExposeCertHash: true,
		Cluster: cluster.New(rdb, id, 3*time.Second, nil), AdvertiseAddr: addr, PublicURL: "https://" + addr + "/moq", Edge: edge,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

// A synthetic publisher on the origin, viewers routed to the edge.
func TestBenchThroughEdge(t *testing.T) {
	mr := miniredis.RunT(t)
	origin := startNode(t, mr, "A", false)
	startNode(t, mr, "E", true)
	api := httptest.NewServer(origin.APIHandler())
	defer api.Close()

	var out bytes.Buffer
	tot, err := bench.Run(context.Background(), bench.Config{
		RouteURL: api.URL + "/v1/route", APIKey: apiKey, APISecret: apiSecret,
		Room: "r1", Publisher: "pub", Publish: true, Viewers: 20, Rate: 200, Duration: 4 * time.Second, Video: "low", Every: time.Second,
	}, &out)
	t.Log(out.String())
	if err != nil {
		t.Fatal(err)
	}
	if tot.PeakViewers != 20 || tot.Failures != 0 {
		t.Errorf("viewers %d, failures %d (%s)", tot.PeakViewers, tot.Failures, tot.LastError)
	}
	// 20 viewers for about 3s of audio at 50 objects/s, plus video.
	if tot.Objects < 20*50 || tot.LatencyMax == 0 {
		t.Errorf("received %d objects, max latency %v", tot.Objects, tot.LatencyMax)
	}
	if origin.Manager.Viewers() != 0 {
		t.Errorf("origin served %d viewers; want all on the edge", origin.Manager.Viewers())
	}
}
