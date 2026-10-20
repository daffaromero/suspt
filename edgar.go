package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

var (
	secBase       = "https://data.sec.gov"
	secTickersURL = "https://www.sec.gov/files/company_tickers.json"
	cacheDir      = "data/cache"
	lastSECCall   time.Time
)

const secCacheTTL = 24 * time.Hour

// errNotFiler marks tickers with no SEC company filings, such as ETFs.
var errNotFiler = errors.New("not in SEC ticker list (ETFs and funds usually are not)")

// secGet fetches url with the SEC-required User-Agent and caches the body at cacheDir/name.
func secGet(ua, url, name string) ([]byte, error) {
	path := filepath.Join(cacheDir, name)
	if fi, err := os.Stat(path); err == nil && time.Since(fi.ModTime()) < secCacheTTL {
		return os.ReadFile(path)
	}
	// ponytail: process-wide spacing keeps a single CLI run under SEC's 10 req/s; needs a shared limiter if fetches go concurrent.
	if d := 110*time.Millisecond - time.Since(lastSECCall); d > 0 {
		time.Sleep(d)
	}
	lastSECCall = time.Now()

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	return body, os.WriteFile(path, body, 0o644)
}

func lookupCIK(ua, ticker string) (int, error) {
	b, err := secGet(ua, secTickersURL, "company_tickers.json")
	if err != nil {
		return 0, err
	}
	var m map[string]struct {
		CIK    int `json:"cik_str"`
		Ticker string
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return 0, err
	}
	for _, c := range m {
		if c.Ticker == ticker {
			return c.CIK, nil
		}
	}
	return 0, errNotFiler
}

type factVal struct {
	Start, End                   string
	Val                          float64
	FY                           *int
	FP, Form, Filed, Accn, Frame string
}

type fact struct {
	Taxonomy, Concept, Unit string
	factVal
}

// parseFacts flattens an EDGAR companyfacts document into one row per reported value.
func parseFacts(b []byte) ([]fact, error) {
	var doc struct {
		Facts map[string]map[string]struct {
			Units map[string][]factVal
		}
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	var out []fact
	for tax, concepts := range doc.Facts {
		for concept, c := range concepts {
			for unit, vals := range c.Units {
				for _, v := range vals {
					out = append(out, fact{tax, concept, unit, v})
				}
			}
		}
	}
	return out, nil
}

// ingestEdgar stores the company's SIC classification and replaces all of its XBRL facts.
// All facts are kept so the screen can pick concepts later without refetching.
func ingestEdgar(db *sql.DB, ua, ticker string) (int, error) {
	cik, err := lookupCIK(ua, ticker)
	if err != nil {
		return 0, err
	}
	id := fmt.Sprintf("CIK%010d", cik)

	b, err := secGet(ua, secBase+"/submissions/"+id+".json", id+"-submissions.json")
	if err != nil {
		return 0, err
	}
	var sub struct{ Name, SIC, SICDescription string }
	if err := json.Unmarshal(b, &sub); err != nil {
		return 0, err
	}

	b, err = secGet(ua, secBase+"/api/xbrl/companyfacts/"+id+".json", id+"-facts.json")
	if err != nil {
		return 0, err
	}
	facts, err := parseFacts(b)
	if err != nil {
		return 0, err
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT OR REPLACE INTO companies (ticker, cik, name, sic, sic_desc) VALUES (?, ?, ?, ?, ?)`,
		ticker, cik, sub.Name, sub.SIC, sub.SICDescription); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM facts WHERE cik = ?`, cik); err != nil {
		return 0, err
	}
	stmt, err := tx.Prepare(`INSERT INTO facts
		(cik, taxonomy, concept, unit, start, end, val, fy, fp, form, filed, accn, frame)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	for _, f := range facts {
		if _, err := stmt.Exec(cik, f.Taxonomy, f.Concept, f.Unit, f.Start, f.End, f.Val, f.FY, f.FP, f.Form, f.Filed, f.Accn, f.Frame); err != nil {
			return 0, err
		}
	}
	return len(facts), tx.Commit()
}
