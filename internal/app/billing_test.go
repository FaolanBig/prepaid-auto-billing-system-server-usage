package app

import (
	"html/template"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/FaolanBig/prepaid-auto-billing-system-server-usage/internal/config"
)

func TestAllocateEnergyAcrossMembershipChange(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	midpoint := start.Add(30 * time.Second)
	item := energyInterval{
		start:    start,
		end:      start.Add(time.Minute),
		energyWh: 100,
		rate:     int64Pointer(350_000),
	}
	members := []membership{
		{userID: 1, start: start},
		{userID: 2, start: midpoint},
	}
	allocations := make(map[int64]userAllocation)

	allocateEnergy(item, item.rate, members, allocations)

	if got := allocations[1].energyWh; math.Abs(got-75) > 1e-9 {
		t.Fatalf("user 1 energy = %f Wh, want 75", got)
	}
	if got := allocations[2].energyWh; math.Abs(got-25) > 1e-9 {
		t.Fatalf("user 2 energy = %f Wh, want 25", got)
	}
	if got := allocations[1].cents + allocations[2].cents; math.Abs(got-3.5) > 1e-9 {
		t.Fatalf("allocated cost = %f cents, want 3.5", got)
	}
}

func TestAllocateEnergyDoesNotChargeWhenNoUsersAreActive(t *testing.T) {
	start := time.Now().UTC()
	item := energyInterval{
		start:    start,
		end:      start.Add(time.Minute),
		energyWh: 50,
		rate:     int64Pointer(350_000),
	}
	allocations := make(map[int64]userAllocation)

	allocateEnergy(item, item.rate, nil, allocations)

	if len(allocations) != 0 {
		t.Fatalf("got %d allocations, want none", len(allocations))
	}
}

func TestRoundedChargesConserveRoundedCost(t *testing.T) {
	allocations := map[int64]userAllocation{
		1: {energyWh: 10, cents: 0.6},
		2: {energyWh: 10, cents: 0.6},
		3: {energyWh: 10, cents: 0.6},
	}

	charges, err := roundedCharges([]int64{1, 2, 3}, allocations)
	if err != nil {
		t.Fatal(err)
	}

	if charges[1]+charges[2]+charges[3] != 2 {
		t.Fatalf("rounded charges sum to %d cents, want 2", charges[1]+charges[2]+charges[3])
	}
	if charges[1] != 1 || charges[2] != 1 || charges[3] != 0 {
		t.Fatalf("unexpected deterministic allocation: %#v", charges)
	}
}

func TestParseScaledDecimal(t *testing.T) {
	tests := []struct {
		input     string
		precision int
		want      int64
		wantErr   bool
	}{
		{"12.34", 2, 1234, false},
		{"0.3", 6, 300000, false},
		{"1", 2, 100, false},
		{"1.234", 2, 0, true},
		{"-1.00", 2, 0, true},
		{"1x", 2, 0, true},
	}
	for _, test := range tests {
		got, err := parseScaledDecimal(test.input, test.precision)
		if (err != nil) != test.wantErr {
			t.Errorf("parseScaledDecimal(%q) error = %v, wantErr %v", test.input, err, test.wantErr)
			continue
		}
		if err == nil && got != test.want {
			t.Errorf("parseScaledDecimal(%q) = %d, want %d", test.input, got, test.want)
		}
	}
}

func TestDateRangeIncludesBothDates(t *testing.T) {
	start, end, err := dateRange("2026-03-01", "2026-03-02")
	if err != nil {
		t.Fatal(err)
	}
	if start.Format(time.RFC3339) != "2026-03-01T00:00:00Z" {
		t.Fatalf("start = %s", start)
	}
	if end.Format(time.RFC3339) != "2026-03-03T00:00:00Z" {
		t.Fatalf("exclusive end = %s", end)
	}
}

