'use strict';
// Read-only dashboard over the suspt JSON API. Every user string goes through esc().

const LC = window.LightweightCharts;
const $ = (s, el = document) => el.querySelector(s);
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);
const css = (v) => getComputedStyle(document.documentElement).getPropertyValue(v).trim();
const NA = 'n/a';

const money = (v, sign = false) => {
  if (v == null || Number.isNaN(v)) return NA;
  const s = Math.abs(v).toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  return (v < 0 ? '-' : sign && v > 0 ? '+' : '') + '$' + s;
};
const pct = (v, digits = 1, sign = true) => (v == null || Number.isNaN(v) ? NA : (sign && v > 0 ? '+' : '') + (v * 100).toFixed(digits) + '%');
const num = (v, d = 2) => (v == null ? NA : v.toLocaleString('en-US', { maximumFractionDigits: d }));
const dir = (v) => (v > 0 ? 'up' : v < 0 ? 'down' : '');

const STATUS = {
  COMPLIANT: ['✓', 'compliant'], NON_COMPLIANT: ['✕', 'non-compliant'], REVIEW: ['?', 'review'],
  NO_DATA: ['·', 'no data'], NO_SCREEN: ['·', 'not screened'], FUND: ['◆', 'sharia fund'],
  filled: ['✓', 'filled'], open: ['○', 'open'], rejected: ['✕', 'rejected'], expired: ['·', 'expired'], cancelled: ['·', 'cancelled'],
  BUY: ['▲', 'buy'], WATCH: ['◎', 'watch'], BLOCKED: ['✕', 'blocked'],
};
const badge = (s) => {
  const [i, label] = STATUS[s] || ['·', String(s).toLowerCase()];
  return `<span class="badge ${esc(s)}"><span class="i" aria-hidden="true">${i}</span>${esc(label)}</span>`;
};
const label = (n, text) => `<h2 class="section-label"><span class="num">${n}</span>${esc(text)}</h2>`;
const empty = (html) => `<p class="empty">${html}</p>`;

const VIEWS = ['portfolio', 'watchlist', 'journal', 'signals', 'orders', 'screen'];
const state = { view: 'portfolio', sel: 0, filter: '', charts: [], data: {}, range: '1Y', hits: false };

async function get(path) {
  const r = await fetch(path, { headers: { Accept: 'application/json' } });
  if (!r.ok) throw new Error(`${path}: ${r.status} ${(await r.text()).trim()}`);
  return r.json();
}

/* charts */
function baseChart(el) {
  const c = LC.createChart(el, {
    width: el.clientWidth, height: el.clientHeight,
    layout: { background: { type: 'solid', color: 'transparent' }, textColor: css('--muted'), fontFamily: css('--mono'), fontSize: 11 },
    grid: { vertLines: { visible: false }, horzLines: { color: css('--grid') } },
    rightPriceScale: { borderVisible: false },
    timeScale: { borderVisible: false },
    crosshair: {
      mode: LC.CrosshairMode.Normal,
      vertLine: { color: css('--faint'), labelBackgroundColor: css('--surface') },
      horzLine: { color: css('--faint'), labelBackgroundColor: css('--surface') },
    },
  });
  const ro = new ResizeObserver(() => c.resize(el.clientWidth, el.clientHeight));
  ro.observe(el);
  state.charts.push({ c, ro });
  return c;
}
function clearCharts() {
  for (const { c, ro } of state.charts) { ro.disconnect(); c.remove(); }
  state.charts = [];
}
const lineOpts = (color, extra = {}) => ({ color, lineWidth: 2, priceLineVisible: false, lastValueVisible: false, crosshairMarkerRadius: 4, ...extra });

function setRange(chart, data, range) {
  if (!data.length) return;
  if (range === 'ALL') { chart.timeScale().fitContent(); return; }
  const months = { '3M': 3, '6M': 6, '1Y': 12, '3Y': 36 }[range];
  const end = new Date(data[data.length - 1].time);
  const from = new Date(end); from.setMonth(from.getMonth() - months);
  chart.timeScale().setVisibleRange({ from: from.toISOString().slice(0, 10), to: data[data.length - 1].time });
}
function rangeButtons(onPick) {
  const wrap = document.createElement('div');
  wrap.className = 'ranges';
  wrap.setAttribute('role', 'group');
  wrap.setAttribute('aria-label', 'Time range');
  for (const r of ['3M', '6M', '1Y', '3Y', 'ALL']) {
    const b = document.createElement('button');
    b.textContent = r.toLowerCase();
    b.setAttribute('aria-pressed', String(r === state.range));
    b.onclick = () => {
      state.range = r;
      wrap.querySelectorAll('button').forEach((x) => x.setAttribute('aria-pressed', String(x === b)));
      onPick(r);
    };
    wrap.append(b);
  }
  return wrap;
}

