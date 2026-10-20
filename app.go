package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// app holds what the web UI needs beyond the database: the background refresh job.
type app struct {
	db     *sql.DB
	dbPath string

	mu          sync.Mutex
	job         jobStatus
	lastAttempt time.Time
}

type jobStatus struct {
	Running  bool   `json:"running"`
	Trigger  string `json:"trigger,omitempty"` // manual or auto
	Started  string `json:"started,omitempty"`
	Finished string `json:"finished,omitempty"`
	Done     int    `json:"done"`
	Total    int    `json:"total"`
	Ticker   string `json:"ticker,omitempty"`
	Message  string `json:"message,omitempty"` // latest log line, e.g. a rate-limit wait
	Error    string `json:"error,omitempty"`
}

// lastLine keeps the most recent log line for the progress display.
type lastLine struct{ a *app }

func (w lastLine) Write(p []byte) (int, error) {
	w.a.mu.Lock()
	w.a.job.Message = strings.TrimSpace(string(p))
	w.a.mu.Unlock()
	return os.Stderr.Write(p)
}

var errBusy = errors.New("a refresh is already running")

// startRefresh runs refresh in the background. The file lock also keeps a terminal
// `suspt update` and the app from fetching at the same time.
func (a *app) startRefresh(trigger string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.job.Running {
		return errBusy
	}
	unlock, err := lockFetches(a.dbPath)
	if err != nil {
		return errors.New("a refresh is already running from the terminal; it will show up here when it finishes")
	}
	a.lastAttempt = time.Now()
	a.job = jobStatus{Running: true, Trigger: trigger, Started: time.Now().Format(time.RFC3339)}
	go func() {
		defer unlock()
		log.SetOutput(lastLine{a})
		defer log.SetOutput(os.Stderr)
		err := refresh(a.db, func(i, n int, t string) {
			a.mu.Lock()
			a.job.Done, a.job.Total, a.job.Ticker = i, n, t
			a.mu.Unlock()
		})
		a.mu.Lock()
		defer a.mu.Unlock()
		a.job.Running, a.job.Done, a.job.Ticker = false, a.job.Total, ""
		a.job.Finished = time.Now().Format(time.RFC3339)
		if err != nil {
			a.job.Error = err.Error()
		}
	}()
	return nil
}

// stale reports whether data is behind: the benchmark lacks the latest closed trading
// day, or watchlist tickers have no prices yet. Holidays look stale too; that costs one
// request per retry because current tickers are skipped.
func (a *app) stale() (bool, error) {
	set, err := loadSettings(a.db)
	if err != nil {
		return false, err
	}
	var last string
	a.db.QueryRow(`SELECT COALESCE(MAX(date), '') FROM prices WHERE ticker = ?`, set["benchmark"]).Scan(&last)
	var missing int
	a.db.QueryRow(`SELECT COUNT(*) FROM watchlist w WHERE NOT EXISTS (SELECT 1 FROM prices p WHERE p.ticker = w.ticker)`).Scan(&missing)
	return last < lastClosedSession(time.Now()) || missing > 0, nil
}

// lastClosedSession is the latest weekday whose regular session (until 16:00 New York,
// plus 30 minutes for data to settle) has ended. Exchange holidays are ignored.
func lastClosedSession(now time.Time) string {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	d := now.In(loc)
	if d.Hour()*60+d.Minute() < 16*60+30 {
		d = d.AddDate(0, 0, -1)
	}
	for d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
		d = d.AddDate(0, 0, -1)
	}
	return d.Format(time.DateOnly)
}

// autoRefresh refreshes on its own when data is stale, at most once an hour.
func (a *app) autoRefresh() {
	for {
		set, err := loadSettings(a.db)
		_, _, keyErr := apiKeys()
		a.mu.Lock()
		due := time.Since(a.lastAttempt) > time.Hour && !a.job.Running
		a.mu.Unlock()
		if err == nil && keyErr == nil && set["auto_refresh"] == "on" && due {
			if s, err := a.stale(); err == nil && s {
				if err := a.startRefresh("auto"); err != nil && !errors.Is(err, errBusy) {
					log.Printf("auto refresh: %v", err)
				}
			}
		}
		time.Sleep(10 * time.Minute)
	}
}

