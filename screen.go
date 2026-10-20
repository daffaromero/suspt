package main

import (
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// S&P Shariah Indices Methodology, October 2026 edition, and Dow Jones Islamic Market
// Indices Methodology, September 2026 edition. Both changed their accounting screens on
// 2026-09-18 (after close); see accountingRule.
const (
	spBuffer    = 0.02 // two percentage points
	spStreak    = 3    // consecutive evaluation periods inside the buffer before status flips
	spMaxNPI    = 0.05 // (non-permissible + all interest income) / total business income
	spAvgMonths = 36

	// Dow Jones Islamic Market Indices Methodology, September 2026 edition: the same
	// limits, buffer and income test, with market cap averaged over 24 months. Shown for
	// reference; S&P decides what can be bought.
	djAvgMonths = 24

	// AAOIFI Shari'ah Standard No. 21, clauses 3/4/2 to 3/4/5 as quoted by secondary
	// sources (AAOIFI's own text is members-only, so this is unverified): interest-bearing
	// debt and interest-taking deposits each "not exceed 30%" of market capitalisation
	// (current, no averaging), prohibited income not over 5% of total income, from the
	// latest financial statements. No buffer. Shown for comparison only.
	aaMaxRatio = 0.30
	aaMaxNPI   = 0.05
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
	DJDebt, DJCash         float64 // against the 24-month average, for the Dow Jones reference
	AADebt, AACash         float64 // against the current market cap, for the AAOIFI reference
	Notes                  []string
}

// evaluate computes the S&P ratios on months[i].Date using only facts filed by then.
// npFrac is the manually reviewed non-permissible share of revenue.
func evaluate(all []sfact, months, splits []bar, i int, npFrac float64) (ratios, bool) {
	d := months[i].Date
	b := book(all).known(d)
	r := ratios{Debt: math.NaN(), Cash: math.NaN(), NPI: math.NaN(), Purif: math.NaN(), DJDebt: math.NaN(), DJCash: math.NaN(), AADebt: math.NaN(), AACash: math.NaN()}

	// Monthly market caps, newest last; S&P averages 36 of them, Dow Jones 24.
	var caps []float64
	for _, m := range months[max(0, i-spAvgMonths+1) : i+1] {
		if sh, end, ok := b.sharesAt(m.Date); ok {
			caps = append(caps, m.Close*sh*splitFactor(splits, end, m.Date))
		}
	}
	avg := func(xs []float64) float64 {
		s := 0.0
		for _, x := range xs {
			s += x
		}
		return s / float64(len(xs))
	}
	n := len(caps)
	bd := b.latestEnd([]string{"Assets"}, false)
	if n == 0 || bd == "" {
		r.Notes = append(r.Notes, "no shares or balance sheet filed yet")
		return r, false
	}
	if n < spAvgMonths {
		r.Notes = append(r.Notes, fmt.Sprintf("market cap avg over %d/%d months", n, spAvgMonths))
	}
	mve, djMVE := avg(caps), avg(caps[max(0, n-djAvgMonths):])

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
	r.DJDebt, r.DJCash = debt/djMVE, cash/djMVE
	r.AADebt, r.AACash = debt/caps[n-1], cash/caps[n-1]

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
// accountingRule is the ratio limit in force on an evaluation date and whether the cash
// test applies. Both change logs give the rule before 2026-09-18 as debt alone under 33%.
// ASSUMPTION: that rule is used for all earlier history; versions before it (e.g. S&P's
// 2023 cash and receivables tests) are not modelled.
func accountingRule(date string) (limit float64, cashTested bool) {
	if date > "2026-09-18" {
		return 0.30, true
	}
	return 0.33, false
}

// worstRatio is the ratio that counts against the limit on date.
func worstRatio(date string, debt, cash float64) (worst, limit float64) {
	limit, cashTested := accountingRule(date)
	if cashTested {
		return max(debt, cash), limit
	}
	return debt, limit
}

func nextState(prev screenState, worst, spMaxRatio float64) screenState {
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
	Streak      int
	Provisional bool // current month: shown, but never gates fills until the month is final
	DJStatus    string
	AAStatus    string
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
	var verdict, reviewedOn sql.NullString
	var npFrac float64
	err = db.QueryRow(`SELECT verdict, nonpermissible_revenue, reviewed_on FROM business_reviews WHERE ticker = ?`, ticker).Scan(&verdict, &npFrac, &reviewedOn)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
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

	var st, dj screenState
	var rows []screenRow
	// SPUS membership stands in for S&P's qualitative business review (done by Ratings
	// Intelligence), which a SIC code can't see: e.g. AMZN and META pass on SIC and ratios
	// but are not held by SPUS.
	var spusAsOf, member string
	db.QueryRow(`SELECT COALESCE(MAX(as_of), '') FROM index_members WHERE fund = 'SPUS'`).Scan(&spusAsOf)
	db.QueryRow(`SELECT as_of FROM index_members WHERE fund = 'SPUS' AND ticker = ?`, ticker).Scan(&member)

	for i := min(spAvgMonths-1, len(months)-1); i < len(months); i++ {
		// Membership and manual verdicts only apply from the day they were recorded, so they
		// can't rewrite past fills. The provisional current month always shows them.
		current := i == len(months)-1
		business, np, note := sicVerdict(sic), 0.0, ""
		if business == "review" {
			note = fmt.Sprintf("its industry code (%s, %s) can't tell whether the business is halal: record a verdict", sic, sicDesc)
		}
		if business == "ok" && spusAsOf != "" && member == "" && (spusAsOf <= months[i].Date || current) {
			business, note = "review", "not held by SPUS, so S&P's own review excludes it: check its business and record a verdict"
		}
		if verdict.Valid && (reviewedOn.String <= months[i].Date || current) {
			business, np, note = verdict.String, npFrac, ""
		}
		r, ok := evaluate(facts, months, splits, i, np)
		row := screenRow{AsOf: months[i].Date, Business: business, ratios: r, Status: "NO_DATA",
			Provisional: current && months[i].Date[:7] >= today(db)[:7]}
		if note != "" {
			row.Notes = append(row.Notes, note)
		}
		if ok {
			worst, limit := worstRatio(months[i].Date, r.Debt, r.Cash)
			st = nextState(st, worst, limit)
			row.Status, row.Streak = status(business, r, st), st.Streak
			// Same business verdict and income test; only the averaging window differs.
			worst, limit = worstRatio(months[i].Date, r.DJDebt, r.DJCash)
			dj = nextState(dj, worst, limit)
			row.DJStatus = status(business, r, dj)
			row.AAStatus = aaoifiStatus(business, r)
			if current && member != "" && business != "excluded" && row.Status == "NON_COMPLIANT" {
				row.Notes = append(row.Notes, "S&P's own fund (SPUS) holds it: its data puts the ratios slightly lower than ours near the limit")
			}
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
		if _, err := tx.Exec(`INSERT INTO screens (ticker, as_of, status, business, debt_ratio, cash_ratio, npi_ratio, purification_ratio, streak, note, provisional, dj_status, dj_debt_ratio, dj_cash_ratio, aa_status, aa_debt_ratio, aa_cash_ratio)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, NULLIF(?, ''), ?, ?)`,
			ticker, r.AsOf, r.Status, r.Business, nullable(r.Debt), nullable(r.Cash), nullable(r.NPI), nullable(r.Purif), r.Streak, strings.Join(r.Notes, "; "), r.Provisional,
			r.DJStatus, nullable(r.DJDebt), nullable(r.DJCash), r.AAStatus, nullable(r.AADebt), nullable(r.AACash)); err != nil {
			return nil, err
		}
	}
	return rows, tx.Commit()
}

// aaoifiStatus applies the quoted SS21 limits with no buffer: each evaluation stands alone.
func aaoifiStatus(business string, r ratios) string {
	switch {
	case business == "excluded", r.NPI > aaMaxNPI, r.AADebt > aaMaxRatio, r.AACash > aaMaxRatio:
		return "NON_COMPLIANT"
	case business == "review", math.IsNaN(r.NPI):
		return "REVIEW"
	}
	return "COMPLIANT"
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

func runScreen(db *sql.DB, tickers []string) error { return screenReport(db, tickers, false) }

// screenReport screens tickers (default: every ingested company); brief prints counts
// and only the tickers that need attention.
func screenReport(db *sql.DB, tickers []string, brief bool) error {
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
	fmt.Fprintln(w, "TICKER\tSTATUS\tDOW JONES\tAAOIFI\tDEBT\tCASH\tNPI\tPURIFY\tBUSINESS\tAS OF\tNOTE")
	counts := map[string]int{}
	for _, t := range tickers {
		t = strings.ToUpper(t)
		rows, err := screenTicker(db, t)
		if err != nil {
			counts["ERROR"]++
			fmt.Fprintf(w, "%s\tERROR\t\t\t\t\t\t\t\t\t%v\n", t, err)
			continue
		}
		r := rows[len(rows)-1]
		counts[r.Status]++
		if brief && r.Status != "REVIEW" && r.Status != "NO_DATA" {
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t, r.Status, orDefault(r.DJStatus, "-"), orDefault(r.AAStatus, "-"), pct(r.Debt), pct(r.Cash), pct(r.NPI), pct(r.Purif), r.Business, r.AsOf, strings.Join(r.Notes, "; "))
	}
	if brief {
		fmt.Fprintf(w, "\nscreened %d: %d compliant, %d non-compliant, %d review, %d no data, %d errors (full table: suspt screen)\n",
			len(tickers), counts["COMPLIANT"], counts["NON_COMPLIANT"], counts["REVIEW"], counts["NO_DATA"], counts["ERROR"])
		return w.Flush()
	}
	fmt.Fprintf(w, "\nS&P Shariah (Oct 2026): debt and cash each < 30%% of 36-month avg market cap (debt < 33%% before 2026-09-18), NPI < %.0f%%. Dow Jones: same, 24-month average.\n", spMaxNPI*100)
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
	return setReview(db, args[0], args[1], pctVal, strings.Join(rest, " "))
}

// setReview records a manual business-activity verdict; pctVal is the non-permissible
// share of revenue in percent. It applies from today (see screenTicker).
func setReview(db *sql.DB, ticker, verdict string, pctVal float64, note string) error {
	ticker = strings.ToUpper(strings.TrimSpace(ticker))
	switch {
	case !tickerRe.MatchString(ticker):
		return fmt.Errorf("%q is not a ticker", ticker)
	case verdict != "ok" && verdict != "excluded":
		return errors.New("verdict must be ok or excluded")
	case !(pctVal >= 0 && pctVal < 100):
		return fmt.Errorf("non-permissible revenue must be a percent from 0 to under 100, got %v", pctVal)
	}
	if _, err := db.Exec(`INSERT OR REPLACE INTO business_reviews (ticker, verdict, nonpermissible_revenue, note, reviewed_on)
		VALUES (?, ?, ?, ?, ?)`, ticker, verdict, pctVal/100, strings.TrimSpace(note), today(db)); err != nil {
		return err
	}
	screenTicker(db, ticker) // best effort: without prices or filings yet, the next refresh screens it
	return nil
}

var spusHoldingsURL = "https://www.sp-funds.com/wp-content/uploads/data/TidalFG_Holdings_SPUS.csv"

var memberTickerRe = regexp.MustCompile(`^[A-Z]{1,5}([./][A-Z])?$`)

// syncSPUS replaces the SPUS member list from the fund's daily holdings file and
// returns the members (Tiingo/SEC style tickers, e.g. BRK-B).
func syncSPUS(db *sql.DB) ([]string, error) {
	req, err := http.NewRequest("GET", spusHoldingsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "suspt/0.1 (+https://github.com/daffaromero/suspt)")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SPUS holdings: %s", resp.Status)
	}
	rows, err := csv.NewReader(io.LimitReader(resp.Body, 5<<20)).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("SPUS holdings: %w", err)
	}
	if len(rows) < 2 {
		return nil, errors.New("SPUS holdings: empty file")
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[strings.TrimSpace(h)] = i
	}
	for _, h := range []string{"Date", "StockTicker", "Weightings"} {
		if _, ok := col[h]; !ok {
			return nil, fmt.Errorf("SPUS holdings: column %q missing, the file format changed", h)
		}
	}
	day, err := time.Parse("1/2/2006", strings.TrimSpace(rows[1][col["Date"]]))
	if err != nil {
		return nil, fmt.Errorf("SPUS holdings: bad date: %w", err)
	}
	asOf := day.Format(time.DateOnly)
	var members []string
	weights := map[string]float64{}
	for _, r := range rows[1:] {
		t := strings.TrimSpace(r[col["StockTicker"]])
		if !memberTickerRe.MatchString(t) { // skips "Cash&Other" and CUSIP placeholders
			continue
		}
		t = strings.NewReplacer(".", "-", "/", "-").Replace(t)
		w, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(r[col["Weightings"]]), "%"), 64)
		members, weights[t] = append(members, t), w/100
	}
	if len(members) < 50 {
		return nil, fmt.Errorf("SPUS holdings: only %d tickers parsed, refusing to replace the member list", len(members))
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM index_members WHERE fund = 'SPUS'`); err != nil {
		return nil, err
	}
	for _, t := range members {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO index_members (ticker, fund, as_of, weight) VALUES (?, 'SPUS', ?, ?)`, t, asOf, weights[t]); err != nil {
			return nil, err
		}
	}
	return members, tx.Commit()
}

// runSyncSPUS refreshes SPUS membership and adds every member to the watchlist.
func runSyncSPUS(db *sql.DB) error {
	before, err := watchlistTickers(db)
	if err != nil {
		return err
	}
	members, err := syncSPUS(db)
	if err != nil {
		return err
	}
	added := 0
	for _, t := range members {
		res, err := db.Exec(`INSERT OR IGNORE INTO watchlist (ticker, added_on) VALUES (?, ?)`, t, today(db))
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		added += int(n)
	}
	var gone []string
	for _, t := range before {
		if !slices.Contains(members, t) {
			gone = append(gone, t)
		}
	}
	fmt.Printf("SPUS holds %d stocks; %d added to the watchlist\n", len(members), added)
	if len(gone) > 0 {
		fmt.Printf("on your watchlist but not in SPUS (kept; screen shows them as REVIEW): %s\n", strings.Join(gone, " "))
	}
	fmt.Println("run `suspt update` to fetch their data (Tiingo's 50 requests/hour makes the first run take a few hours)")
	return nil
}
