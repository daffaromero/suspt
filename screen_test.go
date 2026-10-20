package main

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNextState(t *testing.T) {
	tests := []struct {
		name  string
		start screenState
		worst []float64
		want  bool
	}{
		{"first eval fails at limit", screenState{}, []float64{0.30}, false},
		{"first eval passes", screenState{}, []float64{0.29}, true},
		{"compliant, 2 periods in buffer stays", screenState{Known: true, Compliant: true}, []float64{0.31, 0.31}, true},
		{"compliant, 3rd period in buffer flips", screenState{Known: true, Compliant: true}, []float64{0.31, 0.31, 0.31}, false},
		{"compliant, buffer streak resets on pass", screenState{Known: true, Compliant: true}, []float64{0.31, 0.31, 0.29, 0.31, 0.31}, true},
		{"compliant, above buffer flips now", screenState{Known: true, Compliant: true}, []float64{0.325}, false},
		{"non-compliant, 2 passes in buffer stays", screenState{Known: true}, []float64{0.29, 0.29}, false},
		{"non-compliant, 3rd pass in buffer flips", screenState{Known: true}, []float64{0.29, 0.285, 0.29}, true},
		{"non-compliant, clear pass flips now", screenState{Known: true}, []float64{0.27}, true},
		{"non-compliant, fail resets streak", screenState{Known: true}, []float64{0.29, 0.29, 0.30, 0.29}, false},
	}
	for _, tt := range tests {
		s := tt.start
		for _, w := range tt.worst {
			s = nextState(s, w, 0.30)
		}
		if s.Compliant != tt.want {
			t.Errorf("%s: compliant = %v, want %v (state %+v)", tt.name, s.Compliant, tt.want, s)
		}
	}
}

func TestEvaluate(t *testing.T) {
	// 36 month-ends, price 10 until a 2:1 split on 2025-06-02, then 5. Shares were
	// last reported pre-split, so market cap stays 1000 only if the split is applied.
	var bars []bar
	for m := 0; m < 36; m++ {
		date := fmt.Sprintf("%d-%02d-28", 2023+m/12, m%12+1)
		px := 10.0
		if date > "2025-06" {
			px = 5
		}
		if date[:7] == "2025-06" {
			bars = append(bars, bar{Date: "2025-06-02", Close: 5, Split: 2})
		}
		bars = append(bars, bar{Date: date, Close: px, Split: 1})
	}
	months := monthEnds(bars)
	splits := []bar{{Date: "2025-06-02", Split: 2}}

	bs := func(concept string, val float64, filed string) sfact {
		return sfact{Concept: concept, End: "2025-09-30", Filed: filed, FP: "Q3", Val: val}
	}
	fy := func(concept string, val float64) sfact {
		return sfact{Concept: concept, Start: "2024-10-01", End: "2025-09-30", Filed: "2025-11-01", FP: "FY", Val: val}
	}
	facts := []sfact{
		{Concept: "EntityCommonStockSharesOutstanding", End: "2023-01-01", Filed: "2023-02-01", Val: 100},
		bs("Assets", 5000, "2025-10-30"),
		bs("LongTermDebt", 200, "2025-10-30"),
		bs("LongTermDebt", 999, "2026-02-01"), // restated after the evaluation date: must be invisible
		bs("CommercialPaper", 50, "2025-10-30"),
		bs("CashAndCashEquivalentsAtCarryingValue", 30, "2025-10-30"),
		bs("MarketableSecuritiesCurrent", 20, "2025-10-30"),
		bs("MarketableSecuritiesNoncurrent", 10, "2025-10-30"),
		fy("Revenues", 1000),
		fy("InvestmentIncomeInterest", 20),
		fy("OtherNonoperatingIncomeExpense", 30),
	}

	r, ok := evaluate(facts, months, splits, len(months)-1, 0.01)
	if !ok {
		t.Fatalf("evaluate not ok: %v", r.Notes)
	}
	want := map[string][2]float64{
		"debt":  {r.Debt, 250.0 / 1000},
		"cash":  {r.Cash, 60.0 / 1000},
		"npi":   {r.NPI, (10.0 + 20) / (1000 + 20)},
		"purif": {r.Purif, (10.0 + 20 + 30) / (1000 + 20 + 30)},
	}
	for k, v := range want {
		if math.Abs(v[0]-v[1]) > 1e-9 {
			t.Errorf("%s = %v, want %v", k, v[0], v[1])
		}
	}
	if len(r.Notes) != 0 {
		t.Errorf("unexpected notes: %v", r.Notes)
	}
}

func TestSICVerdict(t *testing.T) {
	for sic, want := range map[string]string{"6021": "excluded", "2111": "excluded", "2080": "review", "3571": "ok", "7830": "excluded", "": "review"} {
		if got := sicVerdict(sic); got != want {
			t.Errorf("sicVerdict(%q) = %s, want %s", sic, got, want)
		}
	}
}

