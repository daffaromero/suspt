package main

import (
	"fmt"
	"math"
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
			s = nextState(s, w)
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
