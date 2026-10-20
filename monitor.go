package main

import (
	"database/sql"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

// The home page: the stocks you follow at a glance, what changed since your last visit,
// and ideas worked out from plain rules. Nothing here is a prediction or advice; every
// idea says which rule produced it.

type tile struct {
	Ticker   string     `json:"ticker"`
	Name     string     `json:"name"`
	Date     string     `json:"date"`
	Close    float64    `json:"close"`
	Change   float64    `json:"change"`   // since the previous close
	Change1M float64    `json:"change1m"` // over 21 trading days
	Spark    []float64  `json:"spark"`    // the last 60 closes
	Status   string     `json:"status"`
	Held     bool       `json:"held"`
	Starred  bool       `json:"starred"`
	Trend    string     `json:"trend"` // up, down or mixed, from the 50- and 200-day averages
	Level    *levelDist `json:"level,omitempty"`
	Signals  []signal   `json:"signals"`
}

type event struct {
	Date   string `json:"date"`
	Kind   string `json:"kind"` // fill, dividend, purification, split, compliance, level, rule, move
	Ticker string `json:"ticker"`
	Text   string `json:"text"`
}

type monitor struct {
	AsOf   string  `json:"asOf"`
	Since  string  `json:"since"`
	Tiles  []tile  `json:"tiles"`
	Events []event `json:"events"`
}

func smaAt(c []float64, i, n int) float64 {
	if i < n-1 {
		return math.NaN()
	}
	sum := 0.0
	for _, v := range c[i-n+1 : i+1] {
		sum += v
	}
	return sum / float64(n)
}

func trendOf(s series) string {
	i := len(s.Close) - 1
	c, a50, a200 := s.Close[i], smaAt(s.Close, i, 50), smaAt(s.Close, i, 200)
	switch {
	case math.IsNaN(a200):
		return "mixed"
	case c > a50 && a50 > a200:
		return "up"
	case c < a50 && a50 < a200:
		return "down"
	}
	return "mixed"
}

func queryStrings(db *sql.DB, q string, args ...any) []string {
	var out []string
	rs, err := db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rs.Close()
	for rs.Next() {
		var s string
		rs.Scan(&s)
		out = append(out, s)
	}
	return out
}

// followed is the shortlist plus what you hold, the stocks the home page is about.
func followed(db *sql.DB) []string {
	return queryStrings(db, `SELECT ticker FROM watchlist WHERE starred = 1 UNION SELECT ticker FROM lots WHERE open_qty > 0 ORDER BY 1`)
}

func latestDate(db *sql.DB) string {
	var d sql.NullString
	db.QueryRow(`SELECT MAX(date) FROM equity`).Scan(&d)
	if !d.Valid {
		db.QueryRow(`SELECT MAX(date) FROM prices`).Scan(&d)
	}
	return d.String
}

func monitorReport(db *sql.DB, since string) (monitor, error) {
	m := monitor{AsOf: latestDate(db), Tiles: []tile{}, Events: []event{}}
	if _, err := time.Parse(time.DateOnly, since); m.AsOf == "" {
		since = "" // no data yet
	} else if err != nil || since >= m.AsOf {
		since = addDays(m.AsOf, -7)
	}
	m.Since = since
	set, err := loadSettings(db)
	if err != nil {
		return m, err
	}
	sigs, err := currentSignals(db)
	if err != nil {
		return m, err
	}
	cache := map[string]series{}
	load := func(t string) (series, error) {
		if s, ok := cache[t]; ok {
			return s, nil
		}
		bars, err := loadOHLC(db, t)
		if err != nil {
			return series{}, err
		}
		cache[t] = adjustedSeries(bars)
		return cache[t], nil
	}
	held := map[string]bool{}
	for _, t := range queryStrings(db, `SELECT DISTINCT ticker FROM lots WHERE open_qty > 0`) {
		held[t] = true
	}
	starred := map[string]bool{}
	for _, t := range queryStrings(db, `SELECT ticker FROM watchlist WHERE starred = 1`) {
		starred[t] = true
	}
	add := func(date, kind, ticker, format string, args ...any) {
		if date > since {
			m.Events = append(m.Events, event{date, kind, ticker, fmt.Sprintf(format, args...)})
		}
	}

	for _, t := range followed(db) {
		s, err := load(t)
		if err != nil {
			return m, err
		}
		tl := tile{Ticker: t, Status: tickerStatus(db, set, t), Held: held[t], Starred: starred[t], Spark: []float64{}, Signals: []signal{}}
		db.QueryRow(`SELECT COALESCE(name, '') FROM companies WHERE ticker = ?`, t).Scan(&tl.Name)
		for _, sg := range sigs {
			if sg.Ticker == t {
				tl.Signals = append(tl.Signals, sg)
			}
		}
		n := len(s.Close)
		if n == 0 {
			m.Tiles = append(m.Tiles, tl)
			continue
		}
		tl.Date, tl.Close, tl.Trend = s.Dates[n-1], s.Close[n-1], trendOf(s)
		if n > 1 {
			tl.Change = s.Close[n-1]/s.Close[n-2] - 1
		}
		if n > 21 {
			tl.Change1M = s.Close[n-1]/s.Close[n-22] - 1
		}
		tl.Spark = s.Close[max(0, n-60):]
		lv, err := loadLevels(db, t)
		if err != nil {
			return m, err
		}
		for name, p := range lv {
			d := levelDist{name, p, (p - tl.Close) / tl.Close}
			if tl.Level == nil || math.Abs(d.Dist) < math.Abs(tl.Level.Dist) {
				tl.Level = &d
			}
		}
		for i := 1; i < n; i++ { // big daily moves
			if chg := s.Close[i]/s.Close[i-1] - 1; math.Abs(chg) >= 0.05 {
				add(s.Dates[i], "move", t, "%s %s %.1f%% in a day, to $%.2f", t, map[bool]string{true: "rose", false: "fell"}[chg > 0], math.Abs(chg)*100, s.Close[i])
			}
		}
		m.Tiles = append(m.Tiles, tl)
	}

	// Your levels: the day the close went through one.
	rs, err := db.Query(`SELECT ticker, name, price FROM levels ORDER BY ticker, name`)
	if err != nil {
		return m, err
	}
	type lvl struct {
		t, name string
		p       float64
	}
	var lvls []lvl
	for rs.Next() {
		var l lvl
		rs.Scan(&l.t, &l.name, &l.p)
		lvls = append(lvls, l)
	}
	rs.Close()
	for _, l := range lvls {
		s, err := load(l.t)
		if err != nil {
			return m, err
		}
		for i := 1; i < len(s.Close); i++ {
			a, b := s.Close[i-1], s.Close[i]
			if a < l.p && b >= l.p {
				add(s.Dates[i], "level", l.t, "%s rose through your %s level ($%.2f)", l.t, l.name, l.p)
			} else if a > l.p && b <= l.p {
				add(s.Dates[i], "level", l.t, "%s fell to your %s level ($%.2f)", l.t, l.name, l.p)
			}
		}
	}

	// Your rules: the day each one turned true, not every day it stayed true.
	rules, err := loadRules(db, false)
	if err != nil {
		return m, err
	}
	watch, err := watchlistTickers(db)
	if err != nil {
		return m, err
	}
	for _, r := range rules {
		tickers := []string{r.Ticker}
		if r.Ticker == "*" {
			tickers = watch
		}
		for _, t := range tickers {
			s, err := load(t)
			if err != nil {
				return m, err
			}
			lv, _ := loadLevels(db, t)
			for i := len(s.Close) - 1; i > 0 && s.Dates[i] > since; i-- {
				if matches(r.clauses, s, i, lv) && !matches(r.clauses, s, i-1, lv) {
					add(s.Dates[i], "rule", t, "your rule %s (%s) turned true for %s", r.Name, strings.ToLower(map[string]string{"BUY": "buy idea", "WATCH": "watch"}[r.Action]), t)
				}
			}
		}
	}

	// Compliance changes for the stocks you watch or hold.
	for _, t := range append(watch, queryStrings(db, `SELECT DISTINCT ticker FROM lots WHERE open_qty > 0`)...) {
		pts, err := loadScreenPoints(db, t)
		if err != nil {
			return m, err
		}
		for i := 1; i < len(pts); i++ {
			if pts[i].Status != pts[i-1].Status && pts[i].Status != "NO_DATA" && pts[i-1].Status != "NO_DATA" {
				label := map[string]string{"COMPLIANT": "compliant", "NON_COMPLIANT": "non-compliant", "REVIEW": "in need of your review"}[pts[i].Status]
				add(pts[i].AsOf, "compliance", t, "%s became %s in the %s screen", t, orDefault(label, strings.ToLower(pts[i].Status)), pts[i].AsOf[:7])
			}
		}
	}

	// The account: fills, dividends, purification, splits.
	rs, err = db.Query(`SELECT date, kind, COALESCE(ticker, ''), COALESCE(qty, 0), COALESCE(price, 0), amount, COALESCE(order_id, 0) FROM ledger
		WHERE date > ? AND kind IN ('buy', 'sell', 'dividend', 'purification', 'split') ORDER BY date, id`, since)
	if err != nil {
		return m, err
	}
	for rs.Next() {
		var date, kind, t string
		var qty, price, amount float64
		var id int64
		rs.Scan(&date, &kind, &t, &qty, &price, &amount, &id)
		switch kind {
		case "buy", "sell":
			verb := map[string]string{"buy": "bought", "sell": "sold"}[kind]
			add(date, "fill", t, "Order #%d filled: you %s %s shares of %s at $%.2f", id, verb, trimNum(qty), t, price)
		case "dividend":
			add(date, "dividend", t, "%s paid you a dividend of $%.2f", t, amount)
		case "purification":
			add(date, "purification", t, "$%.2f of the %s dividend was set aside to give away", -amount, t)
		case "split":
			add(date, "split", t, "%s split its shares %g-for-1; your share count changed, your money didn't", t, qty)
		}
	}
	rs.Close()

	sort.SliceStable(m.Events, func(i, j int) bool { return m.Events[i].Date > m.Events[j].Date })
	if len(m.Events) > 60 {
		m.Events = m.Events[:60]
	}
	return m, nil
}

func trimNum(x float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", x), "0"), ".")
}

type idea struct {
	Kind    string `json:"kind"`
	Ticker  string `json:"ticker,omitempty"`
	Title   string `json:"title"`
	Because string `json:"because"`
	Action  string `json:"action,omitempty"` // buy, sell, open, journal, screen, rules, shortlist
	Rule    string `json:"rule"`             // the plain rule that produced it
}

// ideasReport turns the account and the shortlist into suggestions. Each kind is one
// fixed rule written out in Rule, so the person can see exactly why it appeared.
func ideasReport(db *sql.DB) ([]idea, error) {
	out := []idea{}
	set, err := loadSettings(db)
	if err != nil {
		return out, err
	}
	sum, err := portfolioSummary(db)
	if err != nil {
		return out, err
	}
	heldSectors := map[string]bool{}
	held := map[string]position{}
	for _, p := range sum.Positions {
		held[p.Ticker], heldSectors[p.Sector] = p, true
	}

	// 1. Held but no longer compliant.
	for _, p := range sum.Positions {
		if p.Status == "NON_COMPLIANT" {
			what := "suspt will sell it at a coming market open"
			if set["compliance_sell"] == "alert" {
				what = "your settings say to warn you rather than sell, so the decision is yours"
			}
			out = append(out, idea{"noncompliant", p.Ticker, fmt.Sprintf("%s no longer passes the sharia screen", p.Ticker),
				fmt.Sprintf("You hold %s shares worth $%.2f. Its latest screen failed; %s.", trimNum(p.Qty), p.Value, what), "sell",
				"a stock you hold fails the S&P screen"})
		}
	}
	// 2. Held and close to a screen limit.
	for _, p := range sum.Positions {
		var debt, cash sql.NullFloat64
		db.QueryRow(`SELECT debt_ratio, cash_ratio FROM screens WHERE ticker = ? ORDER BY as_of DESC LIMIT 1`, p.Ticker).Scan(&debt, &cash)
		for _, r := range []struct {
			name string
			v    sql.NullFloat64
		}{{"debt", debt}, {"cash", cash}} {
			if p.Status == "COMPLIANT" && r.v.Valid && r.v.Float64 >= 0.27 {
				out = append(out, idea{"near-limit", p.Ticker, fmt.Sprintf("%s is close to the %s limit", p.Ticker, r.name),
					fmt.Sprintf("Its %s is %.1f%% of company value; the limit is 30%%. A few more months like this and it could stop being compliant.", r.name, r.v.Float64*100),
					"open", "a stock you hold has debt or cash at 27% or more"})
			}
		}
	}
	// 3. Too much in one stock, with shortlist stocks from other sectors to compare.
	limit := set.num("max_position_pct")
	for _, p := range sum.Positions {
		if p.Status == "FUND" || p.Weight*100 <= limit {
			continue
		}
		var others []string
		for _, t := range queryStrings(db, `SELECT w.ticker FROM watchlist w LEFT JOIN index_members m ON m.ticker = w.ticker AND m.fund = 'SPUS'
			WHERE w.starred = 1 ORDER BY COALESCE(m.weight, 0) DESC`) {
			if _, h := held[t]; !h && !heldSectors[sectorOf(db, set, t)] && tickerStatus(db, set, t) == "COMPLIANT" && len(others) < 3 {
				others = append(others, t)
			}
		}
		because := fmt.Sprintf("It is %.0f%% of your stocks; your limit is %g%%. If it falls, most of what you've invested falls with it.", p.Weight*100, limit)
		if len(others) > 0 {
			because += " Shortlist stocks in other sectors: " + strings.Join(others, ", ") + "."
		}
		out = append(out, idea{"concentration", p.Ticker, fmt.Sprintf("A lot of your money is in %s", p.Ticker), because, "open",
			fmt.Sprintf("one stock is over %g%% of holdings (Settings)", limit)})
	}
	// 4. Your own buy rules.
	sigs, err := currentSignals(db)
	if err != nil {
		return out, err
	}
	for _, s := range sigs {
		if s.Action == "BUY" && !s.Blocked {
			out = append(out, idea{"rule", s.Ticker, fmt.Sprintf("Your rule %s is true for %s", s.Rule, s.Ticker),
				fmt.Sprintf("%s on %s. You wrote this rule for the theme %q; check the chart before deciding.", s.Condition, s.Date, s.Thesis), "open",
				"one of your buy-idea rules is true on the latest day"})
		}
	}
	// 5. A thesis that isn't working: down 15% after three months.
	rs, err := db.Query(`SELECT l.ticker, l.thesis, MIN(l.opened_on), SUM(l.open_qty), SUM(l.open_cost), COALESCE(o.reason, '') FROM lots l LEFT JOIN orders o ON o.id = l.order_id
		WHERE l.open_qty > 0 GROUP BY l.ticker, l.thesis`)
	if err != nil {
		return out, err
	}
	for rs.Next() {
		var t, thesis, opened, reason string
		var qty, cost float64
		rs.Scan(&t, &thesis, &opened, &qty, &cost, &reason)
		if px := lastClose(db, t); cost > 0 && opened <= addDays(sum.AsOf, -90) && qty*px/cost-1 <= -0.15 {
			out = append(out, idea{"thesis", t, fmt.Sprintf("Re-read why you bought %s", t),
				fmt.Sprintf("It is down %.0f%% since %s. You wrote: %q. Is that still true? Selling or holding are both fine; deciding on purpose is the point.", (1-qty*px/cost)*100, opened, reason),
				"journal", "a holding is 15% or more below cost after 90 days"})
		}
	}
	rs.Close()
	// 6. Cash doing nothing (waiting buy orders already have a use for theirs).
	var waiting float64
	db.QueryRow(`SELECT COALESCE(SUM(COALESCE(notional, qty * limit_price, 0)), 0) FROM orders WHERE status = 'open' AND side = 'buy'`).Scan(&waiting)
	if idle := sum.Cash - waiting; sum.Value > 0 && idle >= 25 && idle/sum.Value > 0.2 {
		out = append(out, idea{"cash", sum.Benchmark, fmt.Sprintf("$%.2f is sitting in cash", idle),
			fmt.Sprintf("That's %.0f%% of your account, not counting orders still waiting to fill. If you have nothing specific in mind, %s spreads it over every company in the S&P 500 Shariah index in one step; it's also what you're measured against.", idle/sum.Value*100, sum.Benchmark),
			"buy", "cash is over 20% of the account and at least $25"})
	}
	// 7 and 8. Shortlist trends, for learning to read a chart; never a reason on their own.
	for _, t := range queryStrings(db, `SELECT ticker FROM watchlist WHERE starred = 1 ORDER BY ticker`) {
		if tickerStatus(db, set, t) != "COMPLIANT" {
			continue
		}
		bars, err := loadOHLC(db, t)
		if err != nil {
			return out, err
		}
		s := adjustedSeries(bars)
		i := len(s.Close) - 1
		if i < 200 {
			continue
		}
		c, a50, a200 := s.Close[i], smaAt(s.Close, i, 50), smaAt(s.Close, i, 200)
		lv, _ := loadLevels(db, t)
		var near string
		for name, p := range lv {
			if p < c && (c-p)/c <= 0.03 {
				near = name
			}
		}
		switch {
		case a50 > a200 && (math.Abs(c/a50-1) <= 0.03 || near != ""):
			why := fmt.Sprintf("Its 50-day average ($%.2f) is above its 200-day ($%.2f), and the price ($%.2f) has come back to within 3%% of the 50-day.", a50, a200, c)
			if near != "" {
				why = fmt.Sprintf("Its 50-day average is above its 200-day, and the price ($%.2f) is within 3%% of your %s level ($%.2f).", c, near, lv[near])
			}
			out = append(out, idea{"pullback", t, fmt.Sprintf("%s pulled back in an uptrend", t), why + " People who buy dips look for this; it can also keep falling.", "open",
				"shortlist stock: 50-day above 200-day, price within 3% of the 50-day or of a level below it"})
		case c > a50 && a50 > a200:
			out = append(out, idea{"uptrend", t, fmt.Sprintf("%s is in an uptrend", t),
				fmt.Sprintf("Price $%.2f is above its 50-day average ($%.2f), which is above its 200-day ($%.2f). Trends often continue, until they don't.", c, a50, a200), "open",
				"shortlist stock: price above the 50-day, 50-day above the 200-day"})
		}
	}
	// 9. Purification money waiting to be given.
	var purified float64
	db.QueryRow(`SELECT COALESCE(-SUM(amount), 0) FROM ledger WHERE kind = 'purification'`).Scan(&purified)
	if purified >= 0.01 {
		out = append(out, idea{"purify", "", fmt.Sprintf("$%.2f is set aside to give away", purified),
			"Part of each dividend came from interest or haram income, so suspt set it aside. In a real account you'd give this amount to charity; it isn't a gift you get rewarded for, it cleans the income.", "",
			"dividends received include an impure share"})
	}
	if len(sum.Positions) == 0 && len(queryStrings(db, `SELECT ticker FROM watchlist WHERE starred = 1`)) == 0 {
		out = append(out, idea{"start", "", "Start with a shortlist",
			"Following 200 stocks is hard. Pick five to ten you understand, or start with the largest companies in the Sharia index, and watch them for a week before buying.", "shortlist",
			"nothing held and nothing shortlisted"})
	}
	slices.SortStableFunc(out, func(a, b idea) int {
		order := []string{"noncompliant", "near-limit", "concentration", "rule", "thesis", "start", "cash", "pullback", "uptrend", "purify"}
		return slices.Index(order, a.Kind) - slices.Index(order, b.Kind)
	})
	return out, nil
}
