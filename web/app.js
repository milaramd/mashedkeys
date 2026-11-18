const fmt = new Intl.NumberFormat();
const fmt1 = new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 });
const $ = id => document.getElementById(id);
const DAY = 86400;
let rangeDays = 30;
try { const v = +localStorage.getItem("days"); if ([7, 30, 90, 365].includes(v)) rangeDays = v; } catch {}

// ---------- data ----------
async function get(url, signal) {
  const r = await fetch(url, { signal });
  if (!r.ok) throw new Error(r.status + " " + url);
  return r.json();
}
const pad = n => String(n).padStart(2, "0");
const dayKey = d => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
const midnight = (d = new Date()) => new Date(d.getFullYear(), d.getMonth(), d.getDate());

// range is the selected number of days, captured so the response renders with the range it was asked for.
async function load(range, signal) {
  const now = Math.floor(Date.now() / 1000) + 60;
  const today = midnight();
  const from = midnight(new Date(today.getFullYear(), today.getMonth(), today.getDate() - range + 1));
  const weeksFrom = new Date(today.getFullYear(), today.getMonth(), today.getDate() - (today.getDay() + 6) % 7 - 77);
  const daysFrom = Math.min(from, weeksFrom);
  const [sum, days, hoursRange] = await Promise.all([
    get("/api/summary", signal),
    get(`/api/series?period=day&from=${daysFrom / 1000}&to=${now}`, signal),
    get(`/api/series?period=hour&from=${from / 1000}&to=${now}`, signal),
  ]);
  // Today's hours are the tail of the range's hourly series (range >= 7 days).
  const hoursToday = hoursRange.filter(p => p.period.startsWith(dayKey(today)));
  return { range, sum, days, hoursToday, hoursRange, from, today, weeksFrom };
}

// Words per minute of active typing; null when there's too little typing to say.
const MIN_TYPING_MS = 30000;
const wpm = b => b.interval_sum_ms >= MIN_TYPING_MS ? b.words * 60000 / b.interval_sum_ms : null;
const dur = ms => { const m = Math.round(ms / 60000); return m >= 60 ? `${Math.floor(m / 60)}h ${m % 60}m` : `${m}m`; };
const wpmTip = (b, when) => {
  const v = wpm(b);
  return (v === null ? `<b>—</b> under 30s of typing` : `<b>${Math.round(v)}</b> wpm · ${fmt.format(b.words)} words in ${dur(b.interval_sum_ms)}`) +
    `<br><span class="m">${when}</span>`;
};
const ZERO = { keys: 0, words: 0, backspaces: 0, interval_sum_ms: 0 };

// ---------- tiles ----------
function tiles(s) {
  const t = s.today, b = s.since_boot, a = s.all_time;
  const up = (Date.now() - new Date(s.boot_time)) / 1000;
  const upStr = up > DAY ? `${fmt1.format(up / DAY)} days` : `${fmt1.format(up / 3600)} h`;
  const bsPct = t.keys ? (100 * t.backspaces / t.keys) : 0;
  const cards = [
    ["hero", "Keys today", fmt.format(t.keys), `${fmt.format(t.enters)} enters`],
    ["", "Words today", fmt.format(t.words), `${dur(t.interval_sum_ms)} typing today`],
    ["", "Since boot", fmt.format(b.keys), `${fmt.format(b.words)} words · up ${upStr}`],
    ["", "Speed today", wpm(t) === null ? "—" : `${Math.round(wpm(t))} wpm`,
      `${wpm(b) === null ? "—" : Math.round(wpm(b))} wpm since boot · ${dur(b.interval_sum_ms)} typing`],
    ["", "Backspace today", `${fmt1.format(bsPct)}%`, `${fmt.format(t.backspaces)} backspaces · ${fmt.format(t.deletes)} deletes`],
    ["", "All time", fmt.format(a.keys), `${fmt.format(a.words)} words`],
  ];
  $("tiles").innerHTML = cards.map(([cls, l, v, sub]) =>
    `<div class="tile ${cls}"><div class="label">${l}</div><div class="value">${v}</div><div class="sub">${sub}</div></div>`).join("");
}

// ---------- tooltip ----------
const tip = $("tip");
function showTip(e, html) {
  tip.innerHTML = html; tip.style.display = "block";
  const r = (e.target.getBoundingClientRect ? e.target.getBoundingClientRect() : null);
  let x = e.clientX ?? (r.left + r.width / 2), y = e.clientY ?? r.top;
  const w = tip.offsetWidth, h = tip.offsetHeight;
  x = Math.min(Math.max(8, x - w / 2), innerWidth - w - 8);
  y = y - h - 12 < 8 ? y + 16 : y - h - 12;
  tip.style.left = x + "px"; tip.style.top = y + "px";
}
const hideTip = () => tip.style.display = "none";

