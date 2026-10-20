// Command suspt is a sharia-compliant US stock paper-trading tracker.
// Simulation only: it never connects to a brokerage or places real orders.
package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	_ "time/tzdata" // order dates use New York time on any machine

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS prices (
	ticker TEXT NOT NULL,
	date TEXT NOT NULL,
	open REAL, high REAL, low REAL, close REAL, volume REAL,
	div_cash REAL NOT NULL DEFAULT 0,
	split_factor REAL NOT NULL DEFAULT 1,
	PRIMARY KEY (ticker, date)
);
CREATE TABLE IF NOT EXISTS companies (
	ticker TEXT PRIMARY KEY,
	cik INTEGER NOT NULL,
	name TEXT,
	sic TEXT,
	sic_desc TEXT
);
CREATE TABLE IF NOT EXISTS facts (
	cik INTEGER NOT NULL,
	taxonomy TEXT NOT NULL,
	concept TEXT NOT NULL,
	unit TEXT NOT NULL,
	start TEXT, end TEXT NOT NULL,
	val REAL NOT NULL,
	fy INTEGER, fp TEXT, form TEXT, filed TEXT, accn TEXT, frame TEXT
);
CREATE INDEX IF NOT EXISTS facts_lookup ON facts (cik, concept, end);
CREATE TABLE IF NOT EXISTS business_reviews (
	ticker TEXT PRIMARY KEY,
	verdict TEXT NOT NULL CHECK (verdict IN ('ok', 'excluded')),
	nonpermissible_revenue REAL NOT NULL DEFAULT 0, -- fraction of revenue
	note TEXT,
	reviewed_on TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS screens (
	ticker TEXT NOT NULL,
	as_of TEXT NOT NULL,
	status TEXT NOT NULL,
	business TEXT NOT NULL,
	debt_ratio REAL, cash_ratio REAL, npi_ratio REAL, purification_ratio REAL,
	streak INTEGER NOT NULL,
	note TEXT,
	provisional INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (ticker, as_of)
);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS api_calls (provider TEXT NOT NULL, at INTEGER NOT NULL); -- unix seconds, for rate limits
CREATE TABLE IF NOT EXISTS index_members (ticker TEXT NOT NULL, fund TEXT NOT NULL, as_of TEXT NOT NULL, weight REAL, PRIMARY KEY (fund, ticker));
CREATE TABLE IF NOT EXISTS sectors (ticker TEXT PRIMARY KEY, sector TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS watchlist (ticker TEXT PRIMARY KEY, added_on TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS levels (
	ticker TEXT NOT NULL, name TEXT NOT NULL, price REAL NOT NULL CHECK (price > 0),
	PRIMARY KEY (ticker, name)
);
-- Rules are never edited: a change adds a new version and retires the old one.
CREATE TABLE IF NOT EXISTS rules (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL, version INTEGER NOT NULL,
	ticker TEXT NOT NULL, -- or * for the whole watchlist
	action TEXT NOT NULL CHECK (action IN ('BUY', 'WATCH')),
	thesis TEXT NOT NULL, condition TEXT NOT NULL,
	created_on TEXT NOT NULL, retired_on TEXT,
	UNIQUE (name, version)
);
CREATE TABLE IF NOT EXISTS orders (
	id INTEGER PRIMARY KEY,
	placed_on TEXT NOT NULL,
	ticker TEXT NOT NULL,
	side TEXT NOT NULL CHECK (side IN ('buy', 'sell')),
	qty REAL CHECK (qty > 0),           -- shares
	notional REAL CHECK (notional > 0), -- USD; a sell with neither sells the whole position
	limit_price REAL CHECK (limit_price > 0),
	gtc INTEGER NOT NULL DEFAULT 0,
	reason TEXT NOT NULL CHECK (length(trim(reason)) > 0),
	thesis TEXT NOT NULL CHECK (length(trim(thesis)) > 0),
	cancelled_on TEXT,
	-- outcome, rewritten by every replay
	status TEXT NOT NULL DEFAULT 'open',
	filled_on TEXT, fill_price REAL, fill_qty REAL, fee REAL, realized_pnl REAL, note TEXT
);
-- Derived by replay from orders, settings, prices and screens; safe to delete.
CREATE TABLE IF NOT EXISTS ledger (
	id INTEGER PRIMARY KEY,
	date TEXT NOT NULL,
	kind TEXT NOT NULL, -- deposit, buy, sell, dividend, withholding, purification, split, alert
	ticker TEXT, qty REAL, price REAL,
	amount REAL NOT NULL, -- cash effect, negative = out
	fee REAL NOT NULL DEFAULT 0,
	order_id INTEGER, realized_pnl REAL, note TEXT
);
CREATE TABLE IF NOT EXISTS lots (
	id INTEGER PRIMARY KEY,
	order_id INTEGER NOT NULL,
	ticker TEXT NOT NULL, thesis TEXT NOT NULL,
	opened_on TEXT NOT NULL, closed_on TEXT,
	qty REAL NOT NULL, cost REAL NOT NULL,
	open_qty REAL NOT NULL, open_cost REAL NOT NULL,
	realized REAL NOT NULL, income REAL NOT NULL
);
CREATE TABLE IF NOT EXISTS equity (
	date TEXT PRIMARY KEY,
	cash REAL NOT NULL, holdings REAL NOT NULL, value REAL NOT NULL,
	contributions REAL NOT NULL, bench_value REAL NOT NULL
);
`

const usage = `usage:
  suspt update                                    daily routine: ingest watchlist + holdings + benchmark, screen, replay, signals
  suspt serve [ADDR]                              dashboard (default 127.0.0.1:8484)

  suspt sync-spus                                 refresh SPUS (S&P 500 Shariah) holdings and add them to the watchlist
  suspt watch [TICKER...]                         show the watchlist, or add tickers
  suspt unwatch TICKER...
  suspt level TICKER NAME PRICE|rm                set a key level (usable in rules as level:NAME)
  suspt rule add NAME TICKER|* BUY|WATCH THESIS "CONDITION"   e.g. "close crosses_above sma200 and chg > 1"
  suspt rule rm NAME
  suspt rules [-all]                              active rules (-all includes retired versions)
  suspt signals                                   rules firing on the latest bar

  suspt ingest TICKER...                        fetch prices (Tiingo) and fundamentals (SEC EDGAR)
  suspt screen [TICKER...]                        run the S&P Shariah screen (default: all ingested companies)
  suspt review TICKER ok|excluded [PCT] [NOTE...] record a business-activity verdict; PCT = non-permissible revenue %

  suspt buy TICKER (QTY|-usd AMOUNT) [-limit P] [-gtc] -reason TEXT -thesis TAG
  suspt sell TICKER (QTY|all|-usd AMOUNT) [-limit P] [-gtc] -reason TEXT -thesis TAG
  suspt cancel ORDER_ID
  suspt orders                                    list orders and their outcome
  suspt portfolio                                 cash, positions, P&L, benchmark, concentration flags
  suspt journal                                   which reasons and theses worked
  suspt config [KEY VALUE]                        show or change settings (deposits, fees, rules)
  suspt sector TICKER NAME                        override the SIC-derived sector ("auto" to reset)
  suspt run                                       replay the paper account (no network)`

var httpClient = &http.Client{Timeout: time.Minute}

func openDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// WAL lets the dashboard read while an update writes; busy_timeout waits out short
	// write locks instead of failing with "database is locked".
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	// Migration for databases created before screens had the column; screens is derived data.
	if _, err := db.Exec(`ALTER TABLE screens ADD COLUMN provisional INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		db.Close()
		return nil, err
	}
	return db, nil
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("%s is not set: add it to .env in this folder (see .env.example)", name)
	}
	return v
}