/* views */
async function viewPortfolio(main) {
  const s = await get('/api/summary');
  if (!s.asOf) {
    main.innerHTML = label('01', 'portfolio') + empty(`No trading days simulated yet. Run <span class="cmd">suspt update</span> after the next market close.`);
    return;
  }
  const vs = s.twr - s.benchTwr;
  main.innerHTML = `
    ${label('01', 'portfolio')}
    <div class="hero">
      <div class="big">${money(s.value)}</div>
      <div class="sub">as of ${esc(s.asOf)} · started ${esc(s.start)} · cash ${money(s.cash)}</div>
    </div>
    <div class="stats">
      <div class="stat"><div class="k">deposited</div><div class="v">${money(s.contributions)}</div><div class="d">since ${esc(s.start)}</div></div>
      <div class="stat"><div class="k">profit / loss</div><div class="v ${dir(s.pnl)}">${money(s.pnl, true)}</div><div class="d">value minus deposits</div></div>
      <div class="stat"><div class="k">time-weighted return</div><div class="v">${pct(s.twr, 2)}</div><div class="d">deposits don't count as gains</div></div>
      <div class="stat"><div class="k">vs ${esc(s.benchmark)}</div><div class="v ${dir(vs)}">${(vs > 0 ? '+' : '') + (vs * 100).toFixed(2)} pts</div><div class="d">${esc(s.benchmark)} ${pct(s.benchTwr, 2)} · ${money(s.benchValue)}</div></div>
    </div>
    ${label('02', 'value vs benchmark, same deposits on the same dates')}
    <div class="panel">
      <div class="toolbar"><div class="legend" id="eq-legend"></div><span id="eq-ranges"></span></div>
      <div class="chart" id="eq-chart" role="img" aria-label="Portfolio value, benchmark value and deposits over time"></div>
      <details><summary class="fine">show as table (last 20 days)</summary><div class="tbl-wrap" id="eq-table"></div></details>
    </div>
    ${label('03', 'positions')}
    <div class="tbl-wrap" id="positions"></div>
    ${label('04', 'exposure by sector (share of holdings, dashed tick = limit)')}
    <div class="panel"><div class="bars" id="sectors"></div><ul class="flags" id="flags"></ul></div>`;

  const S1 = css('--s1'), S2 = css('--s2'), M = css('--muted');
  const c = baseChart($('#eq-chart'));
  const val = c.addLineSeries(lineOpts(S1, { lastValueVisible: true }));
  const bench = c.addLineSeries(lineOpts(S2, { lastValueVisible: true }));
  const dep = c.addLineSeries(lineOpts(M, { lineWidth: 1, lineStyle: LC.LineStyle.Dashed, lineType: LC.LineType.WithSteps, crosshairMarkerVisible: false }));
  val.setData(s.equity.map((p) => ({ time: p.date, value: p.value })));
  bench.setData(s.equity.map((p) => ({ time: p.date, value: p.bench })));
  dep.setData(s.equity.map((p) => ({ time: p.date, value: p.contrib })));
  const byDate = new Map(s.equity.map((p) => [p.date, p]));
  const legend = (p) => {
    $('#eq-legend').innerHTML = `
      <span><span class="sw" style="border-color:${S1}"></span>portfolio<b>${money(p.value)}</b></span>
      <span><span class="sw" style="border-color:${S2}"></span>${esc(s.benchmark)}<b>${money(p.bench)}</b></span>
      <span><span class="sw dash" style="border-color:${M}"></span>deposited<b>${money(p.contrib)}</b></span>
      <span class="when">${esc(p.date)}</span>`;
  };
  const lastP = s.equity[s.equity.length - 1];
  legend(lastP);
  c.subscribeCrosshairMove((e) => legend((e.time && byDate.get(e.time)) || lastP));
  const pts = s.equity.map((p) => ({ time: p.date }));
  $('#eq-ranges').append(rangeButtons((r) => setRange(c, pts, r)));
  setRange(c, pts, state.range);
  $('#eq-table').innerHTML = `<table><thead><tr><th>date</th><th class="n">portfolio</th><th class="n">${esc(s.benchmark)}</th><th class="n">deposited</th></tr></thead><tbody>${s.equity
    .slice(-20).reverse().map((p) => `<tr><td>${esc(p.date)}</td><td class="n">${money(p.value)}</td><td class="n">${money(p.bench)}</td><td class="n">${money(p.contrib)}</td></tr>`).join('')}</tbody></table>`;

  $('#positions').innerHTML = s.positions.length
    ? `<table><thead><tr><th>ticker</th><th class="n">qty</th><th class="n">cost</th><th class="n">close</th><th class="n">value</th><th class="n">p&amp;l</th><th class="n">weight</th><th>sector</th><th>screen</th></tr></thead><tbody>${s.positions
        .map((p) => `<tr><td><a href="#watchlist/${esc(p.ticker)}">${esc(p.ticker)}</a></td><td class="n">${num(p.qty, 6)}</td><td class="n">${money(p.cost)}</td><td class="n">${money(p.close)}</td><td class="n">${money(p.value)}</td>
          <td class="n ${dir(p.pnl)}">${money(p.pnl, true)}</td><td class="n">${pct(p.weight, 0, false)}</td><td>${esc(p.sector)}</td><td>${badge(p.status)}</td></tr>`).join('')}</tbody></table>`
    : empty(`No positions. Place a paper trade: <span class="cmd">suspt buy NVDA -usd 50 -reason "..." -thesis ai-capex</span>`);

  const maxW = Math.max(0.5, ...s.sectors.map((e) => Math.max(e.weight, e.limit)));
  $('#sectors').innerHTML = s.sectors.length
    ? s.sectors.map((e) => {
        const over = e.sector !== 'Fund' && e.weight > e.limit;
        return `<div class="barrow"><span>${esc(e.sector)}</span>
          <span class="track" title="${esc(e.sector)} ${pct(e.weight, 0, false)} of holdings, limit ${pct(e.limit, 0, false)}">
            <span class="fill ${over ? 'over' : ''}" style="width:${(e.weight / maxW) * 100}%"></span>
            ${e.sector !== 'Fund' ? `<span class="limit" style="left:${(e.limit / maxW) * 100}%"></span>` : ''}
          </span>
          <span class="n">${pct(e.weight, 0, false)}${over ? ' <span class="flag">▲ over</span>' : ''}</span></div>`;
      }).join('')
    : empty('Nothing held.');
  $('#flags').innerHTML = s.flags.map((f) => `<li>${esc(f)}</li>`).join('');
}

