package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
)

// series holds split-adjusted daily prices, so indicators and charts don't jump at splits.
type series struct {
	Dates                  []string
	Open, High, Low, Close []float64
}

func adjustedSeries(bars []ohlc) series {
	n := len(bars)
	s := series{make([]string, n), make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)}
	f := 1.0
	for i := n - 1; i >= 0; i-- {
		b := bars[i]
		s.Dates[i], s.Open[i], s.High[i], s.Low[i], s.Close[i] = b.Date, b.Open/f, b.High/f, b.Low/f, b.Close/f
		if b.Split > 0 && b.Split != 1 { // bars before a split are divided by its factor
			f *= b.Split
		}
	}
	return s
}

// Rule conditions are explicit clauses joined by "and":
//
//	close crosses_above sma200 and chg > 1
//	low <= level:support
//
// Terms: open, high, low, close, chg (daily % change), smaN (N-day simple moving
// average of close), level:NAME (a key level from `suspt level`), or a number.
type operand struct {
	kind string
	n    int
	v    float64
	name string
}

type clause struct {
	L, R operand
	Op   string
}

var ruleOps = []string{"<", "<=", ">", ">=", "crosses_above", "crosses_below"}

func parseOperand(tok string) (operand, error) {
	switch {
	case slices.Contains([]string{"open", "high", "low", "close", "chg"}, tok):
		return operand{kind: tok}, nil
	case strings.HasPrefix(tok, "sma"):
		n, err := strconv.Atoi(tok[3:])
		if err != nil || n < 2 || n > 400 {
			return operand{}, fmt.Errorf("%q isn't a moving average rules support; use sma2 up to sma400", tok)
		}
		return operand{kind: "sma", n: n}, nil
	case strings.HasPrefix(tok, "level:") && len(tok) > 6:
		return operand{kind: "level", name: tok[6:]}, nil
	}
	v, err := strconv.ParseFloat(tok, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return operand{}, fmt.Errorf("rules don't know %q; use open, high, low, close, chg, smaN, level:NAME or a number", tok)
	}
	return operand{kind: "num", v: v}, nil
}

func parseCondition(cond string) ([]clause, error) {
	toks := strings.Fields(cond)
	var out []clause
	for len(toks) > 0 {
		if len(toks) < 3 {
			return nil, fmt.Errorf("%q is incomplete: a condition compares two things, like close > sma200", strings.Join(toks, " "))
		}
		l, err := parseOperand(toks[0])
		if err != nil {
			return nil, err
		}
		if !slices.Contains(ruleOps, toks[1]) {
			return nil, fmt.Errorf("%q isn't a comparison rules know; use %s", toks[1], strings.Join(ruleOps, ", "))
		}
		r, err := parseOperand(toks[2])
		if err != nil {
			return nil, err
		}
		out = append(out, clause{L: l, Op: toks[1], R: r})
		toks = toks[3:]
		if len(toks) > 0 {
			if toks[0] != "and" || len(toks) == 1 {
				return nil, fmt.Errorf("clauses must be joined by \"and\"")
			}
			toks = toks[1:]
		}
	}
	if len(out) == 0 {
		return nil, errors.New("write a condition, like close > sma200")
	}
	return out, nil
}

func (o operand) at(s series, i int, levels map[string]float64) (float64, bool) {
	if i < 0 || i >= len(s.Close) {
		return 0, false
	}
	switch o.kind {
	case "num":
		return o.v, true
	case "open":
		return s.Open[i], true
	case "high":
		return s.High[i], true
	case "low":
		return s.Low[i], true
	case "close":
		return s.Close[i], true
	case "chg":
		if i < 1 || s.Close[i-1] == 0 {
			return 0, false
		}
		return (s.Close[i]/s.Close[i-1] - 1) * 100, true
	case "sma":
		if i+1 < o.n {
			return 0, false
		}
		sum := 0.0
		for _, c := range s.Close[i+1-o.n : i+1] {
			sum += c
		}
		return sum / float64(o.n), true
	case "level":
		v, ok := levels[o.name]
		return v, ok
	}
	return 0, false
}

