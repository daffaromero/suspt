package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

//go:embed web
var webFS embed.FS

var tickerRe = regexp.MustCompile(`^[A-Z][A-Z0-9.\-]{0,9}$`)

type barJSON struct {
	Time  string  `json:"time"`
	Open  float64 `json:"open"`
	High  float64 `json:"high"`
	Low   float64 `json:"low"`
	Close float64 `json:"close"`
}

type fillJSON struct {
	Time    string  `json:"time"`
	Side    string  `json:"side"`
	Price   float64 `json:"price"` // split-adjusted to match the chart
	Qty     float64 `json:"qty"`
	OrderID int64   `json:"orderId"`
	Reason  string  `json:"reason"`
}

type screenJSON struct {
	Ticker   string   `json:"ticker,omitempty"`
	Name     string   `json:"name,omitempty"`
	AsOf     string   `json:"asOf"`
	Status   string   `json:"status"`
	Business string   `json:"business"`
	Debt     *float64 `json:"debt"`
	Cash     *float64 `json:"cash"`
	NPI      *float64 `json:"npi"`
	Purif    *float64 `json:"purif"`
	Note     string   `json:"note"`
}

type hitJSON struct {
	Time    string `json:"time"`
	Rule    string `json:"rule"`
	Version int    `json:"version"`
	Action  string `json:"action"`
}

type tickerDetail struct {
	Ticker  string       `json:"ticker"`
	Name    string       `json:"name"`
	SIC     string       `json:"sic"`
	Sector  string       `json:"sector"`
	Status  string       `json:"status"`
	Held    float64      `json:"held"`
	Bars    []barJSON    `json:"bars"`
	Levels  []levelDist  `json:"levels"`
	Fills   []fillJSON   `json:"fills"`
	Screens []screenJSON `json:"screens"`
	Hits    []hitJSON    `json:"hits"`
}

func ptr(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	return &n.Float64
}

func scanScreens(rs *sql.Rows, withTicker bool) ([]screenJSON, error) {
	defer rs.Close()
	out := []screenJSON{}
	for rs.Next() {
		var s screenJSON
		var d, c, n, p sql.NullFloat64
		dest := []any{&s.AsOf, &s.Status, &s.Business, &d, &c, &n, &p, &s.Note}
		if withTicker {
			dest = append([]any{&s.Ticker, &s.Name}, dest...)
		}
		if err := rs.Scan(dest...); err != nil {
			return nil, err
		}
		s.Debt, s.Cash, s.NPI, s.Purif = ptr(d), ptr(c), ptr(n), ptr(p)
		out = append(out, s)
	}
	return out, rs.Err()
}

func latestScreens(db *sql.DB) ([]screenJSON, error) {
	rs, err := db.Query(`SELECT s.ticker, COALESCE(c.name, ''), s.as_of, s.status, s.business, s.debt_ratio, s.cash_ratio, s.npi_ratio, s.purification_ratio, COALESCE(s.note, '')
		FROM screens s JOIN (SELECT ticker, MAX(as_of) AS as_of FROM screens GROUP BY ticker) m USING (ticker, as_of)
		LEFT JOIN companies c USING (ticker) ORDER BY s.ticker`)
	if err != nil {
		return nil, err
	}
	return scanScreens(rs, true)
}

func loadTickerDetail(db *sql.DB, t string) (tickerDetail, error) {
	set, err := loadSettings(db)
	if err != nil {
		return tickerDetail{}, err
	}
	d := tickerDetail{Ticker: t, Status: tickerStatus(db, set, t), Sector: sectorOf(db, set, t), Bars: []barJSON{}, Fills: []fillJSON{}, Hits: []hitJSON{}}
	db.QueryRow(`SELECT COALESCE(name, ''), COALESCE(sic, '') FROM companies WHERE ticker = ?`, t).Scan(&d.Name, &d.SIC)
	db.QueryRow(`SELECT COALESCE(SUM(open_qty), 0) FROM lots WHERE ticker = ?`, t).Scan(&d.Held)
	bars, err := loadOHLC(db, t)
	if err != nil {
		return d, err
	}
	if len(bars) == 0 {
		return d, errNoTicker
	}
	s := adjustedSeries(bars)
	for i := range s.Dates {
		d.Bars = append(d.Bars, barJSON{s.Dates[i], s.Open[i], s.High[i], s.Low[i], s.Close[i]})
	}
	lv, err := loadLevels(db, t)
	if err != nil {
		return d, err
	}
	last := s.Close[len(s.Close)-1]
	d.Levels = []levelDist{}
	for n, p := range lv {
		d.Levels = append(d.Levels, levelDist{Name: n, Price: p, Dist: (p - last) / last})
	}

	rs, err := db.Query(`SELECT date, kind, price, qty, COALESCE(order_id, 0), COALESCE(note, '') FROM ledger WHERE ticker = ? AND kind IN ('buy', 'sell') ORDER BY date, id`, t)
	if err != nil {
		return d, err
	}
	for rs.Next() {
		var f fillJSON
		if err := rs.Scan(&f.Time, &f.Side, &f.Price, &f.Qty, &f.OrderID, &f.Reason); err != nil {
			rs.Close()
			return d, err
		}
		factor := 1.0
		for _, b := range bars {
			if b.Date > f.Time && b.Split > 0 {
				factor *= b.Split
			}
		}
		f.Price, f.Qty = f.Price/factor, f.Qty*factor
		d.Fills = append(d.Fills, f)
	}
	rs.Close()

	rs, err = db.Query(`SELECT as_of, status, business, debt_ratio, cash_ratio, npi_ratio, purification_ratio, COALESCE(note, '') FROM screens WHERE ticker = ? ORDER BY as_of`, t)
	if err != nil {
		return d, err
	}
	if d.Screens, err = scanScreens(rs, false); err != nil {
		return d, err
	}

	rules, err := loadRules(db, false)
	if err != nil {
		return d, err
	}
	watch, err := watchlistTickers(db)
	if err != nil {
		return d, err
	}
	for _, r := range rules {
		if r.Ticker != t && !(r.Ticker == "*" && slices.Contains(watch, t)) {
			continue
		}
		prev := false
		for i := range s.Dates { // mark only the first day of each run, not every day it stays true
			now := matches(r.clauses, s, i, lv)
			if now && !prev {
				d.Hits = append(d.Hits, hitJSON{Time: s.Dates[i], Rule: r.Name, Version: r.Version, Action: r.Action})
			}
			prev = now
		}
	}
	return d, nil
}