// lockFetches lets only one ingest/update run at a time: two would split the hourly
// Tiingo budget and fetch the same tickers twice. The OS drops the lock if the process
// dies. ponytail: flock is Unix-only; use a lock row in the DB if Windows ever matters.
func lockFetches(dbPath string) (func(), error) {
	f, err := os.OpenFile(dbPath+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another `suspt update` or `suspt ingest` is already running; let it finish (or stop it) and try again")
	}
	return func() { f.Close() }, nil
}

// loadDotEnv sets KEY=VALUE pairs from path that the environment doesn't already define.
func loadDotEnv(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if _, set := os.LookupEnv(k); !set && k != "" && v != "" {
			os.Setenv(k, v)
		}
	}
}

func main() {
	log.SetFlags(0)
	loadDotEnv(".env")
	if len(os.Args) < 2 {
		log.Fatal(usage)
	}
	dbPath := os.Getenv("SUSPT_DB")
	if dbPath == "" {
		dbPath = "data/suspt.db"
	}
	db, err := openDB(dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	args := os.Args[2:]
	if os.Args[1] == "ingest" || os.Args[1] == "update" {
		unlock, err := lockFetches(dbPath)
		if err != nil {
			log.Print(err)
			os.Exit(1)
		}
		defer unlock()
	}
	switch os.Args[1] {
	case "ingest":
		err = runIngest(db, args)
	case "screen":
		err = runScreen(db, args)
	case "review":
		err = runReview(db, args)
	case "buy", "sell":
		err = runOrder(db, os.Args[1], args)
	case "cancel":
		err = runCancel(db, args)
	case "orders":
		err = runOrders(db)
	case "portfolio":
		err = runPortfolio(db)
	case "journal":
		err = runJournal(db)
	case "config":
		err = runConfig(db, args)
	case "sector":
		err = runSector(db, args)
	case "watch":
		err = runWatch(db, args)
	case "unwatch":
		err = runUnwatch(db, args)
	case "level":
		err = runLevel(db, args)
	case "rule":
		err = runRule(db, args)
	case "rules":
		err = runRules(db, args)
	case "signals":
		err = runSignals(db)
	case "update":
		err = runUpdate(db)
	case "sync-spus":
		err = runSyncSPUS(db)
	case "serve":
		err = runServe(db, args)
	case "run":
		if err = replay(db); err == nil {
			err = runPortfolio(db)
		}
	default:
		err = errors.New(usage)
	}
	if err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func runIngest(db *sql.DB, tickers []string) error {
	if len(tickers) == 0 {
		return errors.New(usage)
	}
	tiingoKey, secUA := mustEnv("TIINGO_API_KEY"), mustEnv("SEC_USER_AGENT")
	set, err := loadSettings(db)
	if err != nil {
		return err
	}
	// Fetch the benchmark first: any ticker already at its latest date is current and
	// skipped, which saves Tiingo's hourly budget (and handles market holidays for free).
	bench := set["benchmark"]
	for i := range tickers {
		tickers[i] = strings.ToUpper(tickers[i])
	}
	if i := slices.Index(tickers, bench); i > 0 {
		tickers[0], tickers[i] = tickers[i], tickers[0]
	}
	fresh := ""
	failed := 0
	for _, t := range tickers {
		var last sql.NullString
		db.QueryRow(`SELECT MAX(date) FROM prices WHERE ticker = ?`, t).Scan(&last)
		if t != bench && fresh != "" && last.String >= fresh {
			log.Printf("%s prices: up to date", t)
		} else if n, err := ingestPrices(db, tiingoKey, t); err != nil {
			log.Printf("%s prices: %v", t, err)
			failed++
		} else {
			log.Printf("%s prices: %d bars", t, n)
			if t == bench {
				db.QueryRow(`SELECT COALESCE(MAX(date), '') FROM prices WHERE ticker = ?`, t).Scan(&fresh)
			}
		}
		if n, err := ingestEdgar(db, secUA, t); errors.Is(err, errNotFiler) {
			log.Printf("%s edgar: skipped, %v", t, err)
		} else if err != nil {
			log.Printf("%s edgar: %v", t, err)
			failed++
		} else {
			if n == 0 {
				log.Printf("%s edgar: no new filing", t)
			} else {
				log.Printf("%s edgar: %d facts", t, n)
			}
			// Screen right away so a long, rate-limited run fills the dashboard progressively.
			if _, err := screenTicker(db, t); err != nil {
				log.Printf("%s screen: %v", t, err)
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d ingest step(s) failed", failed)
	}
	return nil
}
