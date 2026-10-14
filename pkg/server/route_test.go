package server

import (
	"testing"

	"github.com/MoQ-it/moqit/pkg/cluster"
)

func TestPickEdge(t *testing.T) {
	nodes := []cluster.NodeInfo{
		{ID: "origin"},
		{ID: "full", Role: cluster.RoleEdge, MaxViewers: 10, Viewers: 10},
		{ID: "busy", Role: cluster.RoleEdge, MaxViewers: 100, Viewers: 90, Region: "eu"},
		{ID: "idle", Role: cluster.RoleEdge, MaxViewers: 100, Region: "us"},
	}
	pick := func(nodes []cluster.NodeInfo, region string) string {
		if n := pickEdge(nodes, region); n != nil {
			return n.ID
		}
		return ""
	}
	picks := map[string]int{}
	for range 2000 {
		picks[pick(nodes, "")]++
	}
	if picks["origin"] != 0 || picks["full"] != 0 {
		t.Errorf("picked a non-edge or a full edge: %v", picks)
	}
	// Spare capacity 10 vs 100.
	if picks["busy"] == 0 || picks["idle"] < 5*picks["busy"] {
		t.Errorf("picks %v; want mostly the idle edge", picks)
	}
	for range 20 {
		if got := pick(nodes, "eu"); got != "busy" {
			t.Fatalf("region eu picked %s", got)
		}
	}
	if got := pick(nodes, "mars"); got == "" {
		t.Error("an unknown region must fall back to any edge")
	}
	if got := pick(nodes[:2], ""); got != "" {
		t.Errorf("with only a full edge picked %s", got)
	}

	// Viewers go to the deepest tier of a cascade, in their region if it
	// has edges.
	cascade := []cluster.NodeInfo{
		{ID: "regional", Role: cluster.RoleEdge, Tier: 1, Region: "eu"},
		{ID: "leaf", Role: cluster.RoleEdge, Tier: 2, Region: "eu"},
		{ID: "us", Role: cluster.RoleEdge, Tier: 1, Region: "us"},
	}
	for range 20 {
		if got := pick(cascade, ""); got != "leaf" {
			t.Fatalf("picked %s; want the tier 2 edge", got)
		}
		if got := pick(cascade, "us"); got != "us" {
			t.Fatalf("in us picked %s", got)
		}
	}
}
