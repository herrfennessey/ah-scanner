package collector

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/blizzard"
	"github.com/herrfennessey/ah-scanner/internal/config"
	"github.com/herrfennessey/ah-scanner/internal/listing"
)

func TestResolveHousesNeedsIDWhenFactionIsAmbiguous(t *testing.T) {
	idx := []blizzard.HouseRef{{ID: 113, Name: "Horde Auction House", Faction: "horde"}, {ID: 114, Name: "Horde Auction House", Faction: "horde"}, {ID: 116, Name: "Blackwater Auction House", Faction: "blackwater"}}
	if _, err := ResolveHouses(idx, []config.House{{Faction: "horde"}}); err == nil || !strings.Contains(err.Error(), "113, 114") {
		t.Fatalf("want ambiguity error listing ids, got %v", err)
	}
	hs, err := ResolveHouses(idx, []config.House{{ID: 113, Faction: "horde"}, {Faction: "blackwater"}})
	if err != nil || len(hs) != 2 || hs[0].ID != 113 || hs[1].ID != 116 {
		t.Fatalf("%+v %v", hs, err)
	}
	if _, err := ResolveHouses(idx, []config.House{{ID: 116, Faction: "horde"}}); err == nil {
		t.Fatal("want error when the pinned id has another faction")
	}
}

func TestStaleFeedDetectedAndAlertedOnce(t *testing.T) {
	s := NewStatus()
	h := House{ID: 113, Faction: "horde"}
	s.Record(h, listing.Commodity, Result{Outcome: "applied"}, nil)
	s.EndCycle(nil)
	now := time.Now()
	if p := s.Problems(3, 3*time.Hour, now); len(p) != 0 {
		t.Fatalf("fresh feed flagged: %v", p)
	}
	// Four hours of successful 304s: polls work, but no new data.
	s.Feeds[FeedKey(h, listing.Commodity)].LastFresh = now.Add(-4 * time.Hour)
	s.Feeds[FeedKey(h, listing.Commodity)].LastApplied = now.Add(-4 * time.Hour)
	s.Record(h, listing.Commodity, Result{Outcome: "not_modified"}, nil)
	p := s.Problems(3, 3*time.Hour, now)
	if len(p) != 1 || !strings.Contains(p[0], "no new data") {
		t.Fatalf("want stale problem, got %v", p)
	}
	if m := s.AlertMessage(3, 3*time.Hour, now); !strings.Contains(m, "frozen") {
		t.Fatalf("first alert: %q", m)
	}
	if m := s.AlertMessage(3, 3*time.Hour, now); m != "" {
		t.Fatalf("alerted twice: %q", m)
	}
	s.Record(h, listing.Commodity, Result{Outcome: "identical"}, nil) // server published again
	if m := s.AlertMessage(3, 3*time.Hour, now); m != "ahc: collector recovered" {
		t.Fatalf("recovery: %q", m)
	}
}

func TestFailureStreakAlert(t *testing.T) {
	s := NewStatus()
	h := House{ID: 116, Faction: "blackwater"}
	for i := 0; i < 3; i++ {
		s.Record(h, listing.Gear, Result{}, errors.New("HTTP 503"))
		s.EndCycle(errors.New("HTTP 503"))
	}
	if m := s.AlertMessage(3, 3*time.Hour, time.Now()); !strings.Contains(m, "3 consecutive failed cycles") {
		t.Fatalf("%q", m)
	}
}

func TestUndeliveredAlertIsRetried(t *testing.T) {
	s := NewStatus()
	h := House{ID: 116, Faction: "blackwater"}
	for i := 0; i < 3; i++ {
		s.Record(h, listing.Gear, Result{}, errors.New("HTTP 503"))
		s.EndCycle(errors.New("HTTP 503"))
	}
	if m := s.AlertMessage(3, 3*time.Hour, time.Now()); m == "" {
		t.Fatal("want first alert")
	}
	s.UndoAlert() // delivery failed
	if m := s.AlertMessage(3, 3*time.Hour, time.Now()); m == "" {
		t.Fatal("undelivered alert must be sent again")
	}
}