// ---------- charts ----------
const NS = "http://www.w3.org/2000/svg";
// Smallest "nice" max (4 equal ticks of 1/2/2.5/5 x 10^n) that fits v.
function niceMax(v) {
  if (v <= 0) return 4;
  const step = v / 4, p = 10 ** Math.floor(Math.log10(step)), n = step / p;
  return 4 * (n <= 1 ? 1 : n <= 2 ? 2 : n <= 2.5 ? 2.5 : n <= 5 ? 5 : 10) * Math.max(1, p);
}
const short = v => v >= 1e6 ? fmt1.format(v / 1e6) + "M" : v >= 1e3 ? fmt1.format(v / 1e3) + "k" : String(v);

// Rounded-top bar anchored to the baseline.
function barPath(x, y, w, h) {
  if (h <= 0) return "";
  const r = Math.min(4, w / 2, h);
  return `M${x},${y + h}V${y + r}Q${x},${y} ${x + r},${y}H${x + w - r}Q${x + w},${y} ${x + w},${y + r}V${y + h}Z`;
}

// data: [{label, value, tip}], tickEvery: show every nth x label
function bars(el, data, { height = 180, tickEvery = 1, unit = "" } = {}) {
  if (!data.some(d => d.value > 0)) { el.innerHTML = `<div class="empty">${unit === "wpm" ? "Not enough typing yet" : "Nothing typed yet"}</div>`; return; }
  const W = el.clientWidth, L = 40, R = 4, T = 8, B = 22, H = height;
  const pw = W - L - R, ph = H - T - B;
  const max = niceMax(Math.max(...data.map(d => d.value)));
  const band = pw / data.length, bw = Math.max(1, Math.min(band - 2, band * 0.72, 48));
  let s = `<svg viewBox="0 0 ${W} ${H}" role="img">`;
  for (let i = 0; i <= 4; i++) {
    const y = T + ph - (ph * i) / 4;
    s += `<line class="${i ? "gridline" : "baseline"}" x1="${L}" x2="${W - R}" y1="${y}" y2="${y}"/>`;
    s += `<text x="${L - 6}" y="${y + 4}" text-anchor="end">${short(max * i / 4)}</text>`;
  }
  data.forEach((d, i) => {
    const x = L + i * band, h = (ph * d.value) / max;
    s += `<rect class="hit" data-i="${i}" tabindex="0" x="${x}" y="${T}" width="${band}" height="${ph}"/>`;
    s += `<path class="bar" d="${barPath(x + (band - bw) / 2, T + ph - h, bw, h)}"/>`;
    if (i % tickEvery === 0) s += `<text x="${x + band / 2}" y="${H - 6}" text-anchor="middle">${d.label}</text>`;
  });
  el.innerHTML = s + "</svg>";
  el.querySelectorAll(".hit").forEach(r => {
    const d = data[r.dataset.i];
    const on = e => showTip(e, d.tip);
    r.addEventListener("mousemove", on); r.addEventListener("focus", on);
    r.addEventListener("mouseleave", hideTip); r.addEventListener("blur", hideTip);
  });
}

function heatmap(el, grid, max) { // grid[weekday 0=Mon][hour]
  const W = el.clientWidth, L = 36, T = 4, B = 20, cols = 24, rows = 7;
  const cw = (W - L) / cols, ch = Math.min(26, Math.max(14, cw * 0.8)), H = T + rows * ch + B;
  const names = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];
  const step = v => v <= 0 ? 0 : Math.min(5, 1 + Math.floor(4 * v / (max || 1)));
  let s = `<svg viewBox="0 0 ${W} ${H}" role="img">`;
  for (let r = 0; r < rows; r++) {
    s += `<text x="${L - 6}" y="${T + r * ch + ch / 2 + 4}" text-anchor="end">${names[r]}</text>`;
    for (let c = 0; c < cols; c++) {
      const v = grid[r][c];
      s += `<rect class="cell" tabindex="0" data-r="${r}" data-c="${c}" x="${L + c * cw + 1}" y="${T + r * ch + 1}" width="${cw - 2}" height="${ch - 2}" rx="2" fill="var(--seq-${step(v)})"/>`;
    }
  }
  for (let c = 0; c < cols; c += 3) s += `<text x="${L + c * cw + cw / 2}" y="${H - 5}" text-anchor="middle">${pad(c)}</text>`;
  el.innerHTML = s + "</svg>";
  el.querySelectorAll(".cell").forEach(cell => {
    const r = +cell.dataset.r, c = +cell.dataset.c;
    const on = e => { cell.classList.add("on"); showTip(e, `<b>${fmt.format(grid[r][c])}</b> keys<br><span class="m">${names[r]} ${pad(c)}:00–${pad(c + 1)}:00, all weeks</span>`); };
    const off = () => { cell.classList.remove("on"); hideTip(); };
    cell.addEventListener("mousemove", on); cell.addEventListener("focus", on);
    cell.addEventListener("mouseleave", off); cell.addEventListener("blur", off);
  });
  $("heat-scale").innerHTML = `<span>0</span><i style="background:var(--seq-0)"></i>` +
    [1, 2, 3, 4, 5].map(i => `<i style="background:var(--seq-${i})"></i>`).join("") + `<span>${fmt.format(max)} keys (busiest hour)</span>`;
}

