package main

import (
	"strings"
	"testing"
)

// The home page reports a rule on the day it turned true, not on every day it stayed
// true, and a level on the day the close went through it.
func TestMonitorEvents(t *testing.T) {
	db := testDB(t)
	closes := []float64{10, 10, 10, 12, 12.2, 12.4, 9}
	for i, c := range closes {
		db.Exec(`INSERT INTO prices (ticker, date, open, high, low, close) VALUES ('AAA', ?, ?, ?, ?, ?)`, addDays("2026-03-01", i), c, c, c, c)
	}
	db.Exec(`INSERT INTO watchlist (ticker, added_on, starred) VALUES ('AAA', '2026-03-01', 1)`)
	if err := setLevel(db, "AAA", "support", 11); err != nil {
		t.Fatal(err)
	}
	if err := addRule(db, &rule{Name: "over", Ticker: "*", Action: "WATCH", Thesis: "t", Condition: "close > 11"}); err != nil {
		t.Fatal(err)
	}
	m, err := monitorReport(db, "2026-03-01")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range m.Events {
		got = append(got, e.Date+" "+e.Kind)
	}
	want := "2026-03-07 move,2026-03-07 level,2026-03-04 move,2026-03-04 level,2026-03-04 rule"
	if strings.Join(got, ",") != want {
		t.Errorf("events %v\nwant %s", got, want)
	}
	if len(m.Tiles) != 1 || m.Tiles[0].Level == nil || m.Tiles[0].Level.Name != "support" {
		t.Errorf("tiles %+v", m.Tiles)
	}
	ideas, err := ideasReport(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range ideas {
		if i.Rule == "" || strings.Contains(strings.ToLower(i.Title+i.Because), "buy now") {
			t.Errorf("idea without its rule, or a buy-now call: %+v", i)
		}
	}
}
