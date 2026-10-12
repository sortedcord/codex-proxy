// Package status owns durable, account-scoped quota observations and polling.
package status

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/sortedcord/codex-proxy/internal/quota"
	"github.com/sortedcord/codex-proxy/internal/storage"
)

const (
	maxHistoryBytes = 16 << 20
	maxRecordBytes  = 64 << 10
	compactBytes    = maxHistoryBytes * 4 / 5
)

type History struct {
	mu         sync.Mutex
	path       string
	accountID  string
	maxSamples int
	bytes      int64
	records    int
	samples    []quota.Snapshot
	lock       *storage.Lock
	fault      error
	closed     bool
	persist    func(string, []byte) error
}

func HistoryPath(dir, accountID string) string {
	digest := sha256.Sum256([]byte(accountID))
	return filepath.Join(dir, fmt.Sprintf("usage-%x.jsonl", digest))
}

func OpenHistory(dir, accountID string, maxSamples int) (*History, error) {
	if accountID == "" || maxSamples < 1 || maxSamples > storage.MaxHistorySamples {
		return nil, errors.New("history requires an account and bounded sample limit")
	}
	if err := storage.PrivateDir(dir); err != nil {
		return nil, err
	}
	path := HistoryPath(dir, accountID)
	lock, err := storage.Acquire(path + ".lock")
	if err != nil {
		return nil, err
	}
	history := &History{
		path:       path,
		accountID:  accountID,
		maxSamples: maxSamples,
		lock:       lock,
		samples:    []quota.Snapshot{},
		persist:    storage.AtomicWrite,
	}
	if err := history.load(); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	return history, nil
}

// Physical journals may retain 25% superseded records between compactions, while
// API-visible samples remain strictly bounded. The byte cap is always enforced.
func (h *History) recordLimit() int {
	return h.maxSamples + max(1, h.maxSamples/4)
}

