package app

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/FaolanBig/prepaid-auto-billing-system-server-usage/internal/config"
)

type App struct {
	cfg    config.Config
	db     *pgxpool.Pool
	influx influxdb2.Client
	http   *http.Client
}

func New(cfg config.Config, db *pgxpool.Pool, influx influxdb2.Client) *App {
	return &App{
		cfg:    cfg,
		db:     db,
		influx: influx,
		http:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (a *App) recordOperationalIssue(ctx context.Context, event string, err error) {
	log.Printf("%s: %v", event, err)
	details := err.Error()
	if runes := []rune(details); len(runes) > 1000 {
		details = string(runes[:1000])
	}
	auditCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	if ctx.Err() != nil {
		cancel()
		auditCtx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	}
	defer cancel()
	if _, auditErr := a.db.Exec(auditCtx, `
		INSERT INTO audit_logs (event, details) VALUES ($1, $2)
	`, event, strings.TrimSpace(details)); auditErr != nil {
		log.Printf("record %s in PostgreSQL audit log: %v", event, auditErr)
	}
}