func (a *app) status() map[string]any {
	a.mu.Lock()
	job := a.job
	a.mu.Unlock()
	stale, _ := a.stale()
	var pending, watching int
	a.db.QueryRow(`SELECT COUNT(*) FROM watchlist w WHERE NOT EXISTS (SELECT 1 FROM prices p WHERE p.ticker = w.ticker)`).Scan(&pending)
	a.db.QueryRow(`SELECT COUNT(*) FROM watchlist`).Scan(&watching)
	_, _, keyErr := apiKeys()
	var asOf string
	a.db.QueryRow(`SELECT COALESCE(MAX(date), '') FROM equity`).Scan(&asOf)
	return map[string]any{"job": job, "stale": stale, "pending": pending, "watching": watching, "keys": keyErr == nil, "asOf": asOf}
}

/* write endpoints */

// decode reads a small JSON body strictly.
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("bad request: %w", err)
	}
	return nil
}

func nullF(p *float64) sql.NullFloat64 {
	if p == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *p, Valid: true}
}

func (a *app) postOrder(r *http.Request) (any, error) {
	var in struct {
		Ticker, Side, Reason, Thesis string
		Shares, USD, Limit           *float64
		GTC                          bool
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	o := order{Ticker: in.Ticker, Side: in.Side, Qty: nullF(in.Shares), Notional: nullF(in.USD), Limit: nullF(in.Limit), GTC: in.GTC, Reason: in.Reason, Thesis: in.Thesis}
	if err := placeOrder(a.db, &o); err != nil {
		return nil, err
	}
	return map[string]any{"id": o.ID, "placedOn": o.PlacedOn}, nil
}

var (
	nameRe  = regexp.MustCompile(`^[\p{L}][\p{L} .'-]{0,59}$`)
	emailRe = regexp.MustCompile(`^[^@\s"]+@[^@\s"]+\.[^@\s"]+$`)
	keyRe   = regexp.MustCompile(`^[A-Za-z0-9]{20,64}$`)
)

// postSetup checks the Tiingo key with one test call, then writes both keys to .env.
// Keys are never sent back to the browser; the status endpoint only says whether they exist.
func (a *app) postSetup(r *http.Request) (any, error) {
	var in struct{ TiingoKey, Name, Email string }
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	in.TiingoKey, in.Name, in.Email = strings.TrimSpace(in.TiingoKey), strings.TrimSpace(in.Name), strings.TrimSpace(in.Email)
	switch {
	case !keyRe.MatchString(in.TiingoKey):
		return nil, errors.New("that doesn't look like a Tiingo API token: copy it from tiingo.com, Account, API, Token")
	case !nameRe.MatchString(in.Name):
		return nil, errors.New("enter your name (letters, spaces, . ' -)")
	case !emailRe.MatchString(in.Email):
		return nil, errors.New("enter a valid email; the SEC asks for a contact address on automated requests")
	}
	req, err := http.NewRequest("GET", tiingoBase+"/api/test", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+in.TiingoKey)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach Tiingo to check the key: %w", err)
	}
	resp.Body.Close()
	a.db.Exec(`INSERT INTO api_calls (provider, at) VALUES ('tiingo', ?)`, time.Now().Unix())
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Tiingo rejected that token (%s)", resp.Status)
	}
	ua := in.Name + " " + in.Email
	if err := writeEnv(".env", map[string]string{"TIINGO_API_KEY": in.TiingoKey, "SEC_USER_AGENT": `"` + ua + `"`}); err != nil {
		return nil, err
	}
	os.Setenv("TIINGO_API_KEY", in.TiingoKey)
	os.Setenv("SEC_USER_AGENT", ua)
	return map[string]any{"ok": true}, nil
}

// writeEnv sets keys in a .env file, keeping every other line, readable only by you.
func writeEnv(path string, vals map[string]string) error {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		k, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "export "), "=")
		if _, replaced := vals[strings.TrimSpace(k)]; !replaced && (line != "" || len(out) > 0) {
			out = append(out, line)
		}
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		out = append(out, k+"="+vals[k])
	}
	return os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o600)
}

func (a *app) settingsJSON() (any, error) {
	set, err := loadSettings(a.db)
	if err != nil {
		return nil, err
	}
	type item struct {
		Key     string `json:"key"`
		Value   string `json:"value"`
		Default string `json:"default"`
		Help    string `json:"help"`
	}
	out := []item{}
	for _, d := range settingDefaults {
		out = append(out, item{d.Key, set[d.Key], d.Default, d.Help})
	}
	return out, nil
}

// guard protects the local app from other websites: requests must be addressed to
// localhost (blocks DNS rebinding), and changes need a custom header that a cross-site
// form or fetch can't add without a CORS preflight, which this server never approves.
func guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "this app only answers on localhost", http.StatusMisdirectedRequest)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-Suspt") != "1" {
				http.Error(w, "missing X-Suspt header", http.StatusForbidden)
				return
			}
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}
