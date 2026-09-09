"use strict";

(() => {
  // 维度顺序与后端 dimensions 的 8 个键一致;heatmap 是第 9 种图。
  const DIMENSIONS = ["day", "hour", "weekday", "month", "client", "model", "provider", "project"];
  const CHART_KINDS = [...DIMENSIONS, "heatmap"];

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
  // >=1000000000 → %.2f B;>=1000000 → %.2f M;>=1000 → %.2f K;否则原样整数。
  // Go 的 %.2f 固定两位小数且不去尾零,toFixed(2) 行为一致。
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

  // spanDays 统计闭区间天数(含两端):按 UTC 解析避免 DST 偏移,
  // YYYY-MM-DD 无时间部分,差值恒为整日数。
  function spanDays(from, to) {
    const a = new Date(from + "T00:00:00Z");
    const b = new Date(to + "T00:00:00Z");
    return Math.round((b - a) / 86400000) + 1;
  }

  // ---------- fetch ----------

  // 4xx 响应体形如 {"error":{"message":"<双语>"}};取不到时退回状态码。
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

  // ---------- 提示横幅(加载中/错误共用) ----------

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
    btn.addEventListener("click", () => {
      requestLoad();
    });
    notice.appendChild(btn);
  }

  // ---------- 状态条 ----------

  function metaReady() {
    return !!(state.meta && state.meta.min_date && state.meta.max_date);
  }

  function renderStatus() {
    const el = $("status-line");
    const m = state.meta;
    const parts = [
      ["range 范围", (state.from || "–") + " ~ " + (state.to || "–")],
      ["data through 数据截至", (m && m.data_through) || "–"],
      ["last collection 最近采集", (m && m.last_collection) || "–"],
      ["version 版本", (m && m.version) || "–"]
    ];
    el.textContent = "";
    parts.forEach((part, i) => {
      if (i > 0) el.appendChild(document.createTextNode("  ·  "));
      el.appendChild(document.createTextNode(part[0] + " "));
      const b = document.createElement("b");
      b.textContent = part[1];
      el.appendChild(b);
    });
  }

  function applyMeta(m) {
    state.meta = m && typeof m === "object" ? m : null;
    document.querySelector('[data-preset="all"]').disabled = !metaReady();
    renderStatus();
  }

  // ---------- 渲染 ----------

  function setKpi(id, value, exact) {
    const el = $(id);
    el.textContent = value;
    el.title = exact || value;
  }

  function renderTokenKpi(id, v) {
    const n = v || 0;
    setKpi(id, formatTokens(n), formatInt(n));
    $(id + "-sub").textContent = formatInt(n);
  }

  function renderTotals(t) {
    const total = (t && t.total) || 0;
    setKpi("kpi-total", formatTokens(total), formatInt(total));
    $("kpi-total-sub").textContent = formatInt(total);
    setKpi("kpi-requests", formatInt((t && t.requests) || 0));
    setKpi("kpi-days", formatInt((t && t.active_days) || 0));
    renderTokenKpi("kpi-fresh", t && t.fresh_input);
    renderTokenKpi("kpi-output", t && t.output);
    renderTokenKpi("kpi-cache-read", t && t.cache_read);
    renderTokenKpi("kpi-cache-create", t && t.cache_create);
    renderTokenKpi("kpi-reasoning", t && t.reasoning);
  }

  function cell(tr, text) {
    const td = document.createElement("td");
    td.textContent = text;
    tr.appendChild(td);
    return td;
  }

  // 数值单元格:data-v 保存精确整数供排序,显示层决定千分位或缩写。
  function numCell(tr, v, display) {
    const td = cell(tr, display);
    td.className = "num";
    td.dataset.v = String(v);
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

  function renderSessions(list) {
    const tbody = $("sessions-body");
    tbody.textContent = "";
    if (!list.length) {
      emptyRow(tbody, 7);
      return;
    }
    list.forEach((s, i) => {
      const tr = document.createElement("tr");
      cell(tr, String(i + 1));
      const title = cell(tr, s.title || "(untitled / 无标题)");
      title.className = "title-cell";
      title.title = s.title || "";
      cell(tr, s.client || "–");
      cell(tr, s.project || "–");
      numCell(tr, s.duration_ms || 0, formatDuration(s.duration_ms || 0));
      numCell(tr, s.requests || 0, formatInt(s.requests || 0));
      numCell(tr, s.total || 0, formatTokens(s.total || 0));
      tbody.appendChild(tr);
    });
  }

  function renderDimension(dim, rows) {
    const count = $("count-" + dim);
    if (count) count.textContent = rows.length + " rows";
    const tbody = $("tbl-" + dim);
    tbody.textContent = "";
    if (!rows.length) {
      emptyRow(tbody, 8);
      return;
    }
    rows.forEach((r) => {
      const tr = document.createElement("tr");
      cell(tr, r.key || "–");
      numCell(tr, r.requests || 0, formatInt(r.requests || 0));
      numCell(tr, r.fresh_input || 0, formatInt(r.fresh_input || 0));
      numCell(tr, r.output || 0, formatInt(r.output || 0));
      numCell(tr, r.cache_read || 0, formatInt(r.cache_read || 0));
      numCell(tr, r.cache_create || 0, formatInt(r.cache_create || 0));
      numCell(tr, r.reasoning || 0, formatInt(r.reasoning || 0));
      numCell(tr, r.total || 0, formatInt(r.total || 0));
      tbody.appendChild(tr);
    });
  }

  // 环比对比:显示串与着色 class 由服务端按 cli compare 口径预计算,
  // 前端原样直绘;change/change_pct 的空串或 "--" 原样显示。
  // 标题行形态与 report 静态页的 .meta 一致(双语 + b 加粗窗口起止)。
  function renderCompare(c, range) {
    if (!c) return;
    const meta = $("compare-meta");
    meta.textContent = "";
    meta.appendChild(document.createTextNode("Current / 当前: "));
    let b = document.createElement("b");
    b.textContent = ((range && range.from) || "–") + " .. " + ((range && range.to) || "–");
    meta.appendChild(b);
    meta.appendChild(document.createTextNode(" · Base / 基线: "));
    b = document.createElement("b");
    b.textContent = (c.base_start || "–") + " .. " + (c.base_end || "–");
    meta.appendChild(b);

    const tbody = $("compare-body");
    tbody.textContent = "";
    (c.rows || []).forEach((r) => {
      const tr = document.createElement("tr");
      cell(tr, r.label || "");
      let td = cell(tr, r.current || "");
      td.className = "num";
      td = cell(tr, r.base || "");
      td.className = "num";
      td = cell(tr, r.change || "");
      td.className = r.change_class || "num";
      td = cell(tr, r.change_pct || "");
      td.className = "num";
      tbody.appendChild(tr);
    });
  }

  // 预估:显示串由服务端按 cli forecast 口径预计算(固定回看窗口,与选区
  // 无关),前端原样直绘;窗口无数据的 "—" 同样原样显示。
  // f 缺失(旧缓存/字段缺失)时跳过,避免清空面板。
  function renderForecast(f) {
    if (!f) return;
    $("forecast-today").textContent = f.today_so_far || "–";
    const tbody = $("forecast-body");
    tbody.textContent = "";
    (f.rows || []).forEach((r) => {
      const tr = document.createElement("tr");
      cell(tr, r.label || "");
      let td = cell(tr, r.total || "");
      td.className = "num";
      td = cell(tr, r.avg_day || "");
      td.className = "num";
      td = cell(tr, r.active || "");
      td.className = "num";
      td = cell(tr, r.estimate || "");
      td.className = "num";
      tbody.appendChild(tr);
    });
  }

  function renderDashboard(data) {
    renderTotals(data.totals);
    renderCompare(data.compare, data.range);
    renderForecast(data.forecast);
    renderSessions(data.sessions || []);
    const dims = data.dimensions || {};
    DIMENSIONS.forEach((dim) => renderDimension(dim, dims[dim] || []));
    renderCharts(data.charts);
  }

  // 图表:9 张 SVG 由后端 /api/dashboard 同一读事务生成并内嵌进载荷
  // (charts 键固定 9 个,已剥离 XML 序言),innerHTML 注入保留悬停 <title>
  // 提示。SVG 来自本服务 charts 包(文本已经 svgEscape 转义),非外部输入。
  // charts 缺失(旧缓存/字段缺失)时跳过,避免清空面板。
  function renderCharts(c) {
    if (!c) return;
    CHART_KINDS.forEach((kind) => {
      const svg = c[kind];
      if (!svg) return;
      const box = $("chart-" + kind);
      box.classList.remove("empty");
      box.innerHTML = svg;
    });
  }

  // ---------- 排序(数值列读 data-v,文本列读 textContent) ----------

  function sortTable(th) {
    const table = th.closest("table");
    if (!table) return;
    const tbody = table.tBodies[0];
    if (!tbody || tbody.rows.length <= 1) return;
    const idx = th.cellIndex;
    // 首次点击降序;已是降序则切升序,再点回降序。
    const toAsc = th.classList.contains("sorted-desc");
    Array.prototype.forEach.call(table.querySelectorAll("th.sortable"), (h) => {
      h.classList.remove("sorted-asc", "sorted-desc");
    });
    th.classList.add(toAsc ? "sorted-asc" : "sorted-desc");
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
      return toAsc ? c : -c;
    });
    rows.forEach((r) => tbody.appendChild(r));
  }

  // ---------- 加载 ----------

  function setBusy(busy) {
    document.querySelectorAll(".controls button").forEach((b) => {
      b.disabled = busy ? true : b.dataset.preset === "all" && !metaReady();
    });
  }

  async function load() {
    if (state.loading) {
      state.dirty = true;
      return;
    }
    state.loading = true;
    state.dirty = false;
    setBusy(true);
    showNoticeLoading();
    renderStatus();
    const qs = "from=" + encodeURIComponent(state.from) + "&to=" + encodeURIComponent(state.to);
    // 仅 dashboard + meta 两个请求并行;9 张图表由后端内嵌在 dashboard
    // 载荷里(同一读事务快照,KPI/明细与图表保证一致),不再单独取图。
    const settled = await Promise.allSettled([
      fetchJSON("/api/dashboard?" + qs),
      fetchJSON("/api/meta")
    ]);
    const errors = [];

    const dashRes = settled[0];
    if (dashRes.status === "fulfilled") {
      renderDashboard(dashRes.value);
    } else {
      errors.push("dashboard: " + errText(dashRes.reason));
    }

    const metaRes = settled[1];
    if (metaRes.status === "fulfilled") {
      applyMeta(metaRes.value);
    } else {
      errors.push("meta: " + errText(metaRes.reason));
    }

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
    requestLoad();
  }

  function applyPreset(name) {
    if (name === "all" && !metaReady()) return;
    const maker = PRESETS[name];
    if (!maker) return;
    const range = maker();
    if (!range[0] || !range[1]) return;
    // 服务端对区间有 366 天上限，「全部」随数据积累迟早越界：预检给出
    // 双语提示，不发必然 400 的请求。
    if (spanDays(range[0], range[1]) > RANGE_LIMIT) {
      showError(["range " + range[0] + ".." + range[1] + " exceeds the " + RANGE_LIMIT +
        "-day limit; pick a shorter range / 区间 " + range[0] + ".." + range[1] +
        " 超过 " + RANGE_LIMIT + " 天上限，请改用更短范围"]);
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

  // ---------- 初始化 ----------

  function init() {
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
    // 回到前台时重新对齐计时,避免恢复后立刻撞上滞留的旧周期。
    document.addEventListener("visibilitychange", () => {
      if (!document.hidden && state.auto > 0) resetTimer();
    });

    // 初始范围:近 30 天(与 HTML 中 d30 按钮的 active 一致)。
    setRange(addDaysStr(todayStr(), -29), todayStr(), "d30");
    resetTimer();
  }

  init();
})();
