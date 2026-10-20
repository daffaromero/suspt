package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

// sectorFromSIC is a coarse SIC-to-sector map. ponytail: approximate on purpose;
// override per ticker with `suspt sector`.
func sectorFromSIC(sic string) string {
	n, err := strconv.Atoi(sic)
	if err != nil {
		return "Unknown"
	}
	switch {
	case n == 3674:
		return "Semiconductors"
	case n >= 3570 && n <= 3579, n >= 3660 && n <= 3679, n >= 7370 && n <= 7379:
		return "Technology"
	case n >= 2830 && n <= 2836, n >= 3841 && n <= 3851, n >= 8000 && n <= 8099:
		return "Health Care"
	case n >= 4800 && n <= 4899:
		return "Communication"
	case n >= 4900 && n <= 4999:
		return "Utilities"
	case n >= 1300 && n <= 1399, n >= 2900 && n <= 2999:
		return "Energy"
	case n >= 1000 && n <= 1499, n >= 2600 && n <= 2699, n >= 2800 && n <= 2829, n >= 3300 && n <= 3399:
		return "Materials"
	case n >= 2000 && n <= 2199, n >= 5400 && n <= 5499:
		return "Consumer Staples"
	case n >= 5000 && n <= 5999, n >= 3711 && n <= 3716, n >= 7000 && n <= 7999, n >= 2300 && n <= 2399:
		return "Consumer Discretionary"
	case n >= 6000 && n <= 6799:
		return "Financials & Real Estate"
	}
	return "Industrials"
}

func sectorOf(db *sql.DB, set settings, ticker string) string {
	var s string
	if db.QueryRow(`SELECT sector FROM sectors WHERE ticker = ?`, ticker).Scan(&s) == nil {
		return s
	}
	if slices.Contains(set.etfs(), ticker) {
		return "Fund"
	}
	var sic string
	if db.QueryRow(`SELECT COALESCE(sic, '') FROM companies WHERE ticker = ?`, ticker).Scan(&sic) != nil {
		return "Unknown"
	}
	return sectorFromSIC(sic)
}

func runSector(db *sql.DB, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: suspt sector TICKER NAME...  (NAME \"auto\" removes the override)")
	}
	t, name := strings.ToUpper(args[0]), strings.Join(args[1:], " ")
	if name == "auto" {
		_, err := db.Exec(`DELETE FROM sectors WHERE ticker = ?`, t)
		return err
	}
	_, err := db.Exec(`INSERT OR REPLACE INTO sectors (ticker, sector) VALUES (?, ?)`, t, name)
	return err
}

func lastClose(db *sql.DB, ticker string) float64 {
	var c float64
	db.QueryRow(`SELECT close FROM prices WHERE ticker = ? ORDER BY date DESC LIMIT 1`, ticker).Scan(&c)
	return c
}

// twr chains daily returns, treating each day's deposit as arriving before the open.
func twr(values, contribs []float64) float64 {
	g, prevV, prevC := 1.0, 0.0, 0.0
	for i := range values {
		if base := prevV + contribs[i] - prevC; base > 0 {
			g *= values[i] / base
		}
		prevV, prevC = values[i], contribs[i]
	}
	return g - 1
}

type position struct {
	Ticker string  `json:"ticker"`
	Sector string  `json:"sector"`
	Status string  `json:"status"`
	Qty    float64 `json:"qty"`
	Cost   float64 `json:"cost"`
	Close  float64 `json:"close"`
	Value  float64 `json:"value"`
	PnL    float64 `json:"pnl"`
	Weight float64 `json:"weight"`
}

type exposure struct {
	Sector string  `json:"sector"`
	Value  float64 `json:"value"`
	Weight float64 `json:"weight"`
	Limit  float64 `json:"limit"`
}

type equityPoint struct {
	Date    string  `json:"date"`
	Value   float64 `json:"value"`
	Contrib float64 `json:"contrib"`
	Bench   float64 `json:"bench"`
}

type summary struct {
	Start         string        `json:"start"`
	AsOf          string        `json:"asOf"`
	Cash          float64       `json:"cash"`
	Holdings      float64       `json:"holdings"`
	Value         float64       `json:"value"`
	Contributions float64       `json:"contributions"`
	PnL           float64       `json:"pnl"`
	TWR           float64       `json:"twr"`
	Benchmark     string        `json:"benchmark"`
	BenchValue    float64       `json:"benchValue"`
	BenchTWR      float64       `json:"benchTwr"`
	Positions     []position    `json:"positions"`
	Sectors       []exposure    `json:"sectors"`
	Flags         []string      `json:"flags"`
	Equity        []equityPoint `json:"equity"`
}

