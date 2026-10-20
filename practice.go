package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

// Practice mode ("time machine"): a copy of the database that only contains what was
// known on the practice date. Starting copies the real database and deletes everything
// after that date; fast-forwarding copies the next slice of prices and filings back in.
// Because the future is absent rather than filtered, no page can leak it.

type scenario struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Start   string   `json:"start"`
	Blurb   string   `json:"blurb"`
	Tickers []string `json:"tickers"`
}

var scenarios = []scenario{
	{ID: "basics", Title: "Learn the basics", Start: "2025-01-02",
		Blurb:   "January 2025, eight of the largest companies in the S&P 500 Shariah index. A calm place to try every button.",
		Tickers: []string{"NVDA", "AAPL", "MSFT", "GOOGL", "AVGO", "LLY", "AMD", "TSLA"}},
	{ID: "ai-2024", Title: "The 2024 AI run-up", Start: "2024-01-02",
		Blurb:   "Chip stocks in January 2024. NVIDIA splits its shares 10-for-1 that June: watch your share count change and your money stay the same.",
		Tickers: []string{"NVDA", "AMD", "AVGO", "MU", "AMAT", "MSFT", "GOOGL", "AAPL"}},
}

const earliestPractice = "2023-01-03" // the screen needs 36 months of prices, which start in 2020

type mission struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Hint  string `json:"hint"`
	Done  bool   `json:"done"`
}

func (a *app) practicePath() string {
	return strings.TrimSuffix(a.dbPath, ".db") + "-practice.db"
}

// practiceDB returns the open practice database, opening it if a practice file exists.
func (a *app) practiceDB() (*sql.DB, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.practice != nil {
		return a.practice, nil
	}
	if _, err := os.Stat(a.practicePath()); err != nil {
		return nil, errNoPractice
	}
	d, err := openDB(a.practicePath())
	if err != nil {
		return nil, err
	}
	a.practice = d
	return d, nil
}

var errNoPractice = errors.New("practice hasn't started: open the practice page to begin")

// dbFor picks the real or practice database for a request.
func (a *app) dbFor(r *http.Request) (*sql.DB, error) {
	if isPractice(r) {
		return a.practiceDB()
	}
	return a.db, nil
}

func isPractice(r *http.Request) bool { return r.Header.Get("X-Suspt-Mode") == "practice" }

// tradingDays lists the benchmark's dates in the real database between from and to.
func tradingDays(db *sql.DB, bench, from, to string) ([]string, error) {
	rs, err := db.Query(`SELECT date FROM prices WHERE ticker = ? AND date >= ? AND date <= ? ORDER BY date`, bench, from, to)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var d string
		if err := rs.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rs.Err()
}

// startPractice creates a fresh practice copy that ends on the first trading day on or
// after date, with tickers on its watchlist.
func (a *app) startPractice(scenarioID, date string, tickers []string) error {
	set, err := loadSettings(a.db)
	if err != nil {
		return err
	}
	if date < earliestPractice {
		return fmt.Errorf("practice can start from %s: the sharia screen needs three years of prices, which begin in 2020", earliestPractice)
	}
	days, err := tradingDays(a.db, set["benchmark"], date, "9999")
	if err != nil {
		return err
	}
	if len(days) < 21 {
		return errors.New("pick a date at least a month ago, so there is something to fast-forward through")
	}
	start := days[0]

	a.mu.Lock()
	if a.practice != nil {
		a.practice.Close()
		a.practice = nil
	}
	a.mu.Unlock()
	path := a.practicePath()
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		os.Remove(f)
	}
	if _, err := a.db.Exec(`VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("copying the data for practice: %w", err)
	}
	d, err := a.practiceDB()
	if err != nil {
		return err
	}
	// Remove the future and everything personal; keep fee and limit settings.
	for _, q := range []string{
		`DELETE FROM prices WHERE date > ?`, `DELETE FROM facts WHERE filed > ?`, `DELETE FROM index_members WHERE as_of > ?`,
		`DELETE FROM screens`, `DELETE FROM orders`, `DELETE FROM ledger`, `DELETE FROM lots`, `DELETE FROM equity`, `DELETE FROM rules`,
		`DELETE FROM levels`, `DELETE FROM business_reviews`, `DELETE FROM watchlist`, `DELETE FROM api_calls`,
	} {
		args := []any{}
		if strings.Contains(q, "?") {
			args = append(args, start)
		}
		if _, err := d.Exec(q, args...); err != nil {
			return err
		}
	}
	for k, v := range map[string]string{"start_date": start, "practice_clock": start, "practice_start": start, "practice_scenario": scenarioID, "auto_refresh": "off"} {
		if _, err := d.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}
	if _, err := addWatch(d, tickers); err != nil {
		return err
	}
	d.Exec(`VACUUM`) // the copy shrinks to what practice needs
	return practiceRefresh(d)
}

// practiceRefresh re-screens the practice stocks and replays the account.
func practiceRefresh(d *sql.DB) error {
	rs, err := d.Query(`SELECT ticker FROM watchlist UNION SELECT ticker FROM orders`)
	if err != nil {
		return err
	}
	var tickers []string
	for rs.Next() {
		var t string
		rs.Scan(&t)
		tickers = append(tickers, t)
	}
	rs.Close()
	for _, t := range tickers {
		screenTicker(d, t) // funds and companies without filings have no screen
	}
	return replay(d)
}

// advancePractice moves the practice date forward by a day, week or month (in trading
// days of the real calendar) and copies in what became known.
func (a *app) advancePractice(step string) (string, error) {
	d, err := a.practiceDB()
	if err != nil {
		return "", err
	}
	set, err := loadSettings(d)
	if err != nil {
		return "", err
	}
	clock := set["practice_clock"]
	c, _ := time.Parse(time.DateOnly, clock)
	var limit time.Time
	switch step {
	case "day":
		limit = c.AddDate(0, 0, 1)
	case "week":
		limit = c.AddDate(0, 0, 7)
	case "month":
		limit = c.AddDate(0, 1, 0)
	default:
		return "", errors.New("step must be day, week or month")
	}
	days, err := tradingDays(a.db, set["benchmark"], addDays(clock, 1), "9999")
	if err != nil {
		return "", err
	}
	if len(days) == 0 {
		return "", errors.New("you've reached today: there is no more history to fast-forward through")
	}
	next := days[0] // always move at least one trading day
	for _, day := range days {
		if day > limit.Format(time.DateOnly) {
			break
		}
		next = day
	}

	ctx := context.Background()
	conn, err := d.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS live`, a.dbPath); err != nil {
		return "", err
	}
	defer conn.ExecContext(ctx, `DETACH DATABASE live`)
	for _, q := range []string{
		`INSERT OR IGNORE INTO prices SELECT * FROM live.prices WHERE date > ? AND date <= ?`,
		`INSERT INTO facts SELECT * FROM live.facts WHERE filed > ? AND filed <= ?`,
	} {
		if _, err := conn.ExecContext(ctx, q, clock, next); err != nil {
			return "", err
		}
	}
	if _, err := conn.ExecContext(ctx, `UPDATE settings SET value = ? WHERE key = 'practice_clock'`, next); err != nil {
		return "", err
	}
	return next, practiceRefresh(d)
}

