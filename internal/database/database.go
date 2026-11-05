package database

import (
	"context"
	"embed"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema embed.FS

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	sql, err := schema.ReadFile("schema.sql")
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, statement := range strings.Split(string(sql), ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("apply schema statement: %w", err)
		}
	}
	return tx.Commit(ctx)
}
