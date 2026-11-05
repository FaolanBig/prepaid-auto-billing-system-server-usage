package app

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFiles embed.FS

type page struct {
	Content string
	Title   string
	CSRF    string
	Error   string
	Notice  string
	Data    any
}

type adminSession struct {
	username string
	csrf     string
	expires  int64
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /login", a.loginPage)
	mux.HandleFunc("POST /login", a.login)
	mux.Handle("GET /", a.admin(http.HandlerFunc(a.dashboard)))
	mux.Handle("POST /logout", a.admin(http.HandlerFunc(a.logout)))
	mux.Handle("GET /users", a.admin(http.HandlerFunc(a.usersPage)))
	mux.Handle("GET /users/new", a.admin(http.HandlerFunc(a.newUserPage)))
	mux.Handle("POST /users", a.admin(http.HandlerFunc(a.createUser)))
	mux.Handle("POST /users/{id}/credit", a.admin(http.HandlerFunc(a.creditUser)))
	mux.Handle("POST /users/{id}/deactivate", a.admin(http.HandlerFunc(a.deactivateUser)))
	mux.Handle("POST /users/{id}/reactivate", a.admin(http.HandlerFunc(a.reactivateUser)))
	mux.Handle("GET /settings", a.admin(http.HandlerFunc(a.settingsPage)))
	mux.Handle("POST /settings", a.admin(http.HandlerFunc(a.updateSettings)))
	mux.Handle("GET /exports", a.admin(http.HandlerFunc(a.exportsPage)))
	mux.Handle("GET /export.csv", a.admin(http.HandlerFunc(a.exportCSV)))
	mux.Handle("GET /logs", a.admin(http.HandlerFunc(a.logsPage)))
	return securityHeaders(mux)
}

func (a *App) admin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := a.sessionFromRequest(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil ||
				!constantTimeEqual(r.FormValue("csrf"), session.csrf) {
				http.Error(w, "Invalid form token", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionFromRequest(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.render(w, "login", "Admin sign in", "", nil, r.URL.Query().Get("error"), "")
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil ||
		!constantTimeEqual(r.FormValue("username"), a.cfg.AdminUsername) ||
		!constantTimeEqual(r.FormValue("password"), a.cfg.AdminPassword) {
		a.render(w, "login", "Admin sign in", "", nil, "Invalid username or password.", "")
		return
	}
	csrf, err := randomToken()
	if err != nil {
		log.Printf("create admin session: %v", err)
		http.Error(w, "Unable to start a session", http.StatusInternalServerError)
		return
	}
	expires := time.Now().Add(12 * time.Hour)
	payload := strings.Join([]string{a.cfg.AdminUsername, csrf, strconv.FormatInt(expires.Unix(), 10)}, "|")
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	signature := a.sign(encoded)
	http.SetCookie(w, &http.Cookie{
		Name:     "powerbilling_session",
		Value:    encoded + "." + signature,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(expires.Sub(time.Now()).Seconds()),
		HttpOnly: true,
		Secure:   a.cfg.CookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "powerbilling_session",
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.cfg.CookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *App) sessionFromRequest(r *http.Request) (adminSession, bool) {
	cookie, err := r.Cookie("powerbilling_session")
	if err != nil {
		return adminSession{}, false
	}
	pieces := strings.Split(cookie.Value, ".")
	if len(pieces) != 2 || !constantTimeEqual(pieces[1], a.sign(pieces[0])) {
		return adminSession{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(pieces[0])
	if err != nil {
		return adminSession{}, false
	}
	fields := strings.Split(string(payload), "|")
	if len(fields) != 3 || fields[0] != a.cfg.AdminUsername {
		return adminSession{}, false
	}
	expires, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || time.Now().Unix() >= expires {
		return adminSession{}, false
	}
	return adminSession{username: fields[0], csrf: fields[1], expires: expires}, true
}

func (a *App) csrfFromRequest(r *http.Request) string {
	session, _ := a.sessionFromRequest(r)
	return session.csrf
}

func (a *App) sign(payload string) string {
	mac := hmac.New(sha256.New, []byte(a.cfg.CookieSecret))
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *App) render(w http.ResponseWriter, content, title, csrf string, data any, errText, notice string) {
	t, err := template.New("").Funcs(template.FuncMap{
		"money": formatMoney,
		"rate":  formatRate,
		"date":  formatDate,
	}).ParseFS(templateFiles, "templates/*.html")
	if err != nil {
		log.Printf("parse admin template: %v", err)
		http.Error(w, "Unable to render page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, content, page{
		Content: content + "-content",
		Title:   title,
		CSRF:    csrf,
		Error:   errText,
		Notice:  notice,
		Data:    data,
	}); err != nil {
		log.Printf("render admin page %q: %v", content, err)
	}
}

func (a *App) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 3*time.Second)
	defer cancel()
	if err := a.db.Ping(ctx); err != nil {
		http.Error(w, "PostgreSQL unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, err := a.influx.Ping(ctx); err != nil {
		http.Error(w, "InfluxDB unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func randomToken() (string, error) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(token), nil
}

func constantTimeEqual(left, right string) bool {
	return hmac.Equal([]byte(left), []byte(right))
}

func formatMoney(cents int64) string {
	sign, euros, fractionalCents := euroParts(cents)
	return fmt.Sprintf("%s€%d.%02d", sign, euros, fractionalCents)
}

func formatRate(micros int64) string {
	return fmt.Sprintf("%.6f", float64(micros)/1_000_000)
}

func formatDate(value time.Time) string {
	return value.UTC().Format("2006-01-02 15:04 UTC")
}

func contextWithTimeout(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), timeout)
}
