package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

var settingDefaults = []struct{ Key, Default, Help string }{
	{"start_date", "", "the day your practice account opens; changing it replays everything from that day"},
	{"initial_deposit", "300", "pretend cash you start with, in dollars"},
	{"monthly_deposit", "50", "pretend cash added every month, in dollars"},
	{"deposit_day", "1", "day of the month the deposit arrives (moves to the next trading day)"},
	{"fee_flat", "0", "broker fee charged on every order, in dollars"},
	{"fee_pct", "0", "broker fee as a percent of the order's value"},
	{"fee_min", "0", "smallest fee charged on any order, in dollars"},
	{"dividend_withholding", "0", "US tax taken from dividends before you receive them, in percent"},
	{"compliance_sell", "sell", "sell (automatically) or alert (only warn): what happens when a stock you hold fails the sharia screen"},
	{"auto_refresh", "on", "on or off: fetch new prices after each US market close while the app is open"},
	{"compliance_grace_days", "0", "days to wait before that automatic sale"},
	{"benchmark", "SPUS", "the fund your results are compared with; it gets the same deposits on the same dates"},
	{"allowed_etfs", "SPUS,HLAL", "sharia funds you may buy even though funds aren't screened like single stocks (comma-separated)"},
	{"max_position_pct", "20", "warn when one stock is more than this percent of what you hold"},
	{"max_sector_pct", "35", "warn when one sector is more than this percent of what you hold"},
	{"max_semis_pct", "25", "warn when semiconductor stocks are more than this percent of what you hold"},
}

type settings map[string]string

func (s settings) num(key string) float64 {
	v, _ := strconv.ParseFloat(s[key], 64)
	return v
}

func (s settings) etfs() []string {
	return strings.Split(strings.ToUpper(strings.ReplaceAll(s["allowed_etfs"], " ", "")), ",")
}

func loadSettings(db *sql.DB) (settings, error) {
	s := settings{}
	for _, d := range settingDefaults {
		s[d.Key] = d.Default
	}
	rs, err := db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	for rs.Next() {
		var k, v string
		if err := rs.Scan(&k, &v); err != nil {
			return nil, err
		}
		s[k] = v
	}
	return s, rs.Err()
}

func runConfig(db *sql.DB, args []string) error {
	s, err := loadSettings(db)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, d := range settingDefaults {
			fmt.Fprintf(w, "%s\t%s\t%s\n", d.Key, s[d.Key], d.Help)
		}
		return w.Flush()
	}
	if len(args) != 2 {
		return errors.New("usage: suspt config [KEY VALUE]")
	}
	return setSetting(db, args[0], args[1])
}

// setSetting validates and stores one setting, then replays the account.
func setSetting(db *sql.DB, key, val string) error {
	val = strings.TrimSpace(val)
	i := slices.IndexFunc(settingDefaults, func(d struct{ Key, Default, Help string }) bool { return d.Key == key })
	if i < 0 {
		return fmt.Errorf("unknown setting %q", key)
	}
	switch key {
	case "start_date":
		if _, err := time.Parse(time.DateOnly, val); err != nil {
			return fmt.Errorf("start_date must be YYYY-MM-DD")
		}
	case "compliance_sell":
		if val != "sell" && val != "alert" {
			return fmt.Errorf("compliance_sell must be sell or alert")
		}
	case "auto_refresh":
		if val != "on" && val != "off" {
			return fmt.Errorf("auto_refresh must be on or off")
		}
	case "benchmark", "allowed_etfs":
		val = strings.ToUpper(val)
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM prices WHERE ticker = ?`, val).Scan(&n); err != nil {
			return err
		}
		if key == "benchmark" && n == 0 {
			// Saving it would leave nothing to compare against, so refuse and save nothing.
			return fmt.Errorf("%s has no prices yet: add it to the watchlist, let the data refresh, then choose it here", val)
		}
	default:
		if v, err := strconv.ParseFloat(val, 64); err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%s must be a number >= 0", key)
		}
		if v, _ := strconv.ParseFloat(val, 64); key == "deposit_day" && (v < 1 || v > 31 || v != math.Trunc(v)) {
			return fmt.Errorf("deposit_day must be 1-31")
		}
	}
	if _, err := db.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`, key, val); err != nil {
		return err
	}
	return replay(db)
}

