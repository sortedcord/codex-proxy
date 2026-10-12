package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortedcord/codex-proxy/internal/quota"
	"github.com/sortedcord/codex-proxy/internal/storage"
)

func TestHistoryCompactionAmortizesReplacements(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	history, err := OpenHistory(dir, "a", 8)
	if err != nil {
		t.Fatal(err)
	}
	var rewrites int
	history.persist = func(path string, body []byte) error {
		rewrites++
		return storage.AtomicWrite(path, body)
	}
	base := time.Now().UTC()
	for index := range 20 {
		if err := history.Append(testSample("a", base.Add(time.Duration(index)*time.Second))); err != nil {
			t.Fatal(err)
		}
		if got := len(history.Samples()); got > 8 {
			t.Fatalf("logical retention exceeded: %d", got)
		}
	}
	if rewrites >= 12 {
		t.Fatalf("still rewrote every saturated append: replacements=%d", rewrites)
	}
	if err := history.Close(); err != nil {
		t.Fatal(err)
	}
	history, err = OpenHistory(dir, "a", 8)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	samples := history.Samples()
	if len(samples) != 8 {
		t.Fatalf("restart retained %d samples, want 8", len(samples))
	}
	if !samples[0].ObservedAt.Equal(base.Add(12 * time.Second)) {
		t.Fatal("restart restored superseded slack records")
	}
}

func TestHistoryReplacementUsesExactRecordLengths(t *testing.T) {
	history, err := OpenHistory(filepath.Join(t.TempDir(), "history"), "a", 5)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	base := time.Now().UTC()
	samples := []quota.Snapshot{
		testSample("a", base), testSample("a", base.Add(time.Second)),
		testSample("a", base.Add(2*time.Second)),
	}
	samples[0].PlanType = strings.Repeat("x", 100)
	encoded1, err := json.Marshal(samples[1])
	if err != nil {
		t.Fatal(err)
	}
	encoded2, err := json.Marshal(samples[2])
	if err != nil {
		t.Fatal(err)
	}
	limit := len(encoded1) + len(encoded2) + 2
	if err := history.replace(samples, limit); err != nil {
		t.Fatal(err)
	}
	if got := len(history.Samples()); got != 2 {
		t.Fatalf("exact trimming retained %d records, want 2", got)
	}
	info, err := os.Stat(history.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(limit) {
		t.Fatalf("journal size=%d, want %d", info.Size(), limit)
	}
}
