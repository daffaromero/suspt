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
go test ./...
```

Ingestion is incremental. Prices are fetched from the last stored date, starting at 2020-01-01. EDGAR responses are cached in `data/cache` for 24h. ETFs have no EDGAR facts, so SPUS/HLAL log `edgar: skipped`.

## Data

- `prices`: raw daily OHLC with `div_cash` and `split_factor` events (no adjusted columns)
- `companies`: ticker, CIK, SIC code (input for the business-activity screen)
- `facts`: every XBRL fact from companyfacts (the screen picks concepts from here)

## Build order

1. ~~Data ingestion~~
2. Sharia screen + tests
3. Paper portfolio + order engine
4. Journal + benchmark
5. Signals
6. Dashboard + charts
