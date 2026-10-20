Classification: INTERNAL (personal project)

# suspt

Sharia-compliant US stock **paper-trading** tracker. Simulation only: no brokerage, no real orders.

## Decisions

| Topic | Decision |
|---|---|
| Sharia standard | S&P Shariah governs buy/sell. AAOIFI and Dow Jones Islamic are computed and shown for reference only. |
| Capital | $300 starting deposit plus $50 per month. The benchmark receives the same deposits on the same dates. |
| Benchmark | SPUS (tracks S&P 500 Shariah, same methodology as the governing screen) |
| Prices | Tiingo free tier (50 req/hour, 1000/day, 500 symbols/month, personal use only) |
| Fundamentals | SEC EDGAR companyfacts + submissions (10 req/s max, User-Agent required) |

## Setup

```sh
cp .env.example .env   # fill in TIINGO_API_KEY and SEC_USER_AGENT
set -a; . ./.env; set +a
go run . ingest AAPL MSFT NVDA SPUS HLAL
go run . screen                       # S&P Shariah status per ticker
go run . review KO ok 0 "soft drinks only"   # manual business verdict, PCT = non-permissible revenue %
go test ./...
```

Ingestion is incremental. Prices are fetched from the last stored date, starting at 2020-01-01. EDGAR responses are cached in `data/cache` for 24h. ETFs have no EDGAR facts, so SPUS/HLAL log `edgar: skipped`.

## Data

- `prices`: raw daily OHLC with `div_cash` and `split_factor` events (no adjusted columns)
- `companies`: ticker, CIK, SIC code (input for the business-activity screen)
- `facts`: every XBRL fact from companyfacts (the screen picks concepts from here)

## Sharia screen

Source: S&P Shariah Indices Methodology, October 2026 edition (accounting screens changed 2026-09-18).

- Debt / 36-month avg market value of equity < 30%
- (Cash & deposits + interest-bearing securities) / same < 30%
- (Non-permissible revenue + all interest income) / (revenue + interest income) < 5%
- Buffer: a compliant stock within 2pp over the limit stays compliant until 3 consecutive breaches; a non-compliant stock within 2pp under the limit needs 3 consecutive passes
- Purification ratio: dividends x (non-permissible revenue + interest + non-operating income) / total income

Our choices (not in the methodology):

- Evaluated at every month-end from the 36th month of price history (S&P evaluates monthly); the current month is provisional. The current rules are applied to all history, including before 2026-09-18.
- Only filings already filed on the evaluation date are used (no lookahead). Shares are split-adjusted.
- XBRL concept mapping for debt, cash, securities and interest income is in `screen.go` (`totalDebt`, `cashAndSecurities`, `interestConcepts`).
- Business activity: SIC code gives `ok` / `excluded` / `review`. Non-permissible revenue (alcohol, pork, etc.) is not in XBRL, so it comes from `suspt review`. A ticker with no review counts it as 0.
- Known gaps: some issuers (AAPL, MSFT) don't tag interest income, so the NPI ratio excludes it (shown in NOTE). XOM's SEC ticker now points to a 2026 holding company with little history.
- AAOIFI and Dow Jones Islamic reference columns: pending the source documents.

## Build order

1. ~~Data ingestion~~
2. ~~Sharia screen + tests~~ (S&P governing; reference standards pending)
3. Paper portfolio + order engine
4. Journal + benchmark
5. Signals
6. Dashboard + charts
