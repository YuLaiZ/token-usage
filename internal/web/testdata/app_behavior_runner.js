"use strict";

/* 仪表板前端行为回归执行器,由 internal/web 的 Go 测试驱动:
     node app_behavior_runner.js <app.js 路径> <场景 JSON 路径>
   在 vm 沙箱里用最小 DOM 桩加载生产 app.js(配合 index.html 的静态结构):
   - 元素注册表:预置 index.html 里 app.js 依赖的 id/class/data-* 骨架,
     getElementById 对任意 id 惰性 memo 化;querySelector(All) 在注册表上按
     粗选择器匹配(#id/.class/tag/[attr]/[attr=v]/空格后代/逗号并集);
   - fetch 桩:按「METHOD 路由前缀」查表,单值或队列按调用次序出队,
     记录全部调用 {url,method,body};
   - 场景步骤用 element.dispatch 手动触发监听,每步后 await 足量 microtask
     flush,等 Promise.then 渲染链落定再取证;
   - 输出按场景聚合的 JSON(fatal/fetchCalls/元素快照/存储/toast/分步状态)。
   判定全部在 Go 侧,本文件只负责执行与取证。 */

const fs = require("fs");
const vm = require("vm");

const appSource = fs.readFileSync(process.argv[2], "utf8");
const scenarioDefs = JSON.parse(fs.readFileSync(process.argv[3], "utf8"));

/* ---------- 粗选择器:#id / .class / tag / [attr] / [attr=value],空格后代,逗号并集 ---------- */
const compoundCache = new Map();

function parseCompound(part) {
  let parsed = compoundCache.get(part);
  if (parsed) return parsed;
  parsed = { tag: null, id: null, classes: [], attrs: [] };
  const re = /(^[a-zA-Z][a-zA-Z0-9-]*|\*)|#([a-zA-Z0-9_-]+)|\.([a-zA-Z0-9_-]+)|\[([a-zA-Z0-9_-]+)(?:=(".*?"|'.*?'|[^\]]*))?\]/g;
  let m;
  while ((m = re.exec(part)) !== null) {
    if (m[1] !== undefined) parsed.tag = m[1];
    else if (m[2] !== undefined) parsed.id = m[2];
    else if (m[3] !== undefined) parsed.classes.push(m[3]);
    else if (m[4] !== undefined) {
      let v = m[5];
      if (v !== undefined) {
        v = v.trim();
        if (v.length >= 2 && ((v[0] === '"' && v.charAt(v.length - 1) === '"') || (v[0] === "'" && v.charAt(v.length - 1) === "'"))) {
          v = v.slice(1, -1);
        }
      }
      parsed.attrs.push({ name: m[4], value: v === undefined ? null : v });
    }
  }
  compoundCache.set(part, parsed);
  return parsed;
}

function matchCompound(elm, part) {
  const c = parseCompound(part);
  if (c.tag && c.tag !== "*" && elm.tagName.toUpperCase() !== c.tag.toUpperCase()) return false;
  if (c.id && elm.id !== c.id) return false;
  for (let i = 0; i < c.classes.length; i++) {
    if (!elm.classList.contains(c.classes[i])) return false;
  }
  for (let i = 0; i < c.attrs.length; i++) {
    const v = elm.getAttribute(c.attrs[i].name);
    if (v === null) return false;
    if (c.attrs[i].value !== null && v !== c.attrs[i].value) return false;
  }
  return true;
}

function matchSelector(elm, selector) {
  const alts = String(selector).split(",");
  for (let a = 0; a < alts.length; a++) {
    const parts = alts[a].trim().split(/\s+/).filter(Boolean);
    if (!parts.length) continue;
    if (!matchCompound(elm, parts[parts.length - 1])) continue;
    /* 后代组合:从右往左逐级向上找匹配祖先,允许跳过不匹配的层级 */
    let node = elm.parentNode;
    let i = parts.length - 2;
    while (i >= 0 && node) {
      if (matchCompound(node, parts[i])) i--;
      node = node.parentNode;
    }
    if (i < 0) return true;
  }
  return false;
}

function inScope(node, scope) {
  while (node) {
    if (node === scope) return true;
    node = node.parentNode;
  }
  return false;
}

function detach(node) {
  const p = node.parentNode;
  if (!p) return;
  const i = p.children.indexOf(node);
  if (i >= 0) p.children.splice(i, 1);
  node.parentNode = null;
}

function todayStr() {
  const d = new Date();
  const pad = (n) => (n < 10 ? "0" : "") + n;
  return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate());
}

