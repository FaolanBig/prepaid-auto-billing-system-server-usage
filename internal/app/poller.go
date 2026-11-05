package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/jackc/pgx/v5"
)

type shellyStatus struct {
	PowerW *float64 `json:"apower"`
	Energy struct {
		TotalWh *float64 `json:"total"`
	} `json:"aenergy"`
}

func (a *App) RunShellyPoller(ctx context.Context) {
	if a.cfg.ShellyURL == "" {
		log.Print("Shelly polling disabled: SHELLY_URL is not configured")
		return
	}

	a.pollShelly(ctx)
	ticker := time.NewTicker(a.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.pollShelly(ctx)
		}
	}
}

func (a *App) pollShelly(ctx context.Context) {
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, a.cfg.ShellyURL, nil)
	if err != nil {
		a.recordOperationalIssue(requestCtx, "shelly_request_failed", err)
		return
	}
	if a.cfg.ShellyUser != "" {
		req.SetBasicAuth(a.cfg.ShellyUser, a.cfg.ShellyPass)
	}
	response, err := a.http.Do(req)
	if err != nil {
		a.recordOperationalIssue(requestCtx, "shelly_read_failed", err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		a.recordOperationalIssue(requestCtx, "shelly_read_failed", fmt.Errorf("HTTP %s", response.Status))
		return
	}

	var status shellyStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		a.recordOperationalIssue(requestCtx, "shelly_response_invalid", err)
		return
	}
	if status.PowerW == nil || status.Energy.TotalWh == nil {
		a.recordOperationalIssue(requestCtx, "shelly_response_invalid",
			fmt.Errorf("response is missing power or cumulative energy"))
		return
	}
	powerW, totalWh := *status.PowerW, *status.Energy.TotalWh
	if math.IsNaN(powerW) || math.IsInf(powerW, 0) ||
		math.IsNaN(totalWh) || math.IsInf(totalWh, 0) || totalWh < 0 {
		a.recordOperationalIssue(requestCtx, "shelly_response_invalid",
			fmt.Errorf("power or cumulative energy measurement is invalid"))
		return
	}

	sampledAt := time.Now().UTC()
	point := influxdb2.NewPointWithMeasurement("shelly_power").
		AddTag("device", "shelly-plug-m-gen3").
		AddField("power_w", powerW).
		AddField("energy_total_wh", totalWh).
		SetTime(sampledAt)
	writer := a.influx.WriteAPIBlocking(a.cfg.InfluxOrg, a.cfg.InfluxBucket)
	if err := writer.WritePoint(requestCtx, point); err != nil {
		a.recordOperationalIssue(requestCtx, "influx_write_failed", err)
		return
	}
	if err := a.storeEnergySample(requestCtx, powerW, totalWh, sampledAt); err != nil {
		a.recordOperationalIssue(requestCtx, "energy_interval_store_failed", err)
	}
}

func (a *App) storeEnergySample(ctx context.Context, powerW, totalWh float64, sampledAt time.Time) error {
	tx, err := a.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var previousTotal float64
	var previousAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT energy_total_wh, sampled_at
		FROM device_state
		WHERE singleton = TRUE
		FOR UPDATE
	`).Scan(&previousTotal, &previousAt)
	hasPrevious := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	if hasPrevious && totalWh < previousTotal {
		if _, err := tx.Exec(ctx, `
			INSERT INTO audit_logs (event, details)
			VALUES ('shelly_counter_reset', 'Cumulative energy counter decreased; interval was not charged')
		`); err != nil {
			return err
		}
		log.Printf("Shelly cumulative energy counter decreased from %.3f Wh to %.3f Wh; resetting baseline",
			previousTotal, totalWh)
	} else if hasPrevious && totalWh > previousTotal && sampledAt.After(previousAt) {
		var rate *int64
		if err := tx.QueryRow(ctx, `
			SELECT price_per_kwh_micros FROM settings WHERE singleton = TRUE
		`).Scan(&rate); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO energy_intervals
				(started_at, ended_at, energy_wh, power_w, price_per_kwh_micros)
			VALUES ($1, $2, $3, $4, $5)
		`, previousAt, sampledAt, totalWh-previousTotal, powerW, rate); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO device_state (singleton, energy_total_wh, power_w, sampled_at)
		VALUES (TRUE, $1, $2, $3)
		ON CONFLICT (singleton) DO UPDATE SET
			energy_total_wh = EXCLUDED.energy_total_wh,
			power_w = EXCLUDED.power_w,
			sampled_at = EXCLUDED.sampled_at
	`, totalWh, powerW, sampledAt); err != nil {
		return fmt.Errorf("update device state: %w", err)
	}
	return tx.Commit(ctx)
}
