package status

import (
	"errors"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/sortedcord/codex-proxy/internal/quota"
)

type Query struct {
	From   *time.Time
	To     *time.Time
	Pool   string
	Window string
	Limit  int
	Offset int
	Order  string
}

type Result struct {
	Samples []quota.Snapshot `json:"samples"`
	Total   int              `json:"total"`
	Limit   int              `json:"limit"`
	Offset  int              `json:"offset"`
	Order   string           `json:"order"`
}

func ParseQuery(raw string) (Query, error) {
	query := Query{Pool: "all", Window: "all", Limit: 100, Order: "desc"}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return query, errors.New("invalid history query")
	}
	for key, entries := range values {
		if len(entries) != 1 {
			return query, errors.New("duplicate history query parameter")
		}
		value := entries[0]
		switch key {
		case "from", "to":
			stamp, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return query, errors.New("from/to must be RFC3339 timestamps")
			}
			if key == "from" {
				query.From = &stamp
			} else {
				query.To = &stamp
			}
		case "pool":
			if value == "" || len(value) > 128 {
				return query, errors.New("invalid pool filter")
			}
			query.Pool = value
		case "window":
			if value != "all" && value != "5h" && value != "five_hour" && value != "weekly" && value != "primary" && value != "secondary" {
				return query, errors.New("window must be all, 5h, weekly, primary or secondary")
			}
			query.Window = value
		case "limit", "offset":
			number, err := strconv.Atoi(value)
			if err != nil {
				return query, errors.New("limit/offset must be integers")
			}
			if key == "limit" {
				if number < 1 || number > 1000 {
					return query, errors.New("limit must be 1 to 1000")
				}
				query.Limit = number
			} else {
				if number < 0 {
					return query, errors.New("offset must be non-negative")
				}
				query.Offset = number
			}
		case "order":
			if value != "asc" && value != "desc" {
				return query, errors.New("order must be asc or desc")
			}
			query.Order = value
		default:
			return query, errors.New("unknown history query parameter")
		}
	}
	bothBounds := query.From != nil && query.To != nil
	if bothBounds && query.From.After(*query.To) {
		return query, errors.New("from must not be later than to")
	}
	return query, nil
}

func queryHistory(samples []quota.Snapshot, query Query) Result {
	filtered := []quota.Snapshot{}
	for _, sample := range samples {
		before := query.From != nil && sample.ObservedAt.Before(*query.From)
		after := query.To != nil && sample.ObservedAt.After(*query.To)
		if before || after {
			continue
		}
		pools := map[string]quota.Limit{}
		for id, pool := range sample.Pools {
			if query.Pool != "all" && id != query.Pool {
				continue
			}
			switch query.Window {
			case "5h", "five_hour", "primary":
				if pool.FiveHour == nil {
					continue
				}
				pool.Weekly = nil
			case "weekly", "secondary":
				if pool.Weekly == nil {
					continue
				}
				pool.FiveHour = nil
			}
			pools[id] = pool
		}
		if len(pools) == 0 && (query.Pool != "all" || query.Window != "all") {
			continue
		}
		sample.Pools = pools
		filtered = append(filtered, sample)
	}
	slices.SortStableFunc(filtered, func(a, b quota.Snapshot) int {
		result := a.ObservedAt.Compare(b.ObservedAt)
		if query.Order == "desc" {
			return -result
		}
		return result
	})
	result := Result{
		Samples: []quota.Snapshot{}, Total: len(filtered), Limit: query.Limit, Offset: query.Offset, Order: query.Order,
	}
	if query.Offset >= len(filtered) {
		return result
	}
	end := query.Offset + query.Limit
	if end > len(filtered) || end < query.Offset {
		end = len(filtered)
	}
	result.Samples = append(result.Samples, filtered[query.Offset:end]...)
	return result
}