func (c clause) holds(s series, i int, levels map[string]float64) bool {
	a, ok1 := c.L.at(s, i, levels)
	b, ok2 := c.R.at(s, i, levels)
	if !ok1 || !ok2 {
		return false
	}
	switch c.Op {
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	pa, ok3 := c.L.at(s, i-1, levels)
	pb, ok4 := c.R.at(s, i-1, levels)
	if !ok3 || !ok4 {
		return false
	}
	if c.Op == "crosses_above" {
		return pa <= pb && a > b
	}
	return pa >= pb && a < b
}

func matches(cs []clause, s series, i int, levels map[string]float64) bool {
	for _, c := range cs {
		if !c.holds(s, i, levels) {
			return false
		}
	}
	return true
}

type rule struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Version   int    `json:"version"`
	Ticker    string `json:"ticker"`
	Action    string `json:"action"`
	Thesis    string `json:"thesis"`
	Condition string `json:"condition"`
	CreatedOn string `json:"createdOn"`
	RetiredOn string `json:"retiredOn,omitempty"`
	clauses   []clause
}

func loadRules(db *sql.DB, all bool) ([]rule, error) {
	q := `SELECT id, name, version, ticker, action, thesis, condition, created_on, COALESCE(retired_on, '') FROM rules`
	if !all {
		q += ` WHERE retired_on IS NULL`
	}
	rs, err := db.Query(q + ` ORDER BY name, version`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []rule
	for rs.Next() {
		var r rule
		if err := rs.Scan(&r.ID, &r.Name, &r.Version, &r.Ticker, &r.Action, &r.Thesis, &r.Condition, &r.CreatedOn, &r.RetiredOn); err != nil {
			return nil, err
		}
		if r.clauses, err = parseCondition(r.Condition); err != nil {
			return nil, fmt.Errorf("rule %s v%d: %w", r.Name, r.Version, err)
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

// runRule adds a rule or a new version of it (the old version is retired, never edited).
func runRule(db *sql.DB, args []string) error {
	if len(args) == 2 && args[0] == "rm" {
		return retireRule(db, args[1])
	}
	if len(args) != 6 || args[0] != "add" {
		return errors.New(`usage: suspt rule add NAME TICKER|* BUY|WATCH THESIS "CONDITION"
       suspt rule rm NAME
example: suspt rule add golden-cross '*' WATCH trend "sma50 crosses_above sma200"`)
	}
	r := rule{Name: args[1], Ticker: args[2], Action: args[3], Thesis: args[4], Condition: args[5]}
	if err := addRule(db, &r); err != nil {
		return err
	}
	fmt.Printf("rule %s v%d: %s %s when %s\n", r.Name, r.Version, r.Action, r.Ticker, r.Condition)
	return nil
}

var ruleNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// addRule saves a rule, or a new version of it (the old version is retired, never edited).
func addRule(db *sql.DB, r *rule) error {
	r.Name, r.Ticker, r.Action = strings.ToLower(strings.TrimSpace(r.Name)), strings.ToUpper(strings.TrimSpace(r.Ticker)), strings.ToUpper(r.Action)
	r.Thesis, r.Condition = strings.ToLower(strings.TrimSpace(r.Thesis)), strings.Join(strings.Fields(r.Condition), " ")
	switch {
	case !ruleNameRe.MatchString(r.Name):
		return errors.New("rule names use lowercase letters, digits and dashes, like golden-cross")
	case r.Ticker != "*" && !tickerRe.MatchString(r.Ticker):
		return fmt.Errorf("%q is not a ticker (use * for the whole watchlist)", r.Ticker)
	case r.Action != "BUY" && r.Action != "WATCH":
		return errors.New("action must be BUY or WATCH")
	case r.Thesis == "":
		return errors.New("a rule needs a thesis tag")
	}
	if _, err := parseCondition(r.Condition); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version), 0) + 1 FROM rules WHERE name = ?`, r.Name).Scan(&r.Version); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE rules SET retired_on = ? WHERE name = ? AND retired_on IS NULL`, today(db), r.Name); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO rules (name, version, ticker, action, thesis, condition, created_on) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.Name, r.Version, r.Ticker, r.Action, r.Thesis, r.Condition, today(db)); err != nil {
		return err
	}
	return tx.Commit()
}

func retireRule(db *sql.DB, name string) error {
	res, err := db.Exec(`UPDATE rules SET retired_on = ? WHERE name = ? AND retired_on IS NULL`, today(db), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("there's no active rule called %q", name)
	}
	return nil
}

