# suspt
<img width="3024" height="638" alt="image" src="https://github.com/user-attachments/assets/c456a81f-116e-4204-90d1-5c20085110d0" />

A Sharia-compliant US stock tracker. Simulation only: no brokerage, no real orders. Made for learning purposes only.
One Go binary, one SQLite file: a web app for everything, plus a CLI for terminal users.

## Getting started (no terminal needed)

1. Install Go once from https://go.dev/dl/ (the app builds itself with it).
2. Download this repo (green **Code** button, **Download ZIP**) and unzip it.
3. Double-click `Suspt.command`. The first time, macOS may block it because it was downloaded: right-click it, choose **Open**, then **Open** again. It builds the app and opens it in your browser.
4. Follow **getting started** on the first page: connect the free data sources (a Tiingo account plus your name and email for the SEC), pick stocks, and place your first paper trade.

Keep the small window that opens: closing it stops the app. While it's open, new prices arrive by themselves after each US market close.

## Decisions

| Topic | Decision |
|---|---|
| Sharia standard | S&P Shariah governs buy/sell. Dow Jones Islamic is shown for comparison. AAOIFI is pending its source document (members-only). |
| Capital | $300 starting deposit plus $50 per month (`suspt config` to change). The benchmark receives the same deposits on the same dates. |
| Benchmark | SPUS (tracks S&P 500 Shariah, same methodology as the governing screen) |
| Prices | Tiingo free tier (50 req/hour, 1000/day, 500 symbols/month, personal use only) |
| Fundamentals | SEC EDGAR companyfacts + submissions (10 req/s max, User-Agent required) |

## Terminal setup

```sh
cp .env.example .env            # fill in TIINGO_API_KEY and SEC_USER_AGENT; suspt reads ./.env itself
go build -o suspt .
./suspt watch NVDA AAPL MSFT    # what you follow
./suspt update                  # fetch, screen, replay, report (run daily after the US close)
./suspt serve                   # dashboard on http://127.0.0.1:8484
go test ./...
```

## Daily use

```sh
./suspt update
./suspt buy NVDA -usd 50 -reason "AI capex still accelerating" -thesis ai-capex
./suspt buy AAPL 0.2 -limit 190 -gtc -reason "wait for pullback" -thesis quality
./suspt sell NVDA all -reason "too concentrated" -thesis risk
./suspt orders | portfolio | journal | signals
./suspt level NVDA support 120
./suspt rule add golden-cross '*' WATCH trend "sma50 crosses_above sma200"
./suspt review KO excluded 0 "alcohol tie-ins"     # manual business verdict
./suspt config fee_flat 1                          # broker fees, withholding, limits
```

`./suspt` with no arguments lists every command.

## How the simulation works

- **Replay, not state.** Orders and settings are the only inputs; the ledger, lots and equity curve are rebuilt from scratch on every run, so nothing drifts.
- **Fills.** An order placed on day D (New York date) fills on a later bar: market at the open; limit at the open if already through, else at the limit if the day touched it. Day orders expire after one bar; `-gtc` stays open.
- **Gate.** A buy only fills if the stock is COMPLIANT on the fill date (or is in `allowed_etfs`).
- **Compliance sell.** When a held stock turns NON_COMPLIANT, an automatic sell is queued (`compliance_sell`, `compliance_grace_days`).
- **Splits and dividends** come from Tiingo events. Dividends are credited on the ex-date, minus `dividend_withholding` and the purification share, which is logged as its own ledger line.
- **Benchmark** receives each deposit and buys at the next open, with the same fee model; its dividends are reinvested.
- **Returns.** Time-weighted return, so monthly deposits don't count as gains.
- **Journal.** Each buy is one lot; sells consume lots FIFO, so profit and dividends trace back to the buy's reason and thesis tag.
- **Concentration.** Flags when one position, one sector, or semiconductors exceed `max_position_pct`, `max_sector_pct`, `max_semis_pct` of holdings. Sector comes from the SIC code (`suspt sector` overrides).

## Signals

Rules are explicit clauses joined by `and`: terms `open high low close chg smaN level:NAME` or numbers, operators `< <= > >= crosses_above crosses_below`. Editing a rule adds a new version and retires the old one (`suspt rules -all`). Indicators use split-adjusted prices. A BUY signal on a stock that is not COMPLIANT shows as BLOCKED.

