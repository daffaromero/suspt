package main

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// S&P Shariah Indices Methodology, October 2026 edition. The accounting screens
// changed on 2026-09-18 (33% -> 30%, receivables test dropped); the current
// rules are applied to all history.
const (
	spMaxRatio  = 0.30 // debt and cash, each over 36-month average market value of equity
	spBuffer    = 0.02 // two percentage points
	spStreak    = 3    // consecutive evaluation periods inside the buffer before status flips
	spMaxNPI    = 0.05 // (non-permissible + all interest income) / total business income
	spAvgMonths = 36
)

// The methodology does not define these line items in XBRL terms. Mapping to
// us-gaap concepts is ours; see totalDebt and cashAndSecurities for the order.
var (
	revenueConcepts  = []string{"Revenues", "RevenueFromContractWithCustomerExcludingAssessedTax", "RevenueFromContractWithCustomerIncludingAssessedTax", "SalesRevenueNet"}
	interestConcepts = []string{"InterestAndDividendIncomeOperating", "InterestIncomeOperating", "InvestmentIncomeInterestAndDividend", "InvestmentIncomeInterest", "InterestIncomeOther", "InterestIncomeNonoperating"}
	screenConcepts   = append(append([]string{
		"Assets",
		"DebtLongtermAndShorttermCombinedAmount", "ShortTermBorrowings", "CommercialPaper", "OtherShortTermBorrowings",
		"LongTermDebt", "LongTermDebtNoncurrent", "LongTermDebtAndCapitalLeaseObligations", "DebtCurrent",
		"LongTermDebtCurrent", "LongTermDebtAndCapitalLeaseObligationsCurrent",
		"CashAndCashEquivalentsAtCarryingValue", "CashCashEquivalentsRestrictedCashAndRestrictedCashEquivalents",
		"AvailableForSaleSecuritiesDebtSecurities", "AvailableForSaleSecuritiesDebtSecuritiesCurrent", "AvailableForSaleSecuritiesDebtSecuritiesNoncurrent",
		"MarketableSecuritiesCurrent", "MarketableSecuritiesNoncurrent", "ShortTermInvestments",
		"EntityCommonStockSharesOutstanding", "CommonStockSharesOutstanding", "WeightedAverageNumberOfSharesOutstandingBasic",
		"OtherNonoperatingIncomeExpense",
	}, revenueConcepts...), interestConcepts...)
)

// sicVerdict is a first pass from the SEC SIC code. "review" means the code is
// too coarse to decide (beverages covers soft drinks and alcohol); record a
// manual verdict with `suspt review`.
func sicVerdict(sic string) string {
	n, err := strconv.Atoi(sic)
	if err != nil {
		return "review"
	}
	switch {
	case n >= 6000 && n <= 6499, n == 6770: // banks, credit, brokers, insurance, blank checks
		return "excluded"
	case n == 2082, n == 5180: // brewers, alcohol wholesale
		return "excluded"
	case n == 2100, n == 2111: // tobacco
		return "excluded"
	case n == 3652, n == 5735, n == 7830: // recorded music, record stores, cinemas
		return "excluded"
	case n == 2080, // beverages: soft drinks or alcohol
		n >= 2011 && n <= 2015, // meat packing: may include pork
		n == 2721, n == 2741,   // periodicals, misc publishing
		n >= 4832 && n <= 4841, // broadcasting, cable
		n == 5810, n == 5812,   // restaurants: alcohol, pork
		n == 6798,            // REITs: tenants and financing vary
		n == 7000, n == 7011, // hotels: casinos, alcohol
		n >= 7812 && n <= 7841,          // film production, distribution, streaming
		n == 7900, n == 7948, n == 7990: // amusement, racing, gambling
		return "review"
	}
	return "ok"
}

type sfact struct {
	Concept, Start, End, Filed, FP string
	Val                            float64
}

func isAnnual(f sfact) bool {
	if f.Start == "" || f.FP != "FY" {
		return false
	}
	s, err1 := time.Parse(time.DateOnly, f.Start)
	e, err2 := time.Parse(time.DateOnly, f.End)
	days := e.Sub(s).Hours() / 24
	return err1 == nil && err2 == nil && days >= 350 && days <= 380
}

// book is the set of facts public on an evaluation date: nothing filed later is visible.
type book []sfact

func (b book) known(date string) book {
	var out book
	for _, f := range b {
		if f.Filed <= date {
			out = append(out, f)
		}
	}
	return out
}

// value returns the latest-filed value of concept for period end (instant, or full fiscal year).
func (b book) value(concept, end string, annual bool) (float64, bool) {
	var best sfact
	ok := false
	for _, f := range b {
		kind := f.Start == ""
		if annual {
			kind = isAnnual(f)
		}
		if f.Concept == concept && f.End == end && kind && (!ok || f.Filed > best.Filed) {
			best, ok = f, true
		}
	}
	return best.Val, ok
}

