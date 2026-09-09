"use strict";

(() => {
  // ---------- 常量 ----------

  // 维度顺序与后端 dimensions 的 8 个键一致。
  const DIMENSIONS = ["day", "hour", "weekday", "month", "client", "model", "provider", "project"];
  const TIME_DIMS = ["day", "hour", "weekday", "month"];
  const PIE_DIMS = ["client", "model", "provider", "project"];

  // 五类 token 的固定识别色:KPI 构成条带、堆叠柱、图例全程共用同一套,
  // 页面只教一次图例。key 与 /api/dashboard 的 totals 字段名一致。
  const METRICS = [
    { key: "fresh_input",  label: "Fresh input",  zh: "新输入",  color: "#5cc8ff" },
    { key: "output",       label: "Output",       zh: "输出",    color: "#ffb454" },
    { key: "cache_read",   label: "Cache read",   zh: "缓存读",  color: "#57d9a3" },
    { key: "cache_create", label: "Cache create", zh: "缓存写",  color: "#7a9eff" },
    { key: "reasoning",    label: "Reasoning",    zh: "推理",    color: "#d98ce5" }
  ];

  // 指标条列元数据:键为载荷 columns 里的 query 指标 ID,field 是 totals 里的
  // 取数字段。指标条按 columns 的顺序与可见性动态渲染(total/requests/
  // cache_hit 由 KPI 行承载,不在条内),与用户配置的 query 输出列完全一致。
  const STRIP_FIELDS = {
    input:        { field: "fresh_input",  label: "Input",        zh: "输入",   color: "#5cc8ff" },
    output:       { field: "output",       label: "Output",       zh: "输出",   color: "#ffb454" },
    cache_read:   { field: "cache_read",   label: "Cache read",   zh: "缓存读", color: "#57d9a3" },
    cache_create: { field: "cache_create", label: "Cache create", zh: "缓存写", color: "#7a9eff" },
    reasoning:    { field: "reasoning",    label: "Reasoning",    zh: "推理",   color: "#d98ce5" }
  };

  // 配置可见的 token 类别(METRICS 子集):每次载荷到达时由 columns 重推导,
  // 构成条带/图例/按天堆叠段/类别图例全部据此渲染——配置列的可见性与顺序
  // 完整反映到页面。
  let activeKindSet = null;
  function syncActiveKinds(columns) {
    if (!columns) return;
    const ids = new Set(columns);
    activeKindSet = new Set();
    METRICS.forEach((m) => {
      const colId = m.key === "fresh_input" ? "input" : m.key;
      if (ids.has(colId)) activeKindSet.add(m.key);
    });
  }
  function activeMetrics() {
    return activeKindSet ? METRICS.filter((m) => activeKindSet.has(m.key)) : METRICS;
  }

  // 占比类维度(环形图)的分类色板。
  const PIE_PALETTE = [
    "#5cc8ff", "#ffb454", "#57d9a3", "#d98ce5", "#ff8a66", "#7a9eff",
    "#ffd166", "#63d3ff", "#9ae6b4", "#f2789f", "#b3a4ff", "#8ce0db"
  ];

  // 热力色阶:0 → 无数据底色,其余按 sqrt(v/max) 分 6 档。
  const HEAT_STEPS = ["#12324a", "#1b4d6e", "#26719c", "#3898c6", "#5cc8ff", "#a5e6ff"];

  const SVG_NS = "http://www.w3.org/2000/svg";

  const state = {
    from: "",
    to: "",
    auto: 30000, // 默认 30s
    timer: null,
    loading: false,
    dirty: false, // 加载期间范围又变化,结束后补一次加载
    meta: null
  };

  const $ = (id) => document.getElementById(id);

  // ---------- 格式化 ----------

  // 千分位整数,与明细表 td.num 的 data-v 精确值配套。
  function formatInt(v) {
    return String(v).replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  }

  // 镜像 internal/querier/querier.go 的 formatTokens:
  // >=1e9 → %.2f B;>=1e6 → %.2f M;>=1e3 → %.2f K;否则原样整数。
  function formatTokens(tokens) {
    if (tokens >= 1000000000) return (tokens / 1000000000).toFixed(2) + " B";
    if (tokens >= 1000000) return (tokens / 1000000).toFixed(2) + " M";
    if (tokens >= 1000) return (tokens / 1000).toFixed(2) + " K";
    return String(tokens);
  }

  // duration_ms → 1h 2m / 3m 4s / 45s / <1s
  function formatDuration(ms) {
    if (typeof ms !== "number" || !isFinite(ms) || ms < 1000) return "<1s";
    const total = Math.floor(ms / 1000);
    const h = Math.floor(total / 3600);
    const m = Math.floor((total % 3600) / 60);
    const s = total % 60;
    if (h > 0) return m > 0 ? h + "h " + m + "m" : h + "h";
    if (m > 0) return m + "m " + s + "s";
    return s + "s";
  }

  // 缓存命中率,镜像 querier.formatCacheHit:
  // cache_read / (fresh_input + cache_read + cache_create),两位小数;无输入 0.00%。
  function cacheHit(freshInput, cacheRead, cacheCreate) {
    const denom = freshInput + cacheRead + cacheCreate;
    if (denom <= 0) return 0;
    return (cacheRead * 100) / denom;
  }

  // 变化率:基线为 0 时返回 null(调用方显示 "–")。
  function changePct(cur, base) {
    if (!base || base <= 0) return null;
    return ((cur - base) * 100) / base;
  }

  // ---------- 日期工具(全部本地时区) ----------

  function pad2(n) {
    return (n < 10 ? "0" : "") + n;
  }

  function toDateStr(d) {
    return d.getFullYear() + "-" + pad2(d.getMonth() + 1) + "-" + pad2(d.getDate());
  }

  function todayStr() {
    return toDateStr(new Date());
  }

  function addDaysStr(dateStr, n) {
    const y = Number(dateStr.slice(0, 4));
    const m = Number(dateStr.slice(5, 7));
    const d = Number(dateStr.slice(8, 10));
    return toDateStr(new Date(y, m - 1, d + n));
  }

  function firstOfMonthStr() {
    return todayStr().slice(0, 8) + "01";
  }

  // 服务端(/api/dashboard)的区间天数上限,前端预检同口径。
  const RANGE_LIMIT = 366;

  // spanDays 统计闭区间天数(含两端):按 UTC 解析避免 DST 偏移。
  function spanDays(from, to) {
    const a = new Date(from + "T00:00:00Z");
    const b = new Date(to + "T00:00:00Z");
    return Math.round((b - a) / 86400000) + 1;
  }

  // ---------- tooltip ----------

  // WeakMap:渲染随自动刷新反复重建图表元素,弱引用让旧元素可被回收,
  // 避免 tooltip 内容随挂机时长无界累积。
  const tipStore = new WeakMap(); // element → {title, rows:[{color,label,value}]}

  function setTip(el, content) {
    el.dataset.tip = "1";
    tipStore.set(el, content);
  }

  function tipRow(color, label, value) {
    return { color, label, value };
  }

  function showTooltip(content, x, y) {
    const tip = $("tooltip");
    tip.textContent = "";
    if (content.title) {
      const t = document.createElement("div");
      t.className = "tt-title";
      t.textContent = content.title;
      tip.appendChild(t);
    }
    (content.rows || []).forEach((r) => {
      const row = document.createElement("div");
      row.className = "tt-row";
      if (r.color) {
        const i = document.createElement("i");
        i.style.background = r.color;
        row.appendChild(i);
      }
      const lab = document.createElement("span");
      lab.textContent = r.label;
      row.appendChild(lab);
      const b = document.createElement("b");
      b.textContent = r.value;
      row.appendChild(b);
      tip.appendChild(row);
    });
    tip.hidden = false;
    const rect = tip.getBoundingClientRect();
    let left = x + 14;
    let top = y + 16;
    if (left + rect.width > window.innerWidth - 8) left = x - rect.width - 12;
    if (top + rect.height > window.innerHeight - 8) top = y - rect.height - 12;
    tip.style.left = Math.max(4, left) + "px";
    tip.style.top = Math.max(4, top) + "px";
  }

  function hideTooltip() {
    $("tooltip").hidden = true;
  }

  function initTooltip() {
    document.addEventListener("mouseover", (e) => {
      const el = e.target.closest && e.target.closest("[data-tip]");
      if (el && tipStore.has(el)) showTooltip(tipStore.get(el), e.clientX, e.clientY);
      else if (!el) hideTooltip();
    });
    document.addEventListener("mousemove", (e) => {
      const el = e.target.closest && e.target.closest("[data-tip]");
      if (el && tipStore.has(el) && !$("tooltip").hidden) showTooltip(tipStore.get(el), e.clientX, e.clientY);
    });
    document.addEventListener("scroll", hideTooltip, true);
  }

  // ---------- SVG 基元 ----------

  function svgEl(tag, attrs) {
    const el = document.createElementNS(SVG_NS, tag);
    for (const k in attrs) el.setAttribute(k, attrs[k]);
    return el;
  }

  // y 轴取整:向上取到 1/2/2.5/5 × 10^k,网格线数值好看。
  function niceMax(v) {
    if (v <= 0) return 1;
    const exp = Math.pow(10, Math.floor(Math.log10(v)));
    for (const m of [1, 2, 2.5, 5, 10]) {
      if (v <= m * exp) return m * exp;
    }
    return 10 * exp;
  }

  // ---------- 柱状图(单列或五类堆叠) ----------

  // rows: [{label, requests, fresh_input, output, cache_read, cache_create, reasoning, total}]
  // stacked=true 时按 METRICS 分段着色,否则单色;onLabelClick 提供柱条点击
  // 钻取(按天/按月图把点击的桶设为选区),null 表示不可点。
  function renderBarChart(container, rows, stacked, onLabelClick) {
    container.textContent = "";
    if (!rows.length) {
      container.appendChild(emptyNote());
      return;
    }
    const maxRow = Math.max(...rows.map((r) => r.total || 0));
    if (maxRow <= 0) {
      // 区间内全为零时空坐标轴没有信息量,直接给无数据提示。
      container.appendChild(emptyNote("no data in range / 区间内无数据"));
      return;
    }
    const W = 720, H = 240;
    const padL = 52, padR = 8, padT = 12, padB = 24;
    const plotW = W - padL - padR;
    const plotH = H - padT - padB;
    const svg = svgEl("svg", { viewBox: `0 0 ${W} ${H}`, role: "img" });

    const maxVal = niceMax(Math.max(...rows.map((r) => r.total || 0)));
    const yOf = (v) => padT + plotH - (v / maxVal) * plotH;

    // 网格线与 y 轴刻度(0/50%/100% 共 5 条,首尾必有)。
    for (let i = 0; i <= 4; i++) {
      const v = (maxVal * i) / 4;
      const y = yOf(v);
      svg.appendChild(svgEl("line", {
        x1: padL, x2: W - padR, y1: y, y2: y,
        stroke: i === 0 ? "#31435a" : "#223041", "stroke-width": 1
      }));
      const lab = svgEl("text", {
        x: padL - 8, y: y + 3.5, "text-anchor": "end",
        fill: "#5b6e80", "font-size": 9.5
      });
      // 轴标签去掉缩写后的小数零(750.00 M → 750 M),网格更清爽。
      lab.textContent = formatTokens(v).replace(/\.00\s(?=[KMB])/g, " ");
      svg.appendChild(lab);
    }

    const n = rows.length;
    const slot = plotW / n;
    const barW = Math.max(2, Math.min(slot * 0.7, 44));
    // X 轴标签:采样密度按像素间隙自适应(≥30px 才落一个标签),双语标签
    // 取中文短段(如 "Monday / 周一" → "周一"),末标签恒绘制且无近邻挤压。
    const labelStep = Math.max(Math.ceil(30 / slot), Math.ceil(n / 14));
    const nearEnd = (i) => (n - 1 - i) * slot < 30;
    rows.forEach((r, i) => {
      const x = padL + slot * i + (slot - barW) / 2;
      let acc = 0;
      const segEls = [];
      if (stacked) {
        activeMetrics().forEach((m) => {
          const v = r[m.key] || 0;
          if (v <= 0) return;
          const y0 = yOf(acc);
          const y1 = yOf(acc + v);
          const rect = svgEl("rect", {
            x, y: y1, width: barW, height: Math.max(0.5, y0 - y1),
            fill: m.color, rx: Math.min(2, barW / 4)
          });
          svg.appendChild(rect);
          segEls.push(tipRow(m.color, m.label + " " + m.zh, formatTokens(v)));
          acc += v;
        });
      } else {
        const v = r.total || 0;
        if (v > 0) {
          const y0 = yOf(v);
          const rect = svgEl("rect", {
            x, y: y0, width: barW, height: Math.max(0.5, padT + plotH - y0),
            fill: "#5cc8ff", rx: Math.min(2, barW / 4)
          });
          svg.appendChild(rect);
        }
        segEls.push(tipRow("#5cc8ff", "Total 总量", formatTokens(v)));
      }

      // 悬停命中区:整列高,柱体不变色,只出 tooltip;提供 onLabelClick 时
      // 柱条可点击钻取(整列命中区也可点)。
      const hit = svgEl("rect", {
        x: padL + slot * i, y: padT, width: slot, height: plotH,
        fill: "transparent"
      });
      const tip = {
        title: r.key,
        rows: [...segEls, tipRow(null, "Requests 请求", formatInt(r.requests || 0))]
      };
      if (onLabelClick) {
        tip.rows.push(tipRow(null, "▸ click to focus / 点击聚焦", shortLabel(r.key)));
        hit.style.cursor = "pointer";
        hit.addEventListener("click", () => onLabelClick(r.key));
      }
      setTip(hit, tip);
      svg.appendChild(hit);

      if ((i % labelStep === 0 && !nearEnd(i)) || i === n - 1) {
        const lab = svgEl("text", {
          x: padL + slot * i + slot / 2, y: H - 8, "text-anchor": "middle",
          fill: "#5b6e80", "font-size": 9.5
        });
        lab.textContent = shortLabel(axisShort(r.key || ""));
        svg.appendChild(lab);
      }
    });
    container.appendChild(svg);
  }

  // 按周聚合阈值:超过该天数按 ISO 周聚合(周一为首)。
  const WEEKLY_ROLLUP_THRESHOLD = 92;

  // rollupWeeks 把逐日行按 ISO 周(周一为首)聚合:七类指标与请求数求和,
  // key 取周起始日;首尾周天然为残周(由选区边界决定)。输出保持周起始日
  // 升序,字段名与维度行一致(key/requests/…/total),可直接进 renderBarChart。
  function rollupWeeks(rows) {
    const SUM_KEYS = ["requests", "fresh_input", "output", "cache_read", "cache_create", "reasoning", "total"];
    const out = [];
    const byWeek = new Map();
    rows.forEach((r) => {
      // 服务端 day 键按契约恒为 YYYY-MM-DD;渲染层对越界形态容灾跳过。
      if (!/^\d{4}-\d{2}-\d{2}$/.test(r.key || "")) return;
      const d = new Date(r.key + "T00:00:00");
      const wk = new Date(d);
      wk.setDate(wk.getDate() - ((d.getDay() + 6) % 7)); // 回退到本周周一
      const k = toDateStr(wk);
      let agg = byWeek.get(k);
      if (!agg) {
        agg = { key: k };
        SUM_KEYS.forEach((sk) => { agg[sk] = 0; });
        byWeek.set(k, agg);
        out.push(agg);
      }
      SUM_KEYS.forEach((sk) => { agg[sk] += r[sk] || 0; });
    });
    return out;
  }

  function shortLabel(s) {
    // 日期标签 "2026-09-08" → "09-08";其余原样。
    if (/^\d{4}-\d{2}-\d{2}$/.test(s)) return s.slice(5);
    return s;
  }

  // axisShort 取双语标签的中文段("Monday / 周一" → "周一"),压缩轴标签宽度;
  // 无 " / " 分隔的标签原样返回。
  function axisShort(s) {
    const k = s.indexOf(" / ");
    return k >= 0 ? s.slice(k + 3) : s;
  }

  function emptyNote(text) {
    const div = document.createElement("div");
    div.className = "chart-empty";
    div.textContent = text || "no data / 无数据";
    return div;
  }

  // ---------- 环形图 ----------

  // rows 同柱状图;返回 donut svg + HTML 图例,悬停双向联动。
  function renderDonut(container, rows) {
    container.textContent = "";
    const data = rows
      .filter((r) => (r.total || 0) > 0)
      .sort((a, b) => (b.total || 0) - (a.total || 0));
    if (!data.length) {
      container.appendChild(emptyNote());
      return;
    }
    // 超 9 项合并为 Other,防小扇区淹没图例。
    let slices = data;
    let other = 0;
    if (data.length > 9) {
      slices = data.slice(0, 9);
      data.slice(9).forEach((r) => { other += r.total || 0; });
    }
    const grand = data.reduce((s, r) => s + (r.total || 0), 0);

    const wrap = document.createElement("div");
    wrap.style.display = "flex";
    wrap.style.flexDirection = "column";
    wrap.style.alignItems = "center";
    wrap.style.gap = "10px";
    wrap.style.width = "100%";

    const size = 190, cx = size / 2, cy = size / 2, R = 82, r = 52;
    const svg = svgEl("svg", {
      viewBox: `0 0 ${size} ${size}`, width: size, height: size, role: "img",
      style: "width:100%;max-width:190px;height:auto;display:block"
    });
    const centerTotal = document.createElementNS(SVG_NS, "text");
    const centerLabel = document.createElementNS(SVG_NS, "text");

    const sliceEls = [];
    let a0 = -Math.PI / 2;
    slices.forEach((row, i) => {
      const frac = (row.total || 0) / grand;
      const a1 = a0 + frac * Math.PI * 2;
      const color = PIE_PALETTE[i % PIE_PALETTE.length];
      const path = svgEl("path", {
        d: arcPath(cx, cy, R, r, a0, Math.min(a1, a0 + Math.PI * 2 - 0.0001)),
        fill: color, stroke: "#131a22", "stroke-width": 2
      });
      const content = {
        title: row.key,
        rows: [
          tipRow(color, "Total 总量", formatTokens(row.total || 0)),
          tipRow(null, "Share 占比", (frac * 100).toFixed(1) + "%"),
          tipRow(null, "Requests 请求", formatInt(row.requests || 0))
        ]
      };
      setTip(path, content);
      path.style.cursor = "default";
      path.addEventListener("mouseenter", () => highlight(i, true));
      path.addEventListener("mouseleave", () => highlight(i, false));
      svg.appendChild(path);
      sliceEls.push({ path, color });
      a0 = a1;
    });
    if (other > 0) {
      const frac = other / grand;
      const a1 = a0 + frac * Math.PI * 2;
      const color = PIE_PALETTE[9 % PIE_PALETTE.length];
      const path = svgEl("path", {
        d: arcPath(cx, cy, R, r, a0, Math.min(a1, a0 + Math.PI * 2 - 0.0001)),
        fill: color, stroke: "#131a22", "stroke-width": 2
      });
      setTip(path, {
        title: "Other 其他",
        rows: [tipRow(color, "Total 总量", formatTokens(other)), tipRow(null, "Share 占比", (frac * 100).toFixed(1) + "%")]
      });
      path.addEventListener("mouseenter", () => highlight(9, true));
      path.addEventListener("mouseleave", () => highlight(9, false));
      svg.appendChild(path);
      sliceEls.push({ path, color });
    }

    centerTotal.setAttribute("x", cx);
    centerTotal.setAttribute("y", cy);
    centerTotal.setAttribute("text-anchor", "middle");
    centerTotal.setAttribute("fill", "#dee8f2");
    centerTotal.setAttribute("font-size", "15");
    centerTotal.setAttribute("font-weight", "700");
    centerTotal.textContent = formatTokens(grand);
    centerLabel.setAttribute("x", cx);
    centerLabel.setAttribute("y", cy + 14);
    centerLabel.setAttribute("text-anchor", "middle");
    centerLabel.setAttribute("fill", "#5b6e80");
    centerLabel.setAttribute("font-size", "8.5");
    centerLabel.textContent = "TOTAL TOKENS";
    svg.appendChild(centerTotal);
    svg.appendChild(centerLabel);
    wrap.appendChild(svg);

    const legend = document.createElement("div");
    legend.style.width = "100%";
    legend.style.display = "flex";
    legend.style.flexDirection = "column";
    legend.style.gap = "3px";

    const legendRows = [];
    const addLegendRow = (label, color, value, frac, idx) => {
      const row = document.createElement("div");
      row.style.display = "flex";
      row.style.alignItems = "center";
      row.style.gap = "7px";
      row.style.fontSize = "11px";
      row.style.padding = "2px 6px";
      row.style.borderRadius = "5px";
      const dot = document.createElement("i");
      dot.style.cssText = `width:8px;height:8px;border-radius:2px;background:${color};flex:none`;
      row.appendChild(dot);
      const name = document.createElement("span");
      name.textContent = label;
      name.style.cssText = "overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:#8ca0b4";
      row.appendChild(name);
      const val = document.createElement("b");
      val.textContent = (frac * 100).toFixed(1) + "%";
      val.style.cssText = "margin-left:auto;color:#dee8f2;font-weight:600;font-variant-numeric:tabular-nums";
      row.appendChild(val);
      row.title = value;
      row.addEventListener("mouseenter", () => highlight(idx, true));
      row.addEventListener("mouseleave", () => highlight(idx, false));
      legend.appendChild(row);
      legendRows[idx] = row;
    };
    slices.forEach((row, i) => {
      addLegendRow(row.key, PIE_PALETTE[i % PIE_PALETTE.length], formatTokens(row.total || 0), (row.total || 0) / grand, i);
    });
    if (other > 0) addLegendRow("Other 其他", PIE_PALETTE[9 % PIE_PALETTE.length], formatTokens(other), other / grand, 9);

    function highlight(idx, on) {
      sliceEls.forEach((s, i) => {
        s.path.style.opacity = on && i !== idx ? 0.35 : 1;
      });
      legendRows.forEach((row, i) => {
        if (!row) return;
        row.style.background = on && i === idx ? "rgba(92,200,255,.09)" : "";
      });
    }

    wrap.appendChild(legend);
    container.appendChild(wrap);
  }

  // 环形弧线路径:a0/a1 为弧度;接近整圆时拆两个半弧避免 path 退化。
  function arcPath(cx, cy, R, r, a0, a1) {
    if (a1 - a0 >= Math.PI * 2 - 0.001) a1 = a0 + Math.PI * 2 - 0.001;
    const large = a1 - a0 > Math.PI ? 1 : 0;
    const x0o = cx + R * Math.cos(a0), y0o = cy + R * Math.sin(a0);
    const x1o = cx + R * Math.cos(a1), y1o = cy + R * Math.sin(a1);
    const x1i = cx + r * Math.cos(a1), y1i = cy + r * Math.sin(a1);
    const x0i = cx + r * Math.cos(a0), y0i = cy + r * Math.sin(a0);
    return `M ${x0o} ${y0o} A ${R} ${R} 0 ${large} 1 ${x1o} ${y1o}` +
      ` L ${x1i} ${y1i} A ${r} ${r} 0 ${large} 0 ${x0i} ${y0i} Z`;
  }

  // ---------- 热力图 ----------

  // hm: {weekdays:[7], hours:[24], values:[[7]×[24]]}
  function renderHeatmap(container, legendEl, hm) {
    container.textContent = "";
    legendEl.textContent = "";
    const values = (hm && hm.values) || [];
    if (values.length !== 7 || !values[0] || values[0].length !== 24) {
      container.appendChild(emptyNote());
      return;
    }
    let max = 0;
    values.forEach((row) => row.forEach((v) => { if (v > max) max = v; }));

    // 行/列合计(对齐 query heatmap 终端视图的尾列/尾行语义):行合计=星期
    // 全天总量,列合计=小时跨七天总量,右下角为全表总量。
    const hourSums = new Array(24).fill(0);
    let grand = 0;
    const daySums = values.map((row) => {
      let s = 0;
      row.forEach((v, hi) => {
        s += v;
        hourSums[hi] += v;
        grand += v;
      });
      return s;
    });
    let hourMax = 0;
    hourSums.forEach((v) => { if (v > hourMax) hourMax = v; });

    const grid = document.createElement("div");
    grid.className = "hm-grid";
    grid.appendChild(Object.assign(document.createElement("div"), { className: "hm-corner" }));
    for (let h = 0; h < 24; h++) {
      const lab = document.createElement("div");
      lab.className = "hm-x";
      // 每 2 小时显示一个刻度,奇数列占位保持网格对齐。
      lab.style.visibility = h % 2 === 0 ? "visible" : "hidden";
      lab.textContent = (hm.hours && hm.hours[h]) || String(h).padStart(2, "0") + ":00";
      grid.appendChild(lab);
    }
    grid.appendChild(Object.assign(document.createElement("div"), { className: "hm-x" }));

    // 热力取色:相对给定最大值走同一色阶;空值回落到底色。
    const heat = (v, maxV) => {
      if (v <= 0 || maxV <= 0) return null;
      const t = Math.sqrt(v / maxV);
      return HEAT_STEPS[Math.min(HEAT_STEPS.length - 1, Math.floor(t * HEAT_STEPS.length))];
    };

    values.forEach((row, wi) => {
      const y = document.createElement("div");
      y.className = "hm-y";
      y.textContent = shortWeekday((hm.weekdays && hm.weekdays[wi]) || "");
      grid.appendChild(y);
      row.forEach((v, hi) => {
        const cell = document.createElement("div");
        cell.className = "hm-cell";
        const bg = heat(v, max);
        if (bg) cell.style.background = bg;
        setTip(cell, {
          title: ((hm.weekdays && hm.weekdays[wi]) || "") + " " + ((hm.hours && hm.hours[hi]) || pad2(hi) + ":00"),
          rows: [
            tipRow(null, "Total 总量", formatTokens(v)),
            tipRow(null, "Weekday total 该日全天", formatTokens(daySums[wi] || 0))
          ]
        });
        grid.appendChild(cell);
      });
      // 行合计列:该星期全天总量(文本)。
      const total = document.createElement("div");
      total.className = "hm-total";
      total.textContent = formatTokens(daySums[wi]);
      total.title = formatInt(daySums[wi]);
      grid.appendChild(total);
    });

    // 合计行:各小时跨七天总量(按小时最大值取色)+ 右下角全表总量。
    const totalY = document.createElement("div");
    totalY.className = "hm-y";
    totalY.textContent = "Total";
    grid.appendChild(totalY);
    hourSums.forEach((v, hi) => {
      const cell = document.createElement("div");
      cell.className = "hm-cell hm-sum";
      const bg = heat(v, hourMax);
      if (bg) cell.style.background = bg;
      setTip(cell, {
        title: (hm.hours && hm.hours[hi]) || pad2(hi) + ":00",
        rows: [tipRow(null, "Total 总量", formatTokens(v))]
      });
      grid.appendChild(cell);
    });
    const grandCell = document.createElement("div");
    grandCell.className = "hm-total";
    grandCell.textContent = formatTokens(grand);
    grandCell.title = formatInt(grand);
    grandCell.style.fontWeight = "700";
    grid.appendChild(grandCell);

    // 首行即小时刻度行(奇数列占位保持网格对齐),其后每行=星期标签+24 格。
    container.appendChild(grid);

    // 图例:0 → max 渐变块。
    if (max > 0) {
      const frag = document.createDocumentFragment();
      const zero = document.createElement("span");
      zero.textContent = "0";
      frag.appendChild(zero);
      HEAT_STEPS.forEach((c) => {
        const i = document.createElement("i");
        i.style.background = c;
        frag.appendChild(i);
      });
      const top = document.createElement("span");
      top.textContent = formatTokens(max);
      frag.appendChild(top);
      legendEl.appendChild(frag);
    }
  }

  function shortWeekday(s) {
    // "周一 Mon" 形态取前两字符中文;纯 ASCII 取前 3 字符。
    if (/^[\u4e00-\u9fa5]/.test(s)) return s.slice(0, 2);
    return s.slice(0, 3);
  }

  // ---------- 状态条与 meta ----------

  function metaReady() {
    return !!(state.meta && state.meta.min_date && state.meta.max_date);
  }

  function renderStatus() {
    const el = $("status-line");
    const m = state.meta;
    el.textContent = "";
    el.title = "data through 数据截至 · last collection 最近采集";
    const rows = [
      ["数据截至 data thru", m && m.data_through],
      ["最近采集 collected", m && m.last_collection]
    ];
    rows.forEach(([label, value]) => {
      const row = document.createElement("span");
      row.className = "status-row";
      const lab = document.createElement("span");
      lab.className = "status-label";
      lab.textContent = label;
      const b = document.createElement("b");
      b.textContent = value || "–";
      row.appendChild(lab);
      row.appendChild(b);
      el.appendChild(row);
    });
  }

  function applyMeta(m) {
    state.meta = m && typeof m === "object" ? m : null;
    $("ver-chip").textContent = (state.meta && state.meta.version) || "–";
    document.querySelector('[data-preset="all"]').disabled = !metaReady();
    renderStatus();
  }

  // ---------- KPI ----------

  function deltaChip(el, cur, base, suffix, title) {
    el.textContent = "";
    el.title = title || "";
    const p = changePct(cur, base);
    const span = document.createElement("span");
    if (p === null) {
      span.className = "flat";
      span.textContent = suffix ? "– " + suffix : "–";
    } else {
      span.className = p >= 0 ? "up" : "down";
      span.textContent = (p >= 0 ? "▲ " : "▼ ") + Math.abs(p).toFixed(1) + "%" + (suffix ? " " + suffix : "");
    }
    el.appendChild(span);
  }

  function renderKpis(totals, compare, columns) {
    const base = (compare && compare.totals) || {};
    // KPI 行随配置列显隐:total/requests/cache_hit 配置了才展示。
    const hasColumn = (id) => !columns || columns.includes(id);
    $("kpi-total-card").hidden = !hasColumn("total");
    $("kpi-requests-card").hidden = !hasColumn("requests");
    $("kpi-hit-card").hidden = !hasColumn("cache_hit");
    const baseLabel = compare && compare.base_start ? "vs " + compare.base_start + " ~ " + compare.base_end : "";
    $("kpi-total").textContent = formatTokens(totals.total || 0);
    $("kpi-total").title = formatInt(totals.total || 0);
    deltaChip($("kpi-total-delta"), totals.total || 0, base.total || 0, baseLabel);

    $("kpi-requests").textContent = formatInt(totals.requests || 0);
    deltaChip($("kpi-requests-delta"), totals.requests || 0, base.requests || 0, "", baseLabel);
    $("kpi-days").textContent = formatInt(totals.active_days || 0);
    deltaChip($("kpi-days-delta"), totals.active_days || 0, base.active_days || 0, "", baseLabel);
    const hit = cacheHit(totals.fresh_input || 0, totals.cache_read || 0, totals.cache_create || 0);
    const baseHit = cacheHit(base.fresh_input || 0, base.cache_read || 0, base.cache_create || 0);
    $("kpi-hit").textContent = hit.toFixed(2) + "%";
    // 命中率变化用百分点差,不用百分比变化。
    const pp = hit - baseHit;
    const hitEl = $("kpi-hit-delta");
    hitEl.textContent = "";
    hitEl.title = baseLabel;
    const span = document.createElement("span");
    if (Math.abs(pp) < 0.005) {
      span.className = "flat";
      span.textContent = "± 0.00pp";
    } else {
      span.className = pp > 0 ? "up" : "down";
      span.textContent = (pp > 0 ? "▲ " : "▼ ") + Math.abs(pp).toFixed(2) + "pp";
    }
    hitEl.appendChild(span);

    // 指标条:按载荷 columns(query 配置的输出列)动态渲染,去掉已升格 KPI 的
    // requests/total/cache_hit——剩余 token 类别与 query 可见列一致。
    const strip = $("metric-strip");
    strip.textContent = "";
    (columns || []).forEach((id) => {
      const def = STRIP_FIELDS[id];
      if (!def) return;
      const v = totals[def.field] || 0;
      const card = document.createElement("div");
      card.className = "card metric";
      const dot = document.createElement("div");
      dot.className = "dot";
      dot.style.background = def.color;
      const label = document.createElement("div");
      label.className = "card-label";
      label.textContent = def.label + " " + def.zh;
      const value = document.createElement("div");
      value.className = "metric-value";
      value.textContent = formatTokens(v);
      value.title = formatInt(v);
      const delta = document.createElement("div");
      delta.className = "delta";
      // 基线窗口日期只进 title,防长标签挤爆小卡片。
      deltaChip(delta, v, base[def.field] || 0, "", baseLabel);
      card.appendChild(dot);
      card.appendChild(label);
      card.appendChild(value);
      card.appendChild(delta);
      strip.appendChild(card);
    });

    // 五类构成条带 + 图例(图例只列实际有量的类别)。
    const ribbon = $("kpi-ribbon");
    const legend = $("kpi-ribbon-legend");
    ribbon.textContent = "";
    legend.textContent = "";
    const grand = totals.total || 0;
    activeMetrics().forEach((m) => {
      const v = totals[m.key] || 0;
      if (grand > 0 && v > 0) {
        const seg = document.createElement("span");
        seg.style.cssText = `width:${(v * 100) / grand}%;background:${m.color}`;
        setTip(seg, {
          title: m.label + " " + m.zh,
          rows: [tipRow(m.color, "Total 总量", formatTokens(v)), tipRow(null, "Share 占比", ((v * 100) / grand).toFixed(1) + "%")]
        });
        ribbon.appendChild(seg);
      }
      if (v <= 0) return; // 图例只列实际有量的类别,恒零字段不占位
      const lg = document.createElement("span");
      lg.className = "lg";
      const dot = document.createElement("i");
      dot.style.background = m.color;
      lg.appendChild(dot);
      lg.appendChild(document.createTextNode(m.label + " "));
      const b = document.createElement("b");
      b.textContent = ((v * 100) / grand).toFixed(1) + "%";
      lg.appendChild(b);
      legend.appendChild(lg);
    });
  }

  // ---------- 对比表 / 预估表 ----------

  function renderCompare(c, range, curDayRows) {
    if (!c) return;
    const meta = $("compare-meta");
    meta.textContent = "";
    meta.appendChild(document.createTextNode("Current 当前 "));
    let b = document.createElement("b");
    b.textContent = ((range && range.from) || "–") + " .. " + ((range && range.to) || "–");
    meta.appendChild(b);
    meta.appendChild(document.createTextNode(" · Base 基线(等长前一窗口) "));
    b = document.createElement("b");
    b.textContent = (c.base_start || "–") + " .. " + (c.base_end || "–");
    meta.appendChild(b);

    renderCompareChart($("chart-compare"), curDayRows || [], c.daily || []);

    const tbody = $("compare-body");
    tbody.textContent = "";
    (c.rows || []).forEach((r) => {
      const tr = document.createElement("tr");
      addCell(tr, r.label || "");
      addCell(tr, r.current || "", "num");
      addCell(tr, r.base || "", "num muted");
      const cls = r.change_class === "pos" ? "num pos" : r.change_class === "neg" ? "num neg" : "num flat";
      addCell(tr, r.change || "", cls);
      addCell(tr, r.change_pct || "", "num");
      tbody.appendChild(tr);
    });
  }

  // ---------- 两期逐日对比曲线 ----------

  // renderCompareChart 在环比面板绘制当前区间与基线窗口的逐日总量折线:
  // 两条线按「窗口内第 N 天」对齐(两侧日期不同),悬停给出两侧日期与数值。
  function renderCompareChart(container, curRows, baseRows) {
    container.textContent = "";
    if (curRows.length < 2 || curRows.length !== baseRows.length) {
      container.appendChild(emptyNote("insufficient span / 跨度不足"));
      return;
    }
    const W = 720, H = 200;
    const padL = 52, padR = 8, padT = 10, padB = 24;
    const plotW = W - padL - padR;
    const plotH = H - padT - padB;
    const svg = svgEl("svg", { viewBox: `0 0 ${W} ${H}`, role: "img" });

    const maxVal = niceMax(Math.max(
      Math.max(...curRows.map((r) => r.total || 0)),
      Math.max(...baseRows.map((r) => r.total || 0))
    ));
    const yOf = (v) => padT + plotH - (v / maxVal) * plotH;
    for (let i = 0; i <= 4; i++) {
      const v = (maxVal * i) / 4;
      const y = yOf(v);
      svg.appendChild(svgEl("line", {
        x1: padL, x2: W - padR, y1: y, y2: y,
        stroke: i === 0 ? "#31435a" : "#223041", "stroke-width": 1
      }));
      const lab = svgEl("text", {
        x: padL - 8, y: y + 3.5, "text-anchor": "end",
        fill: "#5b6e80", "font-size": 9.5
      });
      lab.textContent = formatTokens(v).replace(/\.00\s(?=[KMB])/g, " ");
      svg.appendChild(lab);
    }

    const n = curRows.length;
    const xOf = (i) => padL + (n === 1 ? plotW / 2 : (plotW * i) / (n - 1));
    const line = (rows) => rows
      .map((r, i) => `${xOf(i).toFixed(1)},${yOf(r.total || 0).toFixed(1)}`)
      .join(" ");
    svg.appendChild(svgEl("polyline", {
      points: line(baseRows), fill: "none",
      stroke: "#8ca0b4", "stroke-width": 1.5, "stroke-dasharray": "5 4"
    }));
    svg.appendChild(svgEl("polyline", {
      points: line(curRows), fill: "none",
      stroke: "#5cc8ff", "stroke-width": 2
    }));

    // 悬停命中区:整列,tooltip 同时给两侧日期与数值。
    const slot = plotW / (n - 1 || 1);
    const labelStep = Math.ceil(n / 8);
    curRows.forEach((r, i) => {
      const hit = svgEl("rect", {
        x: padL + slot * i - slot / 2, y: padT, width: slot, height: plotH,
        fill: "transparent"
      });
      setTip(hit, {
        title: "Day " + (i + 1) + " / 第 " + (i + 1) + " 天",
        rows: [
          tipRow("#5cc8ff", "Current 当前 " + shortLabel(r.key || ""), formatTokens(r.total || 0)),
          tipRow("#8ca0b4", "Base 基线 " + shortLabel((baseRows[i] && baseRows[i].key) || ""), formatTokens((baseRows[i] && baseRows[i].total) || 0))
        ]
      });
      svg.appendChild(hit);
      if (i % labelStep === 0 || i === n - 1) {
        // 首标签左对齐、末标签右对齐防裁剪;与末标签相邻的两个采样点跳过,
        // 避免中锚标签与右锚末标签挤压粘连。
        if ((i === n - 2 || i === n - 3) && n > 3) return;
        const anchor = i === 0 ? "start" : i === n - 1 ? "end" : "middle";
        const x = i === 0 ? padL : i === n - 1 ? W - padR : xOf(i);
        const lab = svgEl("text", {
          x, y: H - 8, "text-anchor": anchor,
          fill: "#5b6e80", "font-size": 9.5
        });
        lab.textContent = shortLabel(r.key || "");
        svg.appendChild(lab);
      }
    });
    container.appendChild(svg);
  }

  function renderForecast(f) {
    if (!f) return;
    $("forecast-today").textContent = f.today_so_far || "–";
    const tbody = $("forecast-body");
    tbody.textContent = "";
    (f.rows || []).forEach((r) => {
      const tr = document.createElement("tr");
      addCell(tr, r.label || "");
      addCell(tr, r.total || "", "num");
      addCell(tr, r.avg_day || "", "num");
      addCell(tr, r.active || "", "num");
      const est = document.createElement("td");
      est.className = "num";
      est.style.fontWeight = "700";
      est.textContent = r.estimate || "";
      tr.appendChild(est);
      tbody.appendChild(tr);
    });
  }

  function addCell(tr, text, className) {
    const td = document.createElement("td");
    td.textContent = text;
    if (className) td.className = className;
    tr.appendChild(td);
    return td;
  }

  function emptyRow(tbody, colSpan) {
    const tr = document.createElement("tr");
    const td = document.createElement("td");
    td.colSpan = colSpan;
    td.className = "empty";
    td.textContent = "no data / 无数据";
    tr.appendChild(td);
    tbody.appendChild(tr);
  }

  // ---------- 会话排行 ----------

  function renderSessions(list) {
    const tbody = $("sessions-body");
    tbody.textContent = "";
    const cnt = $("count-sessions");
    if (cnt) cnt.textContent = list.length + " rows";
    const btn = $("export-sessions");
    if (btn) btn.disabled = list.length === 0;
    if (!list.length) {
      emptyRow(tbody, 8);
      return;
    }
    const maxTotal = Math.max(...list.map((s) => s.total || 0));
    list.forEach((s, i) => {
      const tr = document.createElement("tr");
      const rank = addCell(tr, String(i + 1), "muted");
      rank.dataset.v = String(i + 1);
      const title = addCell(tr, s.title || "(untitled / 无标题)", "title-cell");
      title.title = s.title || "";
      const client = addCell(tr, s.client || "–");
      client.innerHTML = "";
      const tag = document.createElement("span");
      tag.className = "tag";
      tag.textContent = s.client || "–";
      client.appendChild(tag);
      const proj = addCell(tr, s.project || "–");
      proj.innerHTML = "";
      const ptag = document.createElement("span");
      ptag.className = "tag";
      ptag.textContent = s.project || "–";
      proj.appendChild(ptag);
      const dur = addCell(tr, formatDuration(s.duration_ms || 0), "num muted");
      dur.dataset.v = String(s.duration_ms || 0);
      const req = addCell(tr, formatInt(s.requests || 0), "num");
      req.dataset.v = String(s.requests || 0);
      const tot = addCell(tr, formatTokens(s.total || 0), "num");
      tot.dataset.v = String(s.total || 0);
      tot.title = formatInt(s.total || 0);
      const bar = document.createElement("td");
      const track = document.createElement("span");
      track.className = "bar-track";
      const fill = document.createElement("span");
      fill.className = "bar-fill";
      fill.style.width = (maxTotal > 0 ? ((s.total || 0) * 100) / maxTotal : 0) + "%";
      track.appendChild(fill);
      bar.appendChild(track);
      tr.appendChild(bar);
      tbody.appendChild(tr);
    });
    reapplySort(tbody);
  }

  // ---------- 数据明细(折叠块) ----------

  const DIM_TITLES = {
    day: ["Day", "按天"], hour: ["Hour", "按小时"], weekday: ["Weekday", "按星期"],
    month: ["Month", "按月"], client: ["Client", "客户端"], model: ["Model", "模型"],
    provider: ["Provider", "供应商"], project: ["Project", "项目"]
  };

  function buildDataBlocks() {
    const root = $("data-blocks");
    root.textContent = "";
    DIMENSIONS.forEach((dim) => {
      const d = document.createElement("details");
      d.className = "data-block";
      d.id = "block-" + dim;
      const summary = document.createElement("summary");
      const name = document.createElement("span");
      name.textContent = DIM_TITLES[dim][0] + " ";
      const zh = document.createElement("span");
      zh.className = "muted";
      zh.textContent = DIM_TITLES[dim][1];
      name.appendChild(zh);
      const right = document.createElement("span");
      right.style.display = "inline-flex";
      right.style.alignItems = "center";
      right.style.gap = "10px";
      const exportBtn = document.createElement("button");
      exportBtn.type = "button";
      exportBtn.className = "export-btn";
      exportBtn.id = "export-" + dim;
      exportBtn.textContent = "Export CSV";
      exportBtn.title = "export current rows as CSV / 按当前排序导出 CSV";
      // 阻止冒泡:点导出不应折叠/展开明细块。
      exportBtn.addEventListener("click", (e) => {
        e.preventDefault();
        e.stopPropagation();
        exportDimensionCSV(dim);
      });
      const cnt = document.createElement("span");
      cnt.className = "cnt";
      cnt.id = "count-" + dim;
      cnt.textContent = "0 rows";
      right.appendChild(exportBtn);
      right.appendChild(cnt);
      summary.appendChild(name);
      summary.appendChild(right);
      const wrap = document.createElement("div");
      wrap.className = "tbl-wrap";
      const table = document.createElement("table");
      const thead = document.createElement("thead");
      const hrow = document.createElement("tr");
      ["Key", "Requests", "Fresh input", "Output", "Cache read", "Cache create", "Reasoning", "Total"].forEach((h, i) => {
        const th = document.createElement("th");
        th.className = i === 0 ? "sortable" : "sortable num";
        th.textContent = h;
        hrow.appendChild(th);
      });
      thead.appendChild(hrow);
      const tbody = document.createElement("tbody");
      tbody.id = "tbl-" + dim;
      table.appendChild(thead);
      table.appendChild(tbody);
      wrap.appendChild(table);
      d.appendChild(summary);
      d.appendChild(wrap);
      root.appendChild(d);
    });
  }

  function renderDimension(dim, rows) {
    const count = $("count-" + dim);
    if (count) count.textContent = rows.length + " rows";
    const exportBtn = $("export-" + dim);
    if (exportBtn) exportBtn.disabled = rows.length === 0;
    const tbody = $("tbl-" + dim);
    tbody.textContent = "";
    if (!rows.length) {
      emptyRow(tbody, 8);
      return;
    }
    rows.forEach((r) => {
      const tr = document.createElement("tr");
      addCell(tr, r.key || "–");
      [["requests", formatInt], ["fresh_input", formatInt], ["output", formatInt],
       ["cache_read", formatInt], ["cache_create", formatInt], ["reasoning", formatInt],
       ["total", formatInt]].forEach(([k, fmt]) => {
        const td = addCell(tr, fmt(r[k] || 0), "num");
        td.dataset.v = String(r[k] || 0);
      });
      tbody.appendChild(tr);
    });
    reapplySort(tbody);
  }

  // ---------- CSV 导出 ----------

  // csvField 按 RFC 4180 转义:含逗号/引号/换行的字段加引号并翻倍内部引号。
  function csvField(v) {
    const s = String(v);
    if (/[",\r\n]/.test(s)) return '"' + s.replace(/"/g, '""') + '"';
    return s;
  }

  // downloadCSV 统一下载:CRLF 行尾 + UTF-8 BOM(便于 Excel 识别),临时
  // <a download> 触发后释放对象 URL。
  function downloadCSV(lines, filename) {
    const blob = new Blob(["\uFEFF" + lines.join("\r\n") + "\r\n"], { type: "text/csv;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }

  // exportDimensionCSV 把该维度明细表按当前排序所见导出为 CSV:键列取文本,
  // 数值列取 data-v 的原始整数(与 export 命令的机器 schema 同为精确值);
  // 文件名含维度与区间。
  function exportDimensionCSV(dim) {
    const tbody = $("tbl-" + dim);
    if (!tbody) return;
    if (tbody.querySelector("td.empty")) return;
    const header = ["Key", "Requests", "Fresh input", "Output", "Cache read", "Cache create", "Reasoning", "Total"];
    const lines = [header.map(csvField).join(",")];
    Array.prototype.forEach.call(tbody.rows, (tr) => {
      const fields = [tr.cells[0].textContent.trim()];
      for (let i = 1; i < tr.cells.length; i++) {
        const td = tr.cells[i];
        fields.push(td.dataset.v !== undefined && td.dataset.v !== "" ? td.dataset.v : "0");
      }
      lines.push(fields.map(csvField).join(","));
    });
    downloadCSV(lines, "token-usage-" + dim + "-" + state.from + "_" + state.to + ".csv");
  }

  // exportSessionsCSV 导出会话排行:Duration 列取 data-v 毫秒原值,文本列
  // 原样转义;条形列(末列)不参与。
  function exportSessionsCSV() {
    const tbody = $("sessions-body");
    if (!tbody || tbody.querySelector("td.empty")) return;
    const header = ["Rank", "Title", "Client", "Project", "DurationMS", "Requests", "Total"];
    const lines = [header.map(csvField).join(",")];
    Array.prototype.forEach.call(tbody.rows, (tr) => {
      const num = (i) => (tr.cells[i] && tr.cells[i].dataset.v) || "0";
      const text = (i) => (tr.cells[i] ? tr.cells[i].textContent.trim() : "");
      lines.push([text(0), text(1), text(2), text(3), num(4), num(5), num(6)].map(csvField).join(","));
    });
    downloadCSV(lines, "token-usage-sessions-" + state.from + "_" + state.to + ".csv");
  }

  // ---------- 图表装配与区间自适应 ----------

  function renderCharts(data) {
    const dims = data.dimensions || {};
    const singleDay = state.from === state.to;

    // 按天:堆叠柱;单日或不足两桶时退化为提示(小时视图接管日内分布)。
    // 点击柱条钻取:把点击的那一天设为选区;超过 92 天(约一个季度)自动
    // 按 ISO 周聚合(周一为首),长区间柱群更可读,此时禁用逐日钻取。
    const dayRows = dims.day || [];
    setNote("note-day", "");
    if (singleDay || dayRows.length < 2) {
      $("card-day").hidden = true;
    } else if (dayRows.length > WEEKLY_ROLLUP_THRESHOLD) {
      $("card-day").hidden = false;
      renderBarChart($("chart-day"), rollupWeeks(dayRows), true, null);
      setNote("note-day", "aggregated by week (Mon) / 超过 92 天按周聚合");
      renderKindLegend($("legend-day"));
    } else {
      $("card-day").hidden = false;
      renderBarChart($("chart-day"), dayRows, true, (label) => setRange(label, label, null));
      setNote("note-day", "click a bar to focus that day / 点击柱条聚焦该日");
      renderKindLegend($("legend-day"));
    }

    // 按小时:仅单日选区展示(多日区间的日内分布被按天/按周聚合覆盖);
    // 按星期:单日选区无意义,隐藏。
    $("card-hour").hidden = !singleDay;
    if (singleDay) renderBarChart($("chart-hour"), dims.hour || [], false);
    $("card-weekday").hidden = singleDay;
    if (!singleDay) renderBarChart($("chart-weekday"), dims.weekday || [], false);

    // 按月:跨度不足两个自然月时同样退化;点击柱条聚焦该月整月。
    const monthRows = dims.month || [];
    setNote("note-month", "");
    if (monthRows.length < 2) {
      $("card-month").hidden = true;
    } else {
      $("card-month").hidden = false;
      renderBarChart($("chart-month"), monthRows, false, monthLabelRange);
      setNote("note-month", "click a bar to focus that month / 点击柱条聚焦该月");
    }

    // 趋势区仅剩一张可见卡时占满整行,避免 2 列网格留空槽。
    const visibleTrend = ["card-day", "card-hour", "card-weekday", "card-month"]
      .filter((id) => !$(id).hidden).length;
    document.querySelector("#trends .grid").classList.toggle("single-col", visibleTrend === 1);

    PIE_DIMS.forEach((dim) => renderDonut($("chart-" + dim), dims[dim] || []));
    renderHeatmap($("chart-heatmap"), $("hm-legend"), data.heatmap);
  }

  // monthLabelRange 把 "YYYY-MM" 月桶映射为整月选区;当月终点钳到今天,
  // 防止把未来日期发成 400。
  function monthLabelRange(label) {
    if (!/^\d{4}-\d{2}$/.test(label)) return;
    const y = Number(label.slice(0, 4));
    const m = Number(label.slice(5, 7));
    const from = label + "-01";
    const last = new Date(y, m, 0);
    let to = toDateStr(last);
    const today = todayStr();
    if (to > today) to = today;
    setRange(from, to, null);
  }

  function renderKindLegend(container) {
    container.textContent = "";
    activeMetrics().forEach((m) => {
      const lg = document.createElement("span");
      lg.className = "lg";
      const dot = document.createElement("i");
      dot.style.background = m.color;
      lg.appendChild(dot);
      lg.appendChild(document.createTextNode(m.label + " " + m.zh));
      container.appendChild(lg);
    });
  }

  function setNote(id, text) {
    const el = $(id);
    if (el) el.textContent = text;
  }

  // 单日选区:退化的环比表隐藏(KPI 增减 chips 仍指向基线窗口);活跃天数
  // 恒为 1、星期×小时热力图退化为单行,均无信息量,一并隐藏。
  function applyRangeMode() {
    const single = state.from === state.to;
    document.body.classList.toggle("single-day", single);
    const daysCard = $("kpi-days-card");
    if (daysCard) daysCard.hidden = single;
    const heatSection = document.getElementById("heatmap");
    if (heatSection) heatSection.hidden = single;
  }

  function renderDashboard(data) {
    applyRangeMode();
    syncActiveKinds(data.columns);
    renderKpis(data.totals || {}, data.compare, data.columns);
    renderCompare(data.compare, data.range, (data.dimensions || {}).day || []);
    renderForecast(data.forecast);
    renderSessions(data.sessions || []);
    const dims = data.dimensions || {};
    DIMENSIONS.forEach((dim) => renderDimension(dim, dims[dim] || []));
    renderCharts(data);
  }

  // ---------- 载荷缓存:消除刷新时的空窗闪烁 ----------

  // 上一次成功渲染的载荷存 sessionStorage:刷新/重开页面时先立即渲染缓存
  // 数据(无空窗),后台拉到新数据后再整体替换。缓存与选区一起存取,范围
  // 不一致时不用缓存(避免拿旧选区的数据渲染新选区)。
  const CACHE_KEY = "tu-payload";
  function savePayloadCache(from, to, data) {
    try {
      sessionStorage.setItem(CACHE_KEY, JSON.stringify({ from, to, data }));
    } catch (_) {
      // 存储不可用(隐私模式/配额)时静默降级:刷新回落到占位符渲染。
    }
  }
  function loadPayloadCache() {
    try {
      return JSON.parse(sessionStorage.getItem(CACHE_KEY) || "null");
    } catch (_) {
      return null;
    }
  }

  // ---------- 排序(数值列读 data-v,文本列读 textContent) ----------

  // sortState 记住每个表(以 tbody id 为键)的排序列与方向:自动刷新会整体
  // 重绘行,若不回放,用户排好的序每 30s 被打回服务端行序。
  const sortState = new Map(); // tbody id → {idx, desc}

  function sortRows(tbody, idx, desc) {
    const table = tbody.closest("table");
    const valueOf = (tr) => {
      const td = tr.cells[idx];
      if (!td) return "";
      if (td.dataset.v !== undefined && td.dataset.v !== "") return parseFloat(td.dataset.v);
      return td.textContent.trim();
    };
    const rows = Array.prototype.slice.call(tbody.rows);
    rows.sort((a, b) => {
      const va = valueOf(a);
      const vb = valueOf(b);
      let c;
      if (typeof va === "number" && typeof vb === "number") c = va - vb;
      else c = String(va).localeCompare(String(vb), "zh-Hans-CN");
      return desc ? -c : c;
    });
    rows.forEach((r) => tbody.appendChild(r));
    Array.prototype.forEach.call(table.querySelectorAll("th.sortable"), (h) => {
      h.classList.remove("sorted-asc", "sorted-desc");
    });
    const th = table.querySelectorAll("thead th")[idx];
    if (th) th.classList.add(desc ? "sorted-desc" : "sorted-asc");
  }

  function sortTable(th) {
    const table = th.closest("table");
    if (!table) return;
    const tbody = table.tBodies[0];
    if (!tbody || tbody.rows.length <= 1) return;
    const idx = th.cellIndex;
    const desc = !th.classList.contains("sorted-desc");
    if (tbody.id) sortState.set(tbody.id, { idx, desc });
    sortRows(tbody, idx, desc);
  }

  // reapplySort 在数据重绘后回放该表记住的排序;无记录则保持服务端行序。
  function reapplySort(tbody) {
    if (!tbody || !tbody.id || !sortState.has(tbody.id)) return;
    const { idx, desc } = sortState.get(tbody.id);
    sortRows(tbody, idx, desc);
  }

  // ---------- fetch ----------

  async function errorMessage(res) {
    try {
      const body = await res.json();
      if (body && body.error && body.error.message) return body.error.message;
    } catch (_) {
      // 响应体非 JSON,退回状态码
    }
    return "HTTP " + res.status;
  }

  async function fetchJSON(url) {
    const res = await fetch(url);
    if (!res.ok) throw new Error(await errorMessage(res));
    return res.json();
  }

  function errText(reason) {
    if (reason instanceof TypeError) return "network error / 网络错误";
    return reason && reason.message ? reason.message : String(reason);
  }

  function showNoticeLoading() {
    const notice = $("notice");
    notice.classList.add("show", "loading");
    notice.textContent = "loading / 加载中…";
  }

  function hideNotice() {
    const notice = $("notice");
    notice.classList.remove("show", "loading");
    notice.textContent = "";
  }

  function showError(messages) {
    const notice = $("notice");
    notice.classList.remove("loading");
    notice.classList.add("show");
    notice.textContent = "";
    const span = document.createElement("span");
    span.textContent = messages.join(" · ");
    notice.appendChild(span);
    const btn = document.createElement("button");
    btn.type = "button";
    btn.textContent = "retry / 重试";
    btn.addEventListener("click", () => requestLoad());
    notice.appendChild(btn);
  }

  // ---------- 加载 ----------

  function setBusy(busy) {
    document.querySelectorAll("#presets button").forEach((b) => {
      b.disabled = busy ? true : b.dataset.preset === "all" && !metaReady();
    });
    $("refresh-btn").disabled = busy;
    document.body.classList.toggle("loading", busy);
  }

  async function load() {
    if (state.loading) {
      state.dirty = true;
      return;
    }
    state.loading = true;
    state.dirty = false;
    setBusy(true);
    // 加载过程静默:旧渲染(或缓存渲染)保持原样,不弹横幅;错误才提示。
    document.body.classList.add("loading");
    const qs = "from=" + encodeURIComponent(state.from) + "&to=" + encodeURIComponent(state.to);
    // 仅 dashboard + meta 两个请求并行;图表全部由前端按数值载荷自绘,
    // KPI/明细/图表天然同一读事务快照。
    const settled = await Promise.allSettled([
      fetchJSON("/api/dashboard?" + qs),
      fetchJSON("/api/meta")
    ]);
    const errors = [];

    const dashRes = settled[0];
    if (dashRes.status === "fulfilled") {
      try {
        renderDashboard(dashRes.value);
        savePayloadCache(state.from, state.to, dashRes.value);
      } catch (e) {
        // 渲染层异常按加载失败处理,横幅可见且 loading 状态可靠复位。
        errors.push("render: " + errText(e));
      }
    } else {
      errors.push("dashboard: " + errText(dashRes.reason));
    }

    const metaRes = settled[1];
    if (metaRes.status === "fulfilled") {
      applyMeta(metaRes.value);
    } else {
      errors.push("meta: " + errText(metaRes.reason));
    }

    document.body.classList.remove("loading");
    if (errors.length) showError(errors);
    else hideNotice();
    state.loading = false;
    setBusy(false);
    if (state.dirty) load();
  }

  function requestLoad() {
    if (state.loading) {
      state.dirty = true;
      return;
    }
    load();
  }

  // ---------- 范围预设 ----------

  const PRESETS = {
    today: () => {
      const t = todayStr();
      return [t, t];
    },
    d7: () => [addDaysStr(todayStr(), -6), todayStr()],
    d30: () => [addDaysStr(todayStr(), -29), todayStr()],
    d90: () => [addDaysStr(todayStr(), -89), todayStr()],
    month: () => [firstOfMonthStr(), todayStr()],
    // 「全部」= meta 的 min_date..max_date(meta 未就绪时按钮本就禁用)。
    all: () => [state.meta.min_date, state.meta.max_date]
  };

  function setRange(from, to, presetName) {
    if (from > to) {
      const tmp = from;
      from = to;
      to = tmp;
    }
    state.from = from;
    state.to = to;
    $("date-from").value = from;
    $("date-to").value = to;
    // presetName 为 null 表示手改 inputs,清除全部预设高亮。
    document.querySelectorAll("[data-preset]").forEach((b) => {
      b.classList.toggle("active", b.dataset.preset === presetName);
    });
    // 记住选区:下次打开恢复(刷新/重开不丢上下文)。
    try {
      localStorage.setItem("tu-range", JSON.stringify({ from, to }));
    } catch (_) {
      // 隐私模式等存储不可用场景静默降级为不记忆。
    }
    requestLoad();
  }

  function applyPreset(name) {
    if (name === "all" && !metaReady()) return;
    const maker = PRESETS[name];
    if (!maker) return;
    const range = maker();
    if (!range[0] || !range[1]) return;
    // 服务端对区间有 366 天上限,「全部」随数据积累迟早越界:预检给出
    // 双语提示,不发必然 400 的请求。
    if (spanDays(range[0], range[1]) > RANGE_LIMIT) {
      showError(["range " + range[0] + ".." + range[1] + " exceeds the " + RANGE_LIMIT +
        "-day limit; pick a shorter range / 区间 " + range[0] + ".." + range[1] +
        " 超过 " + RANGE_LIMIT + " 天上限,请改用更短范围"]);
      return;
    }
    setRange(range[0], range[1], name);
  }

  function onDateInput() {
    const from = $("date-from").value;
    const to = $("date-to").value;
    if (!from || !to) return;
    setRange(from, to, null);
  }

  // ---------- 自动刷新 ----------

  function resetTimer() {
    if (state.timer) {
      clearInterval(state.timer);
      state.timer = null;
    }
    if (state.auto > 0) {
      state.timer = setInterval(() => {
        if (document.hidden) return; // 后台标签页跳过轮询
        load();
      }, state.auto);
    }
  }

  // ---------- 导航滚动高亮 ----------

  // 用滚动位置判定当前章节(顶栏下方判定线以上最近的 section)。轮询而非
  // 监听 scroll:嵌 WebView/自动化环境下 scroll 事件可能不派发、rAF 可能被
  // 节流,而 400ms 一次的 6 次几何读取开销可忽略且在任何环境都能工作。
  function initNavHighlight() {
    const links = Array.prototype.slice.call(document.querySelectorAll(".bar-inner nav a"));
    if (!links.length) return;
    const ids = links.map((a) => a.getAttribute("href").slice(1));
    const update = () => {
      const line = 140;
      let current = ids[0];
      ids.forEach((id) => {
        const sec = document.getElementById(id);
        if (sec && !sec.hidden && sec.getBoundingClientRect().top <= line) current = id;
      });
      links.forEach((a) => {
        a.classList.toggle("active", a.getAttribute("href").slice(1) === current);
      });
    };
    update();
    setInterval(update, 400);
  }

  // ---------- 初始化 ----------

  function init() {
    buildDataBlocks();
    initTooltip();
    document.querySelectorAll("[data-preset]").forEach((b) => {
      b.addEventListener("click", () => applyPreset(b.dataset.preset));
    });
    $("date-from").addEventListener("change", onDateInput);
    $("date-to").addEventListener("change", onDateInput);
    $("refresh-btn").addEventListener("click", () => requestLoad());
    $("auto-select").addEventListener("change", () => {
      state.auto = Number($("auto-select").value) || 0;
      resetTimer();
    });
    // 表格排序:统一事件委托。
    document.addEventListener("click", (e) => {
      const th = e.target.closest("th.sortable");
      if (th) sortTable(th);
    });
    $("export-sessions").addEventListener("click", exportSessionsCSV);
    // 回到前台时重新对齐计时,避免恢复后立刻撞上滞留的旧周期。
    document.addEventListener("visibilitychange", () => {
      if (!document.hidden && state.auto > 0) resetTimer();
    });
    initNavHighlight();

    // 初始范围:首次进入默认 Today;恢复的上次选区若恰为某预设区间,
    // 回亮对应预设按钮,否则清除全部预设高亮。
    let initialFrom = todayStr();
    let initialTo = todayStr();
    let initialPreset = "today";
    try {
      const saved = JSON.parse(localStorage.getItem("tu-range") || "null");
      if (saved && /^\d{4}-\d{2}-\d{2}$/.test(saved.from) && /^\d{4}-\d{2}-\d{2}$/.test(saved.to) && saved.from <= saved.to) {
        initialFrom = saved.from;
        initialTo = saved.to;
        initialPreset = null;
        for (const name of Object.keys(PRESETS)) {
          if (name === "all" && !metaReady()) continue;
          const r = PRESETS[name]();
          if (r[0] === initialFrom && r[1] === initialTo) { initialPreset = name; break; }
        }
      }
    } catch (_) {
      // 存储不可用或内容损坏,回退默认 Today。
    }
    setRange(initialFrom, initialTo, initialPreset);
    resetTimer();

    // 缓存渲染:有与当前选区一致的缓存载荷时立即渲染(消除刷新空窗),
    // 随后 load() 拉到新数据会整体替换;形状异常(旧载荷)则弃用缓存。
    const cached = loadPayloadCache();
    if (cached && cached.from === state.from && cached.to === state.to && cached.data) {
      try {
        renderDashboard(cached.data);
      } catch (_) {
        sessionStorage.removeItem(CACHE_KEY);
      }
    }
  }

  init();
})();
