package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestPracticeNeverShowsTheFuture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "real.db")
	real, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer real.Close()
	for d := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC); d.Before(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)); d = d.AddDate(0, 0, 1) {
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			continue
		}
		for _, tk := range []string{"SPUS", "AAA"} {
			real.Exec(`INSERT INTO prices (ticker, date, open, high, low, close, volume) VALUES (?, ?, 10, 10, 10, 10, 1)`, tk, d.Format(time.DateOnly))
		}
	}
	real.Exec(`INSERT INTO companies (ticker, cik, name, sic) VALUES ('AAA', 1, 'A Co', '3571')`)
	real.Exec(`INSERT INTO facts (cik, taxonomy, concept, unit, end, val, filed) VALUES (1, 'us-gaap', 'Assets', 'USD', '2025-12-31', 1, '2026-01-10'), (1, 'us-gaap', 'Assets', 'USD', '2026-01-31', 1, '2026-02-20')`)
	real.Exec(`INSERT INTO orders (placed_on, ticker, side, notional, reason, thesis) VALUES ('2026-01-02', 'AAA', 'buy', 50, 'real', 'real')`)

	a := &app{db: real, dbPath: path}
	if err := a.startPractice("custom", "2026-01-15", []string{"AAA"}); err != nil {
		t.Fatal(err)
	}
	p, _ := a.practiceDB()
	latest := func(d *sql.DB, q string) string {
		var s sql.NullString
		d.QueryRow(q).Scan(&s)
		return s.String
	}
	check := func(clock string) {
		t.Helper()
		if got := latest(p, `SELECT value FROM settings WHERE key = 'practice_clock'`); got != clock {
			t.Fatalf("clock = %s, want %s", got, clock)
		}
		if got := latest(p, `SELECT MAX(date) FROM prices`); got > clock {
			t.Errorf("practice sees prices from %s, after the practice date %s", got, clock)
		}
		if got := latest(p, `SELECT MAX(filed) FROM facts`); got > clock {
			t.Errorf("practice sees a filing from %s, after the practice date %s", got, clock)
		}
		d, err := loadTickerDetail(p, "AAA")
		if err != nil {
			t.Fatal(err)
		}
		if last := d.Bars[len(d.Bars)-1].Time; last != clock {
			t.Errorf("chart ends %s, want the practice date %s", last, clock)
		}
	}
	check("2026-01-15")

	if today(p) != "2026-01-15" || today(real) == "2026-01-15" {
		t.Fatalf("today: practice %s (want 2026-01-15), real %s (want New York today)", today(p), today(real))
	}
	o := order{Ticker: "SPUS", Side: "buy", Notional: sql.NullFloat64{Float64: 50, Valid: true}, Reason: "practice", Thesis: "t"}
	if err := placeOrder(p, &o); err != nil || o.PlacedOn != "2026-01-15" {
		t.Fatalf("practice order: %v, placed %s; want placed on the practice date", err, o.PlacedOn)
	}
	if next, err := a.advancePractice("week"); err != nil || next != "2026-01-22" {
		t.Fatalf("advance a week from 2026-01-15: %s, %v; want 2026-01-22", next, err)
	}
	check("2026-01-22")
	var filledOn string
	p.QueryRow(`SELECT filled_on FROM orders WHERE id = ?`, o.ID).Scan(&filledOn)
	if filledOn != "2026-01-16" {
		t.Errorf("practice order filled %q, want the next trading day 2026-01-16", filledOn)
	}
	if next, _ := a.advancePractice("month"); next != "2026-02-20" {
		t.Fatalf("advance a month from 2026-01-22: %s, want 2026-02-20", next)
	}
	check("2026-02-20")

	var realOrders int
	real.QueryRow(`SELECT COUNT(*) FROM orders`).Scan(&realOrders)
	var practiceOrders int
	p.QueryRow(`SELECT COUNT(*) FROM orders WHERE reason = 'real'`).Scan(&practiceOrders)
	if realOrders != 1 || practiceOrders != 0 {
		t.Errorf("real orders %d (want 1), real orders copied into practice %d (want 0)", realOrders, practiceOrders)
	}
	if err := a.endPractice(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.practiceDB(); err != errNoPractice {
		t.Errorf("after end: %v, want errNoPractice", err)
	}
}