func (b book) latestEnd(concepts []string, annual bool) string {
	end := ""
	for _, f := range b {
		kind := f.Start == ""
		if annual {
			kind = isAnnual(f)
		}
		if kind && f.End > end && slices.Contains(concepts, f.Concept) {
			end = f.End
		}
	}
	return end
}

// sharesAt returns shares outstanding last reported on or before date, preferring
// point-in-time counts over weighted averages (multi-class issuers like META only
// tag the latter undimensioned).
func (b book) sharesAt(date string) (val float64, end string, ok bool) {
	for _, concepts := range [][]string{{"EntityCommonStockSharesOutstanding", "CommonStockSharesOutstanding"}, {"WeightedAverageNumberOfSharesOutstandingBasic"}} {
		var best sfact
		for _, f := range b {
			if slices.Contains(concepts, f.Concept) && f.End <= date && (!ok || f.End > best.End || f.End == best.End && f.Filed > best.Filed) {
				best, ok = f, true
			}
		}
		if ok {
			return best.Val, best.End, true
		}
	}
	return 0, "", false
}

type getter func(concept string) (float64, bool)

func firstOf(get getter, concepts ...string) (float64, bool) {
	for _, c := range concepts {
		if v, ok := get(c); ok {
			return v, true
		}
	}
	return 0, false
}

func sumOf(get getter, concepts ...string) (float64, bool) {
	var sum float64
	any := false
	for _, c := range concepts {
		if v, ok := get(c); ok {
			sum, any = sum+v, true
		}
	}
	return sum, any
}

// totalDebt is short-term borrowings plus long-term debt including current maturities.
func totalDebt(get getter) (float64, bool) {
	if v, ok := get("DebtLongtermAndShorttermCombinedAmount"); ok {
		return v, true
	}
	short, ok := get("ShortTermBorrowings")
	if !ok {
		short, ok = sumOf(get, "CommercialPaper", "OtherShortTermBorrowings")
	}
	if v, ok := get("LongTermDebt"); ok { // already includes current maturities
		return v + short, true
	}
	nonCur, found := firstOf(get, "LongTermDebtNoncurrent", "LongTermDebtAndCapitalLeaseObligations")
	debtCur, curFound := get("DebtCurrent") // current maturities plus short-term borrowings
	if !found {
		if curFound {
			return debtCur, true
		}
		return short, ok
	}
	if curFound {
		return nonCur + debtCur, true
	}
	cur, _ := firstOf(get, "LongTermDebtCurrent", "LongTermDebtAndCapitalLeaseObligationsCurrent")
	return nonCur + cur + short, true
}

// cashAndSecurities is cash and deposits plus interest-bearing securities.
// Debt securities are preferred over "marketable securities", which can include equities.
func cashAndSecurities(get getter) (float64, bool) {
	cash, ok := firstOf(get, "CashAndCashEquivalentsAtCarryingValue", "CashCashEquivalentsRestrictedCashAndRestrictedCashEquivalents")
	sec, found := get("AvailableForSaleSecuritiesDebtSecurities")
	if !found {
		sec, found = sumOf(get, "AvailableForSaleSecuritiesDebtSecuritiesCurrent", "AvailableForSaleSecuritiesDebtSecuritiesNoncurrent")
	}
	if !found {
		sec, found = sumOf(get, "MarketableSecuritiesCurrent", "MarketableSecuritiesNoncurrent")
	}
	if !found {
		sec, found = get("ShortTermInvestments")
	}
	return cash + sec, ok || found
}

type bar struct {
	Date         string
	Close, Split float64
}

// monthEnds returns the last bar of each calendar month; the current month's is provisional.
func monthEnds(bars []bar) []bar {
	var out []bar
	for _, b := range bars {
		if n := len(out); n > 0 && out[n-1].Date[:7] == b.Date[:7] {
			out[n-1] = b
		} else {
			out = append(out, b)
		}
	}
	return out
}

// splitFactor is the cumulative split ratio after date `after` up to and including `upto`.
func splitFactor(splits []bar, after, upto string) float64 {
	f := 1.0
	for _, s := range splits {
		if s.Date > after && s.Date <= upto {
			f *= s.Split
		}
	}
	return f
}

type ratios struct {
	Debt, Cash, NPI, Purif float64 // NaN when unknown
	Notes                  []string
}

