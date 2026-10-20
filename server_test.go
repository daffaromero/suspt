package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	srv := httptest.NewServer(newServer(db))
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