async function viewWatchlist(main, ticker) {
  const rows = await get('/api/watchlist');
  state.data.watch = rows;
  if (!rows.length) {
    main.innerHTML = label('01', 'watchlist') + empty(`Empty. Add tickers with <span class="cmd">suspt watch NVDA AAPL MSFT</span>, then <span class="cmd">suspt update</span>.`);
    return;
  }
  if (ticker) {
    const i = rows.findIndex((r) => r.ticker === ticker);
    if (i >= 0) state.sel = i;
  } else if (!rows[state.sel]?.date) {
    state.sel = Math.max(0, rows.findIndex((r) => r.date)); // start on a ticker that has data
  }
  const pending = rows.filter((r) => !r.date).length;
  main.innerHTML = `${label('01', 'watchlist')}
    ${pending ? `<p class="fine">${pending} of ${rows.length} tickers are still waiting for prices. <span class="cmd">suspt update</span> fetches them; Tiingo's free tier allows 50 requests an hour, so a large watchlist fills in over a few hours.</p>` : ''}
    <div class="split">
      <div class="list"><input id="filter" placeholder="/ filter" aria-label="Filter watchlist" value="${esc(state.filter)}"><ul id="wl" role="listbox" aria-label="Watchlist"></ul></div>
      <section id="detail" aria-live="polite"></section>
    </div>`;
  $('#filter').addEventListener('input', (e) => { state.filter = e.target.value; drawList(); });
  drawList();
  await showTicker();
}

function visibleRows() {
  const f = state.filter.trim().toUpperCase();
  return (state.data.watch || []).filter((r) => !f || r.ticker.includes(f) || r.sector.toUpperCase().includes(f) || r.status.includes(f));
}

function drawList() {
  const rows = visibleRows();
  state.sel = Math.min(state.sel, Math.max(0, rows.length - 1));
  $('#wl').innerHTML = rows.map((r, i) => `<li><button role="option" aria-selected="${i === state.sel}" data-i="${i}">
      <span class="t">${esc(r.ticker)}</span>${r.date
        ? `<span class="px">${money(r.close)}</span><span class="chg ${dir(r.change)}">${pct(r.change, 2)}</span>`
        : '<span class="px muted">waiting</span><span></span>'}
      <span class="meta">${badge(r.status)}<span>${esc(r.sector)}${r.held ? ' · held' : ''}${r.signals.map((s) => ` · ${s.blocked ? 'blocked' : s.action.toLowerCase()} ${esc(s.rule)}`).join('')}</span></span>
    </button></li>`).join('') || `<li class="empty" style="padding:12px">no match</li>`;
  $('#wl').querySelectorAll('button').forEach((b) => (b.onclick = () => { state.sel = +b.dataset.i; drawList(); showTicker(); }));
}

function sma(bars, n) {
  const out = [];
  let sum = 0;
  bars.forEach((b, i) => {
    sum += b.close;
    if (i >= n) sum -= bars[i - n].close;
    if (i >= n - 1) out.push({ time: b.time, value: sum / n });
  });
  return out;
}

async function showTicker() {
  const row = visibleRows()[state.sel];
  if (!row) { $('#detail').innerHTML = ''; return; }
  history.replaceState(null, '', `#watchlist/${row.ticker}`);
  $('#path').textContent = `~/watchlist/${row.ticker.toLowerCase()}`;
  clearCharts();
  let d;
  try {
    d = await get(`/api/ticker/${encodeURIComponent(row.ticker)}`);
  } catch (e) {
    $('#detail').innerHTML = `<div class="detail-head"><h1>${esc(row.ticker)}</h1>${badge(row.status)}</div>` + (row.date
      ? `<p class="flag">${esc(e.message)}</p>`
      : empty(`No prices yet. <span class="cmd">suspt update</span> will fetch them; with Tiingo's 50 requests an hour that can take a while for a big watchlist. Press <kbd>r</kbd> to check again.`));
    return;
  }
  const last = d.screens[d.screens.length - 1];
  const months = d.screens.filter((s) => s.status !== 'NO_DATA');
  const ok = months.filter((s) => s.status === 'COMPLIANT').length;
  $('#detail').innerHTML = `
    <div class="detail-head"><h1>${esc(d.ticker)}</h1><span class="name">${esc(d.name)}</span>${badge(d.status)}<span class="muted">${esc(d.sector)}${d.held ? ` · holding ${num(d.held, 6)}` : ''}</span></div>
    <div class="panel">
      <div class="toolbar"><div class="legend" id="px-legend"></div><span id="px-ranges"></span></div>
      <div class="chart tall" id="px-chart" role="img" aria-label="${esc(d.ticker)} daily price with entries, exits, key levels and rule triggers"></div>
      <p class="fine">Prices split-adjusted. ▲ B = buy fill, ▼ S = sell fill, ● = a rule turned true (toggle with <kbd>h</kbd>). Up days hollow, down days filled.</p>
    </div>
    <div class="grid2">
      <div class="panel">
        <h3 class="section-label" style="margin-top:0"><span class="num">s&amp;p</span>screen ${last ? `as of ${esc(last.asOf)}` : ''}</h3>
        ${last ? screenBullets(last) : empty('Not screened (ETFs and non-SEC filers are not screened).')}
        ${months.length ? `<div class="strip" aria-hidden="true">${months.map((s) => `<i class="${esc(s.status)}" title="${esc(s.asOf)} ${esc(s.status)}"></i>`).join('')}</div>
          <p class="fine">monthly history: compliant ${ok} of ${months.length} evaluations (green compliant, red non-compliant, amber review)</p>` : ''}
      </div>
      <div class="panel">
        <h3 class="section-label" style="margin-top:0"><span class="num">lv</span>key levels &amp; signals</h3>
        ${d.levels.length ? `<table><tbody>${d.levels.sort((a, b) => b.price - a.price).map((l) => `<tr><td>${esc(l.name)}</td><td class="n">${money(l.price)}</td><td class="n muted">${pct(l.dist)} away</td></tr>`).join('')}</tbody></table>`
          : empty(`No levels. <span class="cmd">suspt level ${esc(d.ticker)} support 120</span>`)}
        ${row.signals.length ? `<p>${row.signals.map((s) => `${badge(s.blocked ? 'BLOCKED' : s.action)} ${esc(s.rule)} v${s.version} <span class="muted">${esc(s.condition)}</span>`).join('<br>')}</p>` : '<p class="fine">No rule fired on the latest bar.</p>'}
        ${d.fills.length ? `<table><thead><tr><th>fill</th><th class="n">qty</th><th class="n">price</th><th>reason</th></tr></thead><tbody>${d.fills.map((f) => `<tr><td>${esc(f.time)} ${esc(f.side)}</td><td class="n">${num(f.qty, 6)}</td><td class="n">${money(f.price)}</td><td class="wrap">${esc(f.reason)}</td></tr>`).join('')}</tbody></table>` : ''}
      </div>
    </div>`;

  const c = baseChart($('#px-chart'));
  const UP = css('--up'), DOWN = css('--down');
  const candles = c.addCandlestickSeries({ upColor: 'rgba(0,0,0,0)', downColor: DOWN, borderUpColor: UP, borderDownColor: DOWN, wickUpColor: UP, wickDownColor: DOWN, priceLineVisible: false });
  candles.setData(d.bars);
  const S1 = css('--s1'), S2 = css('--s2');
  const s50 = c.addLineSeries(lineOpts(S1, { lineWidth: 1, crosshairMarkerVisible: false }));
  const s200 = c.addLineSeries(lineOpts(S2, { lineWidth: 1, crosshairMarkerVisible: false }));
  s50.setData(sma(d.bars, 50));
  s200.setData(sma(d.bars, 200));
  for (const l of d.levels) {
    candles.createPriceLine({ price: l.price, color: css('--accent'), lineWidth: 1, lineStyle: LC.LineStyle.Dashed, axisLabelVisible: true, title: l.name });
  }
  const markers = [
    ...d.fills.map((f) => (f.side === 'buy'
      ? { time: f.time, position: 'belowBar', shape: 'arrowUp', color: UP, text: 'B' }
      : { time: f.time, position: 'aboveBar', shape: 'arrowDown', color: DOWN, text: 'S' })),
    ...(state.hits ? d.hits.map((h) => ({ time: h.time, position: 'aboveBar', shape: 'circle', color: css('--muted'), size: 0.5 })) : []),
  ].sort((a, b) => (a.time < b.time ? -1 : a.time > b.time ? 1 : 0));
  candles.setMarkers(markers);

  const bars = new Map(d.bars.map((b) => [b.time, b]));
  const avg50 = new Map(sma(d.bars, 50).map((p) => [p.time, p.value]));
  const avg200 = new Map(sma(d.bars, 200).map((p) => [p.time, p.value]));
  const legend = (t) => {
    const b = bars.get(t);
    if (!b) return;
    $('#px-legend').innerHTML = `<span>o <b>${num(b.open)}</b> h <b>${num(b.high)}</b> l <b>${num(b.low)}</b> c <b>${num(b.close)}</b></span>
      <span><span class="sw" style="border-color:${S1}"></span>sma50<b>${num(avg50.get(t))}</b></span>
      <span><span class="sw" style="border-color:${S2}"></span>sma200<b>${num(avg200.get(t))}</b></span><span class="when">${esc(t)}</span>`;
  };
  const lastT = d.bars[d.bars.length - 1].time;
  legend(lastT);
  c.subscribeCrosshairMove((e) => legend(e.time || lastT));
  const hitsBtn = document.createElement('button');
  hitsBtn.textContent = `hits ${d.hits.length}`;
  hitsBtn.title = 'show where rules turned true (h)';
  hitsBtn.setAttribute('aria-pressed', String(state.hits));
  hitsBtn.onclick = () => { state.hits = !state.hits; showTicker(); };
  const ranges = rangeButtons((r) => setRange(c, d.bars, r));
  ranges.prepend(hitsBtn);
  $('#px-ranges').append(ranges);
  setRange(c, d.bars, state.range);
}

