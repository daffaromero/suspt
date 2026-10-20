package main

import "testing"

// Regression: ISSUE-003: "not a ticker!!" added NOT and A before failing on TICKER!!
// Found by /qa on 2026-10-20
// Report: .gstack/qa-reports/qa-report-127-0-0-1-2026-10-20.md
func TestAddWatchRejectsWholeInput(t *testing.T) {
	db := testDB(t)
	if _, err := addWatch(db, []string{"not", "a", "ticker!!"}); err == nil {
		t.Fatal("invalid input accepted")
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM watchlist`).Scan(&n)
	if n != 0 {
		t.Fatalf("watchlist has %d entries after a rejected add, want 0", n)
	}
	if added, err := addWatch(db, []string{" nvda", "AAPL ", "nvda"}); err != nil || added != 2 {
		t.Fatalf("valid add: added %d, err %v; want 2, nil", added, err)
	}
}
