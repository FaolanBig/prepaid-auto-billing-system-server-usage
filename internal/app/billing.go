package app

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type energyInterval struct {
	id       int64
	start    time.Time
	end      time.Time
	energyWh float64
	rate     *int64
}

type membership struct {
	userID int64
	start  time.Time
	end    *time.Time
}

type userBalance struct {
	name    string
	email   string
	balance int64
}

type userAllocation struct {
	energyWh float64
	cents    float64
}

func (a *App) RunBillingScheduler(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.billIfDue(ctx); err != nil {
				log.Printf("billing run failed: %v", err)
			}
		}
	}
}

func (a *App) billIfDue(ctx context.Context) error {
	tx, err := a.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var intervalHours int
	var rate *int64
	var nextBillingAt *time.Time
	var billingTime, timezone string
	if err := tx.QueryRow(ctx, `
		SELECT billing_interval_hours, price_per_kwh_micros, next_billing_at,
		       to_char(billing_time, 'HH24:MI'), billing_timezone
		FROM settings WHERE singleton = TRUE FOR UPDATE
	`).Scan(&intervalHours, &rate, &nextBillingAt, &billingTime, &timezone); err != nil {
		return err
	}

	now := time.Now().UTC()
	if rate == nil {
		return tx.Commit(ctx)
	}
	if nextBillingAt == nil {
		next, err := nextBillingOccurrence(now, intervalHours, billingTime, timezone)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE settings SET next_billing_at = $1 WHERE singleton = TRUE`, next); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if now.Before(*nextBillingAt) {
		return tx.Commit(ctx)
	}

	intervals, err := readUnbilledIntervals(ctx, tx, now)
	if err != nil {
		return err
	}
	memberships, err := readMemberships(ctx, tx, intervals)
	if err != nil {
		return err
	}
	users, err := readUserBalances(ctx, tx)
	if err != nil {
		return err
	}

	allocations := make(map[int64]userAllocation)
	var totalEnergyWh float64
	for _, energy := range intervals {
		totalEnergyWh += energy.energyWh
		intervalRate := rate
		if energy.rate != nil {
			intervalRate = energy.rate
		}
		allocateEnergy(energy, intervalRate, memberships, allocations)
	}

	periodStart := nextBillingAt.Add(-time.Duration(intervalHours) * time.Hour)
	for _, energy := range intervals {
		if energy.start.Before(periodStart) {
			periodStart = energy.start
		}
	}
	var runID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO billing_runs (period_start, period_end, energy_wh)
		VALUES ($1, $2, $3)
		RETURNING id
	`, periodStart, now, totalEnergyWh).Scan(&runID); err != nil {
		return err
	}

	userIDs := make([]int64, 0, len(allocations))
	for id, allocation := range allocations {
		if allocation.energyWh > 0 {
			userIDs = append(userIDs, id)
		}
	}
	sort.Slice(userIDs, func(i, j int) bool { return userIDs[i] < userIDs[j] })
	charges, err := roundedCharges(userIDs, allocations)
	if err != nil {
		return err
	}

	var totalCostCents int64
	for _, userID := range userIDs {
		allocation := allocations[userID]
		chargeCents := charges[userID]
		balance, ok := users[userID]
		if !ok {
			return fmt.Errorf("billing allocation references missing user %d", userID)
		}
		if chargeCents > 0 && balance.balance < (-1<<63)+chargeCents {
			return fmt.Errorf("billing charge for user %d exceeds the supported balance range", userID)
		}
		balanceAfter := balance.balance - chargeCents
		var entryID int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO billing_entries
				(billing_run_id, user_id, energy_wh, amount_cents, balance_before_cents, balance_after_cents)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id
		`, runID, userID, allocation.energyWh, chargeCents, balance.balance, balanceAfter).Scan(&entryID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET balance_cents = $1 WHERE id = $2`, balanceAfter, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO wallet_transactions (user_id, kind, amount_cents, balance_after_cents, billing_entry_id)
			VALUES ($1, 'charge', $2, $3, $4)
		`, userID, -chargeCents, balanceAfter, entryID); err != nil {
			return err
		}
		totalCostCents += chargeCents
		balance.balance = balanceAfter
		users[userID] = balance
	}
	negativeUserIDs := make([]int64, 0)
	for userID, user := range users {
		if user.balance < 0 {
			negativeUserIDs = append(negativeUserIDs, userID)
		}
	}
	sort.Slice(negativeUserIDs, func(i, j int) bool { return negativeUserIDs[i] < negativeUserIDs[j] })
	for _, userID := range negativeUserIDs {
		if err := a.queueNegativeBalanceNotices(
			ctx, tx, runID, users[userID], users[userID].balance, charges[userID],
		); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE energy_intervals SET billed_run_id = $1 WHERE id = ANY($2)`,
		runID, intervalIDs(intervals)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE billing_runs SET total_cost_cents = $1 WHERE id = $2
	`, totalCostCents, runID); err != nil {
		return err
	}
	next, err := nextBillingAfter(*nextBillingAt, now, intervalHours, timezone)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE settings SET next_billing_at = $1 WHERE singleton = TRUE
	`, next); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (event, details) VALUES ('billing_run', $1)
	`, fmt.Sprintf("run=%d; energy_wh=%.3f; cost_cents=%d", runID, totalEnergyWh, totalCostCents)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	log.Printf("billing run %d completed: %.3f Wh, %s", runID, totalEnergyWh, formatMoney(totalCostCents))
	return nil
}

func readUnbilledIntervals(ctx context.Context, tx pgx.Tx, before time.Time) ([]energyInterval, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, started_at, ended_at, energy_wh, price_per_kwh_micros
		FROM energy_intervals
		WHERE billed_run_id IS NULL AND ended_at <= $1
		ORDER BY ended_at
		FOR UPDATE
	`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var intervals []energyInterval
	for rows.Next() {
		var item energyInterval
		if err := rows.Scan(&item.id, &item.start, &item.end, &item.energyWh, &item.rate); err != nil {
			return nil, err
		}
		intervals = append(intervals, item)
	}
	return intervals, rows.Err()
}