func (a *app) endPractice() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.practice != nil {
		a.practice.Close()
		a.practice = nil
	}
	path := a.practicePath()
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func count(d *sql.DB, q string) int {
	var n int
	d.QueryRow(q).Scan(&n)
	return n
}

// missions are worked out from what the practice account has actually done.
func practiceMissions(d *sql.DB, start, clock string) []mission {
	s, _ := time.Parse(time.DateOnly, start)
	return []mission{
		{"first-trade", "Buy a compliant stock and write down why",
			"Open a stock in the watchlist and press Buy. The reason you write is what the journal scores later.",
			count(d, `SELECT COUNT(*) FROM orders WHERE side = 'buy'`) > 0},
		{"level-rule", "Mark a price level and make a rule that watches it",
			"On a stock's page, add a level named support. Then on Rules, use the example close <= level:support.",
			count(d, `SELECT COUNT(*) FROM levels`) > 0 && count(d, `SELECT COUNT(*) FROM rules WHERE condition LIKE '%level:%'`) > 0},
		{"first-fill", "Fast-forward until your order fills",
			"Orders fill at the next market open, never at the price you saw. Press next day.",
			count(d, `SELECT COUNT(*) FROM lots`) > 0},
		{"event", "Live through a dividend or a stock split",
			"Hold a dividend payer such as AAPL, MSFT or AVGO for a month or two and look for the purification line in your history. In the 2024 scenario, hold NVIDIA past 10 June.",
			count(d, `SELECT COUNT(*) FROM ledger WHERE kind IN ('dividend', 'split')`) > 0},
		{"quarter", "Finish a full quarter and read your report card",
			"Press next month until three months have passed.",
			clock >= s.AddDate(0, 3, 0).Format(time.DateOnly)},
	}
}

func (a *app) practiceStatus() (any, error) {
	set, err := loadSettings(a.db)
	if err != nil {
		return nil, err
	}
	days, _ := tradingDays(a.db, set["benchmark"], "0000", "9999")
	latest := ""
	if len(days) > 0 {
		latest = days[len(days)-1]
	}
	out := map[string]any{"active": false, "scenarios": scenarios, "earliest": earliestPractice, "latest": addDays(latest, -31)}
	d, err := a.practiceDB()
	if errors.Is(err, errNoPractice) {
		return out, nil
	} else if err != nil {
		return nil, err
	}
	ps, err := loadSettings(d)
	if err != nil {
		return nil, err
	}
	out["active"] = true
	out["clock"], out["start"], out["scenario"] = ps["practice_clock"], ps["practice_start"], ps["practice_scenario"]
	out["missions"] = practiceMissions(d, ps["practice_start"], ps["practice_clock"])
	out["atToday"] = ps["practice_clock"] >= latest
	return out, nil
}

func (a *app) postPracticeStart(r *http.Request) (any, error) {
	var in struct{ Scenario, Date string }
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	i := slices.IndexFunc(scenarios, func(s scenario) bool { return s.ID == in.Scenario })
	var tickers []string
	switch {
	case i >= 0:
		in.Date, tickers = scenarios[i].Start, scenarios[i].Tickers
	case in.Scenario == "custom":
		if _, err := time.Parse(time.DateOnly, in.Date); err != nil {
			return nil, errors.New("pick a start date")
		}
		tickers = scenarios[0].Tickers
	default:
		return nil, fmt.Errorf("unknown scenario %q", in.Scenario)
	}
	if err := a.startPractice(in.Scenario, in.Date, tickers); err != nil {
		return nil, err
	}
	return a.practiceStatus()
}
