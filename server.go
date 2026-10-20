package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
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
	DJStatus string   `json:"djStatus"`
	DJDebt   *float64 `json:"djDebt"`
	DJCash   *float64 `json:"djCash"`
	AAStatus string   `json:"aaStatus"`
	AADebt   *float64 `json:"aaDebt"`
	AACash   *float64 `json:"aaCash"`
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
		var d, c, n, p, djd, djc, aad, aac sql.NullFloat64
		dest := []any{&s.AsOf, &s.Status, &s.Business, &d, &c, &n, &p, &s.Note, &s.DJStatus, &djd, &djc, &s.AAStatus, &aad, &aac}
		if withTicker {
			dest = append([]any{&s.Ticker, &s.Name}, dest...)
		}
		if err := rs.Scan(dest...); err != nil {
			return nil, err
		}
		s.Debt, s.Cash, s.NPI, s.Purif, s.DJDebt, s.DJCash = ptr(d), ptr(c), ptr(n), ptr(p), ptr(djd), ptr(djc)
		s.AADebt, s.AACash = ptr(aad), ptr(aac)
		out = append(out, s)
	}
	return out, rs.Err()
}

func latestScreens(db *sql.DB) ([]screenJSON, error) {
	rs, err := db.Query(`SELECT s.ticker, COALESCE(c.name, ''), s.as_of, s.status, s.business, s.debt_ratio, s.cash_ratio, s.npi_ratio, s.purification_ratio, COALESCE(s.note, ''),
		COALESCE(s.dj_status, ''), s.dj_debt_ratio, s.dj_cash_ratio, COALESCE(s.aa_status, ''), s.aa_debt_ratio, s.aa_cash_ratio
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

	rs, err = db.Query(`SELECT as_of, status, business, debt_ratio, cash_ratio, npi_ratio, purification_ratio, COALESCE(note, ''),
		COALESCE(dj_status, ''), dj_debt_ratio, dj_cash_ratio, COALESCE(aa_status, ''), aa_debt_ratio, aa_cash_ratio
		FROM screens WHERE ticker = ? ORDER BY as_of`, t)
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

func newServer(a *app) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(static))
	reply := func(w http.ResponseWriter, r *http.Request, v any, err error, failStatus int) {
		switch {
		case errors.Is(err, errNoTicker):
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		case errors.Is(err, errNoPractice):
			http.Error(w, err.Error(), http.StatusConflict)
			return
		case err != nil:
			log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, err.Error(), failStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			log.Printf("%s: encode: %v", r.URL.Path, err)
		}
	}
	// Every handler gets the database the request is about: the real account, or the
	// practice copy when the page sends X-Suspt-Mode: practice.
	type handler func(db *sql.DB, r *http.Request) (any, error)
	route := func(pattern string, failStatus int, fn handler) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			db, err := a.dbFor(r)
			if err != nil {
				reply(w, r, nil, err, failStatus)
				return
			}
			v, err := fn(db, r)
			reply(w, r, v, err, failStatus)
		})
	}
	api := func(path string, fn handler) { route("GET "+path, http.StatusInternalServerError, fn) }
	act := func(pattern string, fn handler) { route(pattern, http.StatusBadRequest, fn) } // 400s carry a message for the person
	realOnly := func(fn func(r *http.Request) (any, error)) handler {
		return func(_ *sql.DB, r *http.Request) (any, error) {
			if isPractice(r) {
				return nil, errors.New("not available in practice: practice uses saved history and your real account's settings")
			}
			return fn(r)
		}
	}
	ok := func(err error) (any, error) { return map[string]bool{"ok": err == nil}, err }

	api("/api/summary", func(db *sql.DB, _ *http.Request) (any, error) { return portfolioSummary(db) })
	api("/api/watchlist", func(db *sql.DB, _ *http.Request) (any, error) { return watchlistRows(db) })
	api("/api/journal", func(db *sql.DB, _ *http.Request) (any, error) { return journalReport(db) })
	api("/api/signals", func(db *sql.DB, _ *http.Request) (any, error) { return currentSignals(db) })
	api("/api/orders", func(db *sql.DB, _ *http.Request) (any, error) { return ordersJSON(db) })
	api("/api/screens", func(db *sql.DB, _ *http.Request) (any, error) { return latestScreens(db) })
	api("/api/settings", func(db *sql.DB, _ *http.Request) (any, error) { return settingsJSON(db) })
	api("/api/rules", func(db *sql.DB, _ *http.Request) (any, error) {
		r, err := loadRules(db, true)
		if r == nil {
			r = []rule{}
		}
		return r, err
	})
	api("/api/ticker/{t}", func(db *sql.DB, r *http.Request) (any, error) {
		t := strings.ToUpper(r.PathValue("t"))
		if !tickerRe.MatchString(t) {
			return nil, errNoTicker
		}
		return loadTickerDetail(db, t)
	})

	act("POST /api/orders", postOrder)
	act("POST /api/orders/{id}/cancel", func(db *sql.DB, r *http.Request) (any, error) { return ok(cancelOrder(db, r.PathValue("id"))) })
	act("POST /api/watchlist", func(db *sql.DB, r *http.Request) (any, error) {
		var in struct{ Tickers []string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		if isPractice(r) { // practice can't fetch, so only stocks with saved history make sense
			for _, t := range in.Tickers {
				var n int
				if err := db.QueryRow(`SELECT COUNT(*) FROM prices WHERE ticker = ?`, strings.ToUpper(strings.TrimSpace(t))).Scan(&n); err != nil {
					return nil, err
				}
				if n == 0 {
					return nil, fmt.Errorf("%q has no saved history for this practice date; add it to your real watchlist first", strings.ToUpper(strings.TrimSpace(t)))
				}
			}
		}
		n, err := addWatch(db, in.Tickers)
		if err == nil && isPractice(r) {
			err = practiceRefresh(db)
		}
		return map[string]int{"added": n}, err
	})
	act("DELETE /api/watchlist/{t}", func(db *sql.DB, r *http.Request) (any, error) {
		_, err := db.Exec(`DELETE FROM watchlist WHERE ticker = ?`, strings.ToUpper(r.PathValue("t")))
		return ok(err)
	})
	act("POST /api/levels", func(db *sql.DB, r *http.Request) (any, error) {
		var in struct {
			Ticker, Name string
			Price        float64
		}
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return ok(setLevel(db, strings.ToUpper(in.Ticker), in.Name, in.Price))
	})
	act("DELETE /api/levels/{t}/{name}", func(db *sql.DB, r *http.Request) (any, error) {
		_, err := db.Exec(`DELETE FROM levels WHERE ticker = ? AND name = ?`, strings.ToUpper(r.PathValue("t")), r.PathValue("name"))
		return ok(err)
	})
	act("POST /api/rules", func(db *sql.DB, r *http.Request) (any, error) {
		var in struct{ Name, Ticker, Action, Thesis, Condition string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		ru := rule{Name: in.Name, Ticker: in.Ticker, Action: in.Action, Thesis: in.Thesis, Condition: in.Condition}
		err := addRule(db, &ru)
		return map[string]int{"version": ru.Version}, err
	})
	act("DELETE /api/rules/{name}", func(db *sql.DB, r *http.Request) (any, error) { return ok(retireRule(db, r.PathValue("name"))) })
	act("POST /api/reviews", func(db *sql.DB, r *http.Request) (any, error) {
		var in struct {
			Ticker, Verdict, Note string
			Percent               float64
		}
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return ok(setReview(db, in.Ticker, in.Verdict, in.Percent, in.Note))
	})
	act("POST /api/settings", func(db *sql.DB, r *http.Request) (any, error) {
		var in struct{ Key, Value string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		if isPractice(r) && (in.Key == "start_date" || in.Key == "auto_refresh" || in.Key == "benchmark") {
			return nil, errors.New("practice sets this one itself; start a new practice to change it")
		}
		return ok(setSetting(db, in.Key, in.Value))
	})
	act("POST /api/setup", realOnly(a.postSetup))
	act("POST /api/refresh", realOnly(func(*http.Request) (any, error) { return ok(a.startRefresh("manual")) }))
	act("POST /api/sync-spus", realOnly(func(*http.Request) (any, error) {
		members, err := syncSPUS(a.db)
		if err != nil {
			return nil, err
		}
		n, err := addWatch(a.db, members)
		return map[string]int{"members": len(members), "added": n}, err
	}))

	// Practice ("time machine") control lives outside the per-request database choice.
	plain := func(pattern string, fn func(r *http.Request) (any, error)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			v, err := fn(r)
			reply(w, r, v, err, http.StatusBadRequest)
		})
	}
	plain("GET /api/status", func(*http.Request) (any, error) { return a.status(), nil }) // the real account's data, in either mode
	plain("GET /api/practice", func(*http.Request) (any, error) { return a.practiceStatus() })
	plain("POST /api/practice/start", a.postPracticeStart)
	plain("POST /api/practice/advance", func(r *http.Request) (any, error) {
		var in struct{ Step string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		if _, err := a.advancePractice(in.Step); err != nil {
			return nil, err
		}
		return a.practiceStatus()
	})
	plain("POST /api/practice/end", func(*http.Request) (any, error) { return ok(a.endPractice()) })
	return securityHeaders(guard(mux))
}

// runServe starts the app. Without a login it only listens on this machine. When open
// is set (plain `suspt`), it opens the browser, or just the browser if already running.
func runServe(db *sql.DB, dbPath string, args []string, open bool) error {
	addr := "127.0.0.1:8484"
	if len(args) > 0 {
		addr = args[0]
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("%s would expose the app to other machines, and it has no login yet; use 127.0.0.1", addr)
	}
	url := "http://" + addr
	ln, err := net.Listen("tcp", addr)
	if errors.Is(err, syscall.EADDRINUSE) && open {
		log.Printf("suspt is already running; opening %s", url)
		return openBrowser(url)
	} else if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("%s is already in use: the app is probably already running (open %s), or pass another address: suspt serve 127.0.0.1:8485", addr, url)
	} else if err != nil {
		return err
	}
	if err := replay(db); err != nil {
		log.Printf("replay: %v", err)
	}
	a := &app{db: db, dbPath: dbPath, version: strconv.FormatInt(time.Now().UnixNano(), 36)}
	go a.autoRefresh()
	log.Printf("suspt is running at %s (keep this window open; close it to stop)", url)
	if open {
		if err := openBrowser(url); err != nil {
			log.Printf("open your browser at %s", url)
		}
	}
	return http.Serve(ln, newServer(a))
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "linux":
		return exec.Command("xdg-open", url).Start()
	}
	return errors.New("unsupported OS")
}