func nyToday() string {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	return time.Now().In(loc).Format(time.DateOnly)
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }
func floor6(x float64) float64 { return math.Floor(x*1e6+1e-9) / 1e6 }

func addDays(date string, n int) string {
	t, _ := time.Parse(time.DateOnly, date)
	return t.AddDate(0, 0, n).Format(time.DateOnly)
}

type order struct {
	ID                   int64
	PlacedOn, Ticker     string
	Side                 string
	Qty, Notional, Limit sql.NullFloat64
	GTC                  bool
	Reason, Thesis       string
	CancelledOn          sql.NullString
	// set by replay
	Status, FilledOn, Note            string
	FillPrice, FillQty, Fee, Realized float64
	auto                              bool
}

func (o order) size() string {
	switch {
	case o.Qty.Valid:
		return strconv.FormatFloat(o.Qty.Float64, 'f', -1, 64)
	case o.Notional.Valid:
		return fmt.Sprintf("$%.2f", o.Notional.Float64)
	}
	return "all"
}

func (o order) kind() string {
	k := "market"
	if o.Limit.Valid {
		k = fmt.Sprintf("limit %.2f", o.Limit.Float64)
	}
	if o.GTC {
		return k + " gtc"
	}
	return k + " day"
}

func loadOrders(db *sql.DB) ([]*order, error) {
	rs, err := db.Query(`SELECT id, placed_on, ticker, side, qty, notional, limit_price, gtc, reason, thesis, cancelled_on,
		status, COALESCE(filled_on, ''), COALESCE(fill_price, 0), COALESCE(fill_qty, 0), COALESCE(fee, 0), COALESCE(realized_pnl, 0), COALESCE(note, '')
		FROM orders ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []*order
	for rs.Next() {
		o := &order{}
		if err := rs.Scan(&o.ID, &o.PlacedOn, &o.Ticker, &o.Side, &o.Qty, &o.Notional, &o.Limit, &o.GTC, &o.Reason, &o.Thesis, &o.CancelledOn,
			&o.Status, &o.FilledOn, &o.FillPrice, &o.FillQty, &o.Fee, &o.Realized, &o.Note); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rs.Err()
}

type ohlc struct {
	Date                   string
	Open, High, Low, Close float64
	Div, Split             float64
}

func loadOHLC(db *sql.DB, ticker string) ([]ohlc, error) {
	rs, err := db.Query(`SELECT date, open, high, low, close, div_cash, split_factor FROM prices WHERE ticker = ? ORDER BY date`, ticker)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []ohlc
	for rs.Next() {
		var b ohlc
		if err := rs.Scan(&b.Date, &b.Open, &b.High, &b.Low, &b.Close, &b.Div, &b.Split); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rs.Err()
}

type screenPoint struct {
	AsOf, Status string
	Purif        float64 // NaN when unknown
}

func loadScreenPoints(db *sql.DB, ticker string) ([]screenPoint, error) {
	rs, err := db.Query(`SELECT as_of, status, purification_ratio FROM screens WHERE ticker = ? AND provisional = 0 ORDER BY as_of`, ticker)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []screenPoint
	for rs.Next() {
		var p screenPoint
		var purif sql.NullFloat64
		if err := rs.Scan(&p.AsOf, &p.Status, &purif); err != nil {
			return nil, err
		}
		p.Purif = math.NaN()
		if purif.Valid {
			p.Purif = purif.Float64
		}
		out = append(out, p)
	}
	return out, rs.Err()
}

// statusAt returns the screen in force on date: the latest one evaluated strictly before
// it, because a screen uses that day's close and so cannot gate that day's open.
func statusAt(pts []screenPoint, date string) (screenPoint, bool) {
	i := sort.Search(len(pts), func(i int) bool { return pts[i].AsOf >= date })
	if i == 0 {
		return screenPoint{Purif: math.NaN()}, false
	}
	return pts[i-1], true
}

type lot struct {
	ID                 int
	OrderID            int64
	Ticker, Thesis     string
	OpenedOn, ClosedOn string
	Qty, Cost          float64 // at purchase (Qty split-adjusted), Cost includes the fee
	OpenQty, OpenCost  float64
	Realized, Income   float64 // income: dividends net of withholding and purification
}

type entry struct {
	Date, Kind, Ticker      string
	Qty, Price, Amount, Fee float64
	OrderID                 int64 // 0 = none (deposits, automatic orders)
	Realized                float64
	Note                    string
}

type snapshot struct {
	Date                                  string
	Cash, Holdings, Value, Contrib, Bench float64
}

// sim replays the paper account day by day. ponytail: money is float64 rounded to
// cents on every cash movement; switch to integer cents if drift ever shows up.
type sim struct {
	set     settings
	prices  map[string]map[string]ohlc
	last    map[string]float64
	screens map[string][]screenPoint
	etfs    []string
	cash    float64
	contrib float64
	lots    []*lot
	ledger  []entry
	snaps   []snapshot
	orders  []*order // user orders plus automatic compliance sells
	bench   struct {
		ticker    string
		cash, qty float64
	}
}

func (s *sim) fee(notional float64) float64 {
	return max(s.set.num("fee_min"), s.set.num("fee_flat")+notional*s.set.num("fee_pct")/100)
}

func (s *sim) held(ticker string) float64 {
	q := 0.0
	for _, l := range s.lots {
		if l.Ticker == ticker {
			q += l.OpenQty
		}
	}
	return q
}

func (s *sim) log(e entry) { s.ledger = append(s.ledger, e) }

// depositDates lists the deposit schedule up to `until`: the start date, then
// deposit_day of every later month (clamped to the month's length).
func depositDates(set settings, until string) map[string]float64 {
	out := map[string]float64{}
	start, err := time.Parse(time.DateOnly, set["start_date"])
	if err != nil {
		return out
	}
	out[set["start_date"]] = set.num("initial_deposit")
	day := int(set.num("deposit_day"))
	for m := time.Date(start.Year(), start.Month()+1, 1, 0, 0, 0, 0, time.UTC); m.Format(time.DateOnly) <= until; m = m.AddDate(0, 1, 0) {
		last := m.AddDate(0, 1, -1).Day()
		d := time.Date(m.Year(), m.Month(), min(day, last), 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
		out[d] += set.num("monthly_deposit")
	}
	return out
}

// fillPrice returns the execution price on bar b: market orders take the open; limit
// orders take the open if it is already through the limit, else the limit if touched.
func fillPrice(o *order, b ohlc) (float64, bool) {
	if !o.Limit.Valid {
		return b.Open, true
	}
	l := o.Limit.Float64
	switch {
	case o.Side == "buy" && b.Open <= l, o.Side == "sell" && b.Open >= l:
		return b.Open, true
	case o.Side == "buy" && b.Low <= l, o.Side == "sell" && b.High >= l:
		return l, true
	}
	return 0, false
}

func (s *sim) reject(o *order, format string, args ...any) {
	o.Status, o.Note = "rejected", fmt.Sprintf(format, args...)
}

// execute tries order o on bar b (day t) and reports whether it is done (filled or rejected).
func (s *sim) execute(o *order, b ohlc, t string) bool {
	if o.Side == "sell" && s.held(o.Ticker) <= 1e-9 {
		s.reject(o, "no %s position", o.Ticker)
		return true
	}
	price, ok := fillPrice(o, b)
	if !ok {
		return false
	}
	if o.Side == "buy" && !slices.Contains(s.etfs, o.Ticker) {
		p, ok := statusAt(s.screens[o.Ticker], t)
		if !ok || p.Status != "COMPLIANT" {
			s.reject(o, "%s not COMPLIANT on %s (%s)", o.Ticker, t, orDefault(p.Status, "no screen"))
			return true
		}
	}

	if o.Side == "buy" {
		var q, fee float64
		if o.Qty.Valid {
			q = o.Qty.Float64
			fee = s.fee(q * price)
		} else {
			fee = s.fee(o.Notional.Float64)
			q = floor6((o.Notional.Float64 - fee) / price)
		}
		if q <= 0 {
			s.reject(o, "order too small after fees")
			return true
		}
		cost := round2(q*price + fee)
		if cost > s.cash+1e-9 {
			s.reject(o, "insufficient cash on %s: need %.2f, have %.2f", t, cost, s.cash)
			return true
		}
		s.cash = round2(s.cash - cost)
		s.lots = append(s.lots, &lot{ID: len(s.lots) + 1, OrderID: o.ID, Ticker: o.Ticker, Thesis: o.Thesis, OpenedOn: t,
			Qty: q, Cost: cost, OpenQty: q, OpenCost: cost})
		s.log(entry{Date: t, Kind: "buy", Ticker: o.Ticker, Qty: q, Price: price, Amount: -cost, Fee: fee, OrderID: o.ID, Note: o.Reason})
		o.Status, o.FilledOn, o.FillPrice, o.FillQty, o.Fee = "filled", t, price, q, fee
		return true
	}

	held := s.held(o.Ticker)
	q := held
	switch {
	case o.Qty.Valid:
		q = o.Qty.Float64
		if q > held+1e-9 {
			s.reject(o, "only %g %s shares held", held, o.Ticker)
			return true
		}
	case o.Notional.Valid:
		q = min(held, floor6(o.Notional.Float64/price))
		if q <= 0 {
			s.reject(o, "order smaller than one millionth of a share")
			return true
		}
	}
	fee := s.fee(q * price)
	net := round2(q*price - fee)
	s.cash = round2(s.cash + net)
	realized := s.consume(o.Ticker, q, net, t)
	s.log(entry{Date: t, Kind: "sell", Ticker: o.Ticker, Qty: q, Price: price, Amount: net, Fee: fee, OrderID: o.ID, Realized: realized, Note: o.Reason})
	o.Status, o.FilledOn, o.FillPrice, o.FillQty, o.Fee, o.Realized = "filled", t, price, q, fee, realized
	return true
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// consume sells q shares FIFO across lots for net proceeds and returns the realized P&L.
func (s *sim) consume(ticker string, q, net float64, t string) float64 {
	total, remaining := 0.0, q
	for _, l := range s.lots {
		if l.Ticker != ticker || l.OpenQty <= 0 || remaining <= 1e-9 {
			continue
		}
		take := min(l.OpenQty, remaining)
		c := l.OpenCost * take / l.OpenQty
		pnl := net*take/q - c
		l.Realized += pnl
		l.OpenCost -= c
		l.OpenQty -= take
		if l.OpenQty <= 1e-9 {
			l.OpenQty, l.OpenCost, l.ClosedOn = 0, 0, t
		}
		remaining -= take
		total += pnl
	}
	return total
}

// corporateActions applies splits, then dividends to holdings as of the previous close.
func (s *sim) corporateActions(t string) {
	wh := s.set.num("dividend_withholding") / 100
	var tickers []string
	for _, l := range s.lots {
		if l.OpenQty > 0 {
			tickers = append(tickers, l.Ticker)
		}
	}
	for _, o := range s.orders {
		if o.Status == "open" {
			tickers = append(tickers, o.Ticker)
		}
	}
	slices.Sort(tickers)
	for _, tk := range slices.Compact(tickers) {
		b, ok := s.prices[tk][t]
		if !ok {
			continue
		}
		if b.Split != 1 && b.Split > 0 {
			for _, l := range s.lots {
				if l.Ticker == tk {
					l.Qty *= b.Split
					l.OpenQty *= b.Split
				}
			}
			// Open orders keep their post-split meaning: same stake, same price level.
			for _, o := range s.orders {
				if o.Ticker == tk && o.Status == "open" && o.PlacedOn < t {
					if o.Qty.Valid {
						o.Qty.Float64 *= b.Split
					}
					if o.Limit.Valid {
						o.Limit.Float64 /= b.Split
					}
				}
			}
			s.log(entry{Date: t, Kind: "split", Ticker: tk, Qty: b.Split, Note: fmt.Sprintf("%g:1 split", b.Split)})
		}
		if b.Div > 0 && s.held(tk) > 0 {
			held := s.held(tk)
			gross := round2(held * b.Div)
			p, _ := statusAt(s.screens[tk], t)
			purif, note := p.Purif, ""
			if math.IsNaN(purif) || slices.Contains(s.etfs, tk) {
				purif, note = 0, "purification ratio unknown, nothing deducted"
			}
			tax, pure := round2(gross*wh), round2(gross*purif)
			s.cash = round2(s.cash + gross - tax - pure)
			for _, l := range s.lots {
				if l.Ticker == tk && l.OpenQty > 0 {
					l.Income += (gross - tax - pure) * l.OpenQty / held
				}
			}
			s.log(entry{Date: t, Kind: "dividend", Ticker: tk, Qty: held, Price: b.Div, Amount: gross, Note: note})
			if tax > 0 {
				s.log(entry{Date: t, Kind: "withholding", Ticker: tk, Amount: -tax})
			}
			if pure > 0 {
				s.log(entry{Date: t, Kind: "purification", Ticker: tk, Amount: -pure, Note: fmt.Sprintf("%.2f%% of dividend", purif*100)})
			}
		}
	}
	if b, ok := s.prices[s.bench.ticker][t]; ok {
		if b.Split != 1 && b.Split > 0 {
			s.bench.qty *= b.Split
		}
		s.bench.cash = round2(s.bench.cash + s.bench.qty*b.Div*(1-wh))
	}
}

// complianceCheck reacts to a held stock turning NON_COMPLIANT between prev and t.
func (s *sim) complianceCheck(t, prev string) {
	if prev == "" {
		return
	}
	seen := map[string]bool{}
	for _, l := range s.lots {
		if l.OpenQty <= 0 || seen[l.Ticker] || slices.Contains(s.etfs, l.Ticker) {
			continue
		}
		seen[l.Ticker] = true
		cur, ok := statusAt(s.screens[l.Ticker], t)
		before, pok := statusAt(s.screens[l.Ticker], prev)
		if !ok || !pok || cur.Status != "NON_COMPLIANT" || before.Status == "NON_COMPLIANT" {
			continue
		}
		note := fmt.Sprintf("%s turned NON_COMPLIANT (screen as of %s)", l.Ticker, cur.AsOf)
		if s.set["compliance_sell"] != "sell" {
			s.log(entry{Date: t, Kind: "alert", Ticker: l.Ticker, Note: note + ": sell rule is alert-only"})
			continue
		}
		s.log(entry{Date: t, Kind: "alert", Ticker: l.Ticker, Note: note + ": automatic sell queued"})
		s.orders = append(s.orders, &order{PlacedOn: addDays(t, int(s.set.num("compliance_grace_days"))), Ticker: l.Ticker, Side: "sell",
			GTC: true, Reason: "compliance: " + note, Thesis: "compliance", Status: "open", auto: true})
	}
}

func (s *sim) benchInvest(t string) {
	b, ok := s.prices[s.bench.ticker][t]
	if !ok || s.bench.cash < 0.01 {
		return
	}
	fee := s.fee(s.bench.cash)
	q := floor6((s.bench.cash - fee) / b.Open)
	if q > 0 {
		s.bench.qty += q
		s.bench.cash = max(0, round2(s.bench.cash-q*b.Open-fee))
	}
}

func (s *sim) snapshot(t string) {
	for tk, bars := range s.prices {
		if b, ok := bars[t]; ok {
			s.last[tk] = b.Close
		}
	}
	h := 0.0
	for _, l := range s.lots {
		h += l.OpenQty * s.last[l.Ticker]
	}
	s.snaps = append(s.snaps, snapshot{Date: t, Cash: s.cash, Holdings: h, Value: s.cash + h, Contrib: s.contrib,
		Bench: s.bench.cash + s.bench.qty*s.last[s.bench.ticker]})
}

// simulate runs the account over the benchmark's trading calendar.
func simulate(set settings, orders []*order, prices map[string][]ohlc, screens map[string][]screenPoint) *sim {
	s := &sim{set: set, prices: map[string]map[string]ohlc{}, last: map[string]float64{}, screens: screens, etfs: set.etfs(), orders: orders}
	s.bench.ticker = set["benchmark"]
	for tk, bars := range prices {
		m := map[string]ohlc{}
		for _, b := range bars {
			m[b.Date] = b
		}
		s.prices[tk] = m
	}
	for _, o := range orders {
		o.Status, o.FilledOn, o.Note, o.FillPrice, o.FillQty, o.Fee, o.Realized = "open", "", "", 0, 0, 0, 0
	}
	cal := prices[s.bench.ticker]
	var until string
	if len(cal) > 0 {
		until = cal[len(cal)-1].Date
	}
	deposits := depositDates(set, until)
	depositOrder := make([]string, 0, len(deposits))
	for d := range deposits {
		depositOrder = append(depositOrder, d)
	}
	sort.Strings(depositOrder)
	prev := ""
	for _, day := range cal {
		t := day.Date
		if t < set["start_date"] {
			continue
		}
		s.corporateActions(t)
		for _, d := range depositOrder {
			if d > prev && d <= t {
				amt := deposits[d]
				s.cash, s.contrib, s.bench.cash = round2(s.cash+amt), s.contrib+amt, round2(s.bench.cash+amt)
				s.log(entry{Date: t, Kind: "deposit", Amount: amt, Note: "scheduled " + d})
			}
		}
		s.benchInvest(t)                               // deposits land before the open, so the benchmark buys at today's open
		for _, side := range []string{"sell", "buy"} { // sells first so their cash funds same-day buys
			for _, o := range s.orders {
				if o.Side != side || o.Status != "open" || o.PlacedOn >= t {
					continue
				}
				if o.CancelledOn.Valid && o.CancelledOn.String < t {
					o.Status = "cancelled"
					continue
				}
				b, ok := s.prices[o.Ticker][t]
				done := ok && s.execute(o, b, t)
				if !done && !o.GTC {
					o.Status, o.Note = "expired", "day order not filled on "+t
				}
			}
		}
		s.complianceCheck(t, prev)
		s.snapshot(t)
		prev = t
	}
	for _, o := range orders {
		if o.Status == "open" && o.CancelledOn.Valid {
			o.Status = "cancelled"
		}
	}
	return s
}

// replay rebuilds orders' outcomes, the ledger, lots and the equity curve from scratch.
func replay(db *sql.DB) error {
	set, err := loadSettings(db)
	if err != nil {
		return err
	}
	bench := set["benchmark"]
	cal, err := loadOHLC(db, bench)
	if err != nil {
		return err
	}
	if len(cal) == 0 {
		return fmt.Errorf("no prices for the benchmark %s yet: add it to the watchlist and refresh data", bench)
	}
	if set["start_date"] == "" {
		set["start_date"] = cal[len(cal)-1].Date
		if _, err := db.Exec(`INSERT INTO settings (key, value) VALUES ('start_date', ?)`, set["start_date"]); err != nil {
			return err
		}
	}
	orders, err := loadOrders(db)
	if err != nil {
		return err
	}
	prices := map[string][]ohlc{bench: cal}
	screens := map[string][]screenPoint{}
	for _, o := range orders {
		if _, ok := prices[o.Ticker]; ok {
			continue
		}
		if prices[o.Ticker], err = loadOHLC(db, o.Ticker); err != nil {
			return err
		}
		if screens[o.Ticker], err = loadScreenPoints(db, o.Ticker); err != nil {
			return err
		}
	}
	s := simulate(set, orders, prices, screens)

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, o := range orders {
		if _, err := tx.Exec(`UPDATE orders SET status = ?, filled_on = NULLIF(?, ''), fill_price = ?, fill_qty = ?, fee = ?, realized_pnl = ?, note = ? WHERE id = ?`,
			o.Status, o.FilledOn, o.FillPrice, o.FillQty, o.Fee, o.Realized, o.Note, o.ID); err != nil {
			return err
		}
	}
	for _, q := range []string{`DELETE FROM ledger`, `DELETE FROM lots`, `DELETE FROM equity`} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	for _, e := range s.ledger {
		if _, err := tx.Exec(`INSERT INTO ledger (date, kind, ticker, qty, price, amount, fee, order_id, realized_pnl, note) VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, NULLIF(?, 0), ?, ?)`,
			e.Date, e.Kind, e.Ticker, e.Qty, e.Price, e.Amount, e.Fee, e.OrderID, e.Realized, e.Note); err != nil {
			return err
		}
	}
	for _, l := range s.lots {
		if _, err := tx.Exec(`INSERT INTO lots (id, order_id, ticker, thesis, opened_on, closed_on, qty, cost, open_qty, open_cost, realized, income) VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?)`,
			l.ID, l.OrderID, l.Ticker, l.Thesis, l.OpenedOn, l.ClosedOn, l.Qty, l.Cost, l.OpenQty, l.OpenCost, l.Realized, l.Income); err != nil {
			return err
		}
	}
	for _, p := range s.snaps {
		if _, err := tx.Exec(`INSERT INTO equity (date, cash, holdings, value, contributions, bench_value) VALUES (?, ?, ?, ?, ?, ?)`,
			p.Date, p.Cash, p.Holdings, p.Value, p.Contrib, p.Bench); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// parseArgs lets flags appear before or after positional arguments.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// latestStatus is the newest screen; confirmedOnly skips the provisional current month,
// matching what replay uses to gate fills.
func latestStatus(db *sql.DB, ticker string, confirmedOnly bool) (string, string) {
	var status, note string
	err := db.QueryRow(`SELECT status, COALESCE(note, '') FROM screens WHERE ticker = ? AND provisional <= ? ORDER BY as_of DESC LIMIT 1`,
		ticker, !confirmedOnly).Scan(&status, &note)
	if err != nil {
		return "NO_SCREEN", "run `suspt screen " + ticker + "`"
	}
	return status, note
}

func runOrder(db *sql.DB, side string, args []string) error {
	fs := flag.NewFlagSet(side, flag.ContinueOnError)
	usd := fs.Float64("usd", 0, "order size in USD instead of shares")
	limit := fs.Float64("limit", 0, "limit price (default: market order at the next open)")
	gtc := fs.Bool("gtc", false, "good till cancelled (default: day order)")
	reason := fs.String("reason", "", "why you are making this trade (required)")
	thesis := fs.String("thesis", "", "thesis tag, e.g. ai-capex (required)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	usage := fmt.Errorf("usage: suspt %s TICKER (QTY|-usd AMOUNT%s) [-limit PRICE] [-gtc] -reason TEXT -thesis TAG", side, map[string]string{"sell": "|all"}[side])
	if len(pos) < 1 || len(pos) > 2 {
		return usage
	}
	o := order{Ticker: pos[0], Side: side, GTC: *gtc, Reason: *reason, Thesis: *thesis}
	if len(pos) == 2 && pos[1] != "all" {
		q, err := strconv.ParseFloat(pos[1], 64)
		if err != nil || q <= 0 {
			return fmt.Errorf("quantity must be a positive number, got %q", pos[1])
		}
		o.Qty = sql.NullFloat64{Float64: q, Valid: true}
	}
	if *usd > 0 {
		o.Notional = sql.NullFloat64{Float64: *usd, Valid: true}
	}
	if side == "sell" && len(pos) == 1 && !o.Notional.Valid {
		return usage
	}
	if *limit != 0 {
		o.Limit = sql.NullFloat64{Float64: *limit, Valid: true}
	}
	if err := placeOrder(db, &o); err != nil {
		return err
	}
	fmt.Printf("order #%d: %s %s %s, %s, fills on a trading day after %s\n", o.ID, side, o.Ticker, o.size(), o.kind(), o.PlacedOn)
	return nil
}

// placeOrder validates an order, records it for a later trading day and replays.
// The CLI and the web app both go through here.
func placeOrder(db *sql.DB, o *order) error {
	o.Ticker = strings.ToUpper(strings.TrimSpace(o.Ticker))
	o.Reason, o.Thesis = strings.TrimSpace(o.Reason), strings.ToLower(strings.TrimSpace(o.Thesis))
	switch {
	case o.Side != "buy" && o.Side != "sell":
		return errors.New("side must be buy or sell")
	case !tickerRe.MatchString(o.Ticker):
		return fmt.Errorf("%q is not a ticker", o.Ticker)
	case o.Reason == "" || o.Thesis == "":
		return errors.New("every trade needs a reason and a thesis tag")
	case o.Qty.Valid && o.Notional.Valid:
		return errors.New("give a number of shares or a dollar amount, not both")
	case o.Side == "buy" && !o.Qty.Valid && !o.Notional.Valid:
		return errors.New("a buy needs a number of shares or a dollar amount")
	case o.Qty.Valid && !(o.Qty.Float64 > 0), o.Notional.Valid && !(o.Notional.Float64 > 0):
		return errors.New("the size must be a positive number")
	case o.Limit.Valid && !(o.Limit.Float64 > 0):
		return errors.New("the limit price must be a positive number")
	}
	set, err := loadSettings(db)
	if err != nil {
		return err
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM prices WHERE ticker = ?`, o.Ticker).Scan(&n); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("no prices for %s yet: add it to the watchlist and refresh data first", o.Ticker)
	}
	side := o.Side
	if side == "buy" && !slices.Contains(set.etfs(), o.Ticker) {
		if status, note := latestStatus(db, o.Ticker, true); status != "COMPLIANT" {
			if cur, _ := latestStatus(db, o.Ticker, false); cur == "COMPLIANT" {
				note += " (the current month shows COMPLIANT; it takes effect when the month's screen is final)"
			}
			return fmt.Errorf("%s is %s as of the last monthly screen, buy refused %s", o.Ticker, status, note)
		}
	}
	o.PlacedOn = nyToday()
	res, err := db.Exec(`INSERT INTO orders (placed_on, ticker, side, qty, notional, limit_price, gtc, reason, thesis) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.PlacedOn, o.Ticker, o.Side, o.Qty, o.Notional, o.Limit, o.GTC, o.Reason, o.Thesis)
	if err != nil {
		return err
	}
	o.ID, _ = res.LastInsertId()
	return replay(db)
}

func runCancel(db *sql.DB, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: suspt cancel ORDER_ID")
	}
	if err := cancelOrder(db, args[0]); err != nil {
		return err
	}
	fmt.Printf("order #%s cancelled from %s (fills on or before that day stand)\n", args[0], nyToday())
	return nil
}

func cancelOrder(db *sql.DB, id string) error {
	res, err := db.Exec(`UPDATE orders SET cancelled_on = ? WHERE id = ? AND cancelled_on IS NULL AND status = 'open'`, nyToday(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("order %s is not open", id)
	}
	return replay(db)
}

func runOrders(db *sql.DB) error {
	orders, err := loadOrders(db)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tPLACED\tSIDE\tTICKER\tSIZE\tTYPE\tSTATUS\tFILL\tTHESIS\tREASON / NOTE")
	for _, o := range orders {
		fill := ""
		if o.Status == "filled" {
			fill = fmt.Sprintf("%s %g @ %.2f", o.FilledOn, o.FillQty, o.FillPrice)
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", o.ID, o.PlacedOn, o.Side, o.Ticker, o.size(), o.kind(), o.Status, fill, o.Thesis, strings.TrimSpace(o.Reason+"  "+o.Note))
	}
	return w.Flush()
}