function mapToObj(store) {
  const o = {};
  store.forEach((v, k) => {
    o[k] = v;
  });
  return o;
}

/* ---------- 单场景沙箱:每个场景独立注册表 / 存储 / fetch 路由表 ---------- */
function makeSandbox(opts) {
  const routes = opts.routes || {};
  const registry = [];
  const byIdMap = new Map();
  const storeSession = new Map();
  const storeLocal = new Map();
  const fetchCalls = [];
  const pendingFetches = []; /* gate 响应:被 resolve 步骤放行的挂起 fetch */
  let activeEl = null;

  function queryAll(scope, selector) {
    const out = [];
    for (const elm of registry) {
      if (scope && !inScope(elm.parentNode, scope)) continue;
      if (matchSelector(elm, selector)) out.push(elm);
    }
    return out;
  }

  function makeElement(spec) {
    spec = spec || {};
    const classes = new Set(spec.classes || []);
    const attrs = {};
    const listeners = {};
    const children = [];
    const elm = {
      id: spec.id || "",
      tagName: String(spec.tag || "div").toUpperCase(),
      value: "",
      textContent: spec.text !== undefined ? String(spec.text) : "",
      innerHTML: "",
      disabled: !!spec.disabled,
      hidden: !!spec.hidden,
      type: "",
      title: "",
      className: (spec.classes || []).join(" "),
      dataset: {},
      style: {},
      children: children,
      parentNode: null,
      clientWidth: 800,
      clientHeight: 260,
      offsetWidth: 120,
      offsetHeight: 28,
      getBoundingClientRect() {
        return { left: 0, top: 0, width: 800, height: 260 };
      },
      classList: {
        toggle(name, force) {
          const want = force === undefined ? !classes.has(name) : !!force;
          if (want) classes.add(name);
          else classes.delete(name);
          return want;
        },
        add(...names) {
          names.forEach((n) => classes.add(n));
        },
        remove(...names) {
          names.forEach((n) => classes.delete(n));
        },
        contains(name) {
          return classes.has(name);
        }
      },
      addEventListener(type, fn) {
        (listeners[type] = listeners[type] || []).push(fn);
      },
      removeEventListener(type, fn) {
        const l = listeners[type];
        if (!l) return;
        const i = l.indexOf(fn);
        if (i >= 0) l.splice(i, 1);
      },
      dispatch(type, event) {
        event = event || {};
        if (!event.target) event.target = elm;
        event.type = type;
        if (!event.stopPropagation) event.stopPropagation = () => {};
        if (!event.preventDefault) event.preventDefault = () => {};
        /* DOM 语义:监听器内 this 指向绑定元素(app.js 的 select 监听读 this.value) */
        (listeners[type] || []).slice().forEach((fn) => fn.call(elm, event));
      },
      setAttribute(name, value) {
        name = String(name);
        value = String(value);
        attrs[name] = value;
        if (name === "id") elm.id = value;
        else if (name === "class") {
          value.split(/\s+/).filter(Boolean).forEach((c) => classes.add(c));
          elm.className = value;
        } else if (name.indexOf("data-") === 0) {
          /* data-* 属性与 dataset 双向:app.js 走 dataset 读,选择器走属性匹配 */
          const key = name.slice(5).replace(/-([a-z])/g, (ignore, ch) => ch.toUpperCase());
          elm.dataset[key] = value;
        }
      },
      getAttribute(name) {
        return Object.prototype.hasOwnProperty.call(attrs, String(name)) ? attrs[String(name)] : null;
      },
      removeAttribute(name) {
        delete attrs[String(name)];
      },
      appendChild(child) {
        if (child.parentNode) detach(child);
        child.parentNode = elm;
        children.push(child);
        return child;
      },
      removeChild(child) {
        const i = children.indexOf(child);
        if (i >= 0) children.splice(i, 1);
        child.parentNode = null;
        return child;
      },
      insertBefore(node, ref) {
        if (node.parentNode) detach(node);
        node.parentNode = elm;
        const i = ref ? children.indexOf(ref) : -1;
        if (i < 0) children.push(node);
        else children.splice(i, 0, node);
        return node;
      },
      closest(selector) {
        let n = elm;
        while (n) {
          if (matchSelector(n, selector)) return n;
          n = n.parentNode;
        }
        return null;
      },
      contains(x) {
        while (x) {
          if (x === elm) return true;
          x = x.parentNode;
        }
        return false;
      },
      focus() {
        activeEl = elm;
      },
      blur() {
        if (activeEl === elm) activeEl = null;
      },
      click() {
        elm.dispatch("click");
      },
      scrollIntoView() {},
      setCustomValidity() {},
      reportValidity() {
        return true;
      },
      querySelectorAll(selector) {
        return queryAll(elm, selector);
      },
      querySelector(selector) {
        const r = queryAll(elm, selector);
        return r.length ? r[0] : null;
      }
    };
    Object.defineProperty(elm, "firstChild", { get: () => (children.length ? children[0] : null) });
    Object.defineProperty(elm, "parentElement", { get: () => elm.parentNode });
    elm._listeners = listeners;
    elm._attrs = attrs;
    if (spec.id) attrs.id = spec.id;
    if (spec.classes && spec.classes.length) {
      spec.classes.forEach((c) => classes.add(c));
      attrs.class = spec.classes.join(" ");
    }
    Object.keys(spec.attrs || {}).forEach((k) => elm.setAttribute(k, spec.attrs[k]));
    registry.push(elm);
    if (spec.id && !byIdMap.has(spec.id)) byIdMap.set(spec.id, elm);
    return elm;
  }

  function memoById(id) {
    if (!byIdMap.has(id)) makeElement({ id: id }); /* 未知 id 也惰性出桩 */
    return byIdMap.get(id);
  }

  /* ----- index.html 静态骨架(app.js 依赖的最小结构,id/class/data-* 对齐生产 DOM) ----- */
  const docRoot = makeElement({ tag: "html", attrs: { lang: "zh-CN", "data-theme": "dark", "data-palette": "cobalt", "data-locale": "zh-CN" } });
  const body = makeElement({ tag: "body" });
  docRoot.appendChild(body);
  const appEl = makeElement({ classes: ["app"] });
  body.appendChild(appEl);
  const sidebar = makeElement({ classes: ["sidebar"] });
  appEl.appendChild(sidebar);
  const nav = makeElement({ tag: "nav", classes: ["nav"] });
  sidebar.appendChild(nav);
  const navButtons = [
    makeElement({ tag: "button", classes: ["nav-button"], attrs: { "data-page": "dash", "aria-current": "page", "aria-label": "仪表盘" } }),
    makeElement({ tag: "button", classes: ["nav-button"], attrs: { "data-page": "config", "aria-label": "配置" } })
  ];
  navButtons.forEach((b) => nav.appendChild(b));
  const sideToggle = makeElement({ id: "side-toggle", tag: "button", attrs: { "aria-expanded": "true", "aria-label": "收起侧边栏" } });
  const content = makeElement({ classes: ["content"] });
  appEl.appendChild(content);
  /* renderMeta 现在查询 .asof / .asof .dot 切换离线态 */
  const asofEl = makeElement({ classes: ["asof"] });
  const asofDot = makeElement({ classes: ["dot"] });
  asofEl.appendChild(asofDot);
  asofEl.appendChild(makeElement({ classes: ["asof-short"] }));
  makeElement({ id: "version-badge", hidden: true });
  makeElement({ id: "asof-t" });
  makeElement({ id: "appearance-control", attrs: { "aria-label": "语言与外观" } });
  const localeButtons = [
    makeElement({ tag: "button", attrs: { "data-locale": "zh-CN", "aria-pressed": "true" } }),
    makeElement({ tag: "button", attrs: { "data-locale": "en", "aria-pressed": "false" } })
  ];
  const localeSwitch = makeElement({ id: "locale-switch" });
  localeButtons.forEach((b) => localeSwitch.appendChild(b));
  makeElement({ id: "theme-toggle", tag: "button", attrs: { "aria-label": "切换主题", title: "切换主题" } });
  makeElement({ id: "theme-toggle-use", attrs: { href: "#i-sun" } });
  makeElement({ id: "palette-toggle", tag: "button", attrs: { "aria-expanded": "false", "aria-label": "选择主题配色", title: "选择主题配色" } });
  makeElement({ id: "palette-pop", hidden: true });
  makeElement({ id: "pal-name" });
  const PALS = ["cobalt", "azure", "lake", "navy", "glacier"];
  const paletteChoices = PALS.map((p, i) =>
    makeElement({ tag: "button", classes: ["palette-choice"], attrs: { "data-pal": p, "aria-pressed": i === 0 ? "true" : "false" } })
  );
  const paletteDots = makeElement({ id: "palette-dots" });
  paletteChoices.forEach((b) => paletteDots.appendChild(b));
  const pageDash = makeElement({ id: "page-dash", classes: ["page"], attrs: { "data-active": "true", "aria-hidden": "false" } });
  const pageConfig = makeElement({ id: "page-config", classes: ["page"], attrs: { "aria-hidden": "true" } });
  content.appendChild(pageDash);
  content.appendChild(pageConfig);
  const rangeButtons = ["0", "1", "2", "3", "4", "5", "6"].map((v, i) =>
    makeElement({ tag: "button", attrs: { "data-v": v, "aria-pressed": i === 0 ? "true" : "false" } })
  );
  const rangeSeg = makeElement({ id: "range-seg" });
  rangeButtons.forEach((b) => rangeSeg.appendChild(b));
  const rangeCustom = makeElement({ id: "range-custom", tag: "button", classes: ["button", "range-custom"], attrs: { "aria-expanded": "false", "aria-pressed": "false" } });
  makeElement({ id: "range-custom-txt" });
  makeElement({ id: "range-custom-short" });
  makeElement({ id: "calpop", hidden: true });
  makeElement({ id: "cal-prev", tag: "button" });
  makeElement({ id: "cal-next", tag: "button" });
  makeElement({ id: "cal-grids" });
  makeElement({ id: "cal-range-txt", text: "请选择开始与结束日期" });
  makeElement({ id: "cal-apply", tag: "button" });
  makeElement({ id: "kpis" });
  const dimButtons = ["client", "provider", "model", "project"].map((d, i) =>
    makeElement({ tag: "button", attrs: { "data-v": d, "aria-pressed": i === 0 ? "true" : "false" } })
  );
  const dimSeg = makeElement({ id: "dim-seg" });
  dimButtons.forEach((b) => dimSeg.appendChild(b));
  const viewButtons = ["list", "compose"].map((v, i) =>
    makeElement({ tag: "button", attrs: { "data-v": v, "aria-pressed": i === 0 ? "true" : "false" } })
  );
  const viewSeg = makeElement({ id: "view-seg" });
  viewButtons.forEach((b) => viewSeg.appendChild(b));
  makeElement({ id: "groups" });
  makeElement({ id: "chart", tag: "svg" });
  makeElement({ id: "chart-frame", classes: ["chart-frame"] });
  makeElement({ id: "chart-title-h" });
  makeElement({ id: "chart-sub" });
  const modeButtons = [
    makeElement({ tag: "button", attrs: { "data-v": "trend", "aria-pressed": "true" } }),
    makeElement({ id: "mode-heat-btn", tag: "button", attrs: { "data-v": "heat", "aria-pressed": "false" } })
  ];
  const modeSeg = makeElement({ id: "mode-seg" });
  modeButtons.forEach((b) => modeSeg.appendChild(b));
  makeElement({ id: "tip", hidden: true });
  makeElement({ id: "cviews" });
  makeElement({ id: "sessions-sub", text: "跟随当前时间范围" });
  makeElement({ id: "sessions-body", tag: "tbody" });
  makeElement({ id: "session-title-modal", hidden: true });
  makeElement({ id: "session-title-modal-text" });
  makeElement({ id: "session-title-modal-close", tag: "button" });
  makeElement({ id: "unit-trigger", tag: "button" });
  makeElement({ id: "unit-pop", hidden: true });
  /* ----- 配置页骨架 ----- */
  makeElement({ id: "config-query-title", tag: "h3" });
  makeElement({ id: "client-table", tag: "table" });
  makeElement({ id: "cfg-poll-interval", tag: "input" });
  makeElement({ id: "cfg-autostart", tag: "button", attrs: { "aria-checked": "false" } });
  const chips = ["default", "info", "debug", "warn", "error"].map((l) =>
    makeElement({ tag: "button", classes: ["chip"], attrs: { "aria-pressed": "false" }, text: l })
  );
  const cfgLogLevel = makeElement({ id: "cfg-log-level" });
  chips.forEach((c) => cfgLogLevel.appendChild(c));
  makeElement({ id: "cfg-max-days", tag: "input" });
  makeElement({ id: "cfg-log-dir", tag: "input" });
  const configCardEl = makeElement({ classes: ["config-card"] });
  const qDefaultEl = makeElement({ id: "q-default", tag: "button", classes: ["ti", "dv-trigger"], attrs: { "aria-expanded": "false" } });
  configCardEl.appendChild(qDefaultEl); /* dvOpen 依赖 closest('.config-card') */
  makeElement({ id: "dv-cur", text: "client" });
  makeElement({ id: "dv-pop", hidden: true });
  makeElement({ id: "q-default-reset", tag: "button" });
  makeElement({ id: "cols-editor" });
  makeElement({ id: "cols-default", tag: "button" });
  makeElement({ id: "q-groups" });
  makeElement({ id: "q-subs" });
  makeElement({ id: "alias-add-key", tag: "input" });
  makeElement({ id: "alias-add-val", tag: "input" });
  makeElement({ id: "alias-add-btn", tag: "button" });
  makeElement({ id: "alias-list" });
  makeElement({ id: "cfg-actions", attrs: { "data-dirty": "false" } });
  makeElement({ id: "dirty-pill", text: "有未保存的修改" });
  makeElement({ id: "cfg-reset", tag: "button" });
  makeElement({ id: "cfg-save", tag: "button", disabled: true, text: "已保存" });
  makeElement({ id: "toast", classes: ["toast"] });
  makeElement({ id: "rt-modal", hidden: true });
  makeElement({ id: "rt-modal-title", tag: "h3" });
  makeElement({ id: "rt-opts" });
  makeElement({ id: "rt-save", tag: "button" });
  makeElement({ id: "rt-cancel", tag: "button" });
  makeElement({ id: "reset-modal", hidden: true });
  makeElement({ id: "reset-cancel", tag: "button" });
  makeElement({ id: "reset-confirm", tag: "button" });
  /* 组合查询/自定义视图新增栏:[data-add] 按钮需要 .qdef-addbar 祖先与 input 兄弟 */
  ["q-groups", "q-subs"].forEach((listId) => {
    const bar = makeElement({ classes: ["qdef-addbar"] });
    bar.appendChild(makeElement({ tag: "input", classes: ["ti"] }));
    bar.appendChild(makeElement({ tag: "button", classes: ["button"], attrs: { "data-add": listId } }));
    content.appendChild(bar);
  });

  /* ----- document ----- */
  const docListeners = {};
  const doc = {
    title: "Token Usage",
    hidden: false,
    body: body,
    documentElement: docRoot,
    addEventListener(type, fn) {
      (docListeners[type] = docListeners[type] || []).push(fn);
    },
    removeEventListener(type, fn) {
      const l = docListeners[type];
      if (!l) return;
      const i = l.indexOf(fn);
      if (i >= 0) l.splice(i, 1);
    },
    dispatch(type, event) {
      event = event || {};
      if (!event.target) event.target = body;
      (docListeners[type] || []).slice().forEach((fn) => fn(event));
    },
    getElementById: memoById,
    querySelector(selector) {
      const r = queryAll(null, selector);
      return r.length ? r[0] : null;
    },
    querySelectorAll(selector) {
      return queryAll(null, selector);
    },
    createElement(tag) {
      return makeElement({ tag: tag });
    },
    createElementNS(ns, tag) {
      return makeElement({ tag: tag });
    },
    createTextNode(text) {
      return { nodeType: 3, nodeValue: String(text), parentElement: null, parentNode: null };
    },
    /* body 无文本子节点,walker 首次 nextNode 即返回 null,acceptNode 不会被调用 */
    createTreeWalker() {
      let done = false;
      return {
        nextNode() {
          if (done) return null;
          done = true;
          return null;
        }
      };
    },
    contains() {
      return false;
    }
  };
  Object.defineProperty(doc, "activeElement", { get: () => activeEl });

  /* ----- 存储 / fetch / 环 ----- */
  function makeStorage(store) {
    return {
      getItem: (k) => (store.has(String(k)) ? store.get(String(k)) : null),
      setItem: (k, v) => store.set(String(k), String(v)),
      removeItem: (k) => store.delete(String(k)),
      clear: () => store.clear(),
      key(i) {
        const keys = Array.from(store.keys());
        return i < keys.length ? keys[i] : null;
      },
      get length() {
        return store.size;
      }
    };
  }

  function routeFor(method, url) {
    /* 最长前缀优先:/api/config?defaults=1 必须先于 /api/config 命中 */
    let bestKey = null;
    let bestLen = -1;
    for (const key of Object.keys(routes)) {
      const sp = key.indexOf(" ");
      if (sp < 0) continue;
      if (key.slice(0, sp).toUpperCase() !== method) continue;
      const prefix = key.slice(sp + 1);
      if (url.indexOf(prefix) !== 0) continue;
      if (prefix.length > bestLen) {
        bestKey = key;
        bestLen = prefix.length;
      }
    }
    if (!bestKey) return null;
    const v = routes[bestKey];
    if (Array.isArray(v)) {
      /* 队列按调用次序出队;耗尽后固定返回最后一次响应 */
      if (!v.length) return v._last || null;
      v._last = v[0];
      return v.shift();
    }
    return v;
  }

  function sandboxFetch(url, fetchOpts) {
    const method = String((fetchOpts && fetchOpts.method) || "GET").toUpperCase();
    const urlStr = String(url);
    fetchCalls.push({ url: urlStr, method: method, body: fetchOpts && typeof fetchOpts.body === "string" ? fetchOpts.body : null });
    const spec = routeFor(method, urlStr) || { status: 500, body: { error: { message: "no stubbed route: " + method + " " + urlStr } } };
    if (spec.reject) {
      /* 网络失败模式:fetch 直接 reject,走 api() 调用方的 rejection 分支 */
      return Promise.reject(new Error("stubbed network failure: " + method + " " + urlStr));
    }
    const payload = spec.body === undefined ? {} : spec.body;
    const settle = () => ({
      ok: spec.status >= 200 && spec.status < 300,
      status: spec.status,
      json() {
        return Promise.resolve(payload);
      }
    });
    if (spec.pending) {
      /* gate 模式:响应挂起,由场景步骤 resolve 手动放行(先发后答,构造
         请求重叠,验证旧响应不得回写) */
      return new Promise((resolve) => {
        pendingFetches.push(() => resolve(settle()));
      });
    }
    return Promise.resolve(settle());
  }

  function matchMedia(query) {
    return {
      matches: false,
      media: String(query),
      addEventListener() {},
      removeEventListener() {},
      addListener() {},
      removeListener() {}
    };
  }

  const winListeners = {};
  const sandbox = {
    document: doc,
    sessionStorage: makeStorage(storeSession),
    localStorage: makeStorage(storeLocal),
    fetch: sandboxFetch,
    /* 定时器全部空转:行为测试不等周期任务,也不让 Node 事件循环挂住 */
    setInterval: () => 0,
    clearInterval: () => {},
    setTimeout: () => 0,
    clearTimeout: () => {},
    requestAnimationFrame: () => 0,
    cancelAnimationFrame: () => {},
    matchMedia: matchMedia,
    addEventListener(type, fn) {
      (winListeners[type] = winListeners[type] || []).push(fn);
    },
    removeEventListener() {},
    scrollTo() {},
    console: console,
    NodeFilter: { SHOW_TEXT: 4, FILTER_ACCEPT: 1, FILTER_REJECT: 2, FILTER_SKIP: 3 }
  };
  /* 手动定时器模式(opts.timers==='manual'):setTimeout 记账不执行,由
     __tick(ms) 推进虚拟时钟并按到期顺序回调——自动刷新排程/完成态回退
     都可被场景精确驱动,真实定时器仍然空转。 */
  if (opts.timers === "manual") {
    const timersMap = new Map();
    let nextTimerId = 1;
    let clockMs = 0;
    const runDue = () => {
      for (;;) {
        let dueId = null;
        let dueAt = Infinity;
        timersMap.forEach((t, id) => {
          if (t.at <= clockMs && t.at < dueAt) {
            dueAt = t.at;
            dueId = id;
          }
        });
        if (dueId === null) break;
        const t = timersMap.get(dueId);
        timersMap.delete(dueId);
        t.fn();
      }
    };
    sandbox.setTimeout = (fn, ms) => {
      const id = nextTimerId++;
      timersMap.set(id, { fn: fn, at: clockMs + (ms || 0) });
      return id;
    };
    sandbox.clearTimeout = (id) => {
      timersMap.delete(id);
    };
    sandbox.__tick = (ms) => {
      clockMs += ms;
      runDue();
    };
  }
  sandbox.window = sandbox; /* window.matchMedia / window.scrollTo / resize 监听都落在沙箱全局 */

  /* navType 与 navEntries 都不注入 = performance 整体缺失;navEntries 存在时
     注入「API 在、返回该数组」的 Navigation Timing(空数组即无 navigation 条目),
     保留旧场景的两种环境语义。 */
  if (opts.navType !== undefined || opts.navEntries !== undefined) {
    sandbox.performance = {
      getEntriesByType: (type) =>
        type === "navigation" ? (opts.navEntries !== undefined ? opts.navEntries : [{ type: opts.navType }]) : []
    };
  }

  if (opts.savedRange) storeSession.set("tu-range", JSON.stringify(opts.savedRange));

  vm.createContext(sandbox);
  return {
    sandbox: sandbox,
    pendingCount: () => pendingFetches.length,
    resolvePending: () => {
      const fns = pendingFetches.splice(0, pendingFetches.length);
      fns.forEach((fn) => fn());
    },
    doc: doc,
    byId: memoById,
    fetchCalls: fetchCalls,
    storeSession: storeSession,
    storeLocal: storeLocal,
    navButtons: navButtons,
    localeButtons: localeButtons,
    paletteChoices: paletteChoices,
    rangeButtons: rangeButtons,
    chips: chips,
    appEl: appEl,
    asofEl: asofEl,
    root: docRoot,
    rangeCustom: rangeCustom
  };
}

