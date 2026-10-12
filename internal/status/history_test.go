package status

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sortedcord/codex-proxy/internal/quota"
)

func testSample(account string, stamp time.Time) quota.Snapshot {
	return quota.Snapshot{
		AccountID: account, ObservedAt: stamp, Source: "wham/usage", Pools: map[string]quota.Limit{
			"codex": {ID: "codex", DisplayName: "Codex", Primary: &quota.Window{
				UsedPercent: 12, RemainingPercent: 88, WindowSeconds: 18000,
			}},
		},
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
	if len(samples) != 2 || !samples[0].ObservedAt.Equal(base.Add(2*time.Second)) {
		t.Fatal("history did not survive restart")
	}
	info, _ := os.Stat(HistoryPath(dir, "account-a"))
	if info.Mode().Perm() != 0600 {
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
	file, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
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
	file, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = file.WriteString("complete-corruption\n")
	_ = file.Close()
	if _, err := OpenHistory(dir, "account-a", 10); err == nil {
		t.Fatal("complete corruption was silently removed")
	}
}

func TestHistoryRejectsSymlinksAndAccountMisattribution(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte{}, 0600); err != nil {
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
	if err := os.WriteFile(HistoryPath(dir, "account-b"), body, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenHistory(dir, "account-b", 10); err == nil {
		t.Fatal("another account's journal was attributed to this account")
	}
}

func TestHistoryQueryFiltersAndStablePagination(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	samples := []quota.Snapshot{testSample("a", base), testSample("a", base.Add(time.Second))}
	query, err := ParseQuery("pool=codex&window=primary&limit=1&offset=1&order=desc")
	if err != nil {
		t.Fatal(err)
	}
	result := queryHistory(samples, query)
	if result.Total != 2 || len(result.Samples) != 1 || !result.Samples[0].ObservedAt.Equal(base) {
		t.Fatal("history pagination failed")
	}
	for _, raw := range []string{
		"limit=0", "limit=1001", "offset=-1", "window=5h", "order=random",
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