function table(el, head, rows) {
  el.innerHTML = `<table><thead><tr>${head.map(h => `<th>${h}</th>`).join("")}</tr></thead><tbody>` +
    rows.map(r => `<tr>${r.map(c => `<td>${typeof c === "number" ? fmt.format(c) : c}</td>`).join("")}</tr>`).join("") + "</tbody></table>";
}

// ---------- render ----------
let last;
function render(d) {
  last = d;
  const { range, sum, days, hoursToday, hoursRange, from, today, weeksFrom } = d;
  tiles(sum);

  // Keys per day: fill missing days with zero.
  const byDay = Object.fromEntries(days.map(p => [p.period, p.stats]));
  const dayRows = [];
  const allDays = [];
  for (let t = new Date(Math.min(from, weeksFrom)); t <= today; t.setDate(t.getDate() + 1)) {
    allDays.push({ k: dayKey(t), date: new Date(t), ...(byDay[dayKey(t)] || ZERO) });
  }
  dayRows.push(...allDays.filter(r => r.date >= from));
  const total = dayRows.reduce((a, r) => a + r.keys, 0);
  const active = dayRows.filter(r => r.keys > 0).length;
  $("days-desc").textContent = `${fmt.format(total)} keys in the last ${range} days · ${fmt.format(active ? Math.round(total / active) : 0)} per active day`;
  const dl = { month: "short", day: "numeric" };
  bars($("days"), dayRows.map(r => ({
    value: r.keys,
    label: r.date.toLocaleDateString(undefined, range > 31 ? { month: "short" } : dl),
    tip: `<b>${fmt.format(r.keys)}</b> keys · ${fmt.format(r.words)} words<br><span class="m">${r.date.toLocaleDateString(undefined, { weekday: "short", ...dl })}</span>`,
  })), { tickEvery: Math.ceil(dayRows.length / Math.max(4, Math.floor($("days").clientWidth / 70))) });
  table($("days-tbl"), ["Day", "Keys", "Words", "Backspaces"], dayRows.slice().reverse().map(r => [r.k, r.keys, r.words, r.backspaces]));

  // Today by hour.
  const byHour = Object.fromEntries(hoursToday.map(p => [+p.period.slice(11, 13), p.stats]));
  const hourRows = [...Array(24).keys()].map(h => ({ h, ...(byHour[h] || ZERO) }));
  bars($("hours"), hourRows.map(r => ({
    value: r.keys, label: pad(r.h),
    tip: `<b>${fmt.format(r.keys)}</b> keys · ${fmt.format(r.words)} words<br><span class="m">${pad(r.h)}:00–${pad(r.h + 1)}:00</span>`,
  })), { tickEvery: 3 });
  table($("hours-tbl"), ["Hour", "Keys", "Words"], hourRows.filter(r => r.keys).map(r => [`${pad(r.h)}:00`, r.keys, r.words]));

  // Typing speed per day / hour / week.
  const dayTick = Math.ceil(dayRows.length / Math.max(4, Math.floor($("wpm-day").clientWidth / 70)));
  bars($("wpm-day"), dayRows.map(r => ({
    value: wpm(r) ?? 0, label: r.date.toLocaleDateString(undefined, range > 31 ? { month: "short" } : dl),
    tip: wpmTip(r, r.date.toLocaleDateString(undefined, { weekday: "short", ...dl })),
  })), { tickEvery: dayTick, unit: "wpm" });
  table($("wpm-day-tbl"), ["Day", "WPM", "Words", "Typing time"],
    dayRows.slice().reverse().filter(r => wpm(r) !== null).map(r => [r.k, Math.round(wpm(r)), r.words, dur(r.interval_sum_ms)]));

  bars($("wpm-hour"), hourRows.map(r => ({
    value: wpm(r) ?? 0, label: pad(r.h), tip: wpmTip(r, `${pad(r.h)}:00–${pad(r.h + 1)}:00`),
  })), { tickEvery: 3, unit: "wpm" });
  table($("wpm-hour-tbl"), ["Hour", "WPM", "Words", "Typing time"],
    hourRows.filter(r => wpm(r) !== null).map(r => [`${pad(r.h)}:00`, Math.round(wpm(r)), r.words, dur(r.interval_sum_ms)]));

  const weeks = [];
  for (const r of allDays.filter(r => r.date >= weeksFrom)) {
    if (!weeks.length || r.date.getDay() === 1) weeks.push({ start: r.date, words: 0, keys: 0, interval_sum_ms: 0 });
    const w = weeks[weeks.length - 1];
    w.words += r.words; w.keys += r.keys; w.interval_sum_ms += r.interval_sum_ms;
  }
  bars($("wpm-week"), weeks.map(w => ({
    value: wpm(w) ?? 0, label: w.start.toLocaleDateString(undefined, dl),
    tip: wpmTip(w, `week of ${w.start.toLocaleDateString(undefined, { weekday: "short", ...dl })}`),
  })), { tickEvery: 3, unit: "wpm" });
  table($("wpm-week-tbl"), ["Week of", "WPM", "Words", "Typing time"],
    weeks.slice().reverse().filter(w => wpm(w) !== null).map(w => [dayKey(w.start), Math.round(wpm(w)), w.words, dur(w.interval_sum_ms)]));

  // Gap histogram (today).
  const gl = ["<100ms", "100–200", "200–500", "0.5–1s", "1–2s", "pause 2s+"];
  const hist = sum.today.hist, hs = hist.reduce((a, b) => a + b, 0) || 1;
  bars($("gaps"), hist.map((v, i) => ({
    value: v, label: gl[i],
    tip: `<b>${fmt.format(v)}</b> presses · ${fmt1.format(100 * v / hs)}%<br><span class="m">${gl[i]} after the previous key</span>`,
  })));
  table($("gaps-tbl"), ["Gap", "Presses", "Share"], hist.map((v, i) => [gl[i], v, fmt1.format(100 * v / hs) + "%"]));

  // Weekday x hour heatmap over the range.
  const grid = [...Array(7)].map(() => Array(24).fill(0));
  for (const p of hoursRange) {
    const dt = new Date(p.period.slice(0, 10) + "T00:00");
    grid[(dt.getDay() + 6) % 7][+p.period.slice(11, 13)] += p.stats.keys;
  }
  const hmax = Math.max(0, ...grid.flat());
  $("heat-desc").textContent = `Keys by weekday and hour, summed over the last ${range} days`;
  heatmap($("heat"), grid, hmax);
  const names = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];
  table($("heat-tbl"), ["Day", ...[...Array(24).keys()].map(pad)], grid.map((r, i) => [names[i], ...r]));
}

