package app

import (
	"fmt"
	"time"
)

func nextBillingOccurrence(now time.Time, intervalHours int, billingTime, timezone string) (time.Time, error) {
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Time{}, fmt.Errorf("load billing timezone %q: %w", timezone, err)
	}
	clock, err := time.Parse("15:04", billingTime)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse billing time %q: %w", billingTime, err)
	}
	if intervalHours < 1 || intervalHours > 8760 {
		return time.Time{}, fmt.Errorf("billing interval must be between 1 and 8760 hours")
	}

	localNow := now.In(location)
	next := time.Date(
		localNow.Year(), localNow.Month(), localNow.Day(),
		clock.Hour(), clock.Minute(), 0, 0, location,
	)
	for !next.After(now) {
		next = advanceBillingOccurrence(next, intervalHours, location)
	}
	return next.UTC(), nil
}

func nextBillingAfter(previous, now time.Time, intervalHours int, timezone string) (time.Time, error) {
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Time{}, fmt.Errorf("load billing timezone %q: %w", timezone, err)
	}
	if intervalHours < 1 || intervalHours > 8760 {
		return time.Time{}, fmt.Errorf("billing interval must be between 1 and 8760 hours")
	}
	next := previous.In(location)
	for !next.After(now) {
		next = advanceBillingOccurrence(next, intervalHours, location)
	}
	return next.UTC(), nil
}

func advanceBillingOccurrence(previous time.Time, intervalHours int, location *time.Location) time.Time {
	local := previous.In(location)
	if intervalHours%24 == 0 {
		return time.Date(
			local.Year(), local.Month(), local.Day()+intervalHours/24,
			local.Hour(), local.Minute(), 0, 0, location,
		)
	}
	return previous.Add(time.Duration(intervalHours) * time.Hour).In(location)
}
