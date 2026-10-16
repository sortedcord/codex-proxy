// Package status owns account-scoped quota observations and polling.
package status

import (
	"github.com/sortedcord/codex-proxy/internal/quota"
)

// History is an in-memory holder for status compatibility without disk persistence.
type History struct {
	latest    quota.Snapshot
	available bool
}

// HistoryPath returns an empty string as disk history storage is disabled.
func HistoryPath(dir, accountID string) string {
	return ""
}

// OpenHistory returns an in-memory history holder without touching disk.
func OpenHistory(dir, accountID string, maxSamples int) (*History, error) {
	return &History{}, nil
}

func (h *History) Append(sample quota.Snapshot) error {
	h.latest = sample
	h.available = true
	return nil
}

func (h *History) Latest() (quota.Snapshot, bool) {
	return h.latest, h.available
}

func (h *History) Samples() []quota.Snapshot {
	if !h.available {
		return nil
	}
	return []quota.Snapshot{h.latest}
}

func (h *History) Close() error {
	return nil
}