function screenBullets(s) {
  const one = (name, v, limit, buffer) => {
    if (v == null) return `<div class="bullet"><span>${name}</span><span class="muted">${NA}</span><span></span></div>`;
    const max = Math.max(limit * 2, v * 1.1);
    const over = v >= limit;
    return `<div class="bullet"><span>${name}</span>
      <span class="track" title="${name} ${pct(v, 1, false)}, limit under ${pct(limit, 0, false)}">
        ${buffer ? `<span class="buffer" style="left:${((limit - buffer) / max) * 100}%;width:${((2 * buffer) / max) * 100}%"></span>` : ''}
        <span class="fill ${over ? 'over' : ''}" style="width:${Math.min(100, (v / max) * 100)}%"></span>
        <span class="limit" style="left:${(limit / max) * 100}%"></span>
      </span><span class="n">${pct(v, 1, false)}${over ? ' ✕' : ''}</span></div>`;
  };
  return `${one('debt', s.debt, 0.3, 0.02)}${one('cash', s.cash, 0.3, 0.02)}${one('impure', s.npi, 0.05, 0)}
    <p class="fine">limit tick at 30% / 5%; amber band = 2pt buffer. Purify ${pct(s.purif, 2, false)} of dividends. Business: ${esc(s.business)}.${s.note ? ' ' + esc(s.note) + '.' : ''}</p>`;
}

