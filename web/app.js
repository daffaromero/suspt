'use strict';
// The suspt app over its local JSON API. Every user string goes through esc().
// Changes go through send(), which adds the X-Suspt header the server requires.

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
  COMPLIANT: ['✓', 'compliant'], NON_COMPLIANT: ['✕', 'non-compliant'], REVIEW: ['?', 'needs your review'],
  NO_DATA: ['·', 'no data'], NO_SCREEN: ['·', 'not screened yet'], FUND: ['◆', 'sharia fund'],
  filled: ['✓', 'filled'], open: ['○', 'waiting'], rejected: ['✕', 'rejected'], expired: ['·', 'expired'], cancelled: ['·', 'cancelled'],
  BUY: ['▲', 'buy idea'], WATCH: ['◎', 'watch'], BLOCKED: ['✕', 'blocked'],
};
const badge = (s) => {
  const [i, label] = STATUS[s] || ['·', String(s).toLowerCase()];
  return `<span class="badge ${esc(s)}"><span class="i" aria-hidden="true">${i}</span>${esc(label)}</span>`;
};
const label = (n, text) => `<h2 class="section-label"><span class="num">${n}</span>${esc(text)}</h2>`;
const empty = (html) => `<p class="empty">${html}</p>`;
const btn = (act, text, attrs = '', cls = '') => `<button type="button" class="btn ${cls}" data-act="${act}" ${attrs}>${text}</button>`;

const VIEWS = ['portfolio', 'watchlist', 'journal', 'signals', 'orders', 'screen', 'settings'];
const state = { view: 'portfolio', sel: 0, filter: '', charts: [], data: {}, range: '1Y', hits: false, status: null };