var errNoTicker = errors.New("no prices for this ticker")

type orderJSON struct {
	ID       int64    `json:"id"`
	PlacedOn string   `json:"placedOn"`
	Ticker   string   `json:"ticker"`
	Side     string   `json:"side"`
	Size     string   `json:"size"`
	Kind     string   `json:"kind"`
	Reason   string   `json:"reason"`
	Thesis   string   `json:"thesis"`
	Status   string   `json:"status"`
	FilledOn string   `json:"filledOn"`
	Price    *float64 `json:"price"`
	Qty      *float64 `json:"qty"`
	Note     string   `json:"note"`
}

func ordersJSON(db *sql.DB) ([]orderJSON, error) {
	orders, err := loadOrders(db)
	if err != nil {
		return nil, err
	}
	out := []orderJSON{}
	for i := len(orders) - 1; i >= 0; i-- {
		o := orders[i]
		j := orderJSON{ID: o.ID, PlacedOn: o.PlacedOn, Ticker: o.Ticker, Side: o.Side, Size: o.size(), Kind: o.kind(), Reason: o.Reason,
			Thesis: o.Thesis, Status: o.Status, FilledOn: o.FilledOn, Note: o.Note}
		if o.Status == "filled" {
			j.Price, j.Qty = &o.FillPrice, &o.FillQty
		}
		out = append(out, j)
	}
	return out, nil
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-cache") // embedded files have no mtime; always revalidate after a rebuild
		h.ServeHTTP(w, r)
	})
}

func newServer(db *sql.DB) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(static))
	api := func(path string, fn func(*http.Request) (any, error)) {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			v, err := fn(r)
			if errors.Is(err, errNoTicker) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			} else if err != nil {
				log.Printf("%s: %v", r.URL.Path, err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			if err := json.NewEncoder(w).Encode(v); err != nil {
				log.Printf("%s: encode: %v", r.URL.Path, err)
			}
		})
	}
	api("/api/summary", func(*http.Request) (any, error) { return portfolioSummary(db) })
	api("/api/watchlist", func(*http.Request) (any, error) { return watchlistRows(db) })
	api("/api/journal", func(*http.Request) (any, error) { return journalReport(db) })
	api("/api/signals", func(*http.Request) (any, error) { return currentSignals(db) })
	api("/api/orders", func(*http.Request) (any, error) { return ordersJSON(db) })
	api("/api/screens", func(*http.Request) (any, error) { return latestScreens(db) })
	api("/api/rules", func(*http.Request) (any, error) {
		r, err := loadRules(db, true)
		if r == nil {
			r = []rule{}
		}
		return r, err
	})
	api("/api/ticker/{t}", func(r *http.Request) (any, error) {
		t := strings.ToUpper(r.PathValue("t"))
		if !tickerRe.MatchString(t) {
			return nil, errNoTicker
		}
		return loadTickerDetail(db, t)
	})
	return securityHeaders(mux)
}

func runServe(db *sql.DB, args []string) error {
	addr := "127.0.0.1:8484"
	if len(args) > 0 {
		addr = args[0]
	}
	if host, _, err := net.SplitHostPort(addr); err != nil {
		return err
	} else if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		log.Printf("warning: %s is reachable from other machines and the dashboard has no login", addr)
	}
	if err := replay(db); err != nil {
		log.Printf("replay: %v", err)
	}
	log.Printf("dashboard: http://%s", addr)
	return http.ListenAndServe(addr, newServer(db))
}