async function viewJournal(main) {
  const j = await get('/api/journal');
  if (!j.entries.length) {
    main.innerHTML = label('01', 'journal') + empty('No filled trades yet. Every order needs <span class="cmd">-reason</span> and <span class="cmd">-thesis</span>; this page scores them.');
    return;
  }
  main.innerHTML = `${label('01', 'which theses worked')}
    <div class="tbl-wrap"><table><thead><tr><th>thesis</th><th class="n">trades</th><th class="n">invested</th><th class="n">p&amp;l</th><th class="n">return</th><th class="n">hit rate</th></tr></thead><tbody>
    ${j.theses.map((t) => `<tr><td>${esc(t.thesis)}</td><td class="n">${t.trades}</td><td class="n">${money(t.cost)}</td><td class="n ${dir(t.pnl)}">${money(t.pnl, true)}</td><td class="n">${pct(t.return)}</td><td class="n">${pct(t.hitRate, 0, false)}</td></tr>`).join('')}
    </tbody></table></div>
    ${label('02', 'entries: each buy, scored to today (realized + open + net dividends)')}
    <div class="tbl-wrap"><table><thead><tr><th>#</th><th>date</th><th>ticker</th><th>thesis</th><th class="n">invested</th><th class="n">p&amp;l</th><th class="n">return</th><th>state</th><th>reason</th></tr></thead><tbody>
    ${j.entries.map((e) => `<tr><td>${e.orderId}</td><td>${esc(e.date)}</td><td><a href="#watchlist/${esc(e.ticker)}">${esc(e.ticker)}</a></td><td>${esc(e.thesis)}</td><td class="n">${money(e.cost)}</td><td class="n ${dir(e.pnl)}">${money(e.pnl, true)}</td><td class="n">${pct(e.return)}</td><td>${e.open ? 'open' : 'closed'}</td><td class="wrap">${esc(e.reason)}</td></tr>`).join('')}
    </tbody></table></div>
    ${label('03', 'exits')}
    ${j.exits.length ? `<div class="tbl-wrap"><table><thead><tr><th>#</th><th>date</th><th>ticker</th><th>thesis</th><th class="n">realized</th><th>reason</th></tr></thead><tbody>
    ${j.exits.map((e) => `<tr><td>${e.orderId || 'auto'}</td><td>${esc(e.date)}</td><td>${esc(e.ticker)}</td><td>${esc(e.thesis)}</td><td class="n ${dir(e.realized)}">${money(e.realized, true)}</td><td class="wrap">${esc(e.reason)}</td></tr>`).join('')}
    </tbody></table></div>` : empty('No sells yet.')}`;
}

