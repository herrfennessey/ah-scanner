package collector

import (
	"testing"
	"time"
)

func TestLearnOffset(t *testing.T) {
	at := func(h, m, s int64) int64 { return 1794600000/3600*3600 + h*3600 + m*60 + s }
	if got := learnOffset(nil, 1130); got != 1130 {
		t.Fatalf("default: %d", got)
	}
	// Observed: :18:49–:18:56, with one odd late publish.
	lms := []int64{at(0, 18, 55), at(1, 18, 56), at(2, 18, 49), at(3, 25, 0), at(4, 18, 55)}
	if got := learnOffset(lms, 0); got != 18*60+55 {
		t.Fatalf("median offset %s", fmtOffset(got))
	}
	// Around the hour boundary the median must not split into :00 and :59.
	wrap := []int64{at(0, 59, 50), at(1, 0, 10), at(2, 59, 55), at(3, 0, 5), at(4, 0, 0)}
	if got := learnOffset(wrap, 0); got != 0 {
		t.Fatalf("wrap offset %s", fmtOffset(got))
	}
}

func TestNextPublish(t *testing.T) {
	base := int64(1794600000) / 3600 * 3600
	off := int64(18*60 + 50)
	lm := base + off + 5 // just published at :18:55
	if got := nextPublish(lm, off); got != base+3600+off {
		t.Fatalf("next: %d, want %d", got, base+3600+off)
	}
	if nextPublish(0, off) != 0 {
		t.Fatal("no data yet should be due immediately")
	}
}

func TestFeedSchedule(t *testing.T) {
	p := followParams{}.defaults()
	base := time.Unix(1794600000/3600*3600, 0)
	off := int64(18*60 + 50)
	f := &feedState{offset: off, lastLM: base.Unix() + off}

	// Just caught a publish: next poll is 15 s after the next expected publish.
	now := base.Add(19 * time.Minute)
	f.schedule(now, true, p)
	want := time.Unix(base.Unix()+3600+off, 0).Add(15 * time.Second)
	if !f.nextPoll.Equal(want) {
		t.Fatalf("after fresh: %v, want %v", f.nextPoll, want)
	}

	// Polled on time, still 304: retry in 30 s while inside the window.
	now = time.Unix(f.expected, 0).Add(20 * time.Second)
	f.schedule(now, false, p)
	if !f.nextPoll.Equal(now.Add(30*time.Second)) || !f.inWindow(now, p) {
		t.Fatalf("in window: next %v", f.nextPoll)
	}

	// 12 minutes late: fall back to a 15-minute catch-up poll.
	now = time.Unix(f.expected, 0).Add(12 * time.Minute)
	f.schedule(now, false, p)
	if !f.nextPoll.Equal(now.Add(15*time.Minute)) || f.inWindow(now, p) {
		t.Fatalf("catch-up: next %v", f.nextPoll)
	}
}