## The app

`suspt` (or `Suspt.command`) starts the app on 127.0.0.1 and opens the browser; `suspt serve` starts it without opening one. Everything the CLI does is in the app: trades with a required reason and thesis, watchlist, key levels, rules, business verdicts, settings and API-key setup. Trades from the app and the CLI go through the same validation.

- **Background refresh.** While the app is open it refreshes after each US close (`auto_refresh`), and the top-right button shows progress, including waits for Tiingo's hourly limit. Only one refresh runs at a time, app or terminal.
- **Local only.** It refuses to listen on anything but loopback (no login yet). Requests must be addressed to localhost (blocks DNS rebinding), and every change needs an `X-Suspt` header that other websites can't send without a CORS preflight the server never approves. API keys are written to `.env` (mode 0600) and never sent back to the browser.
- **Look.** Styled after getinsomnia.app: navy, monospace, serif numbers, numbered views, keyboard footer. Keys: `1` to `7` views, `j/k` move, `b` buy, `/` filter, `h` rule hits, `?` help. Charts use TradingView Lightweight Charts 4.2.3 (Apache-2.0), vendored in `web/vendor` after checking the npm integrity hash.

## Sharia screen

Source: S&P Shariah Indices Methodology, October 2026 edition (accounting screens changed 2026-09-18).

- Debt / 36-month avg market value of equity < 30%
- (Cash & deposits + interest-bearing securities) / same < 30%
- (Non-permissible revenue + all interest income) / (revenue + interest income) < 5%
- Buffer: a compliant stock within 2pp over the limit stays compliant until 3 consecutive breaches; a non-compliant stock within 2pp under the limit needs 3 consecutive passes
- Purification ratio: dividends x (non-permissible revenue + interest + non-operating income) / total income

Dow Jones Islamic (September 2026 edition, shown for comparison only): the same limits, buffer and income test, with the market value averaged over 24 months instead of 36.

Rule history: both methodologies changed on 2026-09-18. Before that, the only ratio test was debt under 33% (per both change logs), and that is what older evaluations use, so the 2-point buffer carries stocks across the change exactly as the indices do.

Our choices (not in the methodology):

- Evaluated at every month-end from the 36th month of price history (S&P evaluates monthly); the current month is provisional. Rule versions older than the pre-2026-09-18 one (e.g. S&P's 2023 cash and receivables tests) are not modelled.
- Checked against SPUS's actual holdings: the screen agrees except where a ratio sits within a point or two of the limit (IBM in October 2026), where S&P's data provider differs slightly from SEC figures. The screen notes those cases.
- Only filings already filed on the evaluation date are used (no lookahead). Shares are split-adjusted.
- XBRL concept mapping for debt, cash, securities and interest income is in `screen.go` (`totalDebt`, `cashAndSecurities`, `interestConcepts`).
- Business activity: SIC code gives `ok` / `excluded` / `review`. Non-permissible revenue (alcohol, pork, etc.) is not in XBRL, so it comes from `suspt review`, and applies from the day it is recorded (it never rewrites past fills).
- Known gaps: some issuers (AAPL, MSFT) don't tag interest income, so the NPI ratio excludes it (shown in NOTE). XOM's SEC ticker now points to a 2026 holding company with little history. Foreign filers (20-F, e.g. TSM) have no us-gaap facts and show NO_DATA.

## Data

`data/suspt.db` (gitignored) holds everything; back it up to keep orders, reviews and rules.

- Inputs: `orders`, `settings`, `watchlist`, `levels`, `rules`, `business_reviews`, `sectors`
- Fetched: `prices` (raw OHLC + dividend/split events), `companies`, `facts` (all XBRL facts), EDGAR cache in `data/cache`
- Derived (rebuilt every run): `screens`, `ledger`, `lots`, `equity`

## Build order

1. ~~Data ingestion~~
2. ~~Sharia screen + tests~~ (S&P governing; reference standards pending)
3. ~~Paper portfolio + order engine~~
4. ~~Journal + benchmark~~
5. ~~Signals~~
6. ~~Dashboard + charts~~