func portfolioSummary(db *sql.DB) (summary, error) {
	set, err := loadSettings(db)
	if err != nil {
		return summary{}, err
	}
	s := summary{Start: set["start_date"], Benchmark: set["benchmark"], Positions: []position{}, Sectors: []exposure{}, Flags: []string{}, Equity: []equityPoint{}}
	rs, err := db.Query(`SELECT date, cash, holdings, value, contributions, bench_value FROM equity ORDER BY date`)
	if err != nil {
		return s, err
	}
	var vals, contribs, bench []float64
	for rs.Next() {
		var p equityPoint
		if err := rs.Scan(&p.Date, &s.Cash, &s.Holdings, &p.Value, &p.Contrib, &p.Bench); err != nil {
			rs.Close()
			return s, err
		}
		s.Equity = append(s.Equity, p)
		vals, contribs, bench = append(vals, p.Value), append(contribs, p.Contrib), append(bench, p.Bench)
	}
	rs.Close()
	if n := len(s.Equity); n > 0 {
		last := s.Equity[n-1]
		s.AsOf, s.Value, s.Contributions, s.BenchValue = last.Date, last.Value, last.Contrib, last.Bench
		s.PnL, s.TWR, s.BenchTWR = last.Value-last.Contrib, twr(vals, contribs), twr(bench, contribs)
	}

	rs, err = db.Query(`SELECT ticker, SUM(open_qty), SUM(open_cost) FROM lots WHERE open_qty > 0 GROUP BY ticker ORDER BY ticker`)
	if err != nil {
		return s, err
	}
	for rs.Next() {
		var p position
		if err := rs.Scan(&p.Ticker, &p.Qty, &p.Cost); err != nil {
			rs.Close()
			return s, err
		}
		s.Positions = append(s.Positions, p)
	}
	rs.Close()

	bySector := map[string]float64{}
	for i := range s.Positions {
		p := &s.Positions[i]
		p.Close = lastClose(db, p.Ticker)
		p.Value, p.Sector = p.Qty*p.Close, sectorOf(db, set, p.Ticker)
		p.PnL = p.Value - p.Cost
		p.Status = "FUND"
		if !slices.Contains(set.etfs(), p.Ticker) {
			p.Status, _ = latestStatus(db, p.Ticker, false)
		}
		bySector[p.Sector] += p.Value
	}
	total := 0.0
	for _, p := range s.Positions {
		total += p.Value
	}
	for i := range s.Positions {
		p := &s.Positions[i]
		if total > 0 {
			p.Weight = p.Value / total
		}
		if p.Weight*100 > set.num("max_position_pct") {
			s.Flags = append(s.Flags, fmt.Sprintf("%s is %.0f%% of holdings (limit %g%%)", p.Ticker, p.Weight*100, set.num("max_position_pct")))
		}
		if p.Status != "COMPLIANT" && p.Status != "FUND" {
			s.Flags = append(s.Flags, fmt.Sprintf("%s is held but %s", p.Ticker, p.Status))
		}
	}
	for sec, v := range bySector {
		e := exposure{Sector: sec, Value: v, Limit: set.num("max_sector_pct") / 100}
		if sec == "Semiconductors" {
			e.Limit = set.num("max_semis_pct") / 100
		}
		if total > 0 {
			e.Weight = v / total
		}
		if sec != "Fund" && e.Weight > e.Limit {
			s.Flags = append(s.Flags, fmt.Sprintf("%s is %.0f%% of holdings (limit %.0f%%)", sec, e.Weight*100, e.Limit*100))
		}
		s.Sectors = append(s.Sectors, e)
	}
	sort.Slice(s.Sectors, func(i, j int) bool { return s.Sectors[i].Value > s.Sectors[j].Value })
	sort.Strings(s.Flags)
	return s, nil
}

func runPortfolio(db *sql.DB) error {
	s, err := portfolioSummary(db)
	if err != nil {
		return err
	}
	if s.AsOf == "" {
		fmt.Println("no trading days simulated yet (start", s.Start+")")
		return nil
	}
	fmt.Printf("as of %s (start %s)\n", s.AsOf, s.Start)
	fmt.Printf("value $%.2f = cash $%.2f + holdings $%.2f | deposited $%.2f | P&L $%+.2f | TWR %+.2f%%\n",
		s.Value, s.Cash, s.Holdings, s.Contributions, s.PnL, s.TWR*100)
	fmt.Printf("benchmark %s with the same deposits: $%.2f | TWR %+.2f%% | you vs benchmark %+.2f pts\n\n",
		s.Benchmark, s.BenchValue, s.BenchTWR*100, (s.TWR-s.BenchTWR)*100)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TICKER\tQTY\tCOST\tCLOSE\tVALUE\tP&L\tWEIGHT\tSECTOR\tSTATUS")
	for _, p := range s.Positions {
		fmt.Fprintf(w, "%s\t%.6g\t%.2f\t%.2f\t%.2f\t%+.2f\t%.0f%%\t%s\t%s\n", p.Ticker, p.Qty, p.Cost, p.Close, p.Value, p.PnL, p.Weight*100, p.Sector, p.Status)
	}
	w.Flush()
	for _, f := range s.Flags {
		fmt.Println("FLAG:", f)
	}
	return nil
}

type journalEntry struct {
	OrderID int64   `json:"orderId"`
	Date    string  `json:"date"`
	Ticker  string  `json:"ticker"`
	Thesis  string  `json:"thesis"`
	Reason  string  `json:"reason"`
	Cost    float64 `json:"cost"`
	PnL     float64 `json:"pnl"`
	Return  float64 `json:"return"`
	Open    bool    `json:"open"`
}

