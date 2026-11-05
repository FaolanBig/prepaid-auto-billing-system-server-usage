package app

import (
	"testing"
	"time"
)

func TestNextBillingOccurrenceUsesLocalClockAnchor(t *testing.T) {
	location, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.March, 28, 12, 0, 0, 0, location)

	next, err := nextBillingOccurrence(now, 24, "00:00", "Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	local := next.In(location)
	if local.Year() != 2026 || local.Month() != time.March || local.Day() != 29 ||
		local.Hour() != 0 || local.Minute() != 0 {
		t.Fatalf("next billing time = %s, want 2026-03-29 00:00 Europe/Berlin", local)
	}
}

func TestDailyScheduleKeepsLocalClockAcrossDST(t *testing.T) {
	location, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	previous := time.Date(2026, time.March, 29, 0, 0, 0, 0, location)

	next, err := nextBillingAfter(previous, previous, 24, "Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	local := next.In(location)
	if local.Day() != 30 || local.Hour() != 0 || local.Minute() != 0 {
		t.Fatalf("next billing time = %s, want next local midnight", local)
	}
	if elapsed := next.Sub(previous); elapsed != 23*time.Hour {
		t.Fatalf("elapsed time across DST = %s, want 23 hours while preserving local midnight", elapsed)
	}
}

func TestHourlyIntervalIsAnchoredToConfiguredClock(t *testing.T) {
	location, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.January, 5, 16, 30, 0, 0, location)

	next, err := nextBillingOccurrence(now, 6, "00:00", "Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	local := next.In(location)
	if local.Hour() != 18 || local.Minute() != 0 {
		t.Fatalf("next billing time = %s, want 18:00 Europe/Berlin", local)
	}
}

func TestBillingScheduleRejectsInvalidTimeOrTimezone(t *testing.T) {
	if _, err := nextBillingOccurrence(time.Now(), 24, "25:00", "Europe/Berlin"); err == nil {
		t.Fatal("invalid billing clock was accepted")
	}
	if _, err := nextBillingOccurrence(time.Now(), 24, "00:00", "Not/A_Timezone"); err == nil {
		t.Fatal("invalid timezone was accepted")
	}
}
