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
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	migrationTx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer migrationTx.Rollback(ctx)
	tag, err := migrationTx.Exec(ctx, `
		INSERT INTO schema_migrations (version, name)
		VALUES (1, 'anchor billing schedule to configured local time')
		ON CONFLICT (version) DO NOTHING
	`)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		if _, err := migrationTx.Exec(ctx, `UPDATE settings SET next_billing_at = NULL WHERE singleton = TRUE`); err != nil {
			return err
		}
	}
	return migrationTx.Commit(ctx)
}