func (h *History) load() (loadErr error) {
	if err := storage.InspectFile(h.path); err != nil {
		return err
	}
	if _, err := os.Stat(h.path); errors.Is(err, os.ErrNotExist) {
		if err := h.persist(h.path, []byte{}); err != nil {
			return err
		}
	}
	file, err := os.OpenFile(h.path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); !errors.Is(err, os.ErrClosed) {
			loadErr = errors.Join(loadErr, err)
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > maxHistoryBytes {
		return errors.New("quota history exceeds the 16 MiB limit")
	}
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if err := h.readRecords(file); err != nil {
		return err
	}
	if h.records > h.recordLimit() {
		// Windows requires closing the old file before replacing it.
		if err := file.Close(); err != nil {
			return err
		}
		return h.replace(h.samples, maxHistoryBytes)
	}
	return nil
}

func (h *History) readRecords(file *os.File) error {
	reader := bufio.NewReader(io.LimitReader(file, maxHistoryBytes+1))
	tail := snapshotTail{limit: h.maxSamples, samples: []quota.Snapshot{}}
	var offset int64
	var count int
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if len(line) > maxRecordBytes {
			return errors.New("quota history record exceeds size limit")
		}
		if len(line) > 0 {
			if line[len(line)-1] != '\n' {
				// Recover only an incomplete final line, never complete corruption.
				if err := file.Truncate(offset); err != nil {
					return err
				}
				if err := file.Sync(); err != nil {
					return err
				}
				break
			}
			var sample quota.Snapshot
			if err := json.Unmarshal(line, &sample); err != nil {
				return errors.New("corrupt quota history record")
			}
			if err := h.validate(sample); err != nil {
				return err
			}
			tail.add(sample)
			offset += int64(len(line))
			count++
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	h.samples, h.bytes, h.records = tail.ordered(), offset, count
	return nil
}

// Startup retains only the newest logical samples even when the physical journal
// contains compaction slack. Ring insertion avoids shifting for every old record.
type snapshotTail struct {
	limit   int
	oldest  int
	samples []quota.Snapshot
}

func (t *snapshotTail) add(sample quota.Snapshot) {
	if len(t.samples) < t.limit {
		t.samples = append(t.samples, sample)
		return
	}
	t.samples[t.oldest] = sample
	t.oldest = (t.oldest + 1) % t.limit
}

func (t *snapshotTail) ordered() []quota.Snapshot {
	return append(append([]quota.Snapshot{}, t.samples[t.oldest:]...), t.samples[:t.oldest]...)
}

func (h *History) validate(sample quota.Snapshot) error {
	invalidIdentity := sample.Scope() != h.accountID || sample.ObservedAt.IsZero()
	invalidSchema := sample.Source != "wham/usage" || sample.Pools == nil
	if invalidIdentity || invalidSchema {
		return errors.New("quota history identity or schema mismatch")
	}
	if len(sample.Pools) > 129 || len(sample.PlanType) > 128 {
		return errors.New("invalid quota history dimensions")
	}
	for id, pool := range sample.Pools {
		if id == "" || id != pool.ID || len(id) > 128 || len(pool.DisplayName) > 256 {
			return errors.New("invalid quota history family")
		}
		for _, window := range []*quota.Window{pool.Primary, pool.Secondary} {
			if window == nil {
				continue
			}
			invalidPercentage := window.UsedPercent < 0 || window.UsedPercent > 100
			// encoding/json round-trips float64 bits; recomputing the same
			// subtraction after decoding preserves the original result.
			consistent := window.RemainingPercent == 100-window.UsedPercent
			if invalidPercentage || !consistent || window.WindowSeconds <= 0 {
				return errors.New("invalid quota history measurement")
			}
		}
	}
	return nil
}

func (h *History) Append(sample quota.Snapshot) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return errors.New("quota history is closed")
	}
	if h.fault != nil {
		return h.fault
	}
	if err := h.validate(sample); err != nil {
		return err
	}
	encoded, err := json.Marshal(sample)
	if err != nil {
		return errors.New("cannot encode quota history")
	}
	encoded = append(encoded, '\n')
	if len(encoded) > maxRecordBytes {
		return errors.New("quota observation exceeds size limit")
	}
	bytePressure := h.bytes+int64(len(encoded)) > maxHistoryBytes
	if h.records >= h.recordLimit() || bytePressure {
		samples := append(append([]quota.Snapshot{}, h.samples...), sample)
		if len(samples) > h.maxSamples {
			samples = samples[len(samples)-h.maxSamples:]
		}
		limit := maxHistoryBytes
		if bytePressure {
			limit = compactBytes
		}
		return h.replace(samples, limit)
	}
	if err := storage.InspectFile(h.path); err != nil {
		return err
	}
	file, err := os.OpenFile(h.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(encoded)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		h.fault = errors.New("quota history append durability failure; restart after checking storage")
		return h.fault
	}
	h.samples = append(h.samples, sample)
	if len(h.samples) > h.maxSamples {
		h.samples = h.samples[len(h.samples)-h.maxSamples:]
	}
	h.records++
	h.bytes += int64(len(encoded))
	return nil
}

func (h *History) replace(samples []quota.Snapshot, byteLimit int) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	ends := make([]int, 0, len(samples))
	for _, sample := range samples {
		if err := encoder.Encode(sample); err != nil {
			return err
		}
		ends = append(ends, buffer.Len())
	}
	// Encode once and use exact record lengths, not average-size estimates.
	var start, offset int
	for buffer.Len()-offset > byteLimit && start < len(samples)-1 {
		offset = ends[start]
		start++
	}
	samples = samples[start:]
	body := buffer.Bytes()[offset:]
	err := h.persist(h.path, body)
	var committed *storage.CommitError
	if err != nil && !errors.As(err, &committed) {
		return err
	}
	h.samples = append([]quota.Snapshot{}, samples...)
	h.bytes, h.records = int64(len(body)), len(samples)
	if err != nil {
		h.fault = errors.New("quota history replacement durability failure")
	}
	return err
}

func (h *History) Latest() (quota.Snapshot, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.samples) == 0 {
		return quota.Snapshot{}, false
	}
	return h.samples[len(h.samples)-1], true
}

func (h *History) Samples() []quota.Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]quota.Snapshot{}, h.samples...)
}

func (h *History) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	return h.lock.Close()
}
