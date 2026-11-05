package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

type queuedEmail struct {
	id        int64
	recipient string
	subject   string
	body      string
	attempts  int
}

func (a *App) RunNotificationSender(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.sendOneQueuedEmail(ctx); err != nil {
				log.Printf("process notification queue: %v", err)
			}
		}
	}
}

func (a *App) sendOneQueuedEmail(ctx context.Context) error {
	tx, err := a.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var email queuedEmail
	err = tx.QueryRow(ctx, `
		SELECT id, recipient, subject, body, attempts
		FROM notification_queue
		WHERE sent_at IS NULL AND next_attempt_at <= NOW()
		ORDER BY created_at
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`).Scan(&email.id, &email.recipient, &email.subject, &email.body, &email.attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	email.attempts++
	if _, err := tx.Exec(ctx, `
		UPDATE notification_queue
		SET attempts = $1, next_attempt_at = NOW() + INTERVAL '5 minutes'
		WHERE id = $2
	`, email.attempts, email.id); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	if err := a.sendEmail(email); err != nil {
		delay := time.Duration(math.Min(math.Pow(2, float64(min(email.attempts, 6))), 60)) * time.Minute
		if _, updateErr := a.db.Exec(ctx, `
			UPDATE notification_queue
			SET last_error = $1, next_attempt_at = NOW() + $2::interval
			WHERE id = $3
		`, err.Error(), intervalLiteral(delay), email.id); updateErr != nil {
			return fmt.Errorf("send email: %v; record retry: %w", err, updateErr)
		}
		a.recordOperationalIssue(ctx, "email_delivery_failed",
			fmt.Errorf("notification %d to %s (attempt %d): %w", email.id, email.recipient, email.attempts, err))
		return nil
	}

	if _, err := a.db.Exec(ctx, `
		UPDATE notification_queue
		SET sent_at = NOW(), last_error = NULL
		WHERE id = $1
	`, email.id); err != nil {
		return fmt.Errorf("mark notification sent: %w", err)
	}
	log.Printf("email notification %d sent to %s", email.id, email.recipient)
	return nil
}

func (a *App) sendEmail(email queuedEmail) error {
	from, err := mail.ParseAddress(a.cfg.SMTPFrom)
	if err != nil {
		return fmt.Errorf("invalid SMTP_FROM: %w", err)
	}
	to, err := mail.ParseAddress(email.recipient)
	if err != nil {
		return fmt.Errorf("invalid notification recipient: %w", err)
	}
	if a.cfg.SMTPHost == "" {
		return fmt.Errorf("SMTP_HOST is not configured")
	}
	port, err := strconv.Atoi(a.cfg.SMTPPort)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid SMTP_PORT %q", a.cfg.SMTPPort)
	}

	message := []byte(fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s",
		from.String(), to.String(), email.subject, email.body,
	))
	address := net.JoinHostPort(a.cfg.SMTPHost, strconv.Itoa(port))
	connection, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		return err
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	client, err := smtp.NewClient(connection, a.cfg.SMTPHost)
	if err != nil {
		return err
	}
	defer client.Close()

	if ok, _ := client.Extension("STARTTLS"); !ok {
		return fmt.Errorf("SMTP server does not offer STARTTLS; refusing to send billing notifications without TLS")
	}
	if err := client.StartTLS(&tls.Config{ServerName: a.cfg.SMTPHost, MinVersion: tls.VersionTLS12}); err != nil {
		return err
	}
	var auth smtp.Auth
	if a.cfg.SMTPUsername != "" {
		auth = smtp.PlainAuth("", a.cfg.SMTPUsername, a.cfg.SMTPPassword, a.cfg.SMTPHost)
	}
	if auth != nil {
		if err := client.Auth(auth); err != nil {
			return err
		}
	}
	if err := client.Mail(from.Address); err != nil {
		return err
	}
	if err := client.Rcpt(to.Address); err != nil {
		return err
	}
	body, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := io.WriteString(body, string(message)); err != nil {
		return err
	}
	if err := body.Close(); err != nil {
		return err
	}
	return client.Quit()
}

func intervalLiteral(duration time.Duration) string {
	return fmt.Sprintf("%f seconds", duration.Seconds())
}
