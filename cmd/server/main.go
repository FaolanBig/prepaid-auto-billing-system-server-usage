package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
	_ "time/tzdata"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/FaolanBig/prepaid-auto-billing-system-server-usage/internal/app"
	"github.com/FaolanBig/prepaid-auto-billing-system-server-usage/internal/config"
	"github.com/FaolanBig/prepaid-auto-billing-system-server-usage/internal/database"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	poolConfig, err := postgresPoolConfig(cfg)
	if err != nil {
		log.Fatalf("configure PostgreSQL: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		log.Fatalf("configure PostgreSQL pool: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("connect to PostgreSQL: %v", err)
	}
	if err := database.Migrate(ctx, pool); err != nil {
		log.Fatalf("apply database schema: %v", err)
	}

	influx := influxdb2.NewClient(cfg.InfluxURL, cfg.InfluxToken)
	defer influx.Close()
	pingCtx, cancelPing := context.WithTimeout(ctx, 10*time.Second)
	defer cancelPing()
	if _, err := influx.Ping(pingCtx); err != nil {
		log.Fatalf("connect to InfluxDB: %v", err)
	}

	service := app.New(cfg, pool, influx)
	go service.RunShellyPoller(ctx)
	go service.RunBillingScheduler(ctx)
	go service.RunNotificationSender(ctx)

	server := &http.Server{
		Addr:              cfg.HTTPAddress,
		Handler:           service.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP shutdown: %v", err)
		}
	}()

	log.Printf("admin panel listening on %s", cfg.HTTPAddress)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("HTTP server: %v", err)
	}
}

func postgresPoolConfig(cfg config.Config) (*pgxpool.Config, error) {
	poolConfig, err := pgxpool.ParseConfig("postgres://localhost")
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(cfg.DatabasePort, 10, 16)
	if err != nil || port == 0 {
		return nil, fmt.Errorf("POSTGRES_PORT must be a valid TCP port")
	}
	poolConfig.ConnConfig.Host = cfg.DatabaseHost
	poolConfig.ConnConfig.Port = uint16(port)
	poolConfig.ConnConfig.Database = cfg.DatabaseName
	poolConfig.ConnConfig.User = cfg.DatabaseUser
	poolConfig.ConnConfig.Password = cfg.DatabasePass
	for _, fallback := range poolConfig.ConnConfig.Fallbacks {
		fallback.Host = cfg.DatabaseHost
		fallback.Port = uint16(port)
	}
	return poolConfig, nil
}
