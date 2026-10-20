package main

import (
	"database/sql"
	"maps"
	"math"
	"testing"
)

func testSettings(over map[string]string) settings {
	s := settings{}
	for _, d := range settingDefaults {
		s[d.Key] = d.Default
	}
	maps.Copy(s, over)
	return s
}

func f(v float64) sql.NullFloat64 { return sql.NullFloat64{Float64: v, Valid: true} }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestSimulateScenario(t *testing.T) {
	flat := func(d string, px float64) ohlc {
		return ohlc{Date: d, Open: px, High: px, Low: px, Close: px, Split: 1}
	}
	days := []string{"2026-01-05", "2026-01-06", "2026-01-07", "2026-01-08", "2026-01-09", "2026-01-12", "2026-02-02", "2026-02-03"}
	var bm, bbb []ohlc
	for _, d := range days {
		bm, bbb = append(bm, flat(d, 10)), append(bbb, flat(d, 10))
	}
	aaa := []ohlc{
		{Date: "2026-01-05", Open: 100, High: 101, Low: 99, Close: 100, Split: 1},
		{Date: "2026-01-06", Open: 100, High: 100, Low: 95, Close: 96, Split: 1}, // touches 95, not 90
		{Date: "2026-01-07", Open: 94, High: 95, Low: 93, Close: 94, Split: 1},
		{Date: "2026-01-08", Open: 47, High: 48, Low: 46, Close: 47, Split: 2},
		{Date: "2026-01-09", Open: 47, High: 47, Low: 47, Close: 47, Split: 1, Div: 0.5},
		flat("2026-01-12", 50), flat("2026-02-02", 50), flat("2026-02-03", 50),
	}
	screens := map[string][]screenPoint{
		"AAA": {{AsOf: "2025-12-31", Status: "COMPLIANT", Purif: 0.1}, {AsOf: "2026-01-09", Status: "NON_COMPLIANT", Purif: 0.1}},
	}
	orders := []*order{
		{ID: 1, PlacedOn: "2026-01-02", Ticker: "AAA", Side: "buy", Notional: f(100), Reason: "r", Thesis: "a"},
		{ID: 2, PlacedOn: "2026-01-05", Ticker: "AAA", Side: "buy", Qty: f(1), Limit: f(90), Reason: "r", Thesis: "a"},
		{ID: 3, PlacedOn: "2026-01-05", Ticker: "AAA", Side: "buy", Qty: f(1), Limit: f(95), GTC: true, Reason: "r", Thesis: "b"},
		{ID: 4, PlacedOn: "2026-01-05", Ticker: "BBB", Side: "buy", Qty: f(1), Reason: "r", Thesis: "a"},
		{ID: 5, PlacedOn: "2026-01-06", Ticker: "AAA", Side: "buy", Qty: f(10), Reason: "r", Thesis: "a"},
		{ID: 6, PlacedOn: "2026-01-06", Ticker: "AAA", Side: "buy", Qty: f(1), Limit: f(99), GTC: true, Reason: "r", Thesis: "a",
			CancelledOn: sql.NullString{String: "2026-01-06", Valid: true}},
	}
	set := testSettings(map[string]string{"start_date": "2026-01-05", "benchmark": "BM", "fee_flat": "1", "dividend_withholding": "15"})
	s := simulate(set, orders, map[string][]ohlc{"BM": bm, "AAA": aaa, "BBB": bbb}, screens)

	want := []string{"filled", "expired", "filled", "rejected", "rejected", "cancelled"}
	for i, o := range orders[:6] {
		if o.Status != want[i] {
			t.Errorf("order %d: status %s (%s), want %s", o.ID, o.Status, o.Note, want[i])
		}
	}
	if orders[0].FillQty != 0.99 || orders[2].FillPrice != 95 {
		t.Errorf("fills: #1 qty %v want 0.99, #3 price %v want 95", orders[0].FillQty, orders[2].FillPrice)
	}
	if auto := s.orders[len(s.orders)-1]; len(s.orders) != 7 || !auto.auto || auto.Status != "filled" || auto.FilledOn != "2026-02-02" || !near(auto.FillQty, 3.98) {
		t.Fatalf("compliance auto-sell missing or wrong: %+v", auto)
	}
	kinds := map[string]float64{}
	for _, e := range s.ledger {
		kinds[e.Kind] += e.Amount
	}
	if !near(kinds["dividend"], 1.99) || !near(kinds["withholding"], -0.30) || !near(kinds["purification"], -0.20) {
		t.Errorf("dividend legs = %v", kinds)
	}
	realized := 0.0
	for _, l := range s.lots {
		realized += l.Realized
		if l.OpenQty != 0 || l.ClosedOn != "2026-02-02" {
			t.Errorf("lot %d not closed by the auto-sell: %+v", l.ID, l)
		}
	}
	if !near(realized, 2.0) {
		t.Errorf("realized = %v, want 2.00 (sold 3.98 @ 50 - $1 fee vs $196 cost)", realized)
	}
	last := s.snaps[len(s.snaps)-1]
	// 300 - 100 - 96 + 1.49 net dividend + 198 sale + 50 deposit on 02-02 (Feb 1 is a Sunday)
	if !near(last.Cash, 353.49) || last.Contrib != 350 || last.Holdings != 0 {
		t.Errorf("final cash %v contrib %v holdings %v, want 353.49 350 0", last.Cash, last.Contrib, last.Holdings)
	}
	// Benchmark: $300 buys 29.9 @ 10 at the 01-05 open ($1 fee), $50 buys 4.9 at the 02-02 open.
	if !near(last.Bench, 348) {
		t.Errorf("benchmark = %v, want 348", last.Bench)
	}
}

