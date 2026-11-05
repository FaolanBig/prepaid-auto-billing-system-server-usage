package app

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type dashboardData struct {
	ActiveUsers      int
	Energy24hKWh     float64
	PowerW           float64
	RateText         string
	IntervalHours    int
	LastSample       string
	NextBilling      string
	PendingEmails    int
	ShellyConfigured bool
	RecentRuns       []billingRunRow
}

type billingRunRow struct {
	PeriodStart string
	PeriodEnd   string
	EnergyKWh   float64
	CostCents   int64
}

type userRow struct {
	ID           int64
	Name         string
	Email        string
	BalanceCents int64
	Active       bool
}

type usersData struct {
	Users []userRow
}

type newUserData struct {
	Name    string
	Email   string
	Deposit string
}

type settingsData struct {
	RateValue     string
	IntervalHours int
	NextBilling   string
	PollSeconds   int
}

type auditEvent struct {
	CreatedAt string
	Event     string
	Details   string
}

type logsData struct {
	Events        []auditEvent
	Emails        []emailEvent
	PendingEmails int
	FailedEmails  int
}

type emailEvent struct {
	Recipient string
	Attempts  int
	Status    string
	Error     string
	CreatedAt string
}

func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	data := dashboardData{
		RateText:         "not configured",
		ShellyConfigured: a.cfg.ShellyURL != "",
	}

	if err := a.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE is_active`).Scan(&data.ActiveUsers); err != nil {
		a.serverError(w, "load active users", err)
		return
	}
	if err := a.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(energy_wh), 0)
		FROM energy_intervals
		WHERE ended_at >= NOW() - INTERVAL '24 hours'
	`).Scan(&data.Energy24hKWh); err != nil {
		a.serverError(w, "load recent energy", err)
		return
	}
	data.Energy24hKWh /= 1000

	var rate *int64
	var next *time.Time
	if err := a.db.QueryRow(ctx, `
		SELECT price_per_kwh_micros, billing_interval_hours, next_billing_at
		FROM settings WHERE singleton = TRUE
	`).Scan(&rate, &data.IntervalHours, &next); err != nil {
		a.serverError(w, "load billing settings", err)
		return
	}
	if rate != nil {
		data.RateText = "€" + formatRate(*rate) + " / kWh"
	}
	data.NextBilling = formatOptionalDate(next)

	var lastSample *time.Time
	if err := a.db.QueryRow(ctx, `
		SELECT power_w, sampled_at FROM device_state WHERE singleton = TRUE
	`).Scan(&data.PowerW, &lastSample); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		a.serverError(w, "load latest Shelly sample", err)
		return
	}
	data.LastSample = formatOptionalDate(lastSample)
	if err := a.db.QueryRow(ctx, `SELECT COUNT(*) FROM notification_queue WHERE sent_at IS NULL`).Scan(&data.PendingEmails); err != nil {
		a.serverError(w, "load pending emails", err)
		return
	}

	rows, err := a.db.Query(ctx, `
		SELECT period_start, period_end, energy_wh, total_cost_cents
		FROM billing_runs ORDER BY created_at DESC LIMIT 10
	`)
	if err != nil {
		a.serverError(w, "load billing history", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var run billingRunRow
		var start, end time.Time
		if err := rows.Scan(&start, &end, &run.EnergyKWh, &run.CostCents); err != nil {
			a.serverError(w, "read billing history", err)
			return
		}
		run.EnergyKWh /= 1000
		run.PeriodStart = formatDate(start)
		run.PeriodEnd = formatDate(end)
		data.RecentRuns = append(data.RecentRuns, run)
	}
	if err := rows.Err(); err != nil {
		a.serverError(w, "read billing history", err)
		return
	}

	csrf := a.csrfFromRequest(r)
	a.render(w, "dashboard", "Dashboard", csrf, data, "", "")
}