func TestSyncSPUSAndMembership(t *testing.T) {
	csvBody := "Date,Account,StockTicker,CUSIP,SecurityName,Shares,Price,MarketValue,Weightings\n" +
		"10/20/2026,SPUS,Cash&Other,x,Cash,1,1,1,0.10%\n10/20/2026,SPUS,2602335D,x,TPG,1,1,1,0.00%\n10/20/2026,SPUS,BRK.B,x,B,1,1,1,1.00%\n"
	for i := range 60 {
		csvBody += fmt.Sprintf("10/20/2026,SPUS,T%c%c,x,Co,1,1,1,0.50%%\n", 'A'+i/26, 'A'+i%26)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, csvBody) }))
	defer srv.Close()
	spusHoldingsURL = srv.URL
	db := testDB(t)

	members, err := syncSPUS(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 61 || members[0] != "BRK-B" {
		t.Fatalf("members = %d (first %q), want 61 starting with BRK-B", len(members), members[0])
	}

	// A SIC-"ok" company outside SPUS needs review; a member does not.
	for _, tk := range []string{"TAA", "OUT"} {
		db.Exec(`INSERT INTO companies (ticker, cik, name, sic) VALUES (?, ?, 'x', '3571')`, tk, len(tk)+int(tk[0]))
		db.Exec(`INSERT INTO prices (ticker, date, open, high, low, close, volume) VALUES (?, '2026-10-20', 1, 1, 1, 1, 1)`, tk)
	}
	for tk, want := range map[string]string{"TAA": "ok", "OUT": "review"} {
		rows, err := screenTicker(db, tk)
		if err != nil {
			t.Fatal(err)
		}
		if got := rows[len(rows)-1].Business; got != want {
			t.Errorf("%s business = %s, want %s", tk, got, want)
		}
	}
}

func TestDowJonesWindow(t *testing.T) {
	// 12 months at 20 then 24 at 10, 100 shares: S&P averages 36 months (1333.33),
	// Dow Jones only the last 24 (1000), so the same $250 of debt reads higher for Dow Jones.
	var months []bar
	for m := range 36 {
		px := 10.0
		if m < 12 {
			px = 20
		}
		months = append(months, bar{Date: fmt.Sprintf("%d-%02d-28", 2023+m/12, m%12+1), Close: px, Split: 1})
	}
	facts := []sfact{
		{Concept: "EntityCommonStockSharesOutstanding", End: "2022-12-31", Filed: "2023-01-15", Val: 100},
		{Concept: "Assets", End: "2025-09-30", Filed: "2025-10-30", Val: 1},
		{Concept: "LongTermDebt", End: "2025-09-30", Filed: "2025-10-30", Val: 250},
	}
	r, ok := evaluate(facts, months, nil, len(months)-1, 0)
	if !ok || !near(r.Debt, 250/(48000.0/36)) || !near(r.DJDebt, 0.25) {
		t.Fatalf("S&P debt %v (want %v), Dow Jones debt %v (want 0.25)", r.Debt, 250/(48000.0/36), r.DJDebt)
	}
}

// Regression: IBM and EXPE are held by SPUS but screened NON_COMPLIANT because the
// October 2026 limits (30%, cash test) were applied to history that ran under 33%.
func TestRuleChangeUsesBuffer(t *testing.T) {
	if l, cash := accountingRule("2026-08-31"); l != 0.33 || cash {
		t.Errorf("before the change: %v, %v; want 0.33 without cash test", l, cash)
	}
	if l, cash := accountingRule("2026-09-30"); l != 0.30 || !cash {
		t.Errorf("after the change: %v, %v; want 0.30 with cash test", l, cash)
	}
	// Debt 31% for years: compliant under 33%, then inside the 2-point buffer after the
	// switch, so it stays compliant for two more periods and flips on the third.
	var s screenState
	var got []bool
	for _, d := range []string{"2026-07-31", "2026-08-31", "2026-09-30", "2026-10-30", "2026-11-30"} {
		w, l := worstRatio(d, 0.31, 0.05)
		s = nextState(s, w, l)
		got = append(got, s.Compliant)
	}
	if fmt.Sprint(got) != "[true true true true false]" {
		t.Errorf("compliance over the rule change = %v, want [true true true true false]", got)
	}
}

func TestAAOIFIReference(t *testing.T) {
	// Same history as TestDowJonesWindow: today's value is 10 x 100 = 1000, so $250 of
	// debt is 25% for AAOIFI (no averaging), against 18.75% for S&P's 36-month average.
	var months []bar
	for m := range 36 {
		px := 10.0
		if m < 12 {
			px = 20
		}
		months = append(months, bar{Date: fmt.Sprintf("%d-%02d-28", 2023+m/12, m%12+1), Close: px, Split: 1})
	}
	facts := []sfact{
		{Concept: "EntityCommonStockSharesOutstanding", End: "2022-12-31", Filed: "2023-01-15", Val: 100},
		{Concept: "Assets", End: "2025-09-30", Filed: "2025-10-30", Val: 1},
		{Concept: "LongTermDebt", End: "2025-09-30", Filed: "2025-10-30", Val: 250},
	}
	r, _ := evaluate(facts, months, nil, len(months)-1, 0)
	if !near(r.AADebt, 0.25) {
		t.Fatalf("AAOIFI debt = %v, want 0.25 of today's value", r.AADebt)
	}
	// "Not exceed 30%": exactly 30% passes, anything above fails, with no buffer.
	for debt, want := range map[float64]string{0.30: "COMPLIANT", 0.3001: "NON_COMPLIANT"} {
		if got := aaoifiStatus("ok", ratios{AADebt: debt, AACash: 0, NPI: 0.01}); got != want {
			t.Errorf("AAOIFI at debt %v = %s, want %s", debt, got, want)
		}
	}
	if got := aaoifiStatus("ok", ratios{AADebt: 0.1, AACash: 0.1, NPI: 0.051}); got != "NON_COMPLIANT" {
		t.Errorf("impure income 5.1%% = %s, want NON_COMPLIANT", got)
	}
}