// One request set at a time: a new refresh aborts the old one, and only the
// latest may render or report errors. The next poll is scheduled on completion.
let ctl, timer;
async function refresh() {
  clearTimeout(timer);
  ctl?.abort();
  const me = ctl = new AbortController();
  const live = $("live");
  document.querySelectorAll(".chart").forEach(c => c.style.opacity = last ? .6 : 1);
  try {
    const d = await load(rangeDays, me.signal);
    if (me !== ctl) return;
    render(d);
    live.className = "live";
    live.lastElementChild.textContent = "updated " + new Date().toLocaleTimeString();
  } catch (e) {
    if (me !== ctl) return;
    live.className = "live stale";
    live.lastElementChild.textContent = "can't reach the daemon";
  }
  document.querySelectorAll(".chart").forEach(c => c.style.opacity = 1);
  if (!document.hidden) timer = setTimeout(refresh, 15000);
}
document.addEventListener("visibilitychange", () => document.hidden ? clearTimeout(timer) : refresh());

document.querySelectorAll(".filters button").forEach(b => {
  b.setAttribute("aria-pressed", +b.dataset.days === rangeDays);
  b.addEventListener("click", () => {
    rangeDays = +b.dataset.days;
    try { localStorage.setItem("days", rangeDays); } catch {}
    document.querySelectorAll(".filters button").forEach(x => x.setAttribute("aria-pressed", x === b));
    refresh();
  });
});
let rt, lastW = 0;
new ResizeObserver(([e]) => {
  const w = Math.round(e.contentRect.width);
  if (w === lastW) return;
  lastW = w; clearTimeout(rt); rt = setTimeout(() => last && render(last), 100);
}).observe(document.querySelector("main"));
refresh();
