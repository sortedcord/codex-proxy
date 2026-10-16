package status

import (
	"testing"
	"time"

	"github.com/sortedcord/codex-proxy/internal/quota"
)

func testSample(account string, stamp time.Time) quota.Snapshot {
	allowed := true
	reset := stamp.Add(time.Hour)
	return quota.Snapshot{
		AccountID:   account,
		ObservedAt:  stamp,
		Source:      "test",
		IdentityKey: "test",
		Pools: map[string]quota.Limit{
			"codex": {
				DisplayName: "Codex",
				FiveHour: &quota.Window{
					UsedPercent:      0.1,
					RemainingPercent: 0.9,
					WindowSeconds:    18000,
					ResetAt:          &reset,
				},
				Allowed: &allowed,
			},
		},
	}
}

func TestHistoryQueryFiltersAndStablePagination(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	samples := []quota.Snapshot{testSample("a", base), testSample("a", base.Add(time.Second))}
	query, err := ParseQuery("pool=codex&window=5h&limit=1&offset=1&order=desc")
	if err != nil {
		t.Fatal(err)
	}
	result := queryHistory(samples, query)
	if result.Total != 2 {
		t.Fatalf("filtered total = %d, want 2", result.Total)
	}
	if len(result.Samples) != 1 {
		t.Fatalf("page size = %d, want 1", len(result.Samples))
	}
	if !result.Samples[0].ObservedAt.Equal(base) {
		t.Fatalf("page timestamp = %s, want %s", result.Samples[0].ObservedAt, base)
	}
	for _, raw := range []string{
		"limit=0", "limit=1001", "offset=-1", "window=invalid", "order=random",
		"pool=a&pool=b", "unknown=1", "from=not-a-time",
	} {
		if _, err := ParseQuery(raw); err == nil {
			t.Errorf("invalid query accepted: %s", raw)
		}
	}
}

func FuzzHistoryQuery(f *testing.F) {
	f.Add("pool=codex&limit=10&order=desc")
	f.Fuzz(func(t *testing.T, raw string) { _, _ = ParseQuery(raw) })
}
