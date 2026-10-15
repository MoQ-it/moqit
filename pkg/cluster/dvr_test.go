package cluster

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestDvrRoster(t *testing.T) {
	mr, a, b := pair(t)
	ctx := context.Background()
	e1, err := a.NextDvrEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e2, _ := b.NextDvrEpoch(ctx)
	if e1 == 0 || e2 == e1 {
		t.Fatalf("epochs %d, %d; want distinct and non-zero", e1, e2)
	}
	start := time.Unix(1700000000, 0)
	if err := a.PutDvrEpoch(ctx, "r1", "alice", e1, start, time.Minute); err != nil {
		t.Fatal(err)
	}
	b.PutDvrEpoch(ctx, "r1", "alice", e2, start.Add(time.Hour), time.Minute)
	got, err := b.DvrEpochs(ctx, "r1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(got, func(x, y DvrEpoch) int { return int(x.Epoch) - int(y.Epoch) })
	if len(got) != 2 || got[0] != (DvrEpoch{e1, "A", start}) || got[1] != (DvrEpoch{e2, "B", start.Add(time.Hour)}) {
		t.Fatalf("roster %+v", got)
	}
	mr.FastForward(2 * time.Minute)
	if got, _ := a.DvrEpochs(ctx, "r1", "alice"); len(got) != 0 {
		t.Errorf("roster outlived its ttl: %+v", got)
	}
}
