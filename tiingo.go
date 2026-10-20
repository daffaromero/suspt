package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

var tiingoBase = "https://api.tiingo.com"

// historyStart covers the S&P Shariah 36-month average market cap with room for charts.
const historyStart = "2020-01-01"

// Raw (unadjusted) bars only. Dividends and splits are kept as events, so the
// portfolio can credit dividends and adjust share counts itself, and stored
// history never goes stale the way adjusted prices do after a new split.
type tiingoBar struct {
	Date                         string
	Open, High, Low, Close       float64
	Volume, DivCash, SplitFactor float64
}

// ingestPrices fetches daily bars from the last stored date onward and upserts them.
func ingestPrices(db *sql.DB, key, ticker string) (int, error) {
	start := historyStart
	var last sql.NullString
	if err := db.QueryRow(`SELECT MAX(date) FROM prices WHERE ticker = ?`, ticker).Scan(&last); err != nil {
		return 0, err
	}
	if last.Valid {
		start = last.String
	}

	u := fmt.Sprintf("%s/tiingo/daily/%s/prices?startDate=%s", tiingoBase, url.PathEscape(ticker), start)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Token "+key) // header, not query param, so the key stays out of logged URLs
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		if resp.StatusCode == http.StatusTooManyRequests {
			return 0, fmt.Errorf("tiingo rate limit hit (free tier: 50/hour, 1000/day): %s", body)
		}
		return 0, fmt.Errorf("tiingo %s: %s", resp.Status, body)
	}
	var bars []tiingoBar
	if err := json.NewDecoder(resp.Body).Decode(&bars); err != nil {
		return 0, err
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO prices
		(ticker, date, open, high, low, close, volume, div_cash, split_factor)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	for _, b := range bars {
		if len(b.Date) < 10 {
			return 0, fmt.Errorf("tiingo: bad date %q", b.Date)
		}
		if _, err := stmt.Exec(ticker, b.Date[:10], b.Open, b.High, b.Low, b.Close, b.Volume, b.DivCash, b.SplitFactor); err != nil {
			return 0, err
		}
	}
	return len(bars), tx.Commit()
}
