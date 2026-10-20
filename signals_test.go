package main

import "testing"

func TestAdjustedSeries(t *testing.T) {
	s := adjustedSeries([]ohlc{
		{Date: "d1", Close: 100, Split: 1},
		{Date: "d2", Close: 50, Split: 2}, // 2:1 split: d1 must become 50
		{Date: "d3", Close: 51, Split: 1},
	})
	if s.Close[0] != 50 || s.Close[1] != 50 || s.Close[2] != 51 {
		t.Fatalf("adjusted closes = %v, want [50 50 51]", s.Close)
	}
}

func TestRules(t *testing.T) {
	closes := []float64{10, 10, 10, 9, 12} // sma3: -, -, 10, 9.67, 10.33
	s := series{Dates: make([]string, 5), Open: closes, High: closes, Low: closes, Close: closes}
	lv := map[string]float64{"support": 9.5}
	tests := []struct {
		cond string
		i    int
		want bool
	}{
		{"close crosses_above sma3", 4, true},
		{"close crosses_above sma3", 3, false},
		{"close crosses_below sma3", 3, true},
		{"close < level:support", 3, true},
		{"close < level:missing", 3, false},
		{"chg > 30 and close > sma3", 4, true}, // +33.3%
		{"chg > 40 and close > sma3", 4, false},
		{"sma3 > 0", 1, false}, // not enough history
	}
	for _, tt := range tests {
		cs, err := parseCondition(tt.cond)
		if err != nil {
			t.Fatalf("%q: %v", tt.cond, err)
		}
		if got := matches(cs, s, tt.i, lv); got != tt.want {
			t.Errorf("%q at %d = %v, want %v", tt.cond, tt.i, got, tt.want)
		}
	}
	for _, bad := range []string{"", "close >", "close ~ 5", "close > 5 or chg > 1", "sma1 > 5", "price > 5", "close > 5 and"} {
		if _, err := parseCondition(bad); err == nil {
			t.Errorf("parseCondition(%q) accepted invalid input", bad)
		}
	}
}