/* ---------- 场景步骤:按 app.js 的监听形态手动触发 ---------- */
function applyStep(step, ctx) {
  switch (step.op) {
    case "nav": {
      const btn = ctx.navButtons.find((b) => b.dataset.page === step.page);
      if (!btn) throw new Error("no nav button for page " + step.page);
      btn.dispatch("click");
      return;
    }
    case "click": {
      ctx.byId(step.id).dispatch("click");
      return;
    }
    case "key": {
      /* 元素级键盘事件(如默认视图触发器上的 Escape) */
      ctx.byId(step.id).dispatch("keydown", { key: step.value });
      return;
    }
    case "input": {
      /* 配置页文本输入是 page-config 上的委托 input 监听:改 value 后冒泡派发 */
      const input = ctx.byId(step.id);
      input.value = String(step.value);
      ctx.byId("page-config").dispatch("input", { target: input });
      return;
    }
    case "locale": {
      const btn = ctx.localeButtons.find((b) => b.dataset.locale === step.value);
      if (!btn) throw new Error("no locale button " + step.value);
      ctx.byId("locale-switch").dispatch("click", { target: btn });
      return;
    }
    case "palette": {
      const btn = ctx.paletteChoices.find((b) => b.dataset.pal === step.value);
      if (!btn) throw new Error("no palette choice " + step.value);
      ctx.byId("palette-dots").dispatch("click", { target: btn });
      return;
    }
    case "theme": {
      ctx.byId("theme-toggle").dispatch("click");
      return;
    }
    case "side": {
      ctx.byId("side-toggle").dispatch("click");
      return;
    }
    case "documentKey": {
      ctx.doc.dispatch("keydown", { key: step.value });
      return;
    }
    case "seg": {
      /* 分段控件:在 host 上以子按钮为 target 派发 click(bindSeg 的 closest('button')) */
      const host = ctx.byId(step.id);
      const btn = (host.children || []).find((b) => b.dataset && b.dataset.v === step.value);
      if (!btn) throw new Error("no seg button " + step.id + "=" + step.value);
      host.dispatch("click", { target: btn });
      return;
    }
    case "setSelect": {
      const sel = ctx.byId(step.id);
      sel.value = String(step.value);
      sel.dispatch("change");
      return;
    }
    case "tick": {
      if (typeof ctx.sandbox.__tick !== "function") throw new Error("tick requires opts.timers==='manual'");
      ctx.sandbox.__tick(Number(step.value) || 0);
      return;
    }
    case "resolve": {
      ctx.resolvePending();
      return;
    }
    default:
      throw new Error("unknown step op " + step.op);
  }
}

