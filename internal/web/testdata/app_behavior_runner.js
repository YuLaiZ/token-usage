"use strict";

// 仪表板前端行为回归执行器,由 internal/web 的 Go 测试驱动:
//   node app_behavior_runner.js <app.js 路径> <场景 JSON 路径>
// 在 vm 沙箱里用最小 DOM 桩加载 app.js,按场景注入导航类型与预置
// sessionStorage,回读 init() 的同步产物(选区输入值、预设高亮、存储)
// 输出 JSON。判定在 Go 侧,本文件只负责执行与取证。

const fs = require("fs");
const vm = require("vm");

const appSource = fs.readFileSync(process.argv[2], "utf8");
const scenarios = JSON.parse(fs.readFileSync(process.argv[3], "utf8"));

function makeElement(id) {
  const classes = new Set();
  return {
    id: id || "",
    value: "",
    textContent: "",
    disabled: false,
    hidden: false,
    type: "",
    title: "",
    className: "",
    dataset: {},
    style: {},
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
    addEventListener() {},
    appendChild() {},
    setAttribute() {}
  };
}

function makeSandbox(opts) {
  const elements = new Map();
  const byId = (id) => {
    if (!elements.has(id)) elements.set(id, makeElement(id));
    return elements.get(id);
  };
  const presetNames = ["today", "d7", "d30", "d90", "month", "all"];
  const presetButtons = presetNames.map((name) => {
    const b = makeElement("preset-" + name);
    b.dataset.preset = name;
    return b;
  });
  const store = new Map();
  if (opts.savedRange) store.set("tu-range", JSON.stringify(opts.savedRange));
  const doc = {
    hidden: false,
    body: makeElement("body"),
    addEventListener() {},
    createElement: () => makeElement(""),
    createElementNS: () => makeElement(""),
    getElementById: (id) => byId(id),
    querySelectorAll(selector) {
      if (selector === "[data-preset]" || selector === "#presets button") return presetButtons.slice();
      return [];
    }
  };
  const sandbox = {
    document: doc,
    sessionStorage: {
      getItem: (k) => (store.has(k) ? store.get(k) : null),
      setItem: (k, v) => store.set(k, String(v)),
      removeItem: (k) => store.delete(k)
    },
    // 悬置的 fetch:init() 的同步产物即断言对象,加载结果不参与。
    fetch: () => new Promise(() => {}),
    // 定时器全部空转:行为测试不等周期任务,也不让 Node 事件循环挂住。
    setInterval: () => 0,
    clearInterval: () => {},
    setTimeout: () => 0,
    clearTimeout: () => {},
    console,
    Date
  };
  // navType 与 navEntries 都不注入表示 performance 整体缺失的浏览器环境;
  // navEntries 存在时注入「API 在、返回该数组」的 Navigation Timing(空数组
  // 即有 performance 但无 navigation 条目),与整体缺失是两条不同分支。
  if (opts.navType !== undefined || opts.navEntries !== undefined) {
    sandbox.performance = {
      getEntriesByType: (type) =>
        type === "navigation" ? (opts.navEntries !== undefined ? opts.navEntries : [{ type: opts.navType }]) : []
    };
  }
  vm.createContext(sandbox);
  return { sandbox, store, byId, presetButtons };
}

function runScenario(sc) {
  const { sandbox, store, byId, presetButtons } = makeSandbox(sc.opts);
  let fatal = "";
  try {
    vm.runInContext(appSource, sandbox, { filename: "app.js" });
  } catch (e) {
    fatal = String(e && e.message ? e.message : e);
  }
  const d = new Date();
  const pad = (n) => (n < 10 ? "0" : "") + n;
  // 存储里是 JSON 字符串,报告解开为结构化对象;坏值保留原文以免吞证据。
  const rawSaved = store.get("tu-range") || null;
  let savedRange = null;
  if (rawSaved !== null) {
    try {
      savedRange = JSON.parse(rawSaved);
    } catch (_) {
      savedRange = { raw: rawSaved };
    }
  }
  return {
    name: sc.name,
    fatal,
    from: byId("date-from").value,
    to: byId("date-to").value,
    active: presetButtons.filter((b) => b.classList.contains("active")).map((b) => b.dataset.preset),
    savedRange,
    today: d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate())
  };
}

process.stdout.write(JSON.stringify(scenarios.map(runScenario), null, 2));