// evaluate computes the S&P ratios on months[i].Date using only facts filed by then.
// npFrac is the manually reviewed non-permissible share of revenue.
func evaluate(all []sfact, months, splits []bar, i int, npFrac float64) (ratios, bool) {
	d := months[i].Date
	b := book(all).known(d)
	r := ratios{Debt: math.NaN(), Cash: math.NaN(), NPI: math.NaN(), Purif: math.NaN()}

	var sum float64
	n := 0
	for _, m := range months[max(0, i-spAvgMonths+1) : i+1] {
		if sh, end, ok := b.sharesAt(m.Date); ok {
			sum += m.Close * sh * splitFactor(splits, end, m.Date)
			n++
		}
	}
	bd := b.latestEnd([]string{"Assets"}, false)
	if n == 0 || bd == "" {
		r.Notes = append(r.Notes, "no shares or balance sheet filed yet")
		return r, false
	}
	if n < spAvgMonths {
		r.Notes = append(r.Notes, fmt.Sprintf("market cap avg over %d/%d months", n, spAvgMonths))
	}
	mve := sum / float64(n)

	get := func(c string) (float64, bool) { return b.value(c, bd, false) }
	debt, ok := totalDebt(get)
	if !ok {
		r.Notes = append(r.Notes, "no debt tagged (assumed 0)")
	}
	cash, ok := cashAndSecurities(get)
	if !ok {
		r.Notes = append(r.Notes, "no cash tagged (assumed 0)")
	}
	r.Debt, r.Cash = debt/mve, cash/mve

	fy := b.latestEnd(revenueConcepts, true)
	if fy == "" {
		r.Notes = append(r.Notes, "no annual revenue filed")
		return r, true
	}
	aget := func(c string) (float64, bool) { return b.value(c, fy, true) }
	rev, _ := firstOf(aget, revenueConcepts...)
	interest, ok := firstOf(aget, interestConcepts...)
	if !ok {
		r.Notes = append(r.Notes, "interest income untagged (NPI excludes it)")
	}
	nonop, _ := aget("OtherNonoperatingIncomeExpense")
	nonop = max(nonop, 0)
	np := npFrac*rev + interest
	r.NPI = np / (rev + interest)
	// Purification also counts non-operating income as impure (methodology footnotes 3 and 4).
	r.Purif = (np + nonop) / (rev + interest + nonop)
	return r, true
}

type screenState struct {
	Known, Compliant bool
	Streak           int // consecutive periods inside the buffer
}

// nextState applies the S&P buffer to the worst accounting ratio: status flips on a
// clear breach or clear pass, or after spStreak consecutive periods inside the buffer.
func nextState(prev screenState, worst float64) screenState {
	switch {
	case !prev.Known:
		return screenState{Known: true, Compliant: worst < spMaxRatio}
	case prev.Compliant && worst < spMaxRatio:
		return screenState{Known: true, Compliant: true}
	case prev.Compliant && worst > spMaxRatio+spBuffer:
		return screenState{Known: true}
	case prev.Compliant:
		if prev.Streak+1 >= spStreak {
			return screenState{Known: true}
		}
		return screenState{Known: true, Compliant: true, Streak: prev.Streak + 1}
	case worst >= spMaxRatio:
		return screenState{Known: true}
	case worst < spMaxRatio-spBuffer:
		return screenState{Known: true, Compliant: true}
	default:
		if prev.Streak+1 >= spStreak {
			return screenState{Known: true, Compliant: true}
		}
		return screenState{Known: true, Streak: prev.Streak + 1}
	}
}

type screenRow struct {
	AsOf, Status, Business string
	ratios
	Streak int
}

