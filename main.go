// Command suspt is a sharia-compliant US stock paper-trading tracker.
// Simulation only: it never connects to a brokerage or places real orders.
package main

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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
`

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
	if len(os.Args) < 3 || os.Args[1] != "ingest" {
		log.Fatal("usage: suspt ingest TICKER...")
	}
	tiingoKey, secUA := mustEnv("TIINGO_API_KEY"), mustEnv("SEC_USER_AGENT")
	dbPath := os.Getenv("SUSPT_DB")
	if dbPath == "" {
		dbPath = "data/suspt.db"
	}
	db, err := openDB(dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	failed := 0
	for _, t := range os.Args[2:] {
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
		os.Exit(1)
	}
}