async function viewSignals(main) {
  const [sigs, rules] = await Promise.all([get('/api/signals'), get('/api/rules')]);
  main.innerHTML = `${label('01', 'fired on the latest bar')}
    ${sigs.length ? `<div class="tbl-wrap"><table><thead><tr><th>ticker</th><th>date</th><th>action</th><th>rule</th><th>thesis</th><th>screen</th><th>condition</th></tr></thead><tbody>
      ${sigs.map((s) => `<tr><td><a href="#watchlist/${esc(s.ticker)}">${esc(s.ticker)}</a></td><td>${esc(s.date)}</td><td>${badge(s.blocked ? 'BLOCKED' : s.action)}</td><td>${esc(s.rule)} v${s.version}</td><td>${esc(s.thesis)}</td><td>${badge(s.status)}</td><td class="muted">${esc(s.condition)}</td></tr>`).join('')}
      </tbody></table></div>` : empty('Nothing fired.')}
    ${label('02', 'rules (every edit is a new version; retired versions dimmed)')}
    ${rules.length ? `<div class="tbl-wrap"><table><thead><tr><th>name</th><th>ver</th><th>ticker</th><th>action</th><th>thesis</th><th>created</th><th>retired</th><th>condition</th></tr></thead><tbody>
      ${rules.map((r) => `<tr class="${r.retiredOn ? 'dim' : ''}"><td>${esc(r.name)}</td><td>v${r.version}</td><td>${esc(r.ticker)}</td><td>${esc(r.action)}</td><td>${esc(r.thesis)}</td><td>${esc(r.createdOn)}</td><td>${esc(r.retiredOn || '')}</td><td>${esc(r.condition)}</td></tr>`).join('')}
      </tbody></table></div>` : empty(`No rules. <span class="cmd">suspt rule add golden-cross '*' WATCH trend "sma50 crosses_above sma200"</span>`)}
    <p class="fine">Terms: open high low close chg smaN level:NAME, numbers. Operators: &lt; &lt;= &gt; &gt;= crosses_above crosses_below, joined by "and".</p>`;
}

