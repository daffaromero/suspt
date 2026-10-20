package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
)

var tiingoBase = "https://api.tiingo.com"

// Tiingo's free tier allows 50 requests per hour. Calls are logged in the database so
// the budget holds across separate runs.
const tiingoPerHour = 50

var sleep = time.Sleep

// tiingoWait returns how long to wait before the next request is within budget.
func tiingoWait(db *sql.DB, now time.Time) (time.Duration, error) {
	hourAgo := now.Add(-time.Hour).Unix()
	if _, err := db.Exec(`DELETE FROM api_calls WHERE provider = 'tiingo' AND at <= ?`, hourAgo); err != nil {
		return 0, err
	}
	var n int
	var oldest int64
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MIN(at), 0) FROM api_calls WHERE provider = 'tiingo'`).Scan(&n, &oldest); err != nil {
		return 0, err
	}
	if n < tiingoPerHour {
		return 0, nil
	}
	return time.Unix(oldest, 0).Add(time.Hour + time.Second).Sub(now), nil
}

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

var errTiingoLimit = errors.New("tiingo rate limit")

// ingestPrices fetches daily bars from the last stored date onward and upserts them.
// On a 429 (calls made outside this log, e.g. another machine) it marks the hour as used,
// waits, and retries once.
func ingestPrices(db *sql.DB, key, ticker string) (int, error) {
	n, err := fetchPrices(db, key, ticker)
	if !errors.Is(err, errTiingoLimit) {
		return n, err
	}
	now := time.Now().Unix()
	for range tiingoPerHour {
		if _, err := db.Exec(`INSERT INTO api_calls (provider, at) VALUES ('tiingo', ?)`, now); err != nil {
			return 0, err
		}
	}
	return fetchPrices(db, key, ticker)
}

func fetchPrices(db *sql.DB, key, ticker string) (int, error) {
	start := historyStart
	var last sql.NullString
	if err := db.QueryRow(`SELECT MAX(date) FROM prices WHERE ticker = ?`, ticker).Scan(&last); err != nil {
		return 0, err
	}
	if last.Valid {
		start = last.String
	}

	wait, err := tiingoWait(db, time.Now())
	if err != nil {
		return 0, err
	}
	if wait > 0 {
		log.Printf("Tiingo free tier is %d requests/hour; waiting until %s", tiingoPerHour, time.Now().Add(wait).Format("15:04"))
		sleep(wait)
	}
	if _, err := db.Exec(`INSERT INTO api_calls (provider, at) VALUES ('tiingo', ?)`, time.Now().Unix()); err != nil {
		return 0, err
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
			return 0, fmt.Errorf("%w (free tier: 50/hour, 1000/day): %s", errTiingoLimit, body)
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