/* app.js 的渲染链全是 Promise.then:每步后用足量 microtask 轮次等链路落定 */
async function flush(rounds) {
  const n = rounds || 80;
  for (let i = 0; i < n; i++) await Promise.resolve();
}

function buildReport(def, ctx, fatal, stepStates) {
  const byId = ctx.byId;
  return {
    name: def.name,
    fatal: fatal,
    today: todayStr(),
    title: ctx.doc.title,
    toast: byId("toast").textContent,
    fetchCalls: ctx.fetchCalls,
    rangeSeg: ctx.rangeButtons.map((b) => ({ v: b.dataset.v, pressed: b.getAttribute("aria-pressed") })),
    rangeCustom: {
      pressed: ctx.rangeCustom.getAttribute("aria-pressed"),
      on: ctx.rangeCustom.classList.contains("on"),
      text: byId("range-custom-txt").textContent
    },
    chartSub: byId("chart-sub").textContent,
    pollInput: byId("cfg-poll-interval").value,
    cfgSave: { disabled: !!byId("cfg-save").disabled, text: byId("cfg-save").textContent },
    dirtyPill: { text: byId("dirty-pill").textContent },
    cfgActionsDirty: byId("cfg-actions").getAttribute("data-dirty"),
    asofText: byId("asof-t").textContent,
    asofOffline: ctx.asofEl.classList.contains("offline"),
    dvPopHidden: byId("dv-pop").hidden,
    dvTriggerExpanded: byId("q-default").getAttribute("aria-expanded"),
    dvCurText: byId("dv-cur").textContent,
    activeElementId: ctx.doc.activeElement ? ctx.doc.activeElement.id : "",
    resetModalHidden: byId("reset-modal").hidden,
    kpisHtml: byId("kpis").innerHTML,
    groupsHtml: byId("groups").innerHTML,
    cviewsHtml: byId("cviews").innerHTML,
    chartHtml: byId("chart").innerHTML,
    sessionsSub: byId("sessions-sub").textContent,
    sessionsBodyHtml: byId("sessions-body").innerHTML,
    locale: {
      saved: ctx.storeLocal.get("tu-locale") || null,
      zhPressed: ctx.localeButtons[0].getAttribute("aria-pressed"),
      enPressed: ctx.localeButtons[1].getAttribute("aria-pressed")
    },
    theme: { saved: ctx.storeLocal.get("tu-theme") || null, root: ctx.root.getAttribute("data-theme") },
    palette: {
      saved: ctx.storeLocal.get("tu-palette") || null,
      root: ctx.root.getAttribute("data-palette"),
      name: byId("pal-name").textContent,
      choices: ctx.paletteChoices.map((b) => ({ pal: b.dataset.pal, pressed: b.getAttribute("aria-pressed") }))
    },
    side: {
      saved: ctx.storeLocal.get("tu-side") || null,
      min: ctx.appEl.classList.contains("side-min"),
      expanded: byId("side-toggle").getAttribute("aria-expanded")
    },
    chips: ctx.chips.map((c) => ({ text: c.textContent, pressed: c.getAttribute("aria-pressed") })),
    refresh: {
      state: byId("refresh-state").getAttribute("data-state"),
      label: byId("refresh-label").textContent,
      detail: byId("refresh-detail").textContent,
      pageRefreshing: byId("page-dash").getAttribute("data-refreshing"),
      dashBusy: byId("dash-results").getAttribute("aria-busy")
    },
    refreshCfg: {
      dashSelect: byId("cfg-dashboard-refresh").value,
      dashCustom: byId("cfg-dashboard-refresh-custom").value,
      dashCustomHidden: !!byId("cfg-dashboard-refresh-custom").hidden,
      watchSelect: byId("cfg-watch-refresh").value,
      watchCustom: byId("cfg-watch-refresh-custom").value,
      watchCustomHidden: !!byId("cfg-watch-refresh-custom").hidden
    },
    pendingFetches: ctx.pendingCount(),
    session: mapToObj(ctx.storeSession),
    local: mapToObj(ctx.storeLocal),
    stepStates: stepStates
  };
}