async function viewOrders(main) {
  const orders = await get('/api/orders');
  main.innerHTML = label('01', 'orders') + (orders.length
    ? `<div class="tbl-wrap"><table><thead><tr><th>#</th><th>placed</th><th>side</th><th>ticker</th><th>size</th><th>type</th><th>status</th><th>fill</th><th>thesis</th><th>reason / note</th></tr></thead><tbody>
      ${orders.map((o) => `<tr><td>${o.id}</td><td>${esc(o.placedOn)}</td><td>${esc(o.side)}</td><td>${esc(o.ticker)}</td><td>${esc(o.size)}</td><td>${esc(o.kind)}</td><td>${badge(o.status)}</td>
        <td>${o.price != null ? `${esc(o.filledOn)} ${num(o.qty, 6)} @ ${money(o.price)}` : ''}</td><td>${esc(o.thesis)}</td><td class="wrap">${esc(o.reason)}${o.note ? `<br><span class="flag">${esc(o.note)}</span>` : ''}</td></tr>`).join('')}
      </tbody></table></div>`
    : empty(`No orders. <span class="cmd">suspt buy NVDA -usd 50 -reason "..." -thesis ai-capex</span>`));
}

async function viewScreen(main) {
  const rows = await get('/api/screens');
  main.innerHTML = `${label('01', 's&p shariah screen, latest evaluation per company')}
    <input class="filter" id="filter" placeholder="/ filter" aria-label="Filter companies" value="${esc(state.filter)}">
    <div class="tbl-wrap"><table><thead><tr><th>ticker</th><th>company</th><th>status</th><th class="n">debt</th><th class="n">cash</th><th class="n">impure</th><th class="n">purify</th><th>business</th><th>as of</th><th>note</th></tr></thead><tbody id="sc"></tbody></table></div>
    <p class="fine">Debt and cash each under 30% of the 36-month average market cap; impure income under 5% of revenue (S&amp;P Shariah, Oct 2026).</p>`;
  const draw = () => {
    const f = state.filter.trim().toUpperCase();
    $('#sc').innerHTML = rows.filter((r) => !f || (r.ticker + ' ' + r.name + ' ' + r.status).toUpperCase().includes(f)).map((r) => `<tr>
      <td><a href="#watchlist/${esc(r.ticker)}">${esc(r.ticker)}</a></td><td class="muted">${esc(r.name)}</td><td>${badge(r.status)}</td>
      <td class="n">${pct(r.debt, 1, false)}</td><td class="n">${pct(r.cash, 1, false)}</td><td class="n">${pct(r.npi, 1, false)}</td><td class="n">${pct(r.purif, 1, false)}</td>
      <td>${esc(r.business)}</td><td>${esc(r.asOf)}</td><td class="wrap">${esc(r.note)}</td></tr>`).join('');
  };
  $('#filter').addEventListener('input', (e) => { state.filter = e.target.value; draw(); });
  draw();
}

/* shell */
const RENDER = { portfolio: viewPortfolio, watchlist: viewWatchlist, journal: viewJournal, signals: viewSignals, orders: viewOrders, screen: viewScreen };

function drawNav() {
  $('#nav').innerHTML = VIEWS.map((v, i) => `<a href="#${v}" ${v === state.view ? 'aria-current="page"' : ''}><span class="n">0${i + 1}</span>${v}</a>`).join('');
  const listKeys = state.view === 'watchlist' ? '<span class="hint"><kbd>j/k</kbd>move</span><span class="hint"><kbd>h</kbd>hits</span>' : '';
  const filterKey = state.view === 'watchlist' || state.view === 'screen' ? '<span class="hint"><kbd>/</kbd>filter</span>' : '';
  $('#hints').innerHTML = VIEWS.map((v, i) => `<span class="hint opt"><kbd>${i + 1}</kbd>${v}</span>`).join('') + listKeys + filterKey +
    '<span class="hint"><kbd>r</kbd>reload</span><span class="hint"><kbd>?</kbd>help</span>';
}

