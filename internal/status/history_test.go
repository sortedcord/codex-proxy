package status

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/sortedcord/codex-proxy/internal/quota"
	"github.com/sortedcord/codex-proxy/internal/storage"
)

func testSample(account string, stamp time.Time) quota.Snapshot {
	return quota.Snapshot{
		AccountID: account, ObservedAt: stamp, Source: "wham/usage", Pools: map[string]quota.Limit{
			"codex": {ID: "codex", DisplayName: "Codex", FiveHour: &quota.Window{
				UsedPercent: 12, RemainingPercent: 88, WindowSeconds: 18000,
			}},
		},
	}
}

func TestHistoryPostRenameDurabilityFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	history, err := OpenHistory(dir, "account-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()

	oldStamp := time.Now().UTC()
	if err := history.Append(testSample("account-a", oldStamp)); err != nil {
		t.Fatal(err)
	}

	history.persist = func(path string, body []byte) error {
		if err := storage.AtomicWrite(path, body); err != nil {
			return err
		}
		return &storage.CommitError{Err: errors.New("simulated directory sync failure")}
	}

	if err := history.Append(testSample("account-a", oldStamp.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	newStamp := oldStamp.Add(2 * time.Second)
	err = history.Append(testSample("account-a", newStamp))
	var committed *storage.CommitError
	if !errors.As(err, &committed) {
		t.Fatalf("expected CommitError, got %v", err)
	}

	latest, available := history.Latest()
	if !available {
		t.Fatal("committed sample unavailable")
	}
	if !latest.ObservedAt.Equal(newStamp) {
		t.Fatalf("latest timestamp = %s, want %s", latest.ObservedAt, newStamp)
	}
}

func TestHistoryRetentionDurabilityAndIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	history, err := OpenHistory(dir, "account-a", 2)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	for index := range 4 {
		if err := history.Append(testSample("account-a", base.Add(time.Duration(index)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	if len(history.Samples()) != 2 {
		t.Fatal("history was not bounded")
	}
	if err := history.Append(testSample("account-b", base)); err == nil {
		t.Fatal("another account was accepted")
	}
	if _, err := OpenHistory(dir, "account-a", 2); err == nil {
		t.Fatal("two owners opened one journal")
	}
	if err := history.Close(); err != nil {
		t.Fatal(err)
	}
	history, err = OpenHistory(dir, "account-a", 2)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	samples := history.Samples()
	if len(samples) != 2 {
		t.Fatalf("retained samples after restart = %d, want 2", len(samples))
	}
	if !samples[0].ObservedAt.Equal(base.Add(2 * time.Second)) {
		t.Fatalf("oldest timestamp = %s, want %s", samples[0].ObservedAt, base.Add(2*time.Second))
	}
	info, _ := os.Stat(HistoryPath(dir, "account-a"))
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatal("journal is not private")
	}
	if HistoryPath(dir, "account-a") == HistoryPath(dir, "account-b") {
		t.Fatal("account namespaces collided")
	}
}

func TestHistoryTailRecoveryAndCompleteCorruption(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	history, err := OpenHistory(dir, "account-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := history.Append(testSample("account-a", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	_ = history.Close()
	path := HistoryPath(dir, "account-a")
	file, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = file.WriteString(`{"incomplete":`)
	_ = file.Close()
	history, err = OpenHistory(dir, "account-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Samples()) != 1 {
		t.Fatal("torn tail displaced a durable sample")
	}
	_ = history.Close()
	file, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = file.WriteString("complete-corruption\n")
	_ = file.Close()
	if _, err := OpenHistory(dir, "account-a", 10); err == nil {
		t.Fatal("complete corruption was silently removed")
	}
}

func TestHistoryRejectsSymlinksAndAccountMisattribution(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	path := HistoryPath(dir, "account-a")
	if err := os.Symlink(other, path); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := OpenHistory(dir, "account-a", 10); err == nil {
		t.Fatal("symlink history accepted")
	}
	_ = os.Remove(path)
	history, err := OpenHistory(dir, "account-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := history.Append(testSample("account-a", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	_ = history.Close()
	body, _ := os.ReadFile(path)
	if err := os.WriteFile(HistoryPath(dir, "account-b"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenHistory(dir, "account-b", 10); err == nil {
		t.Fatal("another account's journal was attributed to this account")
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
