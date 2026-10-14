// Command moqit is the moqit CLI.
//
//	moqit token -key devkey -secret ... -room demo -identity alice
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/MoQ-it/moqit/pkg/auth"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "token":
		tokenCmd(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: moqit token [flags]   (moqit token -h for flags)")
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
		Join: *identity != "", Room: *roomName, Admin: *admin, Create: *create, List: *list,
		Publish: publish, Subscribe: subscribe, PublishData: data,
		UpdateOwnMetadata: *updateMD, Hidden: *hidden,
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

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