async function route() {
  const [view, arg] = location.hash.slice(1).split('/');
  if (VIEWS.includes(view) && view !== state.view) state.filter = '';
  state.view = VIEWS.includes(view) ? view : 'portfolio';
  drawNav();
  $('#path').textContent = '~/' + state.view;
  clearCharts();
  const main = $('#view');
  try {
    await RENDER[state.view](main, arg && decodeURIComponent(arg).toUpperCase());
  } catch (e) {
    main.innerHTML = `<p class="flag">${esc(e.message)}</p><p class="muted">Is <span class="cmd">suspt serve</span> running? Try <span class="cmd">suspt update</span>.</p>`;
  }
}

function go(view) { location.hash = view; }

document.addEventListener('keydown', (e) => {
  const help = $('#help');
  const typing = e.target.matches('input, textarea');
  if (e.key === 'Escape') {
    if (!help.hidden) { help.hidden = true; return; }
    if (typing) { e.target.value = ''; state.filter = ''; e.target.dispatchEvent(new Event('input')); e.target.blur(); }
    return;
  }
  if (typing || e.metaKey || e.ctrlKey || e.altKey) return;
  if (e.key === '?') { help.hidden = !help.hidden; return; }
  if (!help.hidden) return;
  const n = Number(e.key);
  if (n >= 1 && n <= VIEWS.length) { go(VIEWS[n - 1]); return; }
  if (e.key === 'r') { route(); return; }
  if (e.key === 'h' && state.view === 'watchlist') { state.hits = !state.hits; showTicker(); return; }
  if (e.key === '/' && $('#filter')) { e.preventDefault(); $('#filter').focus(); return; }
  if (state.view === 'watchlist' && ['j', 'k', 'ArrowDown', 'ArrowUp'].includes(e.key)) {
    e.preventDefault();
    const rows = visibleRows();
    state.sel = Math.max(0, Math.min(rows.length - 1, state.sel + (e.key === 'j' || e.key === 'ArrowDown' ? 1 : -1)));
    drawList();
    $('#wl [aria-selected="true"]')?.scrollIntoView({ block: 'nearest' });
    showTicker();
  }
});
$('#help').addEventListener('click', (e) => { if (e.target.id === 'help') e.currentTarget.hidden = true; });

/* market clock, like insomnia's awake/asleep: NYSE regular hours, holidays ignored */
function nyParts(d) {
  const p = Object.fromEntries(new Intl.DateTimeFormat('en-US', { timeZone: 'America/New_York', weekday: 'short', hour: 'numeric', minute: 'numeric', second: 'numeric', hourCycle: 'h23' })
    .formatToParts(d).map((x) => [x.type, x.value]));
  return { day: p.weekday, secs: +p.hour * 3600 + +p.minute * 60 + +p.second };
}
const hms = (s) => `${Math.floor(s / 3600)}:${String(Math.floor((s % 3600) / 60)).padStart(2, '0')}:${String(s % 60).padStart(2, '0')}`;
function tick() {
  const OPEN = 9.5 * 3600, CLOSE = 16 * 3600;
  const { day, secs } = nyParts(new Date());
  const weekday = !['Sat', 'Sun'].includes(day);
  const el = $('#clock');
  if (weekday && secs >= OPEN && secs < CLOSE) {
    el.className = 'clock open';
    el.innerHTML = `<span class="dot"></span>market open · closes in ${hms(CLOSE - secs)}`;
    return;
  }
  // seconds until the next weekday 09:30 New York
  const order = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
  let wait = 0, idx = order.indexOf(day);
  if (weekday && secs < OPEN) wait = OPEN - secs;
  else {
    wait = 24 * 3600 - secs + OPEN;
    idx = (idx + 1) % 7;
    while (idx === 0 || idx === 6) { wait += 24 * 3600; idx = (idx + 1) % 7; }
  }
  el.className = 'clock';
  el.innerHTML = `<span class="dot"></span>market closed · opens in ${hms(wait)}`;
}

window.addEventListener('hashchange', route);
setInterval(tick, 1000);
tick();
route();