async function runScenario(def) {
  const ctx = makeSandbox(def.opts || {});
  let fatal = "";
  try {
    vm.runInContext(appSource, ctx.sandbox, { filename: "app.js" });
  } catch (e) {
    fatal = String((e && e.message) || e);
  }
  const stepStates = [];
  if (!fatal) {
    await flush();
    for (const step of def.steps || []) {
      try {
        applyStep(step, ctx);
      } catch (e) {
        fatal = "step " + JSON.stringify(step) + " threw: " + String((e && e.message) || e);
        break;
      }
      await flush();
      stepStates.push({
        op: step.op,
        id: step.id || "",
        value: step.value || "",
        pollInput: ctx.byId("cfg-poll-interval").value,
        saveDisabled: !!ctx.byId("cfg-save").disabled,
        saveText: ctx.byId("cfg-save").textContent,
        dirtyPill: ctx.byId("dirty-pill").textContent,
        toast: ctx.byId("toast").textContent,
        dashSelect: ctx.byId("cfg-dashboard-refresh").value,
        watchSelect: ctx.byId("cfg-watch-refresh").value,
        watchCustom: ctx.byId("cfg-watch-refresh-custom").value,
        refreshState: ctx.byId("refresh-state").getAttribute("data-state"),
        refreshLabel: ctx.byId("refresh-label").textContent,
        refreshDetail: ctx.byId("refresh-detail").textContent,
        pageRefreshing: ctx.byId("page-dash").getAttribute("data-refreshing"),
        dashBusy: ctx.byId("dash-results").getAttribute("aria-busy")
      });
    }
  }
  return buildReport(def, ctx, fatal, stepStates);
}

(async function main() {
  const reports = await Promise.all(scenarioDefs.map(runScenario));
  process.stdout.write(JSON.stringify(reports, null, 2));
})().catch((e) => {
  process.stderr.write("runner failed: " + ((e && e.stack) || e) + "\n");
  process.exit(1);
});
