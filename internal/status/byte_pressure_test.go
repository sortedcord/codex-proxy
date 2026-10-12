package status

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortedcord/codex-proxy/internal/quota"
	"github.com/sortedcord/codex-proxy/internal/storage"
)

func TestHistoryBytePressureLeavesAppendHeadroom(t *testing.T) {
	history, err := OpenHistory(filepath.Join(t.TempDir(), "history"), "a", 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	var rewrites int
	history.persist = func(path string, body []byte) error {
		rewrites++
		if len(body) > compactBytes {
			t.Errorf("byte-pressure compaction wrote %d bytes, target %d", len(body), compactBytes)
		}
		return storage.AtomicWrite(path, body)
	}
	balance := strings.Repeat("x", 40<<10)
	base := time.Now().UTC()
	for index := range 500 {
		sample := testSample("a", base.Add(time.Duration(index)*time.Second))
		sample.Credits = &quota.Credits{Balance: &balance}
		if err := history.Append(sample); err != nil {
			t.Fatal(err)
		}
		if history.bytes > maxHistoryBytes {
			t.Fatal("journal exceeded its physical byte cap")
		}
	}
	if rewrites == 0 {
		t.Fatal("test did not reach byte-pressure compaction")
	}
	if rewrites > 3 {
		t.Fatalf("byte pressure still rewrote every append: rewrites=%d", rewrites)
	}
	info, err := os.Stat(history.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != history.bytes {
		t.Fatalf("physical byte accounting = %d, disk = %d", history.bytes, info.Size())
	}
	latest, available := history.Latest()
	if !available {
		t.Fatal("latest observation lost during byte-pressure compaction")
	}
	if !latest.ObservedAt.Equal(base.Add(499 * time.Second)) {
		t.Fatal("compaction dropped the newest observation")
	}
}