func (a *App) usersPage(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(r.Context(), `
		SELECT id, name, email, balance_cents, is_active
		FROM users ORDER BY is_active DESC, LOWER(name), id
	`)
	if err != nil {
		a.serverError(w, "load users", err)
		return
	}
	defer rows.Close()
	data := usersData{Users: []userRow{}}
	for rows.Next() {
		var user userRow
		if err := rows.Scan(&user.ID, &user.Name, &user.Email, &user.BalanceCents, &user.Active); err != nil {
			a.serverError(w, "read users", err)
			return
		}
		data.Users = append(data.Users, user)
	}
	if err := rows.Err(); err != nil {
		a.serverError(w, "read users", err)
		return
	}
	csrf := a.csrfFromRequest(r)
	a.render(w, "users", "Users", csrf, data, "", userNotice(r.URL.Query().Get("notice")))
}

func (a *App) newUserPage(w http.ResponseWriter, r *http.Request) {
	csrf := a.csrfFromRequest(r)
	a.render(w, "new_user", "Add user", csrf, newUserData{Deposit: "0.00"}, "", "")
}

func (a *App) createUser(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	email := strings.TrimSpace(r.FormValue("email"))
	deposit := strings.TrimSpace(r.FormValue("deposit"))
	if deposit == "" {
		deposit = "0"
	}
	data := newUserData{Name: name, Email: email, Deposit: deposit}
	if len(name) == 0 || len(name) > 160 {
		a.renderFormError(w, r, data, "Name must contain 1 to 160 characters.")
		return
	}
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || len(email) > 320 {
		a.renderFormError(w, r, data, "Enter a valid email address.")
		return
	}
	depositCents, err := parseMoneyCents(deposit)
	if err != nil {
		a.renderFormError(w, r, data, "Initial contingent must be a non-negative amount with up to two decimal places.")
		return
	}

	ctx := r.Context()
	tx, err := a.db.Begin(ctx)
	if err != nil {
		a.serverError(w, "create user", err)
		return
	}
	defer tx.Rollback(ctx)
	var userID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO users (name, email, balance_cents) VALUES ($1, $2, $3)
		RETURNING id
	`, name, email, depositCents).Scan(&userID); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "users_email_unique") {
			a.renderFormError(w, r, data, "A user with this email address already exists.")
			return
		}
		a.serverError(w, "create user", err)
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO membership_periods (user_id, started_at) VALUES ($1, NOW())
	`, userID); err != nil {
		a.serverError(w, "start user membership", err)
		return
	}
	if depositCents > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO wallet_transactions (user_id, kind, amount_cents, balance_after_cents)
			VALUES ($1, 'topup', $2, $2)
		`, userID, depositCents); err != nil {
			a.serverError(w, "record initial contingent", err)
			return
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (event, details) VALUES ('user_created', $1)
	`, fmt.Sprintf("user_id=%d", userID)); err != nil {
		a.serverError(w, "record user creation", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		a.serverError(w, "save user", err)
		return
	}
	http.Redirect(w, r, "/users?notice=created", http.StatusSeeOther)
}

