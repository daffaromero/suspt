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

func TestShortlist(t *testing.T) {
	db := testDB(t)
	if _, err := shortlistTop(db, 10); err == nil {
		t.Fatal("shortlistTop without SPUS holdings should explain, not star nothing silently")
	}
	for i := range 12 {
		db.Exec(`INSERT INTO index_members (ticker, fund, as_of, weight) VALUES (?, 'SPUS', '2026-10-20', ?)`, string(rune('A'+i)), float64(i))
	}
	top, err := shortlistTop(db, 10)
	if err != nil || len(top) != 10 || top[0] != "L" || top[9] != "C" {
		t.Fatalf("top = %v, %v; want the 10 heaviest, L first and C last", top, err)
	}
	var starred, watched int
	db.QueryRow(`SELECT SUM(starred), COUNT(*) FROM watchlist`).Scan(&starred, &watched)
	if starred != 10 || watched != 10 {
		t.Fatalf("starred %d of %d watched, want 10 of 10", starred, watched)
	}
	if err := setStar(db, []string{"L"}, false); err != nil {
		t.Fatal(err)
	}
	db.QueryRow(`SELECT SUM(starred) FROM watchlist`).Scan(&starred)
	if starred != 9 {
		t.Fatalf("after unstarring one: %d starred, want 9", starred)
	}
}
