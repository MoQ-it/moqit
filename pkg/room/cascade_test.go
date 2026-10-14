package room

import (
	"fmt"
	"testing"

	"github.com/MoQ-it/moqit/pkg/cluster"
)

func TestPickParent(t *testing.T) {
	nodes := []cluster.NodeInfo{
		{ID: "origin"},
		{ID: "eu1", Role: cluster.RoleEdge, Tier: 1, Region: "eu"},
		{ID: "eu2", Role: cluster.RoleEdge, Tier: 1, Region: "eu"},
		{ID: "us1", Role: cluster.RoleEdge, Tier: 1, Region: "us"},
		{ID: "eu-leaf", Role: cluster.RoleEdge, Tier: 2, Region: "eu"},
	}
	seen := map[string]bool{}
	for i := range 50 {
		room := fmt.Sprint("room", i)
		p := pickParent(nodes, 1, "eu", room)
		if p != "eu1" && p != "eu2" {
			t.Fatalf("parent in eu for %s: %q", room, p)
		}
		if again := pickParent(nodes, 1, "eu", room); again != p {
			t.Fatalf("parent for %s changed: %s then %s", room, p, again)
		}
		seen[p] = true
	}
	if len(seen) != 2 {
		t.Errorf("rooms spread over %v; want both eu parents", seen)
	}
	if p := pickParent(nodes, 1, "ap", "r"); p == "" || p == "eu-leaf" || p == "origin" {
		t.Errorf("no parent in the region: picked %q; want any tier 1 edge", p)
	}
	if p := pickParent(nodes, 2, "eu", "r"); p != "eu-leaf" {
		t.Errorf("tier 2 parent: %q", p)
	}
	if p := pickParent(nodes[:1], 1, "eu", "r"); p != "" {
		t.Errorf("without edges picked %q", p)
	}
}
