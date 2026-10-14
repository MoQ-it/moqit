// Command moqit is the moqit CLI.
//
//	moqit token -key devkey -secret ... -room demo -identity alice
//	moqit bench -route http://node:7880/v1/route -room demo -n 1000 -publish
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/MoQ-it/moqit/internal/bench"
	"github.com/MoQ-it/moqit/pkg/auth"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "token":
		tokenCmd(os.Args[2:])
	case "bench":
		benchCmd(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: moqit token|bench [flags]   (moqit token -h, moqit bench -h for flags)")
	os.Exit(2)
}

func tokenCmd(args []string) {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	key := fs.String("key", envOr("MOQIT_API_KEY", "devkey"), "API key (env MOQIT_API_KEY)")
	secret := fs.String("secret", os.Getenv("MOQIT_API_SECRET"), "API secret (env MOQIT_API_SECRET)")
	roomName := fs.String("room", "", "room to join")
	identity := fs.String("identity", "", "participant identity")
	name := fs.String("name", "", "display name")
	metadata := fs.String("metadata", "", "participant metadata")
	publish := fs.Bool("publish", true, "may publish media")
	subscribe := fs.Bool("subscribe", true, "may subscribe")
	data := fs.Bool("data", true, "may publish data")
	sources := fs.String("sources", "", "comma-separated publish sources (camera,microphone,screen,screen-audio); empty = all")
	updateMD := fs.Bool("update-metadata", false, "may update own name/metadata/attributes")
	hidden := fs.Bool("hidden", false, "not listed in room state")
	viewer := fs.Bool("viewer", false, "read-only viewer of -room: subscribes only, never a participant")
	admin := fs.Bool("admin", false, "RoomService admin grant for -room (every room when -room is empty)")
	create := fs.Bool("create", false, "RoomService create/delete grant")
	list := fs.Bool("list", false, "RoomService list grant")
	validFor := fs.Duration("valid", 6*time.Hour, "validity")
	fs.Parse(args)

	if *secret == "" {
		if *key == "devkey" {
			*secret = "secret-for-development-only-0123456789" // matches moqit-server -dev
		} else {
			fmt.Fprintln(os.Stderr, "missing -secret")
			os.Exit(2)
		}
	}
	g := &auth.RoomGrant{
		Join: *identity != "" && !*viewer, Viewer: *viewer, Room: *roomName, Admin: *admin, Create: *create, List: *list,
		Publish: publish, Subscribe: subscribe, PublishData: data,
		UpdateOwnMetadata: *updateMD, Hidden: *hidden,
	}
	if *viewer && *roomName == "" {
		fmt.Fprintln(os.Stderr, "-viewer needs -room")
		os.Exit(2)
	}
	if *sources != "" {
		g.PublishSources = strings.Split(*sources, ",")
	}
	tok, err := auth.NewAccessToken(*key, *secret).SetIdentity(*identity).SetName(*name).SetMetadata(*metadata).
		SetRoomGrant(g).SetValidFor(*validFor).ToJWT()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(tok)
}

func benchCmd(args []string) {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	key := fs.String("key", envOr("MOQIT_API_KEY", "devkey"), "API key (env MOQIT_API_KEY)")
	secret := fs.String("secret", os.Getenv("MOQIT_API_SECRET"), "API secret (env MOQIT_API_SECRET)")
	routeURL := fs.String("route", "", "a node's /v1/route URL; each session is routed, spreading viewers over edges")
	nodeURL := fs.String("url", "", "a node's WebTransport URL (https://host:port/moq), when not routing")
	certHash := fs.String("cert-hash", "", "pin the -url node's certificate (hex SHA-256)")
	insecure := fs.Bool("insecure", false, "skip certificate verification")
	roomName := fs.String("room", "bench", "room to watch")
	publisher := fs.String("publisher", "bench-publisher", "identity of the participant watched")
	publish := fs.Bool("publish", false, "join as -publisher with synthetic audio and video")
	n := fs.Int("n", 100, "viewers")
	rate := fs.Float64("rate", 100, "viewers connecting per second")
	duration := fs.Duration("duration", time.Minute, "how long to run")
	video := fs.String("video", "medium", "camera rendition viewers receive: high, medium, low or none")
	every := fs.Duration("every", 5*time.Second, "progress interval")
	fs.Parse(args)
	if *secret == "" && *key == "devkey" {
		*secret = "secret-for-development-only-0123456789"
	}
	if *routeURL == "" && *nodeURL == "" {
		fmt.Fprintln(os.Stderr, "pass -route or -url")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	t, err := bench.Run(ctx, bench.Config{
		RouteURL: *routeURL, URL: *nodeURL, CertHash: *certHash, Insecure: *insecure,
		APIKey: *key, APISecret: *secret, Room: *roomName, Publisher: *publisher, Publish: *publish,
		Viewers: *n, Rate: *rate, Duration: *duration, Video: *video, Every: *every,
	}, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if t.Failures > 0 {
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
