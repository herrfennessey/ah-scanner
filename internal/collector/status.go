package collector

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/listing"
)

// FeedStatus is the health of one house+kind feed.
type FeedStatus struct {
	LastSuccess         time.Time `json:"last_success,omitzero"`
	LastApplied         time.Time `json:"last_applied,omitzero"`
	LastFresh           time.Time `json:"last_fresh,omitzero"` // last 200 response: the server published data
	LastResult          string    `json:"last_result,omitempty"`
	LastError           string    `json:"last_error,omitempty"`
	LastErrorAt         time.Time `json:"last_error_at,omitzero"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	NAuctions           int       `json:"n_auctions,omitempty"`
	NotModifiedCount    int       `json:"not_modified_count"`
	AppliedCount        int       `json:"applied_count"`
	LagSeconds          int64     `json:"publish_lag_seconds,omitempty"` // how long after publishing we fetched it
}

// Status is written to /data/status.json after every cycle.
type Status struct {
	mu                  sync.Mutex
	UpdatedAt           time.Time              `json:"updated_at"`
	Feeds               map[string]*FeedStatus `json:"feeds"`
	LastError           string                 `json:"last_error,omitempty"`
	ConsecutiveFailures int                    `json:"consecutive_failed_cycles"`
	Cycles              int                    `json:"cycles"`
	FailedCycles        int                    `json:"failed_cycles"`
	LastMaintenance     time.Time              `json:"last_maintenance,omitzero"`
	alerted             bool                   // not persisted: a restart re-alerts an ongoing problem once
}

func NewStatus() *Status { return &Status{Feeds: map[string]*FeedStatus{}} }

// LoadStatus reads an existing status file so counters survive restarts.
func LoadStatus(path string) *Status {
	s := NewStatus()
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, s)
		if s.Feeds == nil {
			s.Feeds = map[string]*FeedStatus{}
		}
	}
	return s
}

func FeedKey(h House, k listing.Kind) string { return fmt.Sprintf("%s/%d/%s", h.Faction, h.ID, k) }

func (s *Status) Record(h House, k listing.Kind, r Result, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := FeedKey(h, k)
	f := s.Feeds[key]
	if f == nil {
		f = &FeedStatus{}
		s.Feeds[key] = f
	}
	now := time.Now().UTC()
	if err != nil {
		f.LastError = err.Error()
		f.LastErrorAt = now
		f.ConsecutiveFailures++
		f.LastResult = "error"
		return
	}
	f.LastSuccess = now
	f.LastResult = r.Outcome
	f.ConsecutiveFailures = 0
	switch r.Outcome {
	case "applied":
		f.LastApplied = now
		f.LastFresh = now
		f.NAuctions = r.NAuctions
		f.AppliedCount++
	case "identical":
		f.LastFresh = now
		f.NotModifiedCount++
	case "not_modified":
		f.NotModifiedCount++
	}
}

// KeepFeeds removes feeds that don't belong to one of the given houses.
func (s *Status) KeepFeeds(houses []House) {
	keep := map[string]bool{}
	for _, h := range houses {
		for _, k := range kinds {
			keep[FeedKey(h, k)] = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.Feeds {
		if !keep[k] {
			delete(s.Feeds, k)
		}
	}
}

// RecordLag stores how many seconds after Blizzard published a feed we fetched it.
func (s *Status) RecordLag(h House, k listing.Kind, secs int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f := s.Feeds[FeedKey(h, k)]; f != nil {
		f.LagSeconds = secs
	}
}

func (s *Status) EndCycle(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.UpdatedAt = time.Now().UTC()
	s.Cycles++
	if err != nil {
		s.FailedCycles++
		s.ConsecutiveFailures++
		s.LastError = err.Error()
		return
	}
	s.ConsecutiveFailures = 0
}

func (s *Status) SetMaintenance(t time.Time) {
	s.mu.Lock()
	s.LastMaintenance = t.UTC()
	s.mu.Unlock()
}

// Problems lists what is wrong right now: a failure streak, feeds whose polls keep failing,
// and feeds whose polls succeed but bring no new data (a frozen endpoint answers 304 forever).
func (s *Status) Problems(maxFailures int, staleAfter time.Duration, now time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var probs []string
	if maxFailures > 0 && s.ConsecutiveFailures >= maxFailures {
		probs = append(probs, fmt.Sprintf("%d consecutive failed cycles: %s", s.ConsecutiveFailures, s.LastError))
	}
	if len(s.Feeds) == 0 {
		return append(probs, "no feeds recorded yet")
	}
	keys := make([]string, 0, len(s.Feeds))
	for k := range s.Feeds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		f := s.Feeds[k]
		fresh := f.LastFresh
		if fresh.IsZero() {
			fresh = f.LastApplied
		}
		switch {
		case now.Sub(f.LastSuccess) > staleAfter:
			probs = append(probs, fmt.Sprintf("%s: no successful poll since %s (%s)", k, f.LastSuccess.Format(time.RFC3339), f.LastError))
		case now.Sub(fresh) > staleAfter:
			probs = append(probs, fmt.Sprintf("%s: polls succeed but no new data since %s; the endpoint may be frozen", k, fresh.Format(time.RFC3339)))
		}
	}
	return probs
}

// AlertMessage returns a message once when problems start, and once when they clear.
func (s *Status) AlertMessage(maxFailures int, staleAfter time.Duration, now time.Time) string {
	probs := s.Problems(maxFailures, staleAfter, now)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case len(probs) > 0 && !s.alerted:
		s.alerted = true
		return "ahc: " + strings.Join(probs, "; ")
	case len(probs) == 0 && s.alerted:
		s.alerted = false
		return "ahc: collector recovered"
	}
	return ""
}

// UndoAlert reverts the state change of the last AlertMessage, so a message that couldn't be
// delivered is retried on the next cycle instead of being lost.
func (s *Status) UndoAlert() {
	s.mu.Lock()
	s.alerted = !s.alerted
	s.mu.Unlock()
}

func (s *Status) Save(path string) error {
	s.mu.Lock()
	b, err := json.MarshalIndent(s, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
