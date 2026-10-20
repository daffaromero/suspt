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
	"strings"
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
	PRIMARY KEY (ticker, as_of)
);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sectors (ticker TEXT PRIMARY KEY, sector TEXT NOT NULL);
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
  suspt ingest TICKER...                          fetch prices (Tiingo) and fundamentals (SEC EDGAR)
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
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("%s is not set (see .env.example)", name)
	}
	return v
}

func main() {
	log.SetFlags(0)
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
	failed := 0
	for _, t := range tickers {
		t = strings.ToUpper(t)
		if n, err := ingestPrices(db, tiingoKey, t); err != nil {
			log.Printf("%s prices: %v", t, err)
			failed++
		} else {
			log.Printf("%s prices: %d bars", t, n)
		}
		if n, err := ingestEdgar(db, secUA, t); errors.Is(err, errNotFiler) {
			log.Printf("%s edgar: skipped, %v", t, err)
		} else if err != nil {
			log.Printf("%s edgar: %v", t, err)
			failed++
		} else {
			log.Printf("%s edgar: %d facts", t, n)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d ingest step(s) failed", failed)
	}
	return nil
}