func TestTemplatesParseAndRender(t *testing.T) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"money": formatMoney,
		"rate":  formatRate,
		"date":  formatDate,
	}).ParseFS(templateFiles, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dashboard", "users", "new_user", "settings", "exports", "logs", "login"} {
		if tmpl.Lookup(name) == nil {
			t.Errorf("template %q not found", name)
		}
	}

	app := &App{}
	pages := []struct {
		name  string
		title string
		data  any
	}{
		{"dashboard", "Dashboard", dashboardData{RecentRuns: []billingRunRow{}}},
		{"users", "Users", usersData{Users: []userRow{
			{ID: 7, Name: "Removed user", Email: "removed@example.test", Active: false},
		}}},
		{"new_user", "Add user", newUserData{Deposit: "0.00"}},
		{"settings", "Settings", settingsData{IntervalHours: 24}},
		{"exports", "Exports", nil},
		{"logs", "Logs", logsData{Events: []auditEvent{}, Emails: []emailEvent{}}},
		{"login", "Admin sign in", nil},
	}
	for _, test := range pages {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			app.render(recorder, test.name, test.title, "", test.data, "", "")
			if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "<html") {
				t.Fatalf("page did not render: status %d, body %s", recorder.Code, recorder.Body.String())
			}
			if test.name == "login" && !strings.Contains(recorder.Body.String(), "Administrator sign in") {
				t.Fatalf("login content missing: %s", recorder.Body.String())
			}
			if test.name == "users" && !strings.Contains(recorder.Body.String(), "/users/7/reactivate") {
				t.Fatalf("removed user has no reactivation action: %s", recorder.Body.String())
			}
		})
	}
}

func TestCSVFormulaCellsAreEscaped(t *testing.T) {
	for _, value := range []string{"=1+1", "  @SUM(A1)", "+cmd"} {
		if got := csvSafeCell(value); got[0] != '\'' {
			t.Errorf("csvSafeCell(%q) = %q, want apostrophe prefix", value, got)
		}
	}
	if got := csvSafeCell("person@example.com"); got != "person@example.com" {
		t.Errorf("csvSafeCell(email) = %q, want unchanged", got)
	}
}

func TestAdminSessionAndCSRFProtection(t *testing.T) {
	app := &App{cfg: config.Config{
		AdminUsername: "admin",
		AdminPassword: "a-very-long-test-password",
		CookieSecret:  "0123456789abcdef0123456789abcdef",
	}}
	loginRequest := httptest.NewRequest(
		"POST", "/login", strings.NewReader("username=admin&password=a-very-long-test-password"),
	)
	loginRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginResponse := httptest.NewRecorder()
	app.login(loginResponse, loginRequest)
	if loginResponse.Code != 303 {
		t.Fatalf("login status = %d, want 303", loginResponse.Code)
	}
	cookies := loginResponse.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want one session cookie", len(cookies))
	}

	request := httptest.NewRequest("GET", "/", nil)
	request.AddCookie(cookies[0])
	session, ok := app.sessionFromRequest(request)
	if !ok {
		t.Fatal("valid signed session was rejected")
	}
	invalidRequest := httptest.NewRequest("GET", "/", nil)
	tampered := *cookies[0]
	tampered.Value += "tampered"
	invalidRequest.AddCookie(&tampered)
	if _, ok := app.sessionFromRequest(invalidRequest); ok {
		t.Fatal("tampered session was accepted")
	}

	protected := app.admin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	missingCSRF := httptest.NewRequest("POST", "/", strings.NewReader(""))
	missingCSRF.AddCookie(cookies[0])
	missingResponse := httptest.NewRecorder()
	protected.ServeHTTP(missingResponse, missingCSRF)
	if missingResponse.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d, want 403", missingResponse.Code)
	}

	validCSRF := httptest.NewRequest(
		"POST", "/", strings.NewReader("csrf="+url.QueryEscape(session.csrf)),
	)
	validCSRF.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	validCSRF.AddCookie(cookies[0])
	validResponse := httptest.NewRecorder()
	protected.ServeHTTP(validResponse, validCSRF)
	if validResponse.Code != http.StatusNoContent {
		t.Fatalf("valid CSRF status = %d, want 204", validResponse.Code)
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}