func readMemberships(ctx context.Context, tx pgx.Tx, intervals []energyInterval) ([]membership, error) {
	if len(intervals) == 0 {
		return nil, nil
	}
	start, end := intervals[0].start, intervals[0].end
	for _, item := range intervals[1:] {
		if item.start.Before(start) {
			start = item.start
		}
		if item.end.After(end) {
			end = item.end
		}
	}
	rows, err := tx.Query(ctx, `
		SELECT user_id, started_at, ended_at
		FROM membership_periods
		WHERE started_at < $2 AND (ended_at IS NULL OR ended_at > $1)
	`, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []membership
	for rows.Next() {
		var member membership
		if err := rows.Scan(&member.userID, &member.start, &member.end); err != nil {
			return nil, err
		}
		result = append(result, member)
	}
	return result, rows.Err()
}

func readUserBalances(ctx context.Context, tx pgx.Tx) (map[int64]userBalance, error) {
	rows, err := tx.Query(ctx, `SELECT id, name, email, balance_cents FROM users`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64]userBalance)
	for rows.Next() {
		var id int64
		var user userBalance
		if err := rows.Scan(&id, &user.name, &user.email, &user.balance); err != nil {
			return nil, err
		}
		result[id] = user
	}
	return result, rows.Err()
}

func allocateEnergy(item energyInterval, rate *int64, memberships []membership, allocations map[int64]userAllocation) {
	duration := item.end.Sub(item.start)
	if duration <= 0 || item.energyWh <= 0 || rate == nil {
		return
	}
	boundaries := []time.Time{item.start, item.end}
	for _, member := range memberships {
		if member.start.After(item.start) && member.start.Before(item.end) {
			boundaries = append(boundaries, member.start)
		}
		if member.end != nil && member.end.After(item.start) && member.end.Before(item.end) {
			boundaries = append(boundaries, *member.end)
		}
	}
	sort.Slice(boundaries, func(i, j int) bool { return boundaries[i].Before(boundaries[j]) })
	unique := boundaries[:0]
	for _, boundary := range boundaries {
		if len(unique) == 0 || !boundary.Equal(unique[len(unique)-1]) {
			unique = append(unique, boundary)
		}
	}

	for i := 0; i+1 < len(unique); i++ {
		segmentStart, segmentEnd := unique[i], unique[i+1]
		segmentDuration := segmentEnd.Sub(segmentStart)
		if segmentDuration <= 0 {
			continue
		}
		midpoint := segmentStart.Add(segmentDuration / 2)
		var active []int64
		for _, member := range memberships {
			if !member.start.After(midpoint) && (member.end == nil || member.end.After(midpoint)) {
				active = append(active, member.userID)
			}
		}
		if len(active) == 0 {
			continue
		}
		energyWh := item.energyWh * segmentDuration.Seconds() / duration.Seconds() / float64(len(active))
		cents := energyWh * float64(*rate) / 10_000_000
		for _, userID := range active {
			allocation := allocations[userID]
			allocation.energyWh += energyWh
			allocation.cents += cents
			allocations[userID] = allocation
		}
	}
}

