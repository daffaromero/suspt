package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testDB(t *testing.T) *sql.DB {
	db, err := openDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestIngestPricesIncremental(t *testing.T) {
	var starts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token k" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		starts = append(starts, r.URL.Query().Get("startDate"))
		fmt.Fprint(w, `[{"date":"2026-10-19T00:00:00.000Z","open":1,"high":2,"low":0.5,"close":1.5,"volume":100,"adjClose":1.4,"divCash":0.2,"splitFactor":1}]`)
	}))
	defer srv.Close()
	tiingoBase = srv.URL
	db := testDB(t)

	for range 2 {
		if _, err := ingestPrices(db, "k", "AAPL"); err != nil {
			t.Fatal(err)
		}
	}
	if len(starts) != 2 || starts[0] != historyStart || starts[1] != "2026-10-19" {
		t.Fatalf("startDate sequence = %v, want [%s 2026-10-19]", starts, historyStart)
	}
	var n int
	var closePx, div float64
	if err := db.QueryRow(`SELECT COUNT(*), MAX(close), MAX(div_cash) FROM prices`).Scan(&n, &closePx, &div); err != nil {
		t.Fatal(err)
	}
	if n != 1 || closePx != 1.5 || div != 0.2 {
		t.Fatalf("got rows=%d close=%v div=%v, want 1 1.5 0.2", n, closePx, div)
	}
}

func TestIngestEdgar(t *testing.T) {
	var uas []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uas = append(uas, r.Header.Get("User-Agent"))
		switch r.URL.Path {
		case "/files/company_tickers.json":
			fmt.Fprint(w, `{"0":{"cik_str":320193,"ticker":"AAPL","title":"Apple Inc."}}`)
		case "/submissions/CIK0000320193.json":
			fmt.Fprint(w, `{"name":"Apple Inc.","sic":"3571","sicDescription":"Electronic Computers",
				"filings":{"recent":{"form":["8-K","10-Q"],"filingDate":["2026-09-01","2026-08-01"]}}}`)
		case "/api/xbrl/companyfacts/CIK0000320193.json":
			fmt.Fprint(w, `{"cik":320193,"facts":{
				"dei":{"EntityCommonStockSharesOutstanding":{"units":{"shares":[
					{"end":"2026-07-18","val":14800000000,"accn":"a1","fy":2026,"fp":"Q3","form":"10-Q","filed":"2026-08-01"}]}}},
				"us-gaap":{"NotUsedByTheScreen":{"units":{"USD":[{"end":"2026-06-28","val":1,"accn":"a1","form":"10-Q","filed":"2026-08-01"}]}},
				"LongTermDebtNoncurrent":{"units":{"USD":[
					{"end":"2026-06-28","val":85000000000,"accn":"a1","fy":2026,"fp":"Q3","form":"10-Q","filed":"2026-08-01"},
					{"end":"2025-09-27","val":90000000000,"accn":"a0","fy":null,"fp":"FY","form":"10-K","filed":"2025-10-31"}]}}}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	secBase, secTickersURL, cacheDir = srv.URL, srv.URL+"/files/company_tickers.json", t.TempDir()
	db := testDB(t)

	// First run stores the 3 screen facts (not the unused concept); the second sees no
	// 10-K/10-Q newer than what is stored and skips the multi-MB companyfacts download.
	for i, want := range []int{3, 0} {
		n, err := ingestEdgar(db, "test agent", "AAPL")
		if err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Fatalf("run %d: facts = %d, want %d", i+1, n, want)
		}
	}
	if len(uas) != 3 {
		t.Fatalf("network calls = %d, want 3 (tickers, submissions, companyfacts once)", len(uas))
	}
	for _, ua := range uas {
		if ua != "test agent" {
			t.Fatalf("User-Agent = %q", ua)
		}
	}
	var sic string
	var facts, nullFY int
	if err := db.QueryRow(`SELECT sic FROM companies WHERE ticker = 'AAPL'`).Scan(&sic); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*), SUM(fy IS NULL) FROM facts WHERE cik = 320193`).Scan(&facts, &nullFY); err != nil {
		t.Fatal(err)
	}
	if sic != "3571" || facts != 3 || nullFY != 1 {
		t.Fatalf("sic=%s facts=%d nullFY=%d, want 3571 3 1", sic, facts, nullFY)
	}
}

func TestLoadDotEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("# comment\nSUSPT_T_A=plain\nexport SUSPT_T_B=\"Name you@example.com\"\nSUSPT_T_C=from-file\nSUSPT_T_D=\n"), 0o600)
	t.Setenv("SUSPT_T_C", "from-shell")
	loadDotEnv(p)
	for k, want := range map[string]string{"SUSPT_T_A": "plain", "SUSPT_T_B": "Name you@example.com", "SUSPT_T_C": "from-shell"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
		os.Unsetenv(k)
	}
	if _, set := os.LookupEnv("SUSPT_T_D"); set {
		t.Error("empty value should not be set")
	}
}

func TestTiingoWait(t *testing.T) {
	db := testDB(t)
	now := time.Unix(1_800_000_000, 0)
	for i := range tiingoPerHour {
		db.Exec(`INSERT INTO api_calls (provider, at) VALUES ('tiingo', ?)`, now.Add(time.Duration(i-tiingoPerHour)*time.Minute/2).Unix())
	}
	// 50 calls in the last 25 minutes: wait until the oldest is an hour old.
	if w, err := tiingoWait(db, now); err != nil || w != 35*time.Minute+time.Second {
		t.Fatalf("wait = %v, %v; want 35m1s", w, err)
	}
	if w, _ := tiingoWait(db, now.Add(36*time.Minute)); w != 0 {
		t.Fatalf("after the window slides, wait = %v, want 0", w)
	}
}