type exitEntry struct {
	OrderID  int64   `json:"orderId"`
	Date     string  `json:"date"`
	Ticker   string  `json:"ticker"`
	Thesis   string  `json:"thesis"`
	Reason   string  `json:"reason"`
	Realized float64 `json:"realized"`
}

type thesisStat struct {
	Thesis  string  `json:"thesis"`
	Trades  int     `json:"trades"`
	Wins    int     `json:"wins"`
	Cost    float64 `json:"cost"`
	PnL     float64 `json:"pnl"`
	Return  float64 `json:"return"`
	HitRate float64 `json:"hitRate"`
}

type journal struct {
	Theses  []thesisStat   `json:"theses"`
	Entries []journalEntry `json:"entries"`
	Exits   []exitEntry    `json:"exits"`
}

// journalReport scores every buy (one lot each) by realized + unrealized P&L plus net
// dividends, and groups them by thesis tag.
func journalReport(db *sql.DB) (journal, error) {
	j := journal{Theses: []thesisStat{}, Entries: []journalEntry{}, Exits: []exitEntry{}}
	rs, err := db.Query(`SELECT l.order_id, l.opened_on, l.ticker, l.thesis, o.reason, l.cost, l.realized + l.income, l.open_qty, l.open_cost
		FROM lots l JOIN orders o ON o.id = l.order_id ORDER BY l.opened_on, l.id`)
	if err != nil {
		return j, err
	}
	stats := map[string]*thesisStat{}
	for rs.Next() {
		var e journalEntry
		var gained, openQty, openCost float64
		if err := rs.Scan(&e.OrderID, &e.Date, &e.Ticker, &e.Thesis, &e.Reason, &e.Cost, &gained, &openQty, &openCost); err != nil {
			rs.Close()
			return j, err
		}
		e.Open = openQty > 0
		e.PnL = gained
		if e.Open {
			e.PnL += openQty*lastClose(db, e.Ticker) - openCost
		}
		if e.Cost > 0 {
			e.Return = e.PnL / e.Cost
		}
		j.Entries = append(j.Entries, e)
		st := stats[e.Thesis]
		if st == nil {
			st = &thesisStat{Thesis: e.Thesis}
			stats[e.Thesis] = st
		}
		st.Trades++
		st.Cost += e.Cost
		st.PnL += e.PnL
		if e.PnL > 0 {
			st.Wins++
		}
	}
	rs.Close()
	for _, st := range stats {
		st.Return, st.HitRate = st.PnL/st.Cost, float64(st.Wins)/float64(st.Trades)
		j.Theses = append(j.Theses, *st)
	}
	sort.Slice(j.Theses, func(a, b int) bool { return j.Theses[a].PnL > j.Theses[b].PnL })

	rs, err = db.Query(`SELECT COALESCE(l.order_id, 0), l.date, l.ticker, COALESCE(o.thesis, 'compliance'), COALESCE(l.note, ''), COALESCE(l.realized_pnl, 0)
		FROM ledger l LEFT JOIN orders o ON o.id = l.order_id WHERE l.kind = 'sell' ORDER BY l.date, l.id`)
	if err != nil {
		return j, err
	}
	defer rs.Close()
	for rs.Next() {
		var e exitEntry
		if err := rs.Scan(&e.OrderID, &e.Date, &e.Ticker, &e.Thesis, &e.Reason, &e.Realized); err != nil {
			return j, err
		}
		j.Exits = append(j.Exits, e)
	}
	return j, rs.Err()
}

func runJournal(db *sql.DB) error {
	j, err := journalReport(db)
	if err != nil {
		return err
	}
	if len(j.Entries) == 0 {
		fmt.Println("no filled buys yet")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "BY THESIS\nTHESIS\tTRADES\tINVESTED\tP&L\tRETURN\tHIT RATE")
	for _, t := range j.Theses {
		fmt.Fprintf(w, "%s\t%d\t%.2f\t%+.2f\t%+.1f%%\t%.0f%%\n", t.Thesis, t.Trades, t.Cost, t.PnL, t.Return*100, t.HitRate*100)
	}
	fmt.Fprintln(w, "\nENTRIES\n#\tDATE\tTICKER\tTHESIS\tINVESTED\tP&L\tRETURN\tOPEN\tREASON")
	for _, e := range j.Entries {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%.2f\t%+.2f\t%+.1f%%\t%v\t%s\n", e.OrderID, e.Date, e.Ticker, e.Thesis, e.Cost, e.PnL, e.Return*100, e.Open, e.Reason)
	}
	if len(j.Exits) > 0 {
		fmt.Fprintln(w, "\nEXITS\n#\tDATE\tTICKER\tTHESIS\tREALIZED\tREASON")
		for _, e := range j.Exits {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%+.2f\t%s\n", e.OrderID, e.Date, e.Ticker, e.Thesis, e.Realized, e.Reason)
		}
	}
	return w.Flush()
}