func intervalIDs(intervals []energyInterval) []int64 {
	ids := make([]int64, len(intervals))
	for i, item := range intervals {
		ids[i] = item.id
	}
	return ids
}

func roundedCharges(userIDs []int64, allocations map[int64]userAllocation) (map[int64]int64, error) {
	charges := make(map[int64]int64, len(userIDs))
	type remainder struct {
		userID   int64
		fraction float64
	}
	remainders := make([]remainder, 0, len(userIDs))
	var exactTotal float64
	var roundedDownTotal int64
	for _, userID := range userIDs {
		exact := allocations[userID].cents
		if math.IsNaN(exact) || math.IsInf(exact, 0) || exact < 0 ||
			exact >= float64(int64(1<<63-1)) {
			return nil, fmt.Errorf("billing cost for user %d exceeds the supported range", userID)
		}
		floor := int64(math.Floor(exact))
		if floor > (1<<63-1)-roundedDownTotal {
			return nil, fmt.Errorf("billing run cost exceeds the supported range")
		}
		charges[userID] = floor
		roundedDownTotal += floor
		exactTotal += exact
		if math.IsInf(exactTotal, 0) || exactTotal >= float64(int64(1<<63-1)) {
			return nil, fmt.Errorf("billing run cost exceeds the supported range")
		}
		remainders = append(remainders, remainder{userID: userID, fraction: exact - float64(floor)})
	}
	remainingCents := int64(math.Round(exactTotal)) - roundedDownTotal
	sort.Slice(remainders, func(i, j int) bool {
		if remainders[i].fraction == remainders[j].fraction {
			return remainders[i].userID < remainders[j].userID
		}
		return remainders[i].fraction > remainders[j].fraction
	})
	for i := int64(0); i < remainingCents && i < int64(len(remainders)); i++ {
		charges[remainders[i].userID]++
	}
	return charges, nil
}

func (a *App) queueNegativeBalanceNotices(
	ctx context.Context,
	tx pgx.Tx,
	runID int64,
	user userBalance,
	balanceAfter int64,
	chargeCents int64,
) error {
	subject := "Your prepaid electricity balance is negative"
	body := fmt.Sprintf("Hello %s,\n\nYour current balance is %s after billing run %d.\n",
		user.name, formatMoney(balanceAfter), runID)
	if chargeCents > 0 {
		body = fmt.Sprintf("Hello %s,\n\nAn electricity charge of %s was applied in billing run %d. Your current balance is %s.\n",
			user.name, formatMoney(chargeCents), runID, formatMoney(balanceAfter))
	}
	body += "Please contact the administrator to refill your balance and settle the outstanding amount.\n"
	recipients := []string{user.email}
	if a.cfg.AdminEmail != "" && !strings.EqualFold(a.cfg.AdminEmail, user.email) {
		recipients = append(recipients, a.cfg.AdminEmail)
	}
	for _, recipient := range recipients {
		if _, err := tx.Exec(ctx, `
			INSERT INTO notification_queue (recipient, subject, body)
			VALUES ($1, $2, $3)
		`, recipient, subject, body); err != nil {
			return err
		}
	}
	return nil
}