async function get(path) {
  const r = await fetch(path, { headers: { Accept: 'application/json' } });
  if (!r.ok) throw new Error(`${path}: ${r.status} ${(await r.text()).trim()}`);
  return r.json();
}
async function send(method, path, body) {
  const r = await fetch(path, {
    method,
    headers: { 'Content-Type': 'application/json', 'X-Suspt': '1' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = (await r.text()).trim();
  if (!r.ok) throw new Error(text || r.statusText);
  return text ? JSON.parse(text) : {};
}

function toast(msg, kind = 'ok') {
  const el = $('#toast');
  el.textContent = msg;
  el.className = `toast ${kind}`;
  el.hidden = false;
  clearTimeout(toast.t);
  toast.t = setTimeout(() => (el.hidden = true), kind === 'err' ? 9000 : 5000);
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

/* getting started: shown until the three steps are done */
function checklist(st, hasTrades) {
  const steps = [
    [st.keys, 'Connect the free data sources', 'Prices come from Tiingo and company filings from the SEC. Both are free; setup takes two minutes.', btn('go', 'Open settings', 'data-to="settings"', 'primary')],
    [st.watching > 0, 'Choose stocks to follow', 'Start with the 215 stocks in SPUS, the S&amp;P 500 Shariah fund, or add your own.', btn('sync-spus', 'Add all SPUS stocks', '', 'primary') + ' ' + btn('go', 'Pick my own', 'data-to="watchlist"')],
    [hasTrades, 'Make your first paper trade', 'Open a stock in the watchlist and press Buy. Every trade asks why you are making it, so later you can see which reasons worked. No real money moves.', btn('go', 'Go to watchlist', 'data-to="watchlist"')],
  ];
  if (steps.every((s) => s[0])) return '';
  return `${label('00', 'getting started')}<ol class="steps">${steps.map(([done, title, text, action]) => `
    <li class="${done ? 'done' : ''}"><span class="tick" aria-hidden="true">${done ? '✓' : '○'}</span>
      <div><b>${title}</b><p>${text}</p>${done ? '' : action}</div></li>`).join('')}</ol>`;
}

/* views */
async function viewPortfolio(main) {
  const [s, st, orders] = await Promise.all([get('/api/summary'), get('/api/status'), get('/api/orders')]);
  const intro = checklist(st, orders.length > 0);
  if (!s.asOf) {
    main.innerHTML = intro + label('01', 'portfolio') + empty('The simulation starts after the first market day with data. Your starting cash appears here then.');
    return;
  }
  const vs = s.twr - s.benchTwr;
  main.innerHTML = `${intro}
    ${label('01', 'portfolio')}
    <div class="hero">
      <div class="big">${money(s.value)}</div>
      <div class="sub">paper money as of ${esc(s.asOf)} · started ${esc(s.start)} · cash ${money(s.cash)}</div>
    </div>
    <div class="stats">
      <div class="stat"><div class="k">deposited</div><div class="v">${money(s.contributions)}</div><div class="d">since ${esc(s.start)}</div></div>
      <div class="stat"><div class="k">profit / loss</div><div class="v ${dir(s.pnl)}">${money(s.pnl, true)}</div><div class="d">value minus deposits</div></div>
      <div class="stat"><div class="k">your return</div><div class="v">${pct(s.twr, 2)}</div><div class="d">deposits don't count as gains</div></div>
      <div class="stat"><div class="k">vs ${esc(s.benchmark)}</div><div class="v ${dir(vs)}">${(vs > 0 ? '+' : '') + (vs * 100).toFixed(2)} pts</div><div class="d">${esc(s.benchmark)} ${pct(s.benchTwr, 2)} with the same deposits</div></div>
    </div>
    ${label('02', `your money vs simply buying ${s.benchmark}`)}
    <div class="panel">
      <div class="toolbar"><div class="legend" id="eq-legend"></div><span id="eq-ranges"></span></div>
      <div class="chart" id="eq-chart" role="img" aria-label="Portfolio value, benchmark value and deposits over time"></div>
      <details><summary class="fine">show as table (last 20 days)</summary><div class="tbl-wrap" id="eq-table"></div></details>
    </div>
    ${label('03', 'what you hold')}
    <div class="tbl-wrap" id="positions"></div>
    ${label('04', 'how concentrated you are (share of holdings, dashed tick = your limit)')}
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
      <span><span class="sw" style="border-color:${S1}"></span>you<b>${money(p.value)}</b></span>
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
  $('#eq-table').innerHTML = `<table><thead><tr><th>date</th><th class="n">you</th><th class="n">${esc(s.benchmark)}</th><th class="n">deposited</th></tr></thead><tbody>${s.equity
    .slice(-20).reverse().map((p) => `<tr><td>${esc(p.date)}</td><td class="n">${money(p.value)}</td><td class="n">${money(p.bench)}</td><td class="n">${money(p.contrib)}</td></tr>`).join('')}</tbody></table>`;

  $('#positions').innerHTML = s.positions.length
    ? `<table><thead><tr><th>stock</th><th class="n">shares</th><th class="n">cost</th><th class="n">price</th><th class="n">value</th><th class="n">p&amp;l</th><th class="n">weight</th><th>sector</th><th>screen</th><th></th></tr></thead><tbody>${s.positions
        .map((p) => `<tr><td><a href="#watchlist/${esc(p.ticker)}">${esc(p.ticker)}</a></td><td class="n">${num(p.qty, 6)}</td><td class="n">${money(p.cost)}</td><td class="n">${money(p.close)}</td><td class="n">${money(p.value)}</td>
          <td class="n ${dir(p.pnl)}">${money(p.pnl, true)}</td><td class="n">${pct(p.weight, 0, false)}</td><td>${esc(p.sector)}</td><td>${badge(p.status)}</td>
          <td>${btn('trade', 'Sell', `data-ticker="${esc(p.ticker)}" data-side="sell"`, 'small')}</td></tr>`).join('')}</tbody></table>`
    : empty('Nothing yet. Open a stock in the watchlist and press Buy.');

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
  const addForm = `<form class="addrow" data-form="add-watch"><input name="tickers" placeholder="add stocks, e.g. NVDA AAPL" aria-label="Add stocks by ticker" autocomplete="off"><button class="btn" type="submit">Add</button></form>`;
  if (!rows.length) {
    main.innerHTML = `${label('01', 'watchlist')}
      <div class="panel narrow"><p>Your watchlist is empty. Add stocks by their ticker symbol, or start with every stock in SPUS (the S&amp;P 500 Shariah fund).</p>
      ${addForm}<p>${btn('sync-spus', 'Add all SPUS stocks', '', 'primary')}</p></div>`;
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
    ${pending ? `<p class="fine">${pending} of ${rows.length} stocks are still waiting for prices. They fill in by themselves; the free price service allows 50 lookups an hour, so a big list takes a few hours the first time.</p>` : ''}
    <div class="split">
      <div class="list">${addForm}<input id="filter" placeholder="/ filter" aria-label="Filter watchlist" value="${esc(state.filter)}"><ul id="wl" role="listbox" aria-label="Watchlist"></ul></div>
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
  const unwatch = btn('unwatch', 'Remove from watchlist', `data-ticker="${esc(row.ticker)}"`, 'quiet');
  let d;
  try {
    d = await get(`/api/ticker/${encodeURIComponent(row.ticker)}`);
  } catch (e) {
    $('#detail').innerHTML = `<div class="detail-head"><h1>${esc(row.ticker)}</h1>${badge(row.status)}<span class="spacer"></span>${unwatch}</div>` + (row.date
      ? `<p class="flag">${esc(e.message)}</p>`
      : empty('No prices yet. They are fetched in the background; this page updates when they arrive.'));
    return;
  }
  const last = d.screens[d.screens.length - 1];
  const months = d.screens.filter((s) => s.status !== 'NO_DATA');
  const ok = months.filter((s) => s.status === 'COMPLIANT').length;
  const canBuy = d.status === 'COMPLIANT' || d.status === 'FUND';
  $('#detail').innerHTML = `
    <div class="detail-head"><h1>${esc(d.ticker)}</h1><span class="name">${esc(d.name)}</span>${badge(d.status)}<span class="muted">${esc(d.sector)}${d.held ? ` · you hold ${num(d.held, 6)}` : ''}</span>
      <span class="spacer"></span>
      ${btn('trade', 'Buy', `data-ticker="${esc(d.ticker)}" data-side="buy" ${canBuy ? '' : 'title="Only compliant stocks can be bought"'}`, 'primary')}
      ${d.held ? btn('trade', 'Sell', `data-ticker="${esc(d.ticker)}" data-side="sell"`) : ''}
      ${unwatch}</div>
    <div class="panel">
      <div class="toolbar"><div class="legend" id="px-legend"></div><span id="px-ranges"></span></div>
      <div class="chart tall" id="px-chart" role="img" aria-label="${esc(d.ticker)} daily price with your trades, key levels and rule triggers"></div>
      <p class="fine">Prices adjusted for splits. ▲ B = you bought, ▼ S = you sold, dashed lines = your key levels, ● = a rule turned true (toggle with <kbd>h</kbd>). Up days hollow, down days filled.</p>
    </div>
    <div class="grid2">
      <div class="panel">
        <h3 class="section-label" style="margin-top:0"><span class="num">s&amp;p</span>sharia screen ${last ? `as of ${esc(last.asOf)}` : ''}</h3>
        ${last ? screenBullets(last) : empty('Funds and companies that don\'t file with the SEC are not screened.')}
        ${last && last.business === 'review' ? reviewButtons(d.ticker) : ''}
        ${months.length ? `<div class="strip" aria-hidden="true">${months.map((s) => `<i class="${esc(s.status)}" title="${esc(s.asOf)} ${esc(s.status)}"></i>`).join('')}</div>
          <p class="fine">month by month: compliant ${ok} of ${months.length} times (green compliant, red non-compliant, amber needs review)</p>` : ''}
      </div>
      <div class="panel">
        <h3 class="section-label" style="margin-top:0"><span class="num">lv</span>your price levels</h3>
        ${d.levels.length ? `<table><tbody>${d.levels.sort((a, b) => b.price - a.price).map((l) => `<tr><td>${esc(l.name)}</td><td class="n">${money(l.price)}</td><td class="n muted">${pct(l.dist)} away</td>
          <td class="n">${btn('rm-level', '×', `data-ticker="${esc(d.ticker)}" data-name="${esc(l.name)}" aria-label="Remove level ${esc(l.name)}"`, 'small quiet')}</td></tr>`).join('')}</tbody></table>`
          : '<p class="fine">Mark prices you care about, like a support level to buy near. They show on the chart and rules can use them.</p>'}
        <form class="addrow" data-form="add-level" data-ticker="${esc(d.ticker)}">
          <input name="name" placeholder="name, e.g. support" aria-label="Level name" autocomplete="off" required>
          <input name="price" type="number" step="any" min="0" placeholder="price" aria-label="Level price" required>
          <button class="btn" type="submit">Add level</button>
        </form>
        ${row.signals.length ? `<p>${row.signals.map((s) => `${badge(s.blocked ? 'BLOCKED' : s.action)} ${esc(s.rule)} v${s.version} <span class="muted">${esc(s.condition)}</span>`).join('<br>')}</p>` : ''}
        ${d.fills.length ? `<table><thead><tr><th>your trades</th><th class="n">shares</th><th class="n">price</th><th>why</th></tr></thead><tbody>${d.fills.map((f) => `<tr><td>${esc(f.time)} ${esc(f.side)}</td><td class="n">${num(f.qty, 6)}</td><td class="n">${money(f.price)}</td><td class="wrap">${esc(f.reason)}</td></tr>`).join('')}</tbody></table>` : ''}
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
      <span title="50-day average price"><span class="sw" style="border-color:${S1}"></span>50-day avg<b>${num(avg50.get(t))}</b></span>
      <span title="200-day average price"><span class="sw" style="border-color:${S2}"></span>200-day avg<b>${num(avg200.get(t))}</b></span><span class="when">${esc(t)}</span>`;
  };
  const lastT = d.bars[d.bars.length - 1].time;
  legend(lastT);
  c.subscribeCrosshairMove((e) => legend(e.time || lastT));
  const hitsBtn = document.createElement('button');
  hitsBtn.textContent = `rule hits ${d.hits.length}`;
  hitsBtn.title = 'show where your rules turned true (h)';
  hitsBtn.setAttribute('aria-pressed', String(state.hits));
  hitsBtn.onclick = () => { state.hits = !state.hits; showTicker(); };
  const ranges = rangeButtons((r) => setRange(c, d.bars, r));
  ranges.prepend(hitsBtn);
  $('#px-ranges').append(ranges);
  setRange(c, d.bars, state.range);
}

function reviewButtons(ticker) {
  return `<div class="review"><p><b>Is this business halal?</b> The automatic checks can't tell from the company's industry code. If S&amp;P's own fund doesn't hold it, that's usually why. Decide once; it applies from today.</p>
    <form class="addrow" data-form="review" data-ticker="${esc(ticker)}">
      <label class="inline">impure revenue <input name="percent" type="number" min="0" max="99" step="0.1" value="0" aria-label="Percent of revenue from non-permissible sources">%</label>
      <button class="btn" type="submit" name="verdict" value="ok">It's fine</button>
      <button class="btn danger" type="submit" name="verdict" value="excluded">Exclude it</button>
    </form></div>`;
}

function screenBullets(s) {
  const one = (name, title, v, limit, buffer) => {
    if (v == null) return `<div class="bullet"><span title="${title}">${name}</span><span class="muted">${NA}</span><span></span></div>`;
    const max = Math.max(limit * 2, v * 1.1);
    const over = v >= limit;
    return `<div class="bullet"><span title="${title}">${name}</span>
      <span class="track" title="${name} ${pct(v, 1, false)}, must stay under ${pct(limit, 0, false)}">
        ${buffer ? `<span class="buffer" style="left:${((limit - buffer) / max) * 100}%;width:${((2 * buffer) / max) * 100}%"></span>` : ''}
        <span class="fill ${over ? 'over' : ''}" style="width:${Math.min(100, (v / max) * 100)}%"></span>
        <span class="limit" style="left:${(limit / max) * 100}%"></span>
      </span><span class="n">${pct(v, 1, false)}${over ? ' ✕' : ''}</span></div>`;
  };
  return `${one('debt', 'interest-bearing debt vs company value', s.debt, 0.3, 0.02)}${one('cash', 'cash and interest-earning investments vs company value', s.cash, 0.3, 0.02)}${one('impure', 'revenue from interest and haram activities', s.npi, 0.05, 0)}
    <p class="fine">Each bar must stay left of its tick (30%, 30%, 5%); the amber band is S&amp;P's 2-point grace zone. If you get a dividend, give away ${pct(s.purif, 2, false)} of it to purify it.${s.note ? ' ' + esc(s.note) + '.' : ''}</p>`;
}

async function viewJournal(main) {
  const j = await get('/api/journal');
  if (!j.entries.length) {
    main.innerHTML = label('01', 'journal') + empty('No trades have filled yet. Every trade asks why you made it and for a short theme (a "thesis tag"); once trades fill, this page shows which reasons and themes made or lost money.');
    return;
  }
  main.innerHTML = `${label('01', 'which ideas worked')}
    <div class="tbl-wrap"><table><thead><tr><th>thesis</th><th class="n">trades</th><th class="n">invested</th><th class="n">p&amp;l</th><th class="n">return</th><th class="n">winners</th></tr></thead><tbody>
    ${j.theses.map((t) => `<tr><td>${esc(t.thesis)}</td><td class="n">${t.trades}</td><td class="n">${money(t.cost)}</td><td class="n ${dir(t.pnl)}">${money(t.pnl, true)}</td><td class="n">${pct(t.return)}</td><td class="n">${pct(t.hitRate, 0, false)}</td></tr>`).join('')}
    </tbody></table></div>
    ${label('02', 'every buy, scored up to today (sold + still held + dividends)')}
    <div class="tbl-wrap"><table><thead><tr><th>#</th><th>date</th><th>stock</th><th>thesis</th><th class="n">invested</th><th class="n">p&amp;l</th><th class="n">return</th><th>state</th><th>why</th></tr></thead><tbody>
    ${j.entries.map((e) => `<tr><td>${e.orderId}</td><td>${esc(e.date)}</td><td><a href="#watchlist/${esc(e.ticker)}">${esc(e.ticker)}</a></td><td>${esc(e.thesis)}</td><td class="n">${money(e.cost)}</td><td class="n ${dir(e.pnl)}">${money(e.pnl, true)}</td><td class="n">${pct(e.return)}</td><td>${e.open ? 'held' : 'sold'}</td><td class="wrap">${esc(e.reason)}</td></tr>`).join('')}
    </tbody></table></div>
    ${label('03', 'sales')}
    ${j.exits.length ? `<div class="tbl-wrap"><table><thead><tr><th>#</th><th>date</th><th>stock</th><th>thesis</th><th class="n">profit</th><th>why</th></tr></thead><tbody>
    ${j.exits.map((e) => `<tr><td>${e.orderId || 'auto'}</td><td>${esc(e.date)}</td><td>${esc(e.ticker)}</td><td>${esc(e.thesis)}</td><td class="n ${dir(e.realized)}">${money(e.realized, true)}</td><td class="wrap">${esc(e.reason)}</td></tr>`).join('')}
    </tbody></table></div>` : empty('No sales yet.')}`;
}

const RULE_EXAMPLES = [
  ['close crosses_above sma200', 'price rises above its 200-day average'],
  ['sma50 crosses_above sma200', 'the 50-day average crosses above the 200-day ("golden cross")'],
  ['close <= level:support', 'price falls to your "support" level'],
  ['chg < -3', 'drops more than 3% in a day'],
];

async function viewSignals(main) {
  const [sigs, rules] = await Promise.all([get('/api/signals'), get('/api/rules')]);
  main.innerHTML = `${label('01', 'your rules that are true today')}
    ${sigs.length ? `<div class="tbl-wrap"><table><thead><tr><th>stock</th><th>date</th><th>idea</th><th>rule</th><th>thesis</th><th>screen</th><th>condition</th></tr></thead><tbody>
      ${sigs.map((s) => `<tr><td><a href="#watchlist/${esc(s.ticker)}">${esc(s.ticker)}</a></td><td>${esc(s.date)}</td><td>${badge(s.blocked ? 'BLOCKED' : s.action)}</td><td>${esc(s.rule)} v${s.version}</td><td>${esc(s.thesis)}</td><td>${badge(s.status)}</td><td class="muted">${esc(s.condition)}</td></tr>`).join('')}
      </tbody></table></div><p class="fine">Rules only flag ideas; nothing is bought automatically. "Blocked" means the stock fails the sharia screen.</p>` : empty('None of your rules is true on the latest trading day.')}
    ${label('02', 'new rule')}
    <form class="panel ruleform" data-form="add-rule">
      <div class="fields">
        <label>name<input name="name" placeholder="e.g. golden-cross" required autocomplete="off"></label>
        <label>stocks<input name="ticker" value="*" title="a ticker, or * for your whole watchlist" required autocomplete="off"></label>
        <label>when true, flag a<select name="action"><option value="WATCH">watch</option><option value="BUY">buy idea</option></select></label>
        <label>thesis tag<input name="thesis" placeholder="e.g. trend" required autocomplete="off"></label>
      </div>
      <label class="wide">condition<input name="condition" placeholder="e.g. close crosses_above sma200" required autocomplete="off"></label>
      <p class="fine">Click an example: ${RULE_EXAMPLES.map(([c, d]) => `<button type="button" class="chip" data-act="example" data-cond="${esc(c)}" title="${esc(d)}">${esc(c)}</button>`).join(' ')}</p>
      <p class="fine">Words you can use: open high low close (prices), chg (daily % change), sma50 / sma200 (any smaN average), level:NAME (your levels), numbers. Compare with &lt; &lt;= &gt; &gt;= crosses_above crosses_below, and join with "and".</p>
      <button class="btn primary" type="submit">Save rule</button>
    </form>
    ${label('03', 'all rules (editing a rule saves a new version; old versions dimmed)')}
    ${rules.length ? `<div class="tbl-wrap"><table><thead><tr><th>name</th><th>ver</th><th>stocks</th><th>idea</th><th>thesis</th><th>created</th><th>retired</th><th>condition</th><th></th></tr></thead><tbody>
      ${rules.map((r) => `<tr class="${r.retiredOn ? 'dim' : ''}"><td>${esc(r.name)}</td><td>v${r.version}</td><td>${esc(r.ticker === '*' ? 'watchlist' : r.ticker)}</td><td>${esc(r.action.toLowerCase())}</td><td>${esc(r.thesis)}</td><td>${esc(r.createdOn)}</td><td>${esc(r.retiredOn || '')}</td><td>${esc(r.condition)}</td>
        <td>${r.retiredOn ? '' : btn('retire', 'Retire', `data-name="${esc(r.name)}"`, 'small quiet')}</td></tr>`).join('')}
      </tbody></table></div>` : empty('No rules yet.')}`;
}

async function viewOrders(main) {
  const orders = await get('/api/orders');
  main.innerHTML = label('01', 'orders') + (orders.length
    ? `<div class="tbl-wrap"><table><thead><tr><th>#</th><th>placed</th><th>side</th><th>stock</th><th>size</th><th>type</th><th>status</th><th>filled</th><th>thesis</th><th>why / note</th><th></th></tr></thead><tbody>
      ${orders.map((o) => `<tr><td>${o.id}</td><td>${esc(o.placedOn)}</td><td>${esc(o.side)}</td><td>${esc(o.ticker)}</td><td>${esc(o.size)}</td><td>${esc(o.kind)}</td><td>${badge(o.status)}</td>
        <td>${o.price != null ? `${esc(o.filledOn)} ${num(o.qty, 6)} @ ${money(o.price)}` : ''}</td><td>${esc(o.thesis)}</td><td class="wrap">${esc(o.reason)}${o.note ? `<br><span class="flag">${esc(o.note)}</span>` : ''}</td>
        <td>${o.status === 'open' ? btn('cancel', 'Cancel', `data-id="${o.id}"`, 'small quiet') : ''}</td></tr>`).join('')}
      </tbody></table></div><p class="fine">Orders fill at a market open after the day you place them, never at today's prices. That keeps the simulation honest.</p>`
    : empty('No orders yet. Open a stock in the watchlist and press Buy.'));
}

async function viewScreen(main) {
  const rows = await get('/api/screens');
  main.innerHTML = `${label('01', 's&p shariah screen: latest result per company')}
    <input class="filter" id="filter" placeholder="/ filter" aria-label="Filter companies" value="${esc(state.filter)}">
    <div class="tbl-wrap"><table><thead><tr><th>stock</th><th>company</th><th>status</th><th class="n" title="interest-bearing debt vs company value">debt</th><th class="n" title="cash and interest-earning investments vs company value">cash</th><th class="n" title="revenue from interest and haram activities">impure</th><th class="n" title="share of dividends to give away">purify</th><th>business</th><th>as of</th><th>note</th><th></th></tr></thead><tbody id="sc"></tbody></table></div>
    <p class="fine">Debt and cash must each stay under 30% of the company's average value over 3 years; impure income under 5% of revenue (S&amp;P Shariah, October 2026 rules).</p>`;
  const draw = () => {
    const f = state.filter.trim().toUpperCase();
    $('#sc').innerHTML = rows.filter((r) => !f || (r.ticker + ' ' + r.name + ' ' + r.status).toUpperCase().includes(f)).map((r) => `<tr>
      <td><a href="#watchlist/${esc(r.ticker)}">${esc(r.ticker)}</a></td><td class="muted">${esc(r.name)}</td><td>${badge(r.status)}</td>
      <td class="n">${pct(r.debt, 1, false)}</td><td class="n">${pct(r.cash, 1, false)}</td><td class="n">${pct(r.npi, 1, false)}</td><td class="n">${pct(r.purif, 1, false)}</td>
      <td>${esc(r.business)}</td><td>${esc(r.asOf)}</td><td class="wrap">${esc(r.note)}</td>
      <td>${r.business === 'review' ? btn('quick-review', 'Fine', `data-ticker="${esc(r.ticker)}" data-verdict="ok"`, 'small') + ' ' + btn('quick-review', 'Exclude', `data-ticker="${esc(r.ticker)}" data-verdict="excluded"`, 'small danger') : ''}</td></tr>`).join('');
  };
  $('#filter').addEventListener('input', (e) => { state.filter = e.target.value; draw(); });
  draw();
}

const SETTING_LABELS = {
  start_date: 'Simulation start date', initial_deposit: 'Starting deposit ($)', monthly_deposit: 'Monthly deposit ($)',
  deposit_day: 'Deposit day of the month', fee_flat: 'Fee per order ($)', fee_pct: 'Fee (% of order)', fee_min: 'Minimum fee per order ($)',
  dividend_withholding: 'Tax withheld from dividends (%)', compliance_sell: 'When a stock you hold stops being compliant',
  auto_refresh: 'Fetch new data automatically', compliance_grace_days: 'Days to wait before that sale',
  benchmark: 'Compare against (fund ticker)', allowed_etfs: 'Sharia funds you may buy', max_position_pct: 'Warn when one stock is over (% of holdings)',
  max_sector_pct: 'Warn when one sector is over (%)', max_semis_pct: 'Warn when semiconductors are over (%)',
};
const SETTING_CHOICES = { compliance_sell: [['sell', 'sell it automatically'], ['alert', 'just warn me']], auto_refresh: [['on', 'yes, after each US market close'], ['off', 'no, only when I press refresh']] };

async function viewSettings(main) {
  const [items, st] = await Promise.all([get('/api/settings'), get('/api/status')]);
  const field = (it) => {
    if (SETTING_CHOICES[it.key]) {
      return `<select name="value">${SETTING_CHOICES[it.key].map(([v, t]) => `<option value="${v}" ${v === it.value ? 'selected' : ''}>${esc(t)}</option>`).join('')}</select>`;
    }
    const type = it.key === 'start_date' ? 'date' : ['benchmark', 'allowed_etfs'].includes(it.key) ? 'text' : 'number';
    return `<input name="value" type="${type}" ${type === 'number' ? 'step="any" min="0"' : ''} value="${esc(it.value)}" aria-label="${esc(SETTING_LABELS[it.key] || it.key)}">`;
  };
  main.innerHTML = `${label('01', 'data sources')}
    <div class="panel narrow">
      <p>${st.keys ? '<span class="badge COMPLIANT"><span class="i">✓</span>connected</span> Enter new values only if you want to replace them.' : '<span class="badge REVIEW"><span class="i">?</span>not connected yet</span>'}</p>
      <form class="setup" data-form="setup">
        <label>Tiingo API token
          <input name="tiingoKey" type="password" autocomplete="off" required placeholder="paste it here">
          <span class="fine">Free account at <a href="https://www.tiingo.com/account/api/token" target="_blank" rel="noreferrer">tiingo.com</a>: sign up, then Account, API, Token. Used for daily prices.</span></label>
        <label>Your name<input name="name" autocomplete="name" required></label>
        <label>Your email<input name="email" type="email" autocomplete="email" required>
          <span class="fine">The SEC asks tools that read company filings to say who they are. Both stay in a file on this computer and are never shown again.</span></label>
        <button class="btn primary" type="submit">Save and test</button>
      </form>
    </div>
    ${label('02', 'simulation')}
    <div class="settings">${items.map((it) => `
      <form class="setting" data-form="setting" data-key="${esc(it.key)}">
        <label><b>${esc(SETTING_LABELS[it.key] || it.key)}</b><span class="fine">${esc(it.help)}${it.default && it.value !== it.default ? ` (default ${esc(it.default)})` : ''}</span></label>
        ${field(it)}<button class="btn small" type="submit">Save</button>
      </form>`).join('')}</div>
    <p class="fine">Changing a setting re-runs the whole simulation from the start date with the new values.</p>
    ${label('03', 'stocks and data')}
    <div class="panel narrow">
      <p>${btn('sync-spus', 'Add all SPUS stocks to the watchlist')} <span class="fine">the 215 stocks in the S&amp;P 500 Shariah fund</span></p>
      <p>${btn('refresh', 'Fetch new data now')} <span class="fine">${st.job.running ? 'a refresh is running' : `latest data: ${esc(st.asOf || 'none yet')}`}</span></p>
    </div>`;
}

/* trade ticket */
async function openTrade(ticker, side) {
  const [d, j] = await Promise.all([get(`/api/ticker/${encodeURIComponent(ticker)}`), get('/api/journal').catch(() => ({ theses: [] }))]);
  const close = d.bars.length ? d.bars[d.bars.length - 1].close : null;
  const dlg = $('#trade');
  dlg.innerHTML = `<form method="dialog" id="trade-form" class="ticket">
    <h2>${side === 'buy' ? 'Buy' : 'Sell'} ${esc(d.ticker)} <span class="muted">paper money</span></h2>
    <p class="muted">${esc(d.name)} · last close ${money(close)} · ${badge(d.status)}${d.held ? ` · you hold ${num(d.held, 6)}` : ''}</p>
    ${side === 'buy' && d.status !== 'COMPLIANT' && d.status !== 'FUND' ? '<p class="flag">This stock isn\'t compliant right now, so the order will be refused.</p>' : ''}
    <fieldset><legend>How much</legend>
      <label class="inline"><input type="radio" name="mode" value="usd" checked> dollars</label>
      <label class="inline"><input type="radio" name="mode" value="shares"> shares</label>
      ${side === 'sell' ? '<label class="inline"><input type="radio" name="mode" value="all"> everything I hold</label>' : ''}
      <input name="amount" type="number" step="any" min="0" placeholder="e.g. 50" aria-label="Amount">
      <span class="fine" id="est"></span>
    </fieldset>
    <fieldset><legend>Price</legend>
      <label class="inline"><input type="radio" name="kind" value="market" checked> whatever it opens at next</label>
      <label class="inline"><input type="radio" name="kind" value="limit"> only at this price or ${side === 'buy' ? 'lower' : 'higher'}:</label>
      <input name="limit" type="number" step="any" min="0" placeholder="limit price" aria-label="Limit price" disabled>
      <label class="inline"><input type="checkbox" name="gtc"> keep trying until it fills or I cancel</label>
    </fieldset>
    <label>Why are you making this trade?<textarea name="reason" rows="3" required placeholder="e.g. Data-center spending keeps growing and they sell most of the chips"></textarea></label>
    <label>Thesis tag <span class="fine">a short theme to group trades by, e.g. ai-capex</span>
      <input name="thesis" list="theses" required autocomplete="off">
      <datalist id="theses">${j.theses.map((t) => `<option value="${esc(t.thesis)}">`).join('')}</datalist></label>
    <p class="fine">It fills at a market open after today, never at today's price.</p>
    <p class="flag" id="trade-err" role="alert"></p>
    <div class="actions"><button class="btn quiet" value="cancel" formnovalidate>Cancel</button><button class="btn primary" value="place">Place paper order</button></div>
  </form>`;
  const f = $('#trade-form');
  const update = () => {
    const mode = f.mode.value, amt = parseFloat(f.amount.value);
    f.amount.disabled = mode === 'all';
    f.limit.disabled = f.kind.value !== 'limit';
    const px = f.kind.value === 'limit' && parseFloat(f.limit.value) > 0 ? parseFloat(f.limit.value) : close;
    $('#est').textContent = mode === 'all' ? `sells all ${num(d.held, 6)} shares`
      : !(amt > 0) || !px ? ''
      : mode === 'usd' ? `about ${num(amt / px, 4)} shares` : `about ${money(amt * px)}`;
  };
  f.addEventListener('input', update);
  update();
  f.addEventListener('submit', async (e) => {
    if (e.submitter?.value !== 'place') return; // Cancel closes the dialog
    e.preventDefault();
    const mode = f.mode.value, amt = parseFloat(f.amount.value);
    const body = { ticker: d.ticker, side, reason: f.reason.value, thesis: f.thesis.value, gtc: f.gtc.checked };
    if (mode === 'usd') body.usd = amt;
    if (mode === 'shares') body.shares = amt;
    if (f.kind.value === 'limit') body.limit = parseFloat(f.limit.value);
    if (mode !== 'all' && !(amt > 0)) { $('#trade-err').textContent = 'Enter how much.'; return; }
    try {
      const r = await send('POST', '/api/orders', body);
      dlg.close();
      toast(`Order #${r.id} placed. It fills at a market open after ${r.placedOn}.`);
      route();
    } catch (err) {
      $('#trade-err').textContent = err.message;
    }
  });
  if (typeof dlg.showModal !== 'function') throw new Error('this browser is too old for the order ticket; please update it');
  dlg.showModal();
  f.amount.focus();
}

/* actions: buttons with data-act, forms with data-form */
async function run(fn, done) {
  try {
    const r = await fn();
    if (done) toast(typeof done === 'function' ? done(r) : done);
    await route();
    pollStatus(true);
  } catch (e) {
    toast(e.message, 'err');
  }
}

document.addEventListener('click', (e) => {
  const b = e.target.closest('[data-act]');
  if (!b) return;
  const { act, ticker, side, name, id, to, cond, verdict } = b.dataset;
  switch (act) {
    case 'go': location.hash = to; break;
    case 'trade': {
      const text = b.textContent;
      b.disabled = true;
      b.textContent = 'opening…';
      openTrade(ticker, side)
        .catch((err) => toast(`Couldn't open the order ticket: ${err.message}`, 'err'))
        .finally(() => { b.disabled = false; b.textContent = text; });
      break;
    }
    case 'unwatch': if (confirm(`Remove ${ticker} from your watchlist?`)) run(() => send('DELETE', `/api/watchlist/${encodeURIComponent(ticker)}`), `${ticker} removed`); break;
    case 'rm-level': run(() => send('DELETE', `/api/levels/${encodeURIComponent(ticker)}/${encodeURIComponent(name)}`), 'Level removed'); break;
    case 'cancel': if (confirm(`Cancel order #${id}?`)) run(() => send('POST', `/api/orders/${id}/cancel`), `Order #${id} cancelled`); break;
    case 'retire': if (confirm(`Retire the rule "${name}"? Its history is kept.`)) run(() => send('DELETE', `/api/rules/${encodeURIComponent(name)}`), 'Rule retired'); break;
    case 'quick-review': run(() => send('POST', '/api/reviews', { ticker, verdict, percent: 0, note: '' }), `${ticker} marked ${verdict === 'ok' ? 'fine' : 'excluded'}`); break;
    case 'example': { const input = b.closest('form').condition; input.value = cond; input.focus(); break; }
    case 'sync-spus': run(() => send('POST', '/api/sync-spus'), (r) => `${r.added} stocks added. Their data fills in over the next few hours.`).then(() => send('POST', '/api/refresh').catch(() => {})); break;
    case 'refresh': run(() => send('POST', '/api/refresh'), 'Fetching new data in the background'); break;
  }
});

document.addEventListener('submit', (e) => {
  const f = e.target.closest('[data-form]');
  if (!f) return;
  e.preventDefault();
  const v = (n) => f.elements[n]?.value ?? '';
  switch (f.dataset.form) {
    case 'add-watch': {
      const tickers = v('tickers').toUpperCase().split(/[\s,]+/).filter(Boolean);
      if (tickers.length) run(() => send('POST', '/api/watchlist', { tickers }), (r) => `${r.added} added. Data arrives in the background.`).then(() => send('POST', '/api/refresh').catch(() => {}));
      break;
    }
    case 'add-level': run(() => send('POST', '/api/levels', { ticker: f.dataset.ticker, name: v('name'), price: parseFloat(v('price')) }), 'Level saved'); break;
    case 'add-rule': run(() => send('POST', '/api/rules', { name: v('name'), ticker: v('ticker'), action: v('action'), thesis: v('thesis'), condition: v('condition') }), (r) => `Rule saved (version ${r.version})`); break;
    case 'review': run(() => send('POST', '/api/reviews', { ticker: f.dataset.ticker, verdict: e.submitter.value, percent: parseFloat(v('percent')) || 0, note: '' }), 'Verdict saved'); break;
    case 'setting': run(() => send('POST', '/api/settings', { key: f.dataset.key, value: v('value') }), 'Saved; the simulation was re-run'); break;
    case 'setup': run(() => send('POST', '/api/setup', { tiingoKey: v('tiingoKey'), name: v('name'), email: v('email') }), 'Connected. Fetching data in the background.').then(() => send('POST', '/api/refresh').catch(() => {})); break;
  }
});

/* data status in the top bar; the app refreshes itself, this shows progress */
async function pollStatus(once) {
  clearTimeout(pollStatus.t);
  try {
    const s = await get('/api/status');
    const was = state.status?.job?.running;
    state.status = s;
    const el = $('#status');
    let text, cls = '';
    const wait = (s.job.message || '').match(/waiting until (\d\d:\d\d)/);
    if (!s.keys) { text = 'connect data sources'; cls = 'warn'; }
    else if (s.job.running && wait) { text = `paused until ${wait[1]} (free data limit) · ${s.job.done}/${s.job.total}`; cls = 'busy'; }
    else if (s.job.running) { text = `fetching ${s.job.done}/${s.job.total}${s.job.ticker ? ' · ' + s.job.ticker : ''}`; cls = 'busy'; }
    else if (s.pending) { text = `${s.pending} stocks waiting for data`; cls = 'warn'; }
    else { text = `data ${s.asOf || 'none yet'} · refresh`; }
    el.innerHTML = `<span class="dot"></span>${esc(text)}`;
    el.className = `status ${cls}`;
    el.title = s.job.error ? `last refresh: ${s.job.error}` : 'fetch new prices and filings';
    if (was && !s.job.running) {
      toast(s.job.error ? `Refresh finished with a problem: ${s.job.error}` : 'Data updated', s.job.error ? 'err' : 'ok');
      route();
    }
  } catch { /* server stopped; keep the page */ }
  if (once !== true) pollStatus.t = setTimeout(pollStatus, state.status?.job?.running ? 4000 : 30000);
}
$('#status').addEventListener('click', () => {
  if (!state.status?.keys) { location.hash = 'settings'; return; }
  if (!state.status.job.running) run(() => send('POST', '/api/refresh'), 'Fetching new data in the background');
});

/* shell */
const RENDER = { portfolio: viewPortfolio, watchlist: viewWatchlist, journal: viewJournal, signals: viewSignals, orders: viewOrders, screen: viewScreen, settings: viewSettings };

function drawNav() {
  $('#nav').innerHTML = VIEWS.map((v, i) => `<a href="#${v}" ${v === state.view ? 'aria-current="page"' : ''}><span class="n">0${i + 1}</span>${v}</a>`).join('');
  const listKeys = state.view === 'watchlist' ? '<span class="hint"><kbd>j/k</kbd>move</span><span class="hint"><kbd>b</kbd>buy</span><span class="hint"><kbd>h</kbd>rule hits</span>' : '';
  const filterKey = state.view === 'watchlist' || state.view === 'screen' ? '<span class="hint"><kbd>/</kbd>filter</span>' : '';
  $('#hints').innerHTML = VIEWS.map((v, i) => `<span class="hint opt"><kbd>${i + 1}</kbd>${v}</span>`).join('') + listKeys + filterKey +
    '<span class="hint"><kbd>?</kbd>help</span>';
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
    main.innerHTML = `<p class="flag">${esc(e.message)}</p><p class="muted">Is the app still running? Start it again with Suspt.command.</p>`;
  }
}

document.addEventListener('keydown', (e) => {
  const help = $('#help');
  if ($('#trade').open) return; // the dialog handles its own keys
  const typing = e.target.matches('input, textarea, select');
  if (e.key === 'Escape') {
    if (!help.hidden) { help.hidden = true; return; }
    if (typing) { e.target.blur(); }
    return;
  }
  if (typing || e.metaKey || e.ctrlKey || e.altKey) return;
  if (e.key === '?') { help.hidden = !help.hidden; return; }
  if (!help.hidden) return;
  const n = Number(e.key);
  if (n >= 1 && n <= VIEWS.length) { location.hash = VIEWS[n - 1]; return; }
  if (e.key === 'r') { route(); return; }
  if (e.key === 'h' && state.view === 'watchlist') { state.hits = !state.hits; showTicker(); return; }
  if (e.key === 'b' && state.view === 'watchlist') { const r = visibleRows()[state.sel]; if (r) openTrade(r.ticker, 'buy').catch((err) => toast(err.message, 'err')); return; }
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
$('#help-btn').addEventListener('click', () => ($('#help').hidden = false));

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
    el.innerHTML = `<span class="dot"></span>US market open · closes in ${hms(CLOSE - secs)}`;
    return;
  }
  const order = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
  let wait = 0, idx = order.indexOf(day);
  if (weekday && secs < OPEN) wait = OPEN - secs;
  else {
    wait = 24 * 3600 - secs + OPEN;
    idx = (idx + 1) % 7;
    while (idx === 0 || idx === 6) { wait += 24 * 3600; idx = (idx + 1) % 7; }
  }
  el.className = 'clock';
  el.innerHTML = `<span class="dot"></span>US market closed · opens in ${hms(wait)}`;
}

window.addEventListener('hashchange', route);
setInterval(tick, 1000);
tick();
route();
pollStatus();