func TestTWRAndDeposits(t *testing.T) {
	if got := twr([]float64{100, 110, 165}, []float64{100, 100, 150}); !near(got, 0.134375) {
		t.Errorf("twr = %v, want 0.134375", got)
	}
	d := depositDates(testSettings(map[string]string{"start_date": "2026-01-31", "deposit_day": "31"}), "2026-03-31")
	if d["2026-01-31"] != 300 || d["2026-02-28"] != 50 || d["2026-03-31"] != 50 || len(d) != 3 {
		t.Errorf("deposit dates = %v", d)
	}
}

func TestReviewRegressions(t *testing.T) {
	bar := func(d string, o, l float64, split float64) ohlc {
		return ohlc{Date: d, Open: o, High: o, Low: l, Close: o, Split: split}
	}
	bm := []ohlc{bar("2026-01-05", 10, 10, 1), bar("2026-01-06", 10, 10, 1), bar("2026-01-07", 10, 10, 1), bar("2026-01-08", 10, 10, 1)}
	set := testSettings(map[string]string{"start_date": "2026-01-05", "benchmark": "BM"})

	// A GTC limit buy is not rejected on a non-compliant day when it would not have filled anyway.
	aaa := []ohlc{bar("2026-01-05", 100, 100, 1), bar("2026-01-06", 100, 99, 1), bar("2026-01-07", 100, 99, 1), bar("2026-01-08", 90, 90, 1)}
	screens := map[string][]screenPoint{"AAA": {
		{AsOf: "2026-01-02", Status: "COMPLIANT"}, {AsOf: "2026-01-05", Status: "NON_COMPLIANT"}, {AsOf: "2026-01-07", Status: "COMPLIANT"}}}
	o := &order{ID: 1, PlacedOn: "2026-01-05", Ticker: "AAA", Side: "buy", Qty: f(1), Limit: f(95), GTC: true}
	simulate(set, []*order{o}, map[string][]ohlc{"BM": bm, "AAA": aaa}, screens)
	if o.Status != "filled" || o.FilledOn != "2026-01-08" || o.FillPrice != 90 {
		t.Errorf("GTC across a non-compliant month: %s %s @ %v (%s), want filled 2026-01-08 @ 90", o.Status, o.FilledOn, o.FillPrice, o.Note)
	}

	// A 2:1 split doubles an open GTC sell's quantity and halves its limit.
	bbb := []ohlc{bar("2026-01-05", 100, 100, 1), bar("2026-01-06", 100, 100, 1), bar("2026-01-07", 50, 50, 2), bar("2026-01-08", 61, 61, 1)}
	screens = map[string][]screenPoint{"BBB": {{AsOf: "2026-01-02", Status: "COMPLIANT"}}}
	buy := &order{ID: 1, PlacedOn: "2026-01-02", Ticker: "BBB", Side: "buy", Qty: f(1)}
	sell := &order{ID: 2, PlacedOn: "2026-01-05", Ticker: "BBB", Side: "sell", Qty: f(1), Limit: f(120), GTC: true}
	s := simulate(set, []*order{buy, sell}, map[string][]ohlc{"BM": bm, "BBB": bbb}, screens)
	if sell.Status != "filled" || sell.FillQty != 2 || sell.FillPrice != 61 || s.held("BBB") != 0 {
		t.Errorf("split-adjusted sell: %s qty %v @ %v, held %v; want filled 2 @ 61, held 0", sell.Status, sell.FillQty, sell.FillPrice, s.held("BBB"))
	}
}

// Regression: ISSUE-005: switching the benchmark to a fund without prices saved it and broke the replay
// Found by /qa on 2026-10-20
// Report: .gstack/qa-reports/qa-report-127-0-0-1-2026-10-20.md
func TestBenchmarkWithoutPricesIsRefused(t *testing.T) {
	db := testDB(t)
	if err := setSetting(db, "benchmark", "hlal"); err == nil {
		t.Fatal("benchmark without prices accepted")
	}
	set, _ := loadSettings(db)
	if set["benchmark"] != "SPUS" {
		t.Fatalf("benchmark = %q after a refused change, want the default SPUS", set["benchmark"])
	}
}