func (a *App) creditUser(w http.ResponseWriter, r *http.Request) {
	userID, ok := pathID(w, r)
	if !ok {
		return
	}
	amountCents, err := parseMoneyCents(r.FormValue("amount"))
	if err != nil || amountCents <= 0 {
		http.Error(w, "Enter a positive amount with up to two decimal places.", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	tx, err := a.db.Begin(ctx)
	if err != nil {
		a.serverError(w, "add user funds", err)
		return
	}
	defer tx.Rollback(ctx)
	var balance int64
	if err := tx.QueryRow(ctx, `SELECT balance_cents FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&balance); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		a.serverError(w, "load user balance", err)
		return
	}
	if balance > (1<<63-1)-amountCents {
		http.Error(w, "The top-up exceeds the supported balance range.", http.StatusBadRequest)
		return
	}
	balance += amountCents
	if _, err := tx.Exec(ctx, `UPDATE users SET balance_cents = $1 WHERE id = $2`, balance, userID); err != nil {
		a.serverError(w, "update user balance", err)
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO wallet_transactions (user_id, kind, amount_cents, balance_after_cents)
		VALUES ($1, 'topup', $2, $3)
	`, userID, amountCents, balance); err != nil {
		a.serverError(w, "record user top-up", err)
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (event, details) VALUES ('user_topped_up', $1)
	`, fmt.Sprintf("user_id=%d; amount_cents=%d", userID, amountCents)); err != nil {
		a.serverError(w, "record top-up", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		a.serverError(w, "save user top-up", err)
		return
	}
	http.Redirect(w, r, "/users?notice=credited", http.StatusSeeOther)
}

func (a *App) deactivateUser(w http.ResponseWriter, r *http.Request) {
	userID, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	tx, err := a.db.Begin(ctx)
	if err != nil {
		a.serverError(w, "remove user", err)
		return
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		UPDATE membership_periods SET ended_at = GREATEST(started_at, NOW())
		WHERE user_id = $1 AND ended_at IS NULL
	`, userID)
	if err != nil {
		a.serverError(w, "end user membership", err)
		return
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists); err != nil {
			a.serverError(w, "check user", err)
			return
		}
		if !exists {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/users?notice=removed", http.StatusSeeOther)
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET is_active = FALSE WHERE id = $1`, userID); err != nil {
		a.serverError(w, "deactivate user", err)
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (event, details) VALUES ('user_removed', $1)
	`, fmt.Sprintf("user_id=%d", userID)); err != nil {
		a.serverError(w, "record user removal", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		a.serverError(w, "save user removal", err)
		return
	}
	http.Redirect(w, r, "/users?notice=removed", http.StatusSeeOther)
}

func (a *App) settingsPage(w http.ResponseWriter, r *http.Request) {
	var rate *int64
	var next *time.Time
	data := settingsData{PollSeconds: int(a.cfg.PollInterval.Seconds())}
	if err := a.db.QueryRow(r.Context(), `
		SELECT price_per_kwh_micros, billing_interval_hours, next_billing_at
		FROM settings WHERE singleton = TRUE
	`).Scan(&rate, &data.IntervalHours, &next); err != nil {
		a.serverError(w, "load settings", err)
		return
	}
	if rate != nil {
		data.RateValue = formatRate(*rate)
	}
	data.NextBilling = formatOptionalDate(next)
	csrf := a.csrfFromRequest(r)
	a.render(w, "settings", "Settings", csrf, data, "", "")
}

func (a *App) updateSettings(w http.ResponseWriter, r *http.Request) {
	rate, err := parseScaledDecimal(strings.TrimSpace(r.FormValue("rate")), 6)
	if err != nil || rate > 9_999_999_999 {
		a.settingsError(w, r, "Electricity price must be non-negative with up to six decimal places.")
		return
	}
	hours, err := strconv.Atoi(strings.TrimSpace(r.FormValue("interval")))
	if err != nil || hours < 1 || hours > 8760 {
		a.settingsError(w, r, "Billing interval must be between 1 and 8760 hours.")
		return
	}
	ctx := r.Context()
	tx, err := a.db.Begin(ctx)
	if err != nil {
		a.serverError(w, "save billing settings", err)
		return
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `
		UPDATE settings SET
			price_per_kwh_micros = $1,
			next_billing_at = CASE
				WHEN price_per_kwh_micros IS NULL OR billing_interval_hours <> $2 OR next_billing_at IS NULL
					THEN NOW() + ($2 * INTERVAL '1 hour')
				ELSE next_billing_at
			END,
			billing_interval_hours = $2
		WHERE singleton = TRUE
	`, rate, hours)
	if err != nil {
		a.serverError(w, "save billing settings", err)
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (event, details) VALUES ('settings_updated', $1)
	`, fmt.Sprintf("rate_micros=%d; billing_interval_hours=%d", rate, hours)); err != nil {
		a.serverError(w, "record settings change", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		a.serverError(w, "save billing settings", err)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (a *App) exportsPage(w http.ResponseWriter, r *http.Request) {
	csrf := a.csrfFromRequest(r)
	a.render(w, "exports", "Exports", csrf, nil, "", "")
}

func (a *App) logsPage(w http.ResponseWriter, r *http.Request) {
	data := logsData{Events: []auditEvent{}, Emails: []emailEvent{}}
	rows, err := a.db.Query(r.Context(), `
		SELECT created_at, event, details
		FROM audit_logs ORDER BY created_at DESC LIMIT 100
	`)
	if err != nil {
		a.serverError(w, "load audit events", err)
		return
	}
	for rows.Next() {
		var event auditEvent
		var createdAt time.Time
		if err := rows.Scan(&createdAt, &event.Event, &event.Details); err != nil {
			rows.Close()
			a.serverError(w, "read audit events", err)
			return
		}
		event.CreatedAt = formatDate(createdAt)
		data.Events = append(data.Events, event)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		a.serverError(w, "read audit events", err)
		return
	}
	rows.Close()
	if err := a.db.QueryRow(r.Context(), `
		SELECT COUNT(*) FILTER (WHERE sent_at IS NULL),
		       COUNT(*) FILTER (WHERE sent_at IS NULL AND last_error IS NOT NULL)
		FROM notification_queue
	`).Scan(&data.PendingEmails, &data.FailedEmails); err != nil {
		a.serverError(w, "load email delivery status", err)
		return
	}
	emailRows, err := a.db.Query(r.Context(), `
		SELECT recipient, attempts, last_error, sent_at, created_at
		FROM notification_queue
		WHERE sent_at IS NULL OR last_error IS NOT NULL
		ORDER BY created_at DESC LIMIT 50
	`)
	if err != nil {
		a.serverError(w, "load email attempts", err)
		return
	}
	for emailRows.Next() {
		var email emailEvent
		var sentAt *time.Time
		var lastError *string
		var createdAt time.Time
		if err := emailRows.Scan(&email.Recipient, &email.Attempts, &lastError, &sentAt, &createdAt); err != nil {
			emailRows.Close()
			a.serverError(w, "read email attempts", err)
			return
		}
		email.CreatedAt = formatDate(createdAt)
		email.Status = "pending"
		if sentAt != nil {
			email.Status = "sent"
		}
		if lastError != nil {
			email.Error = *lastError
		}
		data.Emails = append(data.Emails, email)
	}
	if err := emailRows.Err(); err != nil {
		emailRows.Close()
		a.serverError(w, "read email attempts", err)
		return
	}
	emailRows.Close()
	csrf := a.csrfFromRequest(r)
	a.render(w, "logs", "Logs", csrf, data, "", "")
}

func (a *App) exportCSV(w http.ResponseWriter, r *http.Request) {
	start, end, err := dateRange(r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	kind := r.URL.Query().Get("type")
	if kind != "consumption" && kind != "usage" && kind != "billing" {
		http.Error(w, "type must be consumption, usage, or billing", http.StatusBadRequest)
		return
	}
	var userID *int64
	if value := r.URL.Query().Get("user_id"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 {
			http.Error(w, "user_id must be a positive integer", http.StatusBadRequest)
			return
		}
		userID = &parsed
	}
	if kind == "consumption" && userID != nil {
		http.Error(w, "user_id can only be used with usage or billing exports", http.StatusBadRequest)
		return
	}
	filename := fmt.Sprintf("powerbilling_%s_%s_%s.csv", kind, r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	file, err := os.CreateTemp("", "powerbilling-export-*.csv")
	if err != nil {
		a.serverError(w, "prepare CSV export", err)
		return
	}
	defer os.Remove(file.Name())
	defer file.Close()

	writer := csv.NewWriter(file)
	switch kind {
	case "consumption":
		err = a.exportConsumption(r.Context(), writer, start, end)
	case "usage":
		err = a.exportUsage(r.Context(), writer, start, end, userID)
	case "billing":
		err = a.exportBilling(r.Context(), writer, start, end, userID)
	}
	writer.Flush()
	if err != nil {
		a.serverError(w, "generate "+kind+" export", err)
		return
	}
	if err := writer.Error(); err != nil {
		a.serverError(w, "write "+kind+" export", err)
		return
	}
	info, err := file.Stat()
	if err != nil {
		a.serverError(w, "inspect CSV export", err)
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		a.serverError(w, "read CSV export", err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	if _, err := io.Copy(w, file); err != nil {
		log.Printf("stream %s export: %v", kind, err)
	}
}

func (a *App) exportConsumption(ctx context.Context, writer *csv.Writer, start, end time.Time) error {
	if err := writer.Write([]string{"sampled_at_utc", "power_w", "cumulative_energy_wh"}); err != nil {
		return err
	}
	query := fmt.Sprintf(`
		from(bucket: %q)
		  |> range(start: time(v: %q), stop: time(v: %q))
		  |> filter(fn: (r) => r._measurement == "shelly_power" and (r._field == "power_w" or r._field == "energy_total_wh"))
		  |> pivot(rowKey: ["_time"], columnKey: ["_field"], valueColumn: "_value")
		  |> sort(columns: ["_time"])
	`, a.cfg.InfluxBucket, start.Format(time.RFC3339), end.Format(time.RFC3339))
	result, err := a.influx.QueryAPI(a.cfg.InfluxOrg).Query(ctx, query)
	if err != nil {
		return err
	}
	defer result.Close()
	for result.Next() {
		record := result.Record()
		if err := writer.Write([]string{
			record.Time().UTC().Format(time.RFC3339),
			influxValue(record.ValueByKey("power_w")),
			influxValue(record.ValueByKey("energy_total_wh")),
		}); err != nil {
			return err
		}
	}
	return result.Err()
}

func (a *App) exportUsage(ctx context.Context, writer *csv.Writer, start, end time.Time, userID *int64) error {
	if err := writer.Write([]string{"billing_run_id", "period_start_utc", "period_end_utc", "user_id", "name", "email", "usage_wh", "charge_eur", "balance_after_eur"}); err != nil {
		return err
	}
	rows, err := a.db.Query(ctx, `
		SELECT br.id, br.period_start, br.period_end, u.id, u.name, u.email,
		       be.energy_wh, be.amount_cents, be.balance_after_cents
		FROM billing_entries be
		JOIN billing_runs br ON br.id = be.billing_run_id
		JOIN users u ON u.id = be.user_id
		WHERE br.period_end >= $1 AND br.period_end < $2
		  AND ($3::bigint IS NULL OR u.id = $3)
		ORDER BY br.period_end, u.id
	`, start, end, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var runID, id, amount, balance int64
		var periodStart, periodEnd time.Time
		var name, email string
		var energyWh float64
		if err := rows.Scan(&runID, &periodStart, &periodEnd, &id, &name, &email, &energyWh, &amount, &balance); err != nil {
			return err
		}
		if err := writer.Write([]string{
			strconv.FormatInt(runID, 10), periodStart.UTC().Format(time.RFC3339), periodEnd.UTC().Format(time.RFC3339),
			strconv.FormatInt(id, 10), csvSafeCell(name), csvSafeCell(email), strconv.FormatFloat(energyWh, 'f', 6, 64),
			euroAmount(amount), euroAmount(balance),
		}); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (a *App) exportBilling(ctx context.Context, writer *csv.Writer, start, end time.Time, userID *int64) error {
	if err := writer.Write([]string{"transaction_time_utc", "user_id", "name", "email", "type", "amount_eur", "balance_after_eur", "usage_wh", "period_start_utc", "period_end_utc"}); err != nil {
		return err
	}
	rows, err := a.db.Query(ctx, `
		SELECT wt.created_at, u.id, u.name, u.email, wt.kind, wt.amount_cents,
		       wt.balance_after_cents, COALESCE(be.energy_wh, 0), br.period_start, br.period_end
		FROM wallet_transactions wt
		JOIN users u ON u.id = wt.user_id
		LEFT JOIN billing_entries be ON be.id = wt.billing_entry_id
		LEFT JOIN billing_runs br ON br.id = be.billing_run_id
		WHERE wt.created_at >= $1 AND wt.created_at < $2
		  AND ($3::bigint IS NULL OR u.id = $3)
		ORDER BY wt.created_at, wt.id
	`, start, end, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var createdAt time.Time
		var id, amount, balance int64
		var name, email, kind string
		var energyWh float64
		var periodStart, periodEnd *time.Time
		if err := rows.Scan(&createdAt, &id, &name, &email, &kind, &amount, &balance, &energyWh, &periodStart, &periodEnd); err != nil {
			return err
		}
		startText, endText := "", ""
		if periodStart != nil {
			startText = periodStart.UTC().Format(time.RFC3339)
			endText = periodEnd.UTC().Format(time.RFC3339)
		}
		if err := writer.Write([]string{
			createdAt.UTC().Format(time.RFC3339), strconv.FormatInt(id, 10), csvSafeCell(name), csvSafeCell(email), kind,
			euroAmount(amount), euroAmount(balance), strconv.FormatFloat(energyWh, 'f', 6, 64), startText, endText,
		}); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (a *App) renderFormError(w http.ResponseWriter, r *http.Request, data newUserData, message string) {
	csrf := a.csrfFromRequest(r)
	a.render(w, "new_user", "Add user", csrf, data, message, "")
}

func (a *App) settingsError(w http.ResponseWriter, r *http.Request, message string) {
	rate := strings.TrimSpace(r.FormValue("rate"))
	hours, _ := strconv.Atoi(r.FormValue("interval"))
	if hours < 1 || hours > 8760 {
		hours = 24
	}
	csrf := a.csrfFromRequest(r)
	a.render(w, "settings", "Settings", csrf, settingsData{
		RateValue: rate, IntervalHours: hours, NextBilling: "unchanged",
		PollSeconds: int(a.cfg.PollInterval.Seconds()),
	}, message, "")
}

func (a *App) serverError(w http.ResponseWriter, action string, err error) {
	log.Printf("%s: %v", action, err)
	http.Error(w, "The request could not be completed. Check the server logs.", http.StatusInternalServerError)
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "Invalid user ID", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func userNotice(value string) string {
	switch value {
	case "created":
		return "User created."
	case "removed":
		return "User removed. Historical usage and billing records were retained."
	case "credited":
		return "User balance updated."
	default:
		return ""
	}
}

func parseMoneyCents(value string) (int64, error) {
	return parseScaledDecimal(value, 2)
}

func parseScaledDecimal(value string, precision int) (int64, error) {
	if value == "" || strings.HasPrefix(value, "-") || strings.HasPrefix(value, "+") {
		return 0, fmt.Errorf("expected a non-negative decimal")
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" {
		return 0, fmt.Errorf("invalid decimal")
	}
	for _, char := range parts[0] {
		if char < '0' || char > '9' {
			return 0, fmt.Errorf("invalid decimal")
		}
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	if len(fraction) > precision {
		return 0, fmt.Errorf("too many decimal places")
	}
	for _, char := range fraction {
		if char < '0' || char > '9' {
			return 0, fmt.Errorf("invalid decimal")
		}
	}
	fraction += strings.Repeat("0", precision-len(fraction))
	valueInteger := parts[0] + fraction
	parsed, err := strconv.ParseInt(valueInteger, 10, 64)
	if err != nil {
		return 0, err
	}
	return parsed, nil
}

func dateRange(from, to string) (time.Time, time.Time, error) {
	start, err := time.Parse("2006-01-02", from)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("from and to must be valid dates in YYYY-MM-DD format")
	}
	lastDay, err := time.Parse("2006-01-02", to)
	if err != nil || lastDay.Before(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("to must be a valid date on or after from")
	}
	return start.UTC(), lastDay.Add(24 * time.Hour).UTC(), nil
}

func formatOptionalDate(value *time.Time) string {
	if value == nil {
		return "not scheduled"
	}
	return formatDate(*value)
}

func euroAmount(cents int64) string {
	sign, euros, fractionalCents := euroParts(cents)
	return fmt.Sprintf("%s%d.%02d", sign, euros, fractionalCents)
}

func csvSafeCell(value string) string {
	trimmed := strings.TrimLeft(value, " \t\r\n")
	if trimmed != "" && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + value
	}
	return value
}

func influxValue(value any) string {
	if value == nil {
		return ""
	}
	if number, ok := value.(float64); ok {
		return strconv.FormatFloat(number, 'f', 6, 64)
	}
	return fmt.Sprint(value)
}

func euroParts(cents int64) (string, uint64, uint64) {
	sign := ""
	var magnitude uint64
	if cents < 0 {
		sign = "-"
		magnitude = uint64(-(cents + 1)) + 1
	} else {
		magnitude = uint64(cents)
	}
	return sign, magnitude / 100, magnitude % 100
}