func runRules(db *sql.DB, args []string) error {
	rules, err := loadRules(db, len(args) > 0 && args[0] == "-all")
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tVER\tTICKER\tACTION\tTHESIS\tCREATED\tRETIRED\tCONDITION")
	for _, r := range rules {
		fmt.Fprintf(w, "%s\tv%d\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.Version, r.Ticker, r.Action, r.Thesis, r.CreatedOn, r.RetiredOn, r.Condition)
	}
	return w.Flush()
}

func loadLevels(db *sql.DB, ticker string) (map[string]float64, error) {
	rs, err := db.Query(`SELECT name, price FROM levels WHERE ticker = ?`, ticker)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[string]float64{}
	for rs.Next() {
		var n string
		var p float64
		if err := rs.Scan(&n, &p); err != nil {
			return nil, err
		}
		out[n] = p
	}
	return out, rs.Err()
}

func watchlistTickers(db *sql.DB) ([]string, error) {
	rs, err := db.Query(`SELECT ticker FROM watchlist ORDER BY ticker`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var t string
		if err := rs.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rs.Err()
}

func runWatch(db *sql.DB, args []string) error {
	if len(args) == 0 {
		return runWatchlist(db)
	}
	n, err := addWatch(db, args)
	if err != nil {
		return err
	}
	fmt.Printf("%d added; run `suspt update` to fetch data\n", n)
	return nil
}

// addWatch adds tickers to the watchlist and returns how many were new. It checks
// every ticker first, so a typo adds nothing rather than half the list.
func addWatch(db *sql.DB, tickers []string) (int, error) {
	for i, t := range tickers {
		tickers[i] = strings.ToUpper(strings.TrimSpace(t))
		if !tickerRe.MatchString(tickers[i]) {
			return 0, fmt.Errorf("%q is not a ticker; nothing was added", tickers[i])
		}
	}
	added := 0
	for _, t := range tickers {
		res, err := db.Exec(`INSERT OR IGNORE INTO watchlist (ticker, added_on) VALUES (?, ?)`, t, today(db))
		if err != nil {
			return added, err
		}
		n, _ := res.RowsAffected()
		added += int(n)
	}
	return added, nil
}

// setStar puts tickers on or off the shortlist (adding them to the watchlist if needed).
func setStar(db *sql.DB, tickers []string, on bool) error {
	if on {
		if _, err := addWatch(db, tickers); err != nil {
			return err
		}
	}
	for _, t := range tickers {
		if _, err := db.Exec(`UPDATE watchlist SET starred = ? WHERE ticker = ?`, on, strings.ToUpper(strings.TrimSpace(t))); err != nil {
			return err
		}
	}
	return nil
}

// shortlistTop stars the n largest SPUS holdings by weight, a familiar set to learn with.
func shortlistTop(db *sql.DB, n int) ([]string, error) {
	rs, err := db.Query(`SELECT ticker FROM index_members WHERE fund = 'SPUS' ORDER BY weight DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	var top []string
	for rs.Next() {
		var t string
		rs.Scan(&t)
		top = append(top, t)
	}
	rs.Close()
	if len(top) == 0 {
		return nil, errors.New("the SPUS holdings list isn't loaded yet: add all SPUS stocks first, or star stocks yourself")
	}
	return top, setStar(db, top, true)
}

func runStar(db *sql.DB, args []string, on bool) error {
	if len(args) == 0 {
		return errors.New("usage: suspt star|unstar TICKER...   (suspt star top10 stars the 10 largest SPUS holdings)")
	}
	if on && len(args) == 1 && args[0] == "top10" {
		top, err := shortlistTop(db, 10)
		if err == nil {
			fmt.Println("shortlist:", strings.Join(top, " "))
		}
		return err
	}
	return setStar(db, args, on)
}

func runUnwatch(db *sql.DB, args []string) error {
	for _, t := range args {
		if _, err := db.Exec(`DELETE FROM watchlist WHERE ticker = ?`, strings.ToUpper(t)); err != nil {
			return err
		}
	}
	return nil
}

func runLevel(db *sql.DB, args []string) error {
	if len(args) != 3 {
		return errors.New("usage: suspt level TICKER NAME PRICE|rm   (e.g. suspt level NVDA support 120)")
	}
	t := strings.ToUpper(args[0])
	if args[2] == "rm" {
		_, err := db.Exec(`DELETE FROM levels WHERE ticker = ? AND name = ?`, t, args[1])
		return err
	}
	p, err := strconv.ParseFloat(args[2], 64)
	if err != nil {
		return fmt.Errorf("price must be a positive number")
	}
	return setLevel(db, t, args[1], p)
}

var levelNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,30}$`)

// setLevel stores a key level; its name is usable in rules as level:NAME.
func setLevel(db *sql.DB, ticker, name string, price float64) error {
	switch {
	case !tickerRe.MatchString(ticker):
		return fmt.Errorf("%q is not a ticker", ticker)
	case !levelNameRe.MatchString(name):
		return errors.New("level names use letters, digits, - or _, like support")
	case !(price > 0) || math.IsInf(price, 0):
		return errors.New("price must be a positive number")
	}
	_, err := db.Exec(`INSERT OR REPLACE INTO levels (ticker, name, price) VALUES (?, ?, ?)`, ticker, name, price)
	return err
}

type levelDist struct {
	Name  string  `json:"name"`
	Price float64 `json:"price"`
	Dist  float64 `json:"dist"` // (price - close) / close
}

type watchRow struct {
	Ticker  string      `json:"ticker"`
	Date    string      `json:"date"`
	Close   float64     `json:"close"`
	Change  float64     `json:"change"`
	Status  string      `json:"status"`
	Sector  string      `json:"sector"`
	Held    bool        `json:"held"`
	Starred bool        `json:"starred"`
	Levels  []levelDist `json:"levels"`
	Signals []signal    `json:"signals"`
}

type signal struct {
	Ticker    string `json:"ticker"`
	Date      string `json:"date"`
	Rule      string `json:"rule"`
	Version   int    `json:"version"`
	Action    string `json:"action"`
	Thesis    string `json:"thesis"`
	Condition string `json:"condition"`
	Status    string `json:"status"`
	Blocked   bool   `json:"blocked"`
}

func tickerStatus(db *sql.DB, set settings, t string) string {
	if slices.Contains(set.etfs(), t) {
		return "FUND"
	}
	s, _ := latestStatus(db, t, false)
	return s
}

// currentSignals evaluates every active rule on each ticker's latest bar.
func currentSignals(db *sql.DB) ([]signal, error) {
	set, err := loadSettings(db)
	if err != nil {
		return nil, err
	}
	rules, err := loadRules(db, false)
	if err != nil {
		return nil, err
	}
	watch, err := watchlistTickers(db)
	if err != nil {
		return nil, err
	}
	out := []signal{}
	cache := map[string]series{}
	for _, r := range rules {
		tickers := []string{r.Ticker}
		if r.Ticker == "*" {
			tickers = watch
		}
		for _, t := range tickers {
			s, ok := cache[t]
			if !ok {
				bars, err := loadOHLC(db, t)
				if err != nil {
					return nil, err
				}
				s = adjustedSeries(bars)
				cache[t] = s
			}
			lv, err := loadLevels(db, t)
			if err != nil {
				return nil, err
			}
			i := len(s.Close) - 1
			if i < 0 || !matches(r.clauses, s, i, lv) {
				continue
			}
			st := tickerStatus(db, set, t)
			out = append(out, signal{Ticker: t, Date: s.Dates[i], Rule: r.Name, Version: r.Version, Action: r.Action, Thesis: r.Thesis,
				Condition: r.Condition, Status: st, Blocked: r.Action == "BUY" && st != "COMPLIANT" && st != "FUND"})
		}
	}
	return out, nil
}

func runSignals(db *sql.DB) error {
	sigs, err := currentSignals(db)
	if err != nil {
		return err
	}
	if len(sigs) == 0 {
		fmt.Println("no rule fired on the latest bar")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TICKER\tDATE\tACTION\tRULE\tTHESIS\tSTATUS\tCONDITION")
	for _, s := range sigs {
		action := s.Action
		if s.Blocked {
			action = "BLOCKED"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s v%d\t%s\t%s\t%s\n", s.Ticker, s.Date, action, s.Rule, s.Version, s.Thesis, s.Status, s.Condition)
	}
	return w.Flush()
}

func watchlistRows(db *sql.DB) ([]watchRow, error) {
	set, err := loadSettings(db)
	if err != nil {
		return nil, err
	}
	watch, err := watchlistTickers(db)
	if err != nil {
		return nil, err
	}
	sigs, err := currentSignals(db)
	if err != nil {
		return nil, err
	}
	out := []watchRow{}
	for _, t := range watch {
		bars, err := loadOHLC(db, t)
		if err != nil {
			return nil, err
		}
		r := watchRow{Ticker: t, Status: tickerStatus(db, set, t), Sector: sectorOf(db, set, t), Levels: []levelDist{}, Signals: []signal{}}
		db.QueryRow(`SELECT starred FROM watchlist WHERE ticker = ?`, t).Scan(&r.Starred)
		if s := adjustedSeries(bars); len(s.Close) > 0 {
			i := len(s.Close) - 1
			r.Date, r.Close = s.Dates[i], s.Close[i]
			if i > 0 {
				r.Change = s.Close[i]/s.Close[i-1] - 1
			}
		}
		lv, err := loadLevels(db, t)
		if err != nil {
			return nil, err
		}
		for n, p := range lv {
			d := 0.0 // JSON has no NaN
			if r.Close > 0 {
				d = (p - r.Close) / r.Close
			}
			r.Levels = append(r.Levels, levelDist{Name: n, Price: p, Dist: d})
		}
		slices.SortFunc(r.Levels, func(a, b levelDist) int { return int(math.Copysign(1, a.Price-b.Price)) })
		var held float64
		db.QueryRow(`SELECT COALESCE(SUM(open_qty), 0) FROM lots WHERE ticker = ?`, t).Scan(&held)
		r.Held = held > 0
		for _, s := range sigs {
			if s.Ticker == t {
				r.Signals = append(r.Signals, s)
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func runWatchlist(db *sql.DB) error {
	rows, err := watchlistRows(db)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("watchlist is empty: suspt watch TICKER...")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TICKER\tDATE\tCLOSE\tCHG\tSTATUS\tSECTOR\tLEVELS\tSIGNALS")
	for _, r := range rows {
		var lv, sg []string
		for _, l := range r.Levels {
			lv = append(lv, fmt.Sprintf("%s %.2f (%+.1f%%)", l.Name, l.Price, l.Dist*100))
		}
		for _, s := range r.Signals {
			a := s.Action
			if s.Blocked {
				a = "BLOCKED"
			}
			sg = append(sg, a+" "+s.Rule)
		}
		fmt.Fprintf(w, "%s\t%s\t%.2f\t%+.2f%%\t%s\t%s\t%s\t%s\n", r.Ticker, r.Date, r.Close, r.Change*100, r.Status, r.Sector, strings.Join(lv, ", "), strings.Join(sg, ", "))
	}
	return w.Flush()
}

// runUpdate is the daily routine: fetch, screen, replay, report.
func runUpdate(db *sql.DB) error {
	if err := refresh(db, nil); err != nil {
		return err
	}
	if err := screenReport(db, nil, true); err != nil {
		return err
	}
	fmt.Println()
	if err := runPortfolio(db); err != nil {
		return err
	}
	fmt.Println()
	return runSignals(db)
}

// refresh fetches watchlist, holdings and benchmark data, refreshes SPUS membership,
// screens every company and replays the account. The CLI and the app's refresh share it.
func refresh(db *sql.DB, step func(i, n int, ticker string)) error {
	set, err := loadSettings(db)
	if err != nil {
		return err
	}
	tickers, err := watchlistTickers(db)
	if err != nil {
		return err
	}
	tickers = append(tickers, set["benchmark"])
	orders, err := loadOrders(db)
	if err != nil {
		return err
	}
	for _, o := range orders {
		tickers = append(tickers, o.Ticker)
	}
	slices.Sort(tickers)
	tickers = slices.Compact(tickers)
	// Shortlisted and held stocks first, so they are current within minutes even when the
	// free tier's hourly limit stretches a big watchlist over hours.
	first := map[string]bool{}
	if rs, err := db.Query(`SELECT ticker FROM watchlist WHERE starred = 1 UNION SELECT ticker FROM lots WHERE open_qty > 0`); err == nil {
		for rs.Next() {
			var t string
			rs.Scan(&t)
			first[t] = true
		}
		rs.Close()
	}
	slices.SortStableFunc(tickers, func(a, b string) int {
		switch {
		case first[a] && !first[b]:
			return -1
		case first[b] && !first[a]:
			return 1
		}
		return 0
	})
	if len(tickers) > 45 {
		log.Printf("warning: %d tickers; Tiingo's free tier allows 50 requests per hour", len(tickers))
	}
	if err := ingestAll(db, tickers, step); errors.Is(err, errNoKeys) {
		return err
	} else if err != nil {
		log.Print(err) // keep going with whatever data we have
	}
	if _, err := syncSPUS(db); err != nil {
		log.Printf("SPUS membership not refreshed: %v", err) // keep the previous list
	}
	rs, err := db.Query(`SELECT ticker FROM companies`)
	if err != nil {
		return err
	}
	var companies []string
	for rs.Next() {
		var t string
		rs.Scan(&t)
		companies = append(companies, t)
	}
	rs.Close()
	for _, t := range companies {
		if _, err := screenTicker(db, t); err != nil {
			log.Printf("%s screen: %v", t, err)
		}
	}
	return replay(db)
}
