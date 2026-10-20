package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServerSmoke(t *testing.T) {
	db := testDB(t)
	for i := range 30 {
		d := fmt.Sprintf("2026-09-%02d", i+1)
		for _, tk := range []string{"SPUS", "AAA"} {
			if _, err := db.Exec(`INSERT INTO prices (ticker, date, open, high, low, close, volume) VALUES (?, ?, 10, 11, 9, 10, 1)`, tk, d); err != nil {
				t.Fatal(err)
			}
		}
	}
	db.Exec(`INSERT INTO settings VALUES ('start_date', '2026-09-01')`)
	db.Exec(`INSERT INTO screens (ticker, as_of, status, business, streak, note) VALUES ('AAA', '2026-08-31', 'COMPLIANT', 'ok', 0, '<script>')`)
	db.Exec(`INSERT INTO watchlist VALUES ('AAA', '2026-09-01')`)
	db.Exec(`INSERT INTO rules (name, version, ticker, action, thesis, condition, created_on) VALUES ('r', 1, '*', 'BUY', 't', 'close > 5', '2026-09-01')`)
	db.Exec(`INSERT INTO orders (placed_on, ticker, side, notional, reason, thesis) VALUES ('2026-09-01', 'AAA', 'buy', 50, 'why', 'tag')`)
	if err := replay(db); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newServer(&app{db: db, dbPath: filepath.Join(t.TempDir(), "x.db")}))
	defer srv.Close()

	for _, p := range []string{"/api/summary", "/api/watchlist", "/api/journal", "/api/signals", "/api/orders", "/api/screens", "/api/rules", "/api/ticker/AAA"} {
		res, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || !json.Valid(body) {
			t.Errorf("%s: %d %s", p, res.StatusCode, body)
		}
	}
	for p, want := range map[string]int{"/api/ticker/..%2Fx": 404, "/api/ticker/ZZZ": 404, "/": 200, "/app.js": 200, "/vendor/lightweight-charts.standalone.production.js": 200} {
		res, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("%s: status %d, want %d", p, res.StatusCode, want)
		}
		if !strings.Contains(res.Header.Get("Content-Security-Policy"), "script-src 'self'") {
			t.Errorf("%s: missing CSP", p)
		}
	}
	var s summary
	res, _ := http.Get(srv.URL + "/api/summary")
	json.NewDecoder(res.Body).Decode(&s)
	res.Body.Close()
	if len(s.Positions) != 1 || s.Positions[0].Ticker != "AAA" || s.Contributions != 300 {
		t.Errorf("summary = %+v", s)
	}
}

func TestAppWritesAndGuard(t *testing.T) {
	db := testDB(t)
	for i := range 5 {
		db.Exec(`INSERT INTO prices (ticker, date, open, high, low, close, volume) VALUES ('SPUS', ?, 10, 10, 10, 10, 1)`, fmt.Sprintf("2026-09-%02d", i+1))
	}
	srv := httptest.NewServer(newServer(&app{db: db, dbPath: filepath.Join(t.TempDir(), "x.db")}))
	defer srv.Close()
	send := func(method, path, body string, hdr map[string]string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	ours := map[string]string{"X-Suspt": "1", "Content-Type": "application/json"}

	if code, _ := send("POST", "/api/watchlist", `{"tickers":["NVDA"]}`, nil); code != 403 {
		t.Errorf("write without X-Suspt: %d, want 403", code)
	}
	if code, _ := send("POST", "/api/watchlist", `{"tickers":["NVDA"]}`, map[string]string{"X-Suspt": "1", "Origin": "https://evil.example"}); code != 403 {
		t.Errorf("cross-origin write: %d, want 403", code)
	}
	// A DNS-rebinding page reaches this server under its own host name.
	req, _ := http.NewRequest("GET", srv.URL+"/api/summary", nil)
	req.Host = "evil.example"
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("foreign Host: %v %v, want 421", res.StatusCode, err)
	} else {
		res.Body.Close()
	}
	if code, body := send("POST", "/api/watchlist", `{"tickers":["nvda"]}`, ours); code != 200 || !strings.Contains(body, `"added":1`) {
		t.Errorf("add watch: %d %s", code, body)
	}
	if code, body := send("POST", "/api/orders", `{"ticker":"NVDA","side":"buy","usd":50,"reason":"","thesis":"x"}`, ours); code != 400 || !strings.Contains(body, "reason") {
		t.Errorf("order without reason: %d %s", code, body)
	}
	if code, body := send("POST", "/api/rules", `{"name":"t","ticker":"*","action":"WATCH","thesis":"x","condition":"close >"}`, ours); code != 400 || !strings.Contains(body, "incomplete") {
		t.Errorf("bad rule: %d %s", code, body)
	}
	if code, body := send("POST", "/api/setup", `{"tiingoKey":"short","name":"A","email":"a@b.co"}`, ours); code != 400 || !strings.Contains(body, "Tiingo") {
		t.Errorf("bad key: %d %s", code, body)
	}
	if code, _ := send("POST", "/api/settings", `{"key":"monthly_deposit","value":"75"}`, ours); code != 200 {
		t.Errorf("setting: %d", code)
	}
}

func TestWriteEnvAndSession(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("# keep me\nTIINGO_API_KEY=old\nSUSPT_DB=x.db\n"), 0o600)
	if err := writeEnv(p, map[string]string{"TIINGO_API_KEY": "new", "SEC_USER_AGENT": `"A a@b.co"`}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if got := string(b); got != "# keep me\nSUSPT_DB=x.db\nSEC_USER_AGENT=\"A a@b.co\"\nTIINGO_API_KEY=new\n" {
		t.Errorf(".env = %q", got)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf(".env mode %v, want 0600", fi.Mode().Perm())
	}
	ny, _ := time.LoadLocation("America/New_York")
	for at, want := range map[string]string{
		"2026-10-20 15:00": "2026-10-19", // Tuesday before the close: Monday
		"2026-10-20 17:00": "2026-10-20", // after the close: today
		"2026-10-19 09:00": "2026-10-16", // Monday morning: Friday
		"2026-10-18 12:00": "2026-10-16", // Sunday: Friday
	} {
		tm, _ := time.ParseInLocation("2006-01-02 15:04", at, ny)
		if got := lastClosedSession(tm); got != want {
			t.Errorf("lastClosedSession(%s) = %s, want %s", at, got, want)
		}
	}
}

// Regression: ISSUE-004: refresh without API keys said "fetching" and failed silently
// Found by /qa on 2026-10-20
// Report: .gstack/qa-reports/qa-report-127-0-0-1-2026-10-20.md
func TestRefreshWithoutKeysIsRefused(t *testing.T) {
	t.Setenv("TIINGO_API_KEY", "")
	t.Setenv("SEC_USER_AGENT", "")
	a := &app{db: testDB(t), dbPath: filepath.Join(t.TempDir(), "x.db")}
	err := a.startRefresh("manual")
	if err == nil || !strings.Contains(err.Error(), "data sources") {
		t.Fatalf("startRefresh without keys: %v, want a 'connect the data sources' error", err)
	}
	if a.status()["job"].(jobStatus).Running {
		t.Fatal("a refresh job started without keys")
	}
}