func screenTicker(db *sql.DB, ticker string) ([]screenRow, error) {
	var cik int
	var sic, sicDesc string
	err := db.QueryRow(`SELECT cik, COALESCE(sic, ''), COALESCE(sic_desc, '') FROM companies WHERE ticker = ?`, ticker).Scan(&cik, &sic, &sicDesc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFiler
	} else if err != nil {
		return nil, err
	}
	business := sicVerdict(sic)
	var verdict sql.NullString
	var npFrac float64
	err = db.QueryRow(`SELECT verdict, nonpermissible_revenue FROM business_reviews WHERE ticker = ?`, ticker).Scan(&verdict, &npFrac)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if verdict.Valid {
		business = verdict.String
	}

	args := []any{cik}
	for _, c := range screenConcepts {
		args = append(args, c)
	}
	rs, err := db.Query(`SELECT concept, COALESCE(start, ''), end, COALESCE(filed, ''), COALESCE(fp, ''), val FROM facts
		WHERE cik = ? AND form IN ('10-K', '10-K/A', '10-Q', '10-Q/A')
		AND concept IN (?`+strings.Repeat(", ?", len(screenConcepts)-1)+`)`, args...)
	if err != nil {
		return nil, err
	}
	var facts []sfact
	for rs.Next() {
		var f sfact
		if err := rs.Scan(&f.Concept, &f.Start, &f.End, &f.Filed, &f.FP, &f.Val); err != nil {
			rs.Close()
			return nil, err
		}
		facts = append(facts, f)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}

	rs, err = db.Query(`SELECT date, close, split_factor FROM prices WHERE ticker = ? ORDER BY date`, ticker)
	if err != nil {
		return nil, err
	}
	var bars, splits []bar
	for rs.Next() {
		var b bar
		if err := rs.Scan(&b.Date, &b.Close, &b.Split); err != nil {
			rs.Close()
			return nil, err
		}
		bars = append(bars, b)
		if b.Split != 1 {
			splits = append(splits, b)
		}
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	months := monthEnds(bars)
	if len(months) == 0 {
		return nil, fmt.Errorf("no prices, run ingest first")
	}

	var st screenState
	var rows []screenRow
	for i := min(spAvgMonths-1, len(months)-1); i < len(months); i++ {
		r, ok := evaluate(facts, months, splits, i, npFrac)
		row := screenRow{AsOf: months[i].Date, Business: business, ratios: r, Status: "NO_DATA"}
		if business == "review" && !verdict.Valid {
			row.Notes = append(row.Notes, fmt.Sprintf("SIC %s %s: record a verdict with `suspt review`", sic, sicDesc))
		}
		if ok {
			st = nextState(st, max(r.Debt, r.Cash))
			row.Status, row.Streak = status(business, r, st), st.Streak
		}
		rows = append(rows, row)
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM screens WHERE ticker = ?`, ticker); err != nil {
		return nil, err
	}
	for _, r := range rows {
		if _, err := tx.Exec(`INSERT INTO screens (ticker, as_of, status, business, debt_ratio, cash_ratio, npi_ratio, purification_ratio, streak, note)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ticker, r.AsOf, r.Status, r.Business, nullable(r.Debt), nullable(r.Cash), nullable(r.NPI), nullable(r.Purif), r.Streak, strings.Join(r.Notes, "; ")); err != nil {
			return nil, err
		}
	}
	return rows, tx.Commit()
}

func status(business string, r ratios, st screenState) string {
	switch {
	case business == "excluded", r.NPI >= spMaxNPI, !st.Compliant:
		return "NON_COMPLIANT"
	case business == "review", math.IsNaN(r.NPI):
		return "REVIEW"
	}
	return "COMPLIANT"
}

func nullable(v float64) any {
	if math.IsNaN(v) {
		return nil
	}
	return v
}

func pct(v float64) string {
	if math.IsNaN(v) {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", v*100)
}

func runScreen(db *sql.DB, tickers []string) error {
	if len(tickers) == 0 {
		rs, err := db.Query(`SELECT ticker FROM companies ORDER BY ticker`)
		if err != nil {
			return err
		}
		for rs.Next() {
			var t string
			if err := rs.Scan(&t); err != nil {
				rs.Close()
				return err
			}
			tickers = append(tickers, t)
		}
		rs.Close()
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TICKER\tSTATUS\tDEBT\tCASH\tNPI\tPURIFY\tBUSINESS\tAS OF\tNOTE")
	for _, t := range tickers {
		t = strings.ToUpper(t)
		rows, err := screenTicker(db, t)
		if err != nil {
			fmt.Fprintf(w, "%s\tERROR\t\t\t\t\t\t\t%v\n", t, err)
			continue
		}
		r := rows[len(rows)-1]
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t, r.Status, pct(r.Debt), pct(r.Cash), pct(r.NPI), pct(r.Purif), r.Business, r.AsOf, strings.Join(r.Notes, "; "))
	}
	fmt.Fprintf(w, "\nS&P Shariah (Oct 2026): debt and cash each < %.0f%% of 36-month avg market cap, NPI < %.0f%%.\n", spMaxRatio*100, spMaxNPI*100)
	return w.Flush()
}

func runReview(db *sql.DB, args []string) error {
	if len(args) < 2 || (args[1] != "ok" && args[1] != "excluded") {
		return errors.New("usage: suspt review TICKER ok|excluded [NONPERMISSIBLE_REVENUE_PCT] [NOTE...]")
	}
	pctVal, rest := 0.0, args[2:]
	if len(rest) > 0 {
		if v, err := strconv.ParseFloat(rest[0], 64); err == nil {
			pctVal, rest = v, rest[1:]
		}
	}
	if pctVal < 0 || pctVal >= 100 {
		return fmt.Errorf("non-permissible revenue must be a percent in [0, 100), got %v", pctVal)
	}
	_, err := db.Exec(`INSERT OR REPLACE INTO business_reviews (ticker, verdict, nonpermissible_revenue, note, reviewed_on)
		VALUES (?, ?, ?, ?, date('now'))`, strings.ToUpper(args[0]), args[1], pctVal/100, strings.Join(rest, " "))
	return err
}
