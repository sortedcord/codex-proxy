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
)

type History struct {
	mu         sync.Mutex
	path       string
	accountID  string
	maxSamples int
	bytes      int64
	samples    []quota.Snapshot
	lock       *storage.Lock
	fault      error
	closed     bool
}

func HistoryPath(dir, accountID string) string {
	digest := sha256.Sum256([]byte(accountID))
	return filepath.Join(dir, fmt.Sprintf("usage-%x.jsonl", digest))
}

func OpenHistory(dir, accountID string, maxSamples int) (*History, error) {
	if accountID == "" || maxSamples < 1 || maxSamples > 10000 {
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
		path: path, accountID: accountID, maxSamples: maxSamples, lock: lock, samples: []quota.Snapshot{},
	}
	if err := history.load(); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return history, nil
}

func (h *History) load() error {
	if err := storage.InspectFile(h.path); err != nil {
		return err
	}
	if _, err := os.Stat(h.path); errors.Is(err, os.ErrNotExist) {
		if err := storage.AtomicWrite(h.path, []byte{}); err != nil {
			return err
		}
	}
	file, err := os.OpenFile(h.path, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > maxHistoryBytes {
		return errors.New("quota history exceeds the 16 MiB limit")
	}
	if err := file.Chmod(0600); err != nil {
		return err
	}
	reader := bufio.NewReader(io.LimitReader(file, maxHistoryBytes+1))
	var offset int64
	var oldest int
	compact := false
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
				if err := file.Truncate(offset); err != nil {
					return err
				}
				if err := file.Sync(); err != nil {
					return err
				}
				break // Recover only an incomplete last record, never complete corruption.
			}
			sample := quota.Snapshot{}
			if err := json.Unmarshal(line, &sample); err != nil {
				return errors.New("corrupt quota history record")
			}
			if err := h.validate(sample); err != nil {
				return err
			}
			if len(h.samples) < h.maxSamples {
				h.samples = append(h.samples, sample)
			} else {
				h.samples[oldest] = sample
				oldest = (oldest + 1) % h.maxSamples
				compact = true
			}
			offset += int64(len(line))
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	h.bytes = offset
	if compact {
		h.samples = append(append([]quota.Snapshot{}, h.samples[oldest:]...), h.samples[:oldest]...)
		// Close the old file before replacing it (required on Windows).
		if err := file.Close(); err != nil {
			return err
		}
		return h.replace(h.samples)
	}
	return nil
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
	full := len(h.samples) >= h.maxSamples || h.bytes+int64(len(encoded)) > maxHistoryBytes
	if full {
		samples := append(append([]quota.Snapshot{}, h.samples...), sample)
		for len(samples) > h.maxSamples {
			samples = samples[1:]
		}
		return h.replace(samples)
	}
	if err := storage.InspectFile(h.path); err != nil {
		return err
	}
	file, err := os.OpenFile(h.path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(encoded)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		h.fault = errors.New("quota history append durability failure; restart after checking storage")
		return h.fault
	}
	h.samples = append(h.samples, sample)
	h.bytes += int64(len(encoded))
	return nil
}

func (h *History) replace(samples []quota.Snapshot) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	for _, sample := range samples {
		if err := encoder.Encode(sample); err != nil {
			return err
		}
	}
	for buffer.Len() > maxHistoryBytes && len(samples) > 1 {
		samples = samples[1:]
		buffer.Reset()
		for _, sample := range samples {
			if err := encoder.Encode(sample); err != nil {
				return err
			}
		}
	}
	if err := storage.AtomicWrite(h.path, buffer.Bytes()); err != nil {
		var committed *storage.CommitError
		if errors.As(err, &committed) {
			h.fault = errors.New("quota history replacement durability failure")
		}
		return err
	}
	h.samples = append([]quota.Snapshot{}, samples...)
	h.bytes = int64(buffer.Len())
	return nil
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
