/* token-usage 仪表板生产脚本（配合 index.html + app.css）。
   数据全部来自本地服务真实接口：GET /api/meta（版本与数据边界）、
   GET /api/dashboard（区间汇总/维度行/热力/自定义视图/会话）、
   GET|PUT /api/config（配置草稿读写）。
   浏览器本地偏好键统一 tu- 前缀：tu-locale、tu-theme、tu-palette、tu-side；
   自定义统计区间记忆使用 sessionStorage['tu-range']。
   无第三方依赖，单 IIFE，ES5 语法。 */
(function(){
'use strict';
var el = function(id){ return document.getElementById(id); };
var root = document.documentElement;
function esc(v){
  return String(v==null?'':v).replace(/[&<>"']/g,function(ch){ return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch]; });
}
function clone(v){ return v==null ? v : JSON.parse(JSON.stringify(v)); }

/* ---------- 界面语言：中英文即时切换，中文原文即 key ---------- */
var LOCALE = root.getAttribute('data-locale')==='en' ? 'en' : 'zh-CN';
function ui(zh, en){ return LOCALE==='en' ? en : zh; }
var UI_TEXT = {
  '页面导航':'Page navigation','仪表盘':'Dashboard','配置':'Config',
  '在 GitHub 查看 Token Usage':'View Token Usage on GitHub',
  '收起侧边栏':'Collapse sidebar','展开侧边栏':'Expand sidebar',
  '语言与外观':'Language and appearance','界面语言':'Interface language','切换主题':'Switch theme','选择主题配色':'Choose color palette',
  '主题配色':'Color palette','配色方案候选':'Color palette options',
  '用量概览':'Usage overview','按客户端、供应商、模型和项目查看用量':'Explore usage by client, provider, model, and project','查看数值单位说明':'View value unit definitions','数值单位':'Value units','十进制':'Decimal','一千':'thousand','一百万':'million','十亿':'billion',
  '统计区间':'Date range','自定义统计区间':'Custom date range','今日':'Today','本周':'This week','本月':'This month','本年':'This year','近 7 天':'Last 7 days','近 30 天':'Last 30 days','近 1 年':'Last 12 months',
  '上一月':'Previous month','下一月':'Next month','请选择开始与结束日期':'Select a start and end date','查询':'Apply',
  '按维度查看':'Explore by dimension','维度切换':'Dimension switcher','客户端':'Client','供应商':'Provider','模型':'Model','项目':'Project',
  '分组视图':'Group view','列表':'List','构成':'Composition','时间序列图表':'Time-series chart',
  '用量与缓存命中率趋势':'Token and Cache Hit trend','图表类型':'Chart type','趋势':'Trend','热力':'Heatmap',
  '用量趋势':'Usage trend','用量':'Tokens','峰值 ':'Peak ','按小时':'Hourly','按周':'Weekly','按日':'Daily',
  '空白为尚未发生':'Blank = not yet','低':'Low','高':'High',
  '用量最高的会话':'Highest-usage sessions','跟随当前时间范围':'Uses the current date range',
  '会话':'Session','时长':'Duration','查看完整会话标题':'View full session title','完整会话标题':'Full session title','关闭':'Close',
  '该区间暂无会话':'No sessions in this range','加载中':'Loading','数据加载失败':'Failed to load data',
  '本地设置':'Local settings','管理本机采集、查询与显示设置':'Manage local collection, query, and display settings',
  '采集与日志':'Collection and logs','管理数据来源、采集调度和运行日志':'Manage data sources, collection scheduling, and runtime logs',
  '客户端设置':'Clients','设置各客户端的数据来源与路由归因':'Configure each client data source and router attribution',
  '守护进程':'Daemon','设置自动采集的运行方式':'Configure automatic collection behavior','轮询间隔 poll_interval':'Polling interval · poll_interval',
  'SQLite 轮询间隔（秒），默认 30':'SQLite polling interval in seconds; default 30','轮询间隔（秒）':'Polling interval in seconds',
  '开机自启 autostart':'Launch at login · autostart','登录后自动拉起守护进程':'Start the daemon automatically after login','开机自启':'Launch at login',
  '日志':'Logging','设置日志级别、保留周期与目录':'Configure log level, retention, and directory','级别 level':'Level · level','default 跟随运行时默认值':'default follows the runtime default',
  '保留天数 max_days':'Retention days · max_days','超期日志自动清理，默认 7':'Automatically remove expired logs; default 7','日志保留天数':'Log retention days',
  '目录 dir':'Directory · dir','日志输出目录':'Log output directory',
  '查询与展示':'Queries and display','设置默认视图、输出列、自定义视图和组合查询':'Configure the default view, output columns, custom views, and groups',
  '默认视图':'Default view','选择打开仪表盘时首先展示的视图':'Choose the view shown when the dashboard opens',
  '默认打开':'Open by default','可选择内置视图、自定义视图或组合查询':'Choose a built-in view, custom view, or group',
  '恢复为 client':'Restore to client','将启动视图恢复为内置 client':'Restore the startup view to the built-in client view',
  '输出列':'Output columns','统一控制内置表格与自定义视图的指标列及顺序，不影响顶部概览':'Controls metric columns and order for built-in tables and custom views; does not affect the overview',
  '恢复默认七列':'Restore seven default columns','输出列已恢复为默认七列':'Output columns restored to the default seven',
  '已选输出列':'Selected output columns','可选输出列':'Available output columns','至少保留一列指标':'Keep at least one metric column',
  '组合查询':'Grouped queries','按顺序组合内置视图和自定义视图，一次输出多张表':'Combine built-in and custom views in order and output multiple tables at once',
  '自定义视图':'Custom views','选择至少两个内置维度，维度顺序就是结果表的列顺序':'Choose at least two built-in dimensions; their order becomes the result table column order',
  '新组合查询名':'New grouped query name','新增组合查询':'Add grouped query','新自定义视图名':'New custom view name','新增自定义视图':'Add custom view',
  '已选成员':'Selected members','可选视图':'Available views','组合查询至少需要 2 个成员':'A group needs at least two members','自定义视图至少需要 2 个维度':'A custom view needs at least two dimensions',
  '名称须为小写标识符（字母开头）':'Name must be a lowercase identifier beginning with a letter','保留名不可用':'Reserved names cannot be used','名称已存在':'Name already exists','名称已被另一张表使用':'Name is already used by another query',
  '供应商别名':'Provider aliases','将原始供应商标识替换为易读名称；修改时请删除后重新添加':'Replace raw provider identifiers with readable names; delete and re-add an entry to change it',
  '原始标签':'Raw identifier','显示名称':'Display name','例如 account:provider-plan':'e.g. account:provider-plan','例如 Provider Pro':'e.g. Provider Pro',
  '新别名 key':'New alias key','新别名 value':'New alias value','添加':'Add','删除':'Delete',
  'key 和 value 不能为空':'key and value are required','key 已存在':'key already exists',
  '恢复全部默认值':'Restore all defaults','恢复全部默认值？':'Restore all defaults?','客户端、路由、守护进程、日志、查询与展示设置和供应商别名都将载入为全部默认值（尚未保存）；确认后请再点击“保存”才会写入配置。':'Clients, routing, daemon, log, query & display settings, and provider aliases will be loaded as all-default values (not saved yet); confirm, then click Save to write the config.',
  '客户端、路由、查询、输出列和供应商别名都将恢复为服务端当前已保存的配置，当前未保存修改会丢失。':'Clients, routing, queries, output columns, and provider aliases will be restored to the configuration currently saved on the server. Unsaved changes will be lost.',
  '已恢复为已保存配置':'Restored saved config',
  '有未保存的修改':'Unsaved changes','已保存':'Saved','已修改（未保存）':'Modified (unsaved)','该区间暂无用量':'No usage in this range','已载入全部默认值（未保存）':'Defaults loaded (unsaved)','读取默认配置失败':'Failed to load defaults','离线 · 无法连接本地服务':'Offline · cannot reach the local service','离线':'Offline',
  '路由中间件选项':'Routing middleware options','修改路由中间件':'Change routing middleware','取消':'Cancel','保存':'Save','保存中…':'Saving…','应用':'Apply',
  '配置已保存':'Config saved','配置未变化':'No changes','已保存，但需要注意':'Saved, but attention needed','保存失败':'Save failed','读取配置失败':'Failed to load config',
  '配置已在别处被修改，请刷新页面后重试；当前修改已保留':'Config was changed elsewhere; reload the page and retry. Local edits are kept.',
  '查询配置存在解析问题，页面按默认值运行；悬停查看诊断':'Query config has parse issues; defaults are in effect. Hover for details.',
  '数据来源':'Data source','启用':'Enabled','路由中间件':'Routing middleware','未对接':'None','该客户端不支持路由归因':'This client does not support routing attribution',
  '路由中间件未发生变化':'Routing middleware unchanged',
  '名称':'Name','合计':'Total','总计':'Total','核心概览':'Key metrics','无':'None',
  '请选择开始和结束日期':'Select a start and end date','已选 ':'Selected ','已选开始 ':'Start ','，请选择结束日期':' selected; choose an end date',
  '已应用自定义区间 ':'Custom range applied: ','自定义区间':'Custom range','点击打开日历选择自定义区间':'Open the calendar to choose a custom range',
  '上移 ':'Move up ','下移 ':'Move down ','移除 ':'Remove ','点击加入':'Add','已上移 ':'Moved up ','已下移 ':'Moved down ','已加入 ':'Added ','已移除 ':'Removed ','顺序已更新':'Order updated',
  '切换到中文':'Switch to Chinese','切换到英文':'Switch to English',
  '当前浅色模式 · 点击切换到深色模式':'Light mode · Switch to dark mode','当前深色模式 · 点击切换到浅色模式':'Dark mode · Switch to light mode',
  '钴蓝':'Cobalt','湛蓝':'Azure','湖蓝':'Lake','藏青':'Navy','冰青':'Glacier','高对比、清晰':'High contrast and crisp','明亮、通透':'Bright and airy','青蓝、柔和':'Soft cyan blue','沉稳、低饱和':'Calm and muted','冷调、轻盈':'Cool and light'
};
var UI_REVERSE = {};
Object.keys(UI_TEXT).forEach(function(k){ UI_REVERSE[UI_TEXT[k]]=k; });
var textNodes = new WeakMap(), attrValues = new WeakMap();
function applyStaticLocale(scope){
  scope = scope || document.body;
  var walker = document.createTreeWalker(scope, NodeFilter.SHOW_TEXT, {
    acceptNode:function(n){ return n.parentElement && !/^(SCRIPT|STYLE)$/.test(n.parentElement.tagName) ? NodeFilter.FILTER_ACCEPT : NodeFilter.FILTER_REJECT; }
  });
  var nodes=[], n;
  while((n=walker.nextNode())) nodes.push(n);
  nodes.forEach(function(node){
    var key=textNodes.get(node), raw=node.nodeValue.trim();
    if(!key && UI_TEXT[raw]){ key=raw; textNodes.set(node,key); }
    else if(!key && UI_REVERSE[raw]){ key=UI_REVERSE[raw]; textNodes.set(node,key); }
    if(!key) return;
    var lead=node.nodeValue.match(/^\s*/)[0], tail=node.nodeValue.match(/\s*$/)[0];
    node.nodeValue=lead+ui(key,UI_TEXT[key])+tail;
  });
  scope.querySelectorAll('*').forEach(function(node){
    var stored=attrValues.get(node)||{};
    ['aria-label','title','placeholder'].forEach(function(attr){
      var cur=node.getAttribute(attr), key=stored[attr];
      if(!key && cur && UI_TEXT[cur]){ key=cur; stored[attr]=key; }
      else if(!key && cur && UI_REVERSE[cur]){ key=UI_REVERSE[cur]; stored[attr]=key; }
      if(key) node.setAttribute(attr,ui(key,UI_TEXT[key]));
    });
    attrValues.set(node,stored);
  });
}
function rangeName(i){ return ui(['今日','本周','本月','本年','近 7 天','近 30 天','近 1 年','自定义'][i],['Today','This week','This month','This year','Last 7 days','Last 30 days','Last 12 months','Custom'][i]); }
var DIM_LABELS_EN = { model:'Model', provider:'Provider', client:'Client', project:'Project', day:'Date', month:'Month', hour:'Hour', weekday:'Weekday' };
var DIM_LABELS_ZH = { model:'模型', provider:'供应商', client:'客户端', project:'项目', day:'日期', month:'月份', hour:'小时', weekday:'星期' };
function dimLabel(d){ return (LOCALE==='en'?DIM_LABELS_EN:DIM_LABELS_ZH)[d]||d; }
applyStaticLocale(document.body);

/* ---------- 输出指标列：ID 为 /api/config query.output_columns 契约字面量；
   field 是 /api/dashboard 行内取数字段（input → fresh_input）。 ---------- */
var COLS = [
  { id:'requests',    field:'requests',    label:'Requests',    zh:'请求数',   shortEn:'Req',     shortZh:'请求' },
  { id:'input',       field:'fresh_input', label:'Input',       zh:'输入',     shortEn:'In',      shortZh:'输入' },
  { id:'output',      field:'output',      label:'Output',      zh:'输出',     shortEn:'Out',     shortZh:'输出' },
  { id:'cache_read',  field:'cache_read',  label:'Cache Read',  zh:'缓存读取', shortEn:'C.Read',  shortZh:'缓存读' },
  { id:'reasoning',   field:'reasoning',   label:'Reasoning',   zh:'推理',     shortEn:'Reason',  shortZh:'推理' },
  { id:'total',       field:'total',       label:'Total',       zh:'总量',     shortEn:'Total',   shortZh:'总量' },
  { id:'cache_hit',   field:'cache_hit',   label:'Cache Hit',   zh:'缓存命中率', shortEn:'Hit',   shortZh:'命中率' },
  { id:'cache_create',field:'cache_create',label:'Cache Create',zh:'缓存写入', shortEn:'C.Create',shortZh:'缓存写' }
];
/* 默认七列（不含 cache_create），对齐服务端 ui.DefaultOutputColumns */
var DEFAULT_OUT = ['requests','input','output','cache_read','reasoning','total','cache_hit'];
function colById(id){ for(var i=0;i<COLS.length;i++){ if(COLS[i].id===id) return COLS[i]; } return null; }
function colLabel(c){ return c ? (LOCALE==='en' ? c.label : c.zh) : ''; }
function colShort(c){ return c ? (LOCALE==='en' ? c.shortEn : c.shortZh) : ''; }
/* 生效输出列：配置草稿已加载时跟随草稿，否则用默认七列 */
function colIds(){
  var c = CFG.draft && CFG.draft.query && CFG.draft.query.output_columns;
  if(Array.isArray(c) && c.length){
    var out=[], seen={};
    c.forEach(function(k){ if(colById(k) && !seen[k]){ seen[k]=1; out.push(k); } });
    if(out.length) return out;
  }
  return DEFAULT_OUT.slice();
}

/* ---------- 数值与日期格式化 ---------- */
function fmtInt(n){ return (n||0).toLocaleString('en-US'); }
/* 十进制 K/M/B（1K=1e3）；<1000 显示整数。入参为真实 token 数。 */
function fmtTok(v){
  v = +v||0;
  if(v>=1e9) return (v/1e9).toFixed(2)+'B';
  if(v>=1e8) return Math.round(v/1e6)+'M';
  if(v>=1e7) return (v/1e6).toFixed(1)+'M';
  if(v>=1e6) return (v/1e6).toFixed(2)+'M';
  if(v>=1e3) return (v/1e3).toFixed(v>=1e5?0:2)+'K';
  return fmtInt(Math.round(v));
}
function axisFmt(v){
  v = +v||0;
  if(v>=1e9) return (v/1e9).toFixed(1)+'B';
  if(v>=1e6) return (v/1e6)>=10 ? Math.round(v/1e6)+'M' : (Math.round(v/1e6*10)/10)+'M';
  if(v>=1e3) return (v/1e3)>=10 ? Math.round(v/1e3)+'K' : (Math.round(v/1e3*10)/10)+'K';
  return String(Math.round(v));
}
function niceMax(v){
  if(v<=0) return 1;
  var p = Math.pow(10, Math.floor(Math.log(v)/Math.LN10));
  var steps=[1,2,2.5,5,10];
  for(var i=0;i<steps.length;i++){ if(v <= steps[i]*p) return steps[i]*p; }
  return 10*p;
}
/* 命中率 = cache_read / (fresh_input + cache_read + cache_create)；分母 0 → null（显示 —/断线） */
function hitOf(inp, cr, cc){
  var den=(+inp||0)+(+cr||0)+(+cc||0);
  return den ? (+cr||0)/den : null;
}
function pctText(frac, dec){ return frac==null ? '—' : (frac*100).toFixed(dec)+'%'; }
function pad2(n){ return (n<10?'0':'')+n; }
function ymd(d){ return d.getFullYear()+'-'+pad2(d.getMonth()+1)+'-'+pad2(d.getDate()); }
function fmtMD(d){ return pad2(d.getMonth()+1)+'-'+pad2(d.getDate()); }
function dateOf(ds){ var p=String(ds||'').split('-'); return new Date(+p[0], +p[1]-1, +p[2]); }
function startToday(){ var d=new Date(); return new Date(d.getFullYear(), d.getMonth(), d.getDate()); }
function fmtDur(ms){
  ms = +ms||0; if(ms<0) ms=0;
  var h=Math.floor(ms/3600000), m=Math.floor((ms%3600000)/60000);
  return h+'h '+pad2(m)+'m';
}

/* ---------- 全局状态 ---------- */
var state = { range:0, view:'list', mode:'trend', meta:{}, data:null, heat:[] };
var CUSTOM = null;            /* 自定义区间 {s,e} 日期串；预设区间时恒为 null */
var DIMROWS = {};             /* 各维度当前展示行（构成图联动用） */
var fetchSeq = 0;             /* 区间请求序号：丢弃过期响应 */
var RANGE_KEY = 'tu-range';
function rangeBoundsFor(i){
  var t = startToday();
  if(i===7 && CUSTOM){
    var cs=dateOf(CUSTOM.s), ce=dateOf(CUSTOM.e);
    if(cs>ce){ var ct=cs; cs=ce; ce=ct; }
    return { s:cs, e:ce };
  }
  if(i===1){ var ws=new Date(t); ws.setDate(ws.getDate()-((ws.getDay()+6)%7)); return { s:ws, e:t }; }
  if(i===2) return { s:new Date(t.getFullYear(), t.getMonth(), 1), e:t };
  if(i===3) return { s:new Date(t.getFullYear(), 0, 1), e:t };
  var back=[0,0,0,0,6,29,364][i];
  var s=new Date(t); s.setDate(s.getDate()-back);
  return { s:s, e:t };
}
function rangeBounds(){ return rangeBoundsFor(state.range); }
function rangeDateText(){
  var b = rangeBounds();
  if(b.s.getFullYear()!==b.e.getFullYear()) return ymd(b.s)+' ~ '+ymd(b.e);
  return fmtMD(b.s)===fmtMD(b.e) ? fmtMD(b.s) : fmtMD(b.s)+' ~ '+fmtMD(b.e);
}
function restoreSavedRange(){
  CUSTOM=null; state.range=0;
  var raw=null;
  try{ raw=sessionStorage.getItem(RANGE_KEY); }catch(e){}
  if(!raw) return;
  try{
    var v=JSON.parse(raw);
    if(v && /^\d{4}-\d{2}-\d{2}$/.test(v.s||'') && /^\d{4}-\d{2}-\d{2}$/.test(v.e||'')){
      CUSTOM={ s:v.s, e:v.e }; state.range=7;
    }
  }catch(e){}
}
function saveCustomRange(){
  try{
    if(state.range===7 && CUSTOM) sessionStorage.setItem(RANGE_KEY, JSON.stringify({s:CUSTOM.s, e:CUSTOM.e}));
    else sessionStorage.removeItem(RANGE_KEY);
  }catch(e){}
}

/* ---------- toast ---------- */
var toastT = null;
function toast(msg){
  var t = el('toast');
  if(!t) return;
  t.textContent = msg;
  t.setAttribute('data-show','true');
  clearTimeout(toastT);
  toastT = setTimeout(function(){ t.setAttribute('data-show','false'); }, 3600);
}

/* ---------- 主题与配色 ---------- */
var PAL_NAMES = {
  cobalt:{zh:'钴蓝',en:'Cobalt'}, azure:{zh:'湛蓝',en:'Azure'}, lake:{zh:'湖蓝',en:'Lake'},
  navy:{zh:'藏青',en:'Navy'}, glacier:{zh:'冰青',en:'Glacier'}
};
function paletteName(k){ var p=PAL_NAMES[k]; return p ? (LOCALE==='en'?p.en:p.zh) : k; }
function setPalettePop(open){
  var pop=el('palette-pop'), btn=el('palette-toggle');
  if(!pop || !btn) return;
  pop.hidden=!open;
  btn.setAttribute('aria-expanded', open?'true':'false');
}
el('palette-toggle').addEventListener('click', function(e){
  e.stopPropagation();
  if(typeof calpop!=='undefined' && calpop && !calpop.hidden) closeCal();
  if(typeof DV!=='undefined' && DV && DV.open) dvClose(false);
  if(el('unit-pop') && !el('unit-pop').hidden) setUnitPop(false);
  setPalettePop(el('palette-pop').hidden);
});
el('palette-pop').addEventListener('click', function(e){ e.stopPropagation(); });
document.addEventListener('click', function(){ setPalettePop(false); });
document.addEventListener('keydown', function(e){
  if(e.key==='Escape' && el('palette-pop') && !el('palette-pop').hidden){ setPalettePop(false); el('palette-toggle').focus(); }
});
el('palette-dots').addEventListener('click', function(e){
  var b = e.target.closest('[data-pal]'); if(!b) return;
  root.setAttribute('data-palette', b.dataset.pal);
  try{ localStorage.setItem('tu-palette', b.dataset.pal); }catch(_){}
  syncPalette();
  setPalettePop(false);
  el('palette-toggle').focus();
});
function syncPalette(){
  var cur = root.getAttribute('data-palette');
  document.querySelectorAll('.palette-choice').forEach(function(b){
    b.setAttribute('aria-pressed', b.dataset.pal===cur ? 'true':'false');
  });
  var pn = el('pal-name');
  if(pn) pn.textContent = paletteName(cur);
  var pt=el('palette-toggle');
  if(pt){
    var pl=ui('当前配色：'+paletteName(cur)+' · 点击选择主题配色','Current palette: '+paletteName(cur)+' · Choose a color palette');
    pt.setAttribute('aria-label',pl); pt.setAttribute('title',pl);
  }
  document.querySelectorAll('.palette-choice').forEach(function(b){
    var name=paletteName(b.dataset.pal), lab=ui('切换配色：'+name,'Switch palette: '+name);
    b.title=name; b.setAttribute('aria-label',lab);
  });
}
/* 深浅主题：单按钮表示下一步动作；tu-theme 持久化，不入 Config 保存流程 */
function applyTheme(next){
  root.setAttribute('data-theme', next);
  try{ localStorage.setItem('tu-theme', next); }catch(_){}
  syncThemeSeg();
}
function syncThemeSeg(){
  var t = root.getAttribute('data-theme');
  var sw = el('theme-toggle');
  if(!sw) return;
  var next = t==='light' ? 'dark' : 'light';
  var lab = t==='light'
    ? ui('当前浅色模式 · 点击切换到深色模式','Light mode · Switch to dark mode')
    : ui('当前深色模式 · 点击切换到浅色模式','Dark mode · Switch to light mode');
  sw.setAttribute('aria-label', lab);
  sw.setAttribute('title', lab);
  var use=el('theme-toggle-use');
  if(use) use.setAttribute('href', next==='light' ? '#i-sun' : '#i-moon');
}
el('theme-toggle').addEventListener('click', function(){
  applyTheme(root.getAttribute('data-theme')==='light' ? 'dark' : 'light');
});
syncThemeSeg();
try{
  matchMedia('(prefers-color-scheme: light)').addEventListener('change', function(e){
    var t=null; try{ t=localStorage.getItem('tu-theme'); }catch(_){}
    if(!t) applyTheme(e.matches?'light':'dark');
  });
}catch(_){}
syncPalette();

/* ---------- 导航（仅处理带 data-page 的导航行） ---------- */
document.querySelectorAll('.nav-button[data-page]').forEach(function(b){
  b.addEventListener('click', function(){
    setPalettePop(false);
    if(typeof calpop!=='undefined' && calpop && !calpop.hidden) closeCal();
    if(typeof DV!=='undefined' && DV && DV.open) dvClose(false);
    document.querySelectorAll('.nav-button').forEach(function(x){ x.removeAttribute('aria-current'); });
    b.setAttribute('aria-current','page');
    document.querySelectorAll('.page').forEach(function(p){ p.removeAttribute('data-active'); p.setAttribute('aria-hidden','true'); p.hidden=true; });
    var targetPage=el('page-'+b.dataset.page);
    targetPage.hidden=false;
    targetPage.setAttribute('aria-hidden','false');
    targetPage.setAttribute('data-active','true');
    window.scrollTo(0, 0);
    if(b.dataset.page==='dash' && state.data) drawChart();
    if(b.dataset.page==='config' && !CFG.loaded && !CFG.loading) loadConfig();
  });
});

/* ---------- 工具条：区间 / 视图 / 形态 ---------- */
function bindSeg(id, fn){
  var host=el(id); if(!host) return;
  host.addEventListener('click', function(e){
    var b = e.target.closest('button'); if(!b) return;
    host.querySelectorAll('button').forEach(function(x){ x.setAttribute('aria-pressed', x===b?'true':'false'); });
    fn(b.dataset.v);
  });
}
function syncRangeSeg(){
  el('range-seg').querySelectorAll('button').forEach(function(b){
    b.setAttribute('aria-pressed', +b.dataset.v===state.range?'true':'false');
  });
  var on = state.range===7;
  el('range-custom').classList.toggle('on', on);
  el('range-custom').setAttribute('aria-pressed', on?'true':'false');
  syncCustomTxt();
}
function syncCustomTxt(){
  /* 日期入口 = 当前查询实际区间的反馈 + 日历入口；自定义半选时临时显示已选开始 */
  var b = rangeBounds();
  var s = ymd(b.s), e = ymd(b.e);
  var full, short;
  if(state.range===7 && CAL.selS && !CAL.selE){
    full = CAL.selS; short = CAL.selS.slice(5);
  }else{
    full = s===e ? s : s+' ~ '+e;
    short = fmtMD(b.s)===fmtMD(b.e) ? fmtMD(b.s) : fmtMD(b.s)+' ~ '+fmtMD(b.e);
  }
  el('range-custom-txt').textContent = full;
  el('range-custom-short').textContent = short;
  var tip = (state.range===7 ? ui('自定义区间','Custom range') : rangeName(state.range))+' '+full+' · '+ui('点击打开日历选择自定义区间','Open the calendar to choose a custom range');
  el('range-custom').title = tip;
  el('range-custom').setAttribute('aria-label', tip);
}
bindSeg('range-seg', function(v){
  setPalettePop(false);
  if(!calpop.hidden) closeCal();
  state.range = +v;
  CUSTOM = state.range===7 ? CUSTOM : null;
  if(state.range!==7) saveCustomRange();
  var b = rangeBounds();
  setCalSel(ymd(b.s), ymd(b.e));
  renderCal();
  syncCustomTxt();
  loadDashboard();
});
bindSeg('view-seg', function(v){ state.view=v; if(state.data) renderGroups(); });
bindSeg('mode-seg', function(v){ state.mode=v; drawChart(); });

/* ---------- 自定义区间：双月日历选择器（点起止两端 + hover 预览 + 查询应用） ---------- */
var CAL = { view:null, selS:null, selE:null, hover:null };
var calpop = el('calpop'), calGrids = el('cal-grids');
var WDH_ZH = ['一','二','三','四','五','六','日'];
var WDH_EN = ['M','T','W','T','F','S','S'];
function setCalSel(s, e){
  CAL.selS = s; CAL.selE = e; CAL.hover = null;
  var p = s.split('-');
  CAL.view = new Date(+p[0], +p[1]-1, 1);
}
function openCal(){
  /* 打开日历即预选当前查询范围：丢弃未应用的残留半选，始终从当前区间出发 */
  if(!(state.range===7 && CAL.selS && CAL.selE)){
    var b = rangeBounds();
    setCalSel(ymd(b.s), ymd(b.e));
    renderCal();
  }
  calpop.hidden = false;
  el('range-custom').setAttribute('aria-expanded','true');
}
function closeCal(){
  calpop.hidden = true;
  el('range-custom').setAttribute('aria-expanded','false');
}
function calShift(dir){
  CAL.view = new Date(CAL.view.getFullYear(), CAL.view.getMonth()+dir, 1);
  renderCal();
}
function calCell(d){
  var ds = ymd(d);
  var cls = 'cal-d';
  var lo = null, hi = null;
  if(CAL.selS && CAL.selE){ lo = CAL.selS<CAL.selE?CAL.selS:CAL.selE; hi = CAL.selS<CAL.selE?CAL.selE:CAL.selS; }
  else if(CAL.selS && CAL.hover){ lo = CAL.selS<CAL.hover?CAL.selS:CAL.hover; hi = CAL.selS<CAL.hover?CAL.hover:CAL.selS; }
  if(ds===CAL.selS || ds===CAL.selE) cls += ' sel';
  else if(lo && hi && ds>=lo && ds<=hi) cls += (CAL.selS && !CAL.selE) ? ' pv' : ' in';
  var dateLabel=LOCALE==='en'?d.toLocaleDateString('en-US',{year:'numeric',month:'long',day:'numeric'}):ds.replace(/-/g,' 年 ').replace(' 年 '+pad2(d.getDate()),' 月 '+pad2(d.getDate())+' 日');
  return '<button type="button" class="'+cls+'" data-d="'+ds+'" aria-label="'+dateLabel+'" aria-pressed="'+(ds===CAL.selS||ds===CAL.selE)+'">'+d.getDate()+'</button>';
}
function renderCal(){
  var html = '';
  for(var mi=0; mi<2; mi++){
    var y = CAL.view.getFullYear(), m = CAL.view.getMonth()+mi;
    var off = (new Date(y, m, 1).getDay()+6)%7;
    var dim = new Date(y, m+1, 0).getDate();
    var rows = Math.ceil((off+dim)/7);
    var wd=LOCALE==='en'?WDH_EN:WDH_ZH;
    var mlabel = LOCALE==='en' ? new Date(y,m,1).toLocaleDateString('en-US',{month:'long',year:'numeric'}) : y+' 年 '+(m+1)+' 月';
    html += '<div class="cal-grid" role="grid" aria-label="'+mlabel+'"><div class="cal-mt">'+mlabel+'</div><div class="cal-wd">'+wd.map(function(w){ return '<span>'+w+'</span>'; }).join('')+'</div><div class="cal-days">';
    for(var i=0;i<rows*7;i++){
      if(i<off || i>=off+dim){ html += '<span class="cal-pad"></span>'; continue; }
      html += calCell(new Date(y, m, 1-off+i));
    }
    html += '</div></div>';
  }
  calGrids.innerHTML = html;
  el('cal-next').disabled=false;
  el('cal-range-txt').textContent = (CAL.selS && CAL.selE)
    ? ui('已选 ','Selected ')+CAL.selS.slice(5).replace('-','/')+' ~ '+CAL.selE.slice(5).replace('-','/')
    : (CAL.selS ? ui('已选开始 '+CAL.selS.slice(5)+'，请选择结束日期','Start '+CAL.selS.slice(5)+' selected; choose an end date') : ui('请选择开始与结束日期','Select a start and end date'));
}
function calPick(ds){
  if(!CAL.selS || (CAL.selS && CAL.selE)){ CAL.selS = ds; CAL.selE = null; }
  else if(ds === CAL.selS){ CAL.selE = ds; }
  else if(ds < CAL.selS){ CAL.selE = CAL.selS; CAL.selS = ds; }
  else { CAL.selE = ds; }
  renderCal();
  syncCustomTxt();
}
el('range-custom').addEventListener('click', function(e){
  e.stopPropagation();
  setPalettePop(false);
  if(typeof DV!=='undefined' && DV && DV.open) dvClose(false);
  if(el('unit-pop') && !el('unit-pop').hidden) setUnitPop(false);
  if(calpop.hidden) openCal(); else closeCal();
});
calGrids.addEventListener('click', function(e){
  e.stopPropagation();
  var b = e.target.closest('button.cal-d'); if(!b) return;
  calPick(b.dataset.d);
});
calGrids.addEventListener('mouseover', function(e){
  var b = e.target.closest('button.cal-d'); if(!b) return;
  if(CAL.hover !== b.dataset.d){ CAL.hover = b.dataset.d; renderCal(); }
});
calGrids.addEventListener('mouseleave', function(){
  if(CAL.hover !== null){ CAL.hover = null; renderCal(); }
});
el('cal-prev').addEventListener('click', function(){ calShift(-1); });
el('cal-next').addEventListener('click', function(){ calShift(1); });
el('cal-apply').addEventListener('click', function(){
  if(!CAL.selS || !CAL.selE){ toast(ui('请选择开始和结束日期','Select a start and end date')); return; }
  CUSTOM = { s: CAL.selS, e: CAL.selE };
  state.range = 7;
  saveCustomRange();
  closeCal();
  syncRangeSeg();
  loadDashboard();
  toast(ui('已应用自定义区间 ','Custom range applied: ')+rangeDateText());
});
document.addEventListener('click', function(e){
  if(!calpop.hidden && !e.target.closest('.calwrap') && !e.target.closest('#range-seg')) closeCal();
});
document.addEventListener('keydown', function(e){
  if(e.key!=='Escape') return;
  if(!calpop.hidden){ closeCal(); el('range-custom').focus(); }
});

/* ---------- 有序点选器：已选=单列排序列表（纵向拖拽 + 上移/下移键盘操作），可选=chip 点击加入 ---------- */
function pchip(v, text){
  return '<button type="button" class="pchip" data-k="'+esc(v)+'" title="'+ui('点击加入','Add')+'">'+esc(text||v)+'</button>';
}
function ordItemHtml(k, text, i, n){
  return '<div class="ord-item" draggable="true" data-k="'+esc(k)+'">' +
    '<span class="ord-grip" aria-hidden="true"><i></i></span>' +
    '<span class="ord-idx">'+(i+1)+'</span>' +
    '<span class="ord-name" title="'+esc(text)+'">'+esc(text)+'</span>' +
    '<button type="button" class="ord-btn" data-move="up" aria-label="'+ui('上移 ','Move up ')+esc(text)+'"'+(i===0?' disabled':'')+'><svg class="ic" style="transform:rotate(90deg)"><use href="#i-chev-l"/></svg></button>' +
    '<button type="button" class="ord-btn" data-move="down" aria-label="'+ui('下移 ','Move down ')+esc(text)+'"'+(i===n-1?' disabled':'')+'><svg class="ic" style="transform:rotate(-90deg)"><use href="#i-chev-l"/></svg></button>' +
    '<button type="button" class="ord-btn del" data-remove aria-label="'+ui('移除 ','Remove ')+esc(text)+'"><svg class="ic" style="transform:rotate(45deg)"><use href="#i-plus"/></svg></button>' +
    '</div>';
}
function makeOrderedPick(mount, cfg){
  var host = typeof mount==='string' ? el(mount) : mount;
  function sel(){
    return [].slice.call(host.querySelectorAll('.ord-item')).map(function(c){ return c.dataset.k; });
  }
  /* commit = 以 cfg.selected 为唯一事实源重新渲染，再通知外部 */
  function commit(message){
    render();
    if(cfg.onChange) cfg.onChange();
    if(message) toast(message);
  }
  function move(k, dir){
    var i = cfg.selected.indexOf(k), j = i+dir;
    if(i<0 || j<0 || j>=cfg.selected.length) return;
    var a = cfg.selected.slice();
    a[i] = cfg.selected[j]; a[j] = cfg.selected[i];
    cfg.selected = a;
    commit((dir<0 ? ui('已上移 ','Moved up ') : ui('已下移 ','Moved down '))+cfg.text(k));
  }
  function render(){
    var s = cfg.selected.slice();
    var off = cfg.values.filter(function(v){ return s.indexOf(v)<0; });
    host.innerHTML =
      '<div class="pz"><span class="pz-l">'+cfg.labelOn+'</span><div class="ord-list">' +
      (s.length ? s.map(function(v,i){ return ordItemHtml(v, cfg.text(v), i, s.length); }).join('') : '<span class="pz-empty">'+ui('无','None')+'</span>') +
      '</div></div>' +
      '<div class="pz"><span class="pz-l">'+cfg.labelOff+'</span><div class="pz-chips">' +
      (off.length ? off.map(function(v){ return pchip(v, cfg.text(v)); }).join('') : '<span class="pz-empty">'+ui('无','None')+'</span>') +
      '</div></div>';
    /* 可选区：一般追加；输出列按候选规范位置重新插入 */
    host.querySelectorAll('.pz-chips .pchip').forEach(function(c){
      c.addEventListener('click', function(){
        var k = c.dataset.k;
        if(cfg.insertByReference){
          var ref = cfg.values.indexOf(k), at = cfg.selected.length;
          for(var i=0;i<cfg.selected.length;i++){
            if(cfg.values.indexOf(cfg.selected[i])>ref){ at=i; break; }
          }
          cfg.selected.splice(at,0,k);
        }else{
          cfg.selected.push(k);
        }
        commit(ui('已加入 ','Added ')+cfg.text(k));
      });
    });
    /* 已选区：移除 / 上移 / 下移（键盘可达，min 保护） */
    host.querySelectorAll('.ord-item').forEach(function(item){
      var k = item.dataset.k;
      item.querySelector('[data-remove]').addEventListener('click', function(){
        if(cfg.selected.length<=cfg.min){ toast(cfg.minMsg); return; }
        cfg.selected = cfg.selected.filter(function(x){ return x!==k; });
        commit(ui('已移除 ','Removed ')+cfg.text(k));
      });
      item.querySelector('[data-move="up"]').addEventListener('click', function(){ move(k,-1); });
      item.querySelector('[data-move="down"]').addEventListener('click', function(){ move(k,1); });
    });
    /* 纵向拖拽：单列整行按中线判定前后 */
    var list = host.querySelector('.ord-list');
    if(!list) return;
    var dragging = null;
    list.addEventListener('dragstart', function(e){
      var item = e.target.closest('.ord-item'); if(!item) return;
      dragging = item;
      e.dataTransfer.setData('text/plain', item.dataset.k);
      e.dataTransfer.effectAllowed = 'move';
      requestAnimationFrame(function(){ item.classList.add('dragging'); });
    });
    list.addEventListener('dragover', function(e){
      if(!dragging) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
      var over = e.target.closest('.ord-item');
      if(!over || over===dragging) return;
      var r = over.getBoundingClientRect();
      if(e.clientY < r.top + r.height/2){ if(over.previousSibling!==dragging) list.insertBefore(dragging, over); }
      else if(over.nextSibling!==dragging){ list.insertBefore(dragging, over.nextSibling); }
    });
    list.addEventListener('drop', function(e){ e.preventDefault(); });
    list.addEventListener('dragend', function(){
      if(!dragging) return;
      dragging.classList.remove('dragging');
      dragging = null;
      var order = sel();
      if(order.join(',') !== cfg.selected.join(',')){ cfg.selected = order; commit(ui('顺序已更新','Order updated')); }
    });
  }
  render();
  return {
    get: function(){ return sel(); },
    set: function(arr){ cfg.selected = arr.slice(); render(); if(cfg.onChange) cfg.onChange(); }
  };
}

/* ---------- 输出列编辑器：编辑直接写入配置草稿（保存后生效，不写 localStorage） ---------- */
var colsPick = null;
function buildColsEditor(){
  var host = el('cols-editor');
  if(!host) return;
  if(!CFG.draft){ host.innerHTML=''; colsPick=null; return; }
  colsPick = makeOrderedPick(host, {
    values: COLS.map(function(c){ return c.id; }),
    selected: colIds(),
    labelOn:ui('已选输出列','Selected output columns'), labelOff:ui('可选输出列','Available output columns'),
    insertByReference:true,
    min:1, minMsg:ui('至少保留一列指标','Keep at least one metric column'),
    text: function(k){ var c = colById(k); return c ? colLabel(c) : k; },
    onChange: function(){
      if(colsPick) CFG.draft.query.output_columns = colsPick.get();
      recomputeDirty();
      if(state.data && state.view==='list') renderGroups();
      renderCustomViews();
    }
  });
}
el('cols-default').addEventListener('click', function(){
  if(!CFG.draft) return;
  CFG.draft.query.output_columns = DEFAULT_OUT.slice();
  buildColsEditor();
  if(state.data && state.view==='list') renderGroups();
  renderCustomViews();
  recomputeDirty();
  toast(ui('输出列已恢复为默认七列','Output columns restored to the default seven'));
});

/* ---------- 侧栏收起/展开（64px 图标栏，选择持久化） ---------- */
var appEl = document.querySelector('.app');
var sideBtn = el('side-toggle');
function syncSide(){
  var min = false;
  try{ min = localStorage.getItem('tu-side')==='min'; }catch(e){}
  appEl.classList.toggle('side-min', min);
  sideBtn.setAttribute('aria-expanded', min?'false':'true');
  sideBtn.setAttribute('aria-label', min?ui('展开侧边栏','Expand sidebar'):ui('收起侧边栏','Collapse sidebar'));
}
sideBtn.addEventListener('click', function(){
  var min = !appEl.classList.contains('side-min');
  appEl.classList.toggle('side-min', min);
  try{ localStorage.setItem('tu-side', min?'min':'full'); }catch(e){}
  syncSide();
  if(el('page-dash').getAttribute('data-active')==='true' && state.data) drawChart();
});
syncSide();

/* ---------- KPI ---------- */
function kpiValueHTML(value, explicitUnit){
  var text=String(value), unit=explicitUnit || '';
  if(!unit){
    var match=text.match(/^(.+?)([KMB%])$/);
    if(match){ text=match[1]; unit=match[2]; }
  }
  return '<span>'+esc(text)+'</span>'+(unit?'<small class="metric-unit">'+unit+'</small>':'');
}
function renderKPIs(){
  var host = el('kpis');
  if(!host) return;
  if(!state.data){
    host.innerHTML = '<p class="page-note">'+ui('加载中','Loading')+'</p>';
    return;
  }
  var T = state.data.totals||{};
  var hit = hitOf(T.fresh_input, T.cache_read, T.cache_create);
  var cReq=colById('requests'), cIn=colById('input'), cOut=colById('output'), cCr=colById('cache_read'), cRs=colById('reasoning'), cHit=colById('cache_hit'), cTot=colById('total');
  var metrics = [
    [colLabel(cReq), fmtInt(T.requests)],
    [colLabel(cIn), fmtTok(T.fresh_input)],
    [colLabel(cOut), fmtTok(T.output)],
    [colLabel(cCr), fmtTok(T.cache_read)],
    [colLabel(cRs), fmtTok(T.reasoning)],
    [colLabel(cHit), pctText(hit, 2)]
  ];
  var metricHTML=metrics.map(function(m){
    return '<div class="kpi-metric"><p class="stat-label">'+m[0]+'</p><span class="metric-value">'+kpiValueHTML(m[1])+'</span></div>';
  }).join('');
  /* 核心概览：七项固定口径均可读，不随 Config「输出列」变化 */
  host.innerHTML =
    '<section class="kpi-rail" aria-label="'+ui('核心概览','Key metrics')+'">' +
      '<div class="kpi-metric kpi-metric-total"><p class="stat-label">'+colLabel(cTot)+' · '+rangeName(state.range)+'</p><span class="metric-value">'+kpiValueHTML(fmtTok(T.total))+'</span></div>' +
      '<div class="kpi-support">'+metricHTML+'</div>' +
    '</section>';
}

function setUnitPop(open){
  var pop=el('unit-pop'), btn=el('unit-trigger');
  if(!pop || !btn) return;
  pop.hidden=!open;
  btn.setAttribute('aria-expanded',open?'true':'false');
}
el('unit-trigger').addEventListener('click',function(e){
  e.stopPropagation();
  if(!el('palette-pop').hidden) setPalettePop(false);
  if(!calpop.hidden) closeCal();
  if(typeof DV!=='undefined' && DV && DV.open) dvClose(false);
  setUnitPop(el('unit-pop').hidden);
});
el('unit-pop').addEventListener('click',function(e){ e.stopPropagation(); });
document.addEventListener('click',function(){ setUnitPop(false); });
document.addEventListener('keydown',function(e){
  if(e.key==='Escape' && el('unit-pop') && !el('unit-pop').hidden){ setUnitPop(false); el('unit-trigger').focus(); }
});

/* ---------- 按维度查看：真实区间聚合行，直接渲染 ---------- */
var HUES = { client:'--d1', provider:'--d2', model:'--d3', project:'--d4' };
var DIMS = ['client','provider','model','project'];
function normRows(raw){
  return (raw||[]).map(function(r){
    var o = {
      name: r.key||'',
      requests: +r.requests||0, fresh_input: +r.fresh_input||0, output: +r.output||0,
      cache_read: +r.cache_read||0, cache_create: +r.cache_create||0, reasoning: +r.reasoning||0,
      total: +r.total||0, hit: null, isOther:false, hiddenCount:0
    };
    o.hit = hitOf(o.fresh_input, o.cache_read, o.cache_create);
    return o;
  });
}
function rowVal(r, id){
  var c = colById(id);
  if(!c) return 0;
  if(c.id==='cache_hit') return r.hit;
  return r[c.field];
}
function aggRows(rows){
  var t={requests:0,fresh_input:0,output:0,cache_read:0,cache_create:0,reasoning:0,total:0,hit:null};
  rows.forEach(function(r){
    t.requests+=r.requests; t.fresh_input+=r.fresh_input; t.output+=r.output;
    t.cache_read+=r.cache_read; t.cache_create+=r.cache_create||0; t.reasoning+=r.reasoning; t.total+=r.total;
  });
  t.hit = hitOf(t.fresh_input, t.cache_read, t.cache_create);
  return t;
}
/* 尾部小项收拢：≥4 独立项、候选份额<5%、并入后不超过前一项；并入 ≥2 项才聚合成唯一的“其他”行 */
function compactDimensionTail(rows){
  var ordered=rows.slice().sort(function(a,b){ return b.total-a.total || String(a.name).localeCompare(String(b.name)); });
  var total=aggRows(ordered).total, picked=[], mergedTotal=0;
  while(ordered.length>4){
    var candidate=ordered[ordered.length-1], previous=ordered[ordered.length-2];
    if(!total || candidate.total/total>=0.05 || mergedTotal+candidate.total>=previous.total) break;
    picked.unshift(ordered.pop());
    mergedTotal+=candidate.total;
  }
  if(picked.length<2){
    while(picked.length) ordered.push(picked.shift());
    return ordered.sort(function(a,b){ return b.total-a.total || String(a.name).localeCompare(String(b.name)); });
  }
  var other={name:ui('其他（'+picked.length+' 项）','Other ('+picked.length+')'),isOther:true,hiddenCount:picked.length,
    requests:0,fresh_input:0,output:0,cache_read:0,cache_create:0,reasoning:0,total:0,hit:null};
  picked.forEach(function(r){
    other.requests+=r.requests; other.fresh_input+=r.fresh_input; other.output+=r.output;
    other.cache_read+=r.cache_read; other.cache_create+=r.cache_create||0; other.reasoning+=r.reasoning; other.total+=r.total;
  });
  other.hit = hitOf(other.fresh_input, other.cache_read, other.cache_create);
  ordered.push(other); /* “其他”是尾部小项的合计，固定最后，不参与降序插队 */
  return ordered;
}
function cellFor(k, r, extra){
  if(k==='cache_hit') return '<span class="nv">'+pctText(r.hit,1)+'</span>';
  if(k==='total') return '<span class="nv">'+fmtTok(rowVal(r,k))+'</span><i class="mb mb-a" style="width:'+extra+'%"></i>';
  if(k==='requests') return fmtInt(rowVal(r,k));
  return '<span class="nv">'+fmtTok(rowVal(r,k))+'</span>';
}
function cellAgg(k, T){
  if(k==='cache_hit') return pctText(T.hit,1);
  if(k==='requests') return fmtInt(T.requests);
  return fmtTok(rowVal(T,k));
}
function tableView(rows){
  var ids=colIds();
  var maxT = rows.length && rows[0].total>0 ? rows[0].total : 1, T = aggRows(rows);
  var colgroup = '<colgroup><col style="width:21%">'+ids.map(function(){ return '<col>'; }).join('')+'</colgroup>';
  var head = ids.map(function(k){
    var c = colById(k);
    return '<th scope="col" title="'+colLabel(c)+'">'+colShort(c)+'</th>';
  }).join('');
  var h = '<div class="group-table"><div class="table-wrap group-rows"><table>'+colgroup+'<thead><tr><th scope="col">'+ui('名称','Name')+'</th>' + head +
    '</tr></thead><tbody>';
  rows.forEach(function(r){
    h += '<tr class="group-row" tabindex="0" aria-expanded="false"><th scope="row" class="tname">'+esc(r.name)+'</th>' +
      ids.map(function(k){
        return '<td class="num'+(k==='total'?' tot':'')+'" data-k="'+k+'" data-label="'+colLabel(colById(k))+'">'+cellFor(k, r, (r.total/maxT*100).toFixed(1))+'</td>';
      }).join('') + '</tr>';
  });
  h += '</tbody></table></div><div class="table-wrap group-summary"><table aria-label="'+ui('合计','Total')+'">'+colgroup+'<tbody><tr class="trow"><th scope="row">'+ui('合计','Total')+'</th>' +
    ids.map(function(k){ return '<td class="num'+(k==='total'?' tot':'')+'" data-k="'+k+'" data-label="'+colLabel(colById(k))+'">'+cellAgg(k, T)+'</td>'; }).join('') +
    '</tr>';
  return h + '</tbody></table></div></div>';
}
var RAMP = [100,83,67,52,39,27];
function arcPath(r1, r2, a0, a1){
  var rad = function(a){ return (a-90)*Math.PI/180; };
  var pt = function(r, a){ return (100 + r*Math.cos(rad(a))).toFixed(2)+' '+(100 + r*Math.sin(rad(a))).toFixed(2); };
  var large = (a1-a0) > 180 ? 1 : 0;
  return 'M '+pt(r2,a0)+' A '+r2+' '+r2+' 0 '+large+' 1 '+pt(r2,a1)+
         ' L '+pt(r1,a1)+' A '+r1+' '+r1+' 0 '+large+' 0 '+pt(r1,a0)+' Z';
}
/* 构成图小份额合并：isOther 行或排名≥3 且份额<5% 的扇区并入图例“其他”（标注成员数） */
function donutMetas(rows){
  var T = aggRows(rows), metas = [], otherN = 0, otherTotal = 0;
  rows.forEach(function(r,i){
    if(r.isOther){
      /* hiddenCount 由聚合方显式给出;数字正则只作旧数据兜底 */
      otherN += r.hiddenCount!=null ? r.hiddenCount : (String(r.name||'').match(/\d+/) ? +String(r.name).match(/\d+/)[0] : 1);
      otherTotal += r.total;
      return;
    }
    if(i>=3 && T.total && r.total/T.total<0.05){ otherN++; otherTotal += r.total; return; }
    metas.push({ name:r.name, total:r.total });
  });
  if(otherN) metas.push({ name:ui('其他（'+otherN+' 项）','Other ('+otherN+')'), total:otherTotal });
  return { metas:metas, T:T };
}
function composeView(dimId, rows){
  var meta = donutMetas(rows), T = meta.T, metas = meta.metas;
  var title = dimLabel(dimId);
  var segs = '', acc = 0, GAP = 1.2;
  metas.forEach(function(m,i){
    var share = T.total ? m.total/T.total : 0;
    var a0 = acc*360, a1 = Math.min((acc + share)*360, 360);
    acc += share;
    var s0 = a0 + GAP/2, s1 = Math.max(a1 - GAP/2, s0 + 0.4);
    segs += '<path class="seg-d" data-i="'+i+'" d="'+arcPath(50, 94, s0, s1)+'" fill="color-mix(in srgb, var(--gh) '+RAMP[i%RAMP.length]+'%, var(--raised))">' +
      '<title>'+esc(m.name)+' · '+(share*100).toFixed(1)+'% · '+fmtTok(m.total)+'</title></path>';
  });
  var lg = metas.map(function(m,i){
    var share = T.total ? m.total/T.total : 0;
    return '<li tabindex="0" data-i="'+i+'" aria-label="'+esc(m.name)+ui('，占比 ',', ')+(share*100).toFixed(1)+'%, '+fmtTok(m.total)+'">' +
      '<i class="lg-dot" style="background:color-mix(in srgb, var(--gh) '+RAMP[i%RAMP.length]+'%, var(--raised))"></i>' +
      '<span class="lg-name">'+esc(m.name)+'</span><span class="lg-val">'+(share*100).toFixed(1)+'%</span></li>';
  }).join('');
  return '<div class="compose"><div class="donut-wrap">' +
    '<svg class="donut" viewBox="0 0 200 200" role="img" aria-label="'+title+ui('构成图',' composition chart')+'">' +
    '<circle cx="100" cy="100" r="72" fill="none" stroke-width="44" style="stroke:color-mix(in srgb, var(--gh) 14%, var(--raised))"/>' +
    '<g transform="rotate(-90 100 100)">'+segs+'</g></svg>' +
    '<div class="donut-c"><b>'+fmtTok(T.total)+'</b><span>'+title+' · '+colLabel(colById('total'))+'</span></div></div>' +
    '<ul class="legend">'+lg+'</ul></div>';
}
function renderGroups(){
  var host = el('groups');
  if(!host) return;
  if(!state.data){ host.innerHTML='<p class="page-note">'+ui('加载中','Loading')+'</p>'; return; }
  var dims = (state.data.dimensions)||{};
  var html = '';
  DIMS.forEach(function(id){
    var rows = compactDimensionTail(normRows(dims[id]));
    DIMROWS[id] = rows;
    html += '<article class="group" data-gid="'+id+'" style="--gh:var('+HUES[id]+')">' +
      '<header class="group-head"><div class="group-title"><i class="g-dot"></i>' +
      '<div><h4>'+dimLabel(id)+'</h4></div></div></header>' +
      (state.view==='list' ? tableView(rows) : composeView(id, rows)) +
      '</article>';
  });
  host.innerHTML = html;
  applyNarrowDim();
  if(state.view==='compose'){
    DIMS.forEach(function(id){
      var art = groupsEl.querySelector('.group[data-gid="'+id+'"]');
      if(art) art._donut = donutMetas(DIMROWS[id]);
    });
  }
}
/* 构成图联动高亮 */
var groupsEl = el('groups');
groupsEl.addEventListener('mouseover', function(e){
  var art = e.target.closest('.group'); if(!art) return;
  var rows = DIMROWS[art.dataset.gid]; if(!rows) return;
  var seg = e.target.closest('.seg-d');
  var li = e.target.closest('.legend [data-i]');
  var i = seg ? +seg.dataset.i : (li ? +li.dataset.i : -1);
  highlightDonut(art, rows, i);
});
groupsEl.addEventListener('mouseout', function(e){
  var art = e.target.closest('.group');
  if(art && !art.contains(e.relatedTarget)) highlightDonut(art, DIMROWS[art.dataset.gid]||[], -1);
});
function highlightDonut(art, rows, i){
  var wrap = art.querySelector('.donut-wrap');
  if(wrap) wrap.classList.toggle('has-hl', i>=0);
  var circles = art.querySelectorAll('.seg-d');
  circles.forEach(function(c,j){ c.classList.toggle('hl', j===i); });
  art.querySelectorAll('.legend li').forEach(function(li,j){ li.classList.toggle('on', j===i); });
  var cb = art.querySelector('.donut-c b'), cs = art.querySelector('.donut-c span');
  if(!cb || !cs) return;
  var meta = art._donut;
  if(i>=0 && meta && meta.metas[i]){
    cb.textContent = fmtTok(meta.metas[i].total);
    cs.textContent = meta.metas[i].name+' · '+(meta.T.total ? (meta.metas[i].total/meta.T.total*100).toFixed(1) : '0.0')+'%';
  }else if(i>=0 && rows[i]){
    var T = aggRows(rows);
    cb.textContent = fmtTok(rows[i].total);
    cs.textContent = rows[i].name+' · '+(T.total ? (rows[i].total/T.total*100).toFixed(1) : '0.0')+'%';
  }else{
    cb.textContent = art.dataset.defVal||''; cs.textContent = art.dataset.defLab||'';
  }
}
groupsEl.addEventListener('mouseover', function(e){
  var art = e.target.closest('.group'); if(!art || art.dataset.defVal) return;
  var rows = DIMROWS[art.dataset.gid]; if(!rows) return;
  var T = aggRows(rows);
  art.dataset.defVal = fmtTok(T.total);
  art.dataset.defLab = dimLabel(art.dataset.gid)+' · '+colLabel(colById('total'));
});

/* ---------- 窄屏维度切换：一次只显示一个维度 ---------- */
var narrowMQ = window.matchMedia ? window.matchMedia('(max-width:768px)') : null;
var narrowDim = 'client';
function applyNarrowDim(){
  var active = !!(narrowMQ && narrowMQ.matches);
  DIMS.forEach(function(id){
    var art = groupsEl.querySelector('.group[data-gid="'+id+'"]');
    if(!art) return;
    if(active){ art.hidden = (id!==narrowDim); }
    else if(art.hidden){ art.hidden = false; }
  });
  el('dim-seg').querySelectorAll('button').forEach(function(b){
    b.setAttribute('aria-pressed', b.dataset.v===narrowDim ? 'true':'false');
  });
}
el('dim-seg').addEventListener('click', function(e){
  var b = e.target.closest('button'); if(!b) return;
  narrowDim = b.dataset.v;
  applyNarrowDim();
});
if(narrowMQ){
  var onNarrowChange = function(){ applyNarrowDim(); };
  if(narrowMQ.addEventListener) narrowMQ.addEventListener('change', onNarrowChange);
  else if(narrowMQ.addListener) narrowMQ.addListener(onNarrowChange);
}
/* 窄屏记录卡：点按展开/收起全部字段（合计行除外） */
groupsEl.addEventListener('click', function(e){
  if(!narrowMQ || !narrowMQ.matches) return;
  var tr = e.target.closest('tr');
  if(!tr || !groupsEl.contains(tr) || tr.classList.contains('trow')) return;
  if(!tr.parentElement || tr.parentElement.tagName!=='TBODY') return;
  tr.classList.toggle('expanded');
  tr.setAttribute('aria-expanded',tr.classList.contains('expanded')?'true':'false');
});
groupsEl.addEventListener('keydown',function(e){
  if(!narrowMQ || !narrowMQ.matches || (e.key!=='Enter'&&e.key!==' ')) return;
  var tr=e.target.closest('.group-row'); if(!tr) return;
  e.preventDefault(); tr.classList.toggle('expanded'); tr.setAttribute('aria-expanded',tr.classList.contains('expanded')?'true':'false');
});

/* ---------- 构成图非鼠标交互：图例可聚焦、回车/空格高亮、触屏点按 ---------- */
groupsEl.addEventListener('focusin', function(e){
  var li = e.target.closest('.legend [data-i]'); if(!li) return;
  var art = li.closest('.group'); var rows = DIMROWS[art.dataset.gid]; if(!rows) return;
  highlightDonut(art, rows, +li.dataset.i);
});
groupsEl.addEventListener('focusout', function(e){
  var art = e.target.closest('.group'); if(!art) return;
  if(art.contains(e.relatedTarget)) return;
  var rows = DIMROWS[art.dataset.gid]; if(!rows) return;
  highlightDonut(art, rows, -1);
});
groupsEl.addEventListener('keydown', function(e){
  if(e.key!=='Enter' && e.key!==' ') return;
  var li = e.target.closest('.legend [data-i]'); if(!li) return;
  e.preventDefault();
  var art = li.closest('.group'); var rows = DIMROWS[art.dataset.gid]; if(!rows) return;
  highlightDonut(art, rows, li.classList.contains('on') ? -1 : +li.dataset.i);
});
groupsEl.addEventListener('click', function(e){
  var li = e.target.closest('.legend [data-i]'); if(!li) return;
  var art = li.closest('.group'); var rows = DIMROWS[art.dataset.gid]; if(!rows) return;
  highlightDonut(art, rows, li.classList.contains('on') ? -1 : +li.dataset.i);
});

/* ---------- 图表：趋势=每日/每周总量柱；热力=真实日期×小时数据 ---------- */
var svg = el('chart'), frame = el('chart-frame'), tip = el('tip');
var CUR = { t:[], labels:[], hourly:false, granularity:'day' };
var HEATVALS = [], HEATLAB = [], HEATSCOPE = '', HEATTIPS = [];
var WD_ZH = ['一','二','三','四','五','六','日'];
var WD_EN = ['Mon','Tue','Wed','Thu','Fri','Sat','Sun'];
/* “尚未发生”分界=今天(本机时钟):日期晚于今天的格子是尚未发生的未来,
   显示空白;今天及之前的零用量日期显示最低档色(data_through 只表达数据
   新鲜度,不作为空白分界——最后一条消息之后的已过去日期是真实零值)。 */
function isFutureDate(ds){
  var today = ymd(new Date());
  return ds > today;
}
function heatMap(){
  var m = {};
  (state.heat||[]).forEach(function(d){ m[d.date]=d; });
  return m;
}
function heatDay(ds){
  var days = state.heat||[];
  for(var i=0;i<days.length;i++){ if(days[i].date===ds) return days[i]; }
  return null;
}
/* 趋势序列：单日=24 小时柱；≤62 天=每日柱；更长=按周聚合柱。数据来自 heatmap.days */
function seriesFor(){
  var days = state.heat||[];
  if(!days.length) return { t:[], labels:[], hourly:false, granularity:'day' };
  var b = rangeBounds(), s = ymd(b.s), e = ymd(b.e);
  if(s===e){
    var day = heatDay(s);
    var hours = (day && day.hours) ? day.hours.map(function(v){ return +v||0; }) : [];
    return { t:hours, labels:hours.map(function(_,i){ return i+ui('时',':00'); }), hourly:true, granularity:'hour' };
  }
  var t = days.map(function(d){ return +d.total||0; });
  var labels = days.map(function(d){ return String(d.date).slice(5); });
  /* 年度或较长自定义区间按周聚合，避免数百根窄柱失去可读性 */
  if(t.length>62){
    var wt=[], wl=[];
    for(var j=0;j<t.length;j+=7){
      var sum=0;
      for(var k=j;k<Math.min(j+7,t.length);k++) sum+=t[k];
      wt.push(sum);
      wl.push(labels[Math.min(j+6, labels.length-1)]);
    }
    return { t:wt, labels:wl, hourly:false, granularity:'week' };
  }
  return { t:t, labels:labels, hourly:false, granularity:'day' };
}
function drawChart(){
  syncModeSeg();
  if(state.mode==='trend') drawTrend(); else drawHeat();
}
function syncModeSeg(){
  var hb = el('mode-heat-btn');
  if(!hb) return;
  hb.disabled = false;
  hb.title = ui('按当前区间查看用量热力','View a usage heatmap for the current range');
  el('mode-seg').querySelectorAll('button').forEach(function(x){
    x.setAttribute('aria-pressed', x.dataset.v===state.mode ? 'true':'false');
  });
}
function drawTrend(){
  frame.classList.remove('calendar-layout');
  var W = Math.max(320, frame.clientWidth-24), H = frame.clientHeight || 260;
  var s = seriesFor();
  CUR = { t:s.t, labels:s.labels, hourly:s.hourly, granularity:s.granularity };
  /* 空区间(全零序列,零请求即零用量):显示明确空状态,不渲染「峰值 0」
     的假图表;未来范围与无数据范围同语义。 */
  var empty = true;
  for(var zi=0; zi<s.t.length; zi++){ if(s.t[zi]>0){ empty=false; break; } }
  if(!s.t.length || empty){
    svg.setAttribute('viewBox','0 0 '+W+' '+H);
    svg.innerHTML = '<text class="ax ta-c" x="'+(W/2).toFixed(1)+'" y="'+(H/2).toFixed(1)+'">'+ui('该区间暂无用量','No usage in this range')+'</text>';
    el('chart-title-h').textContent = ui('用量趋势','Usage trend');
    el('chart-sub').textContent = rangeName(state.range)+' · '+rangeDateText();
    return;
  }
  var padL=46, padR=40, padT=12, padB=20, pw=W-padL-padR, ph=H-padT-padB;
  var tmax = niceMax(Math.max.apply(null, s.t));
  var X = function(i){ return padL+(i+0.5)*pw/s.t.length; };
  var Y = function(v){ return padT+ph-(v/tmax)*ph; };
  var out = '';
  for(var g=0; g<=4; g++){
    var gv = tmax*g/4, y = Y(gv);
    out += '<line class="gl" x1="'+padL+'" x2="'+(W-padR)+'" y1="'+y.toFixed(1)+'" y2="'+y.toFixed(1)+'"/>';
    out += '<text class="ax ta-e" x="'+(padL-6)+'" y="'+(y+3).toFixed(1)+'">'+axisFmt(gv)+'</text>';
  }
  var n = s.t.length, slot = pw/n, bw = Math.min(slot*0.6, 34);
  var peakI = 0;
  s.t.forEach(function(v,i){ if(v>s.t[peakI]) peakI = i; });
  s.t.forEach(function(v,i){
    var x = X(i)-bw/2, y = Y(v);
    out += '<rect class="bar-t" x="'+x.toFixed(1)+'" y="'+y.toFixed(1)+'" width="'+bw.toFixed(1)+'" height="'+Math.max(padT+ph-y,0).toFixed(1)+'" rx="2.5"/>';
  });
  out += '<text class="ax-strong ta-c" x="'+X(peakI).toFixed(1)+'" y="'+Math.max(Y(s.t[peakI])-7, 11).toFixed(1)+'">'+fmtTok(s.t[peakI])+'</text>';
  /* 刻度数按绘图区宽度与标签实际宽度动态抽稀:窄屏不拥挤,宽屏不过疏 */
  var labW = ((s.labels[0]||'').length||3)*6.4+16, minGap = Math.max(48, labW);
  var maxLabels = Math.max(3, Math.floor(pw/minGap));
  var step = Math.ceil(n/maxLabels), labelIdx = [];
  s.labels.forEach(function(_,i){ if(i%step===0) labelIdx.push(i); });
  if(labelIdx[labelIdx.length-1]!==n-1){
    if(labelIdx.length>1 && X(n-1)-X(labelIdx[labelIdx.length-1])<minGap) labelIdx.pop();
    labelIdx.push(n-1);
  }
  labelIdx.forEach(function(i){
    out += '<text class="ax ta-c" x="'+X(i).toFixed(1)+'" y="'+(H-5)+'">'+s.labels[i]+'</text>';
  });
  s.t.forEach(function(_,i){
    out += '<rect x="'+(padL+i*slot).toFixed(1)+'" y="'+padT+'" width="'+slot.toFixed(1)+'" height="'+ph+'" fill="transparent" data-i="'+i+'"/>';
  });
  svg.setAttribute('viewBox','0 0 '+W+' '+H);
  svg.setAttribute('aria-label', rangeName(state.range)+ui(' 用量柱状图',' tokens bar chart'));
  svg.innerHTML = out;
  decorateChartTargets();
  el('chart-title-h').textContent = ui('用量趋势','Usage trend');
  var grain = CUR.granularity==='hour' ? ui('按小时','Hourly') : (CUR.granularity==='week' ? ui('按周','Weekly') : ui('按日','Daily'));
  el('chart-sub').innerHTML = ui('峰值 ','Peak ')+s.labels[peakI]+' '+fmtTok(s.t[peakI])+' · '+rangeName(state.range)+' · '+rangeDateText()+' · '+grain+' · <i class="key-dot"></i>'+ui('用量','Tokens');
}
function drawHeat(){
  var b = rangeBounds(), days = Math.round((b.e-b.s)/86400000)+1;
  frame.classList.toggle('calendar-layout',days>31);
  var W = Math.max(320, frame.clientWidth-24), H = frame.clientHeight || 260;
  var map = heatMap();
  HEATVALS=[]; HEATLAB=[]; HEATTIPS=[]; HEATSCOPE=rangeDateText();
  var out='', sub2='', maxCell=1, hasBlank=false;

  if(days<=7){
    /* 1–7 天：每个日期一行、每小时一格，值为该小时真实用量 */
    var rows=[];
    for(var di=0;di<days;di++){
      var d=new Date(b.s); d.setDate(d.getDate()+di);
      var ds=ymd(d), wi=(d.getDay()+6)%7;
      var future=isFutureDate(ds), rec=map[ds];
      var vals=[];
      if(future || !rec){
        for(var z=0;z<24;z++) vals.push(null);
        if(future) hasBlank=true;
      }else{
        vals=(rec.hours||[]).map(function(v){ return +v||0; });
        while(vals.length<24) vals.push(0);
      }
      rows.push({label:LOCALE==='en'?WD_EN[wi]+' '+fmtMD(d):fmtMD(d)+' 周'+WD_ZH[wi],date:ds,vals:vals});
    }
    rows.forEach(function(r){r.vals.forEach(function(v){if(v!=null&&v>maxCell)maxCell=v;});});
    var padT=8,padB=18,padL=76,padR=8,gap=2;
    var cw=Math.min((W-padL-padR-23*gap)/24,42), ch=(H-padT-padB-(rows.length-1)*gap)/rows.length;
    var gridW=24*cw+23*gap,x0=padL+Math.max((W-padL-padR-gridW)/2,0);
    rows.forEach(function(r,ri){
      HEATVALS.push(r.vals); HEATLAB.push(r.label); HEATTIPS.push([]);
      var y=padT+ri*(ch+gap);
      out+='<text class="ax ta-e" x="'+(x0-8)+'" y="'+(y+ch/2+3).toFixed(1)+'">'+r.label+'</text>';
      r.vals.forEach(function(val,c){
        if(val===null){ out+='<rect class="hc-empty" x="'+(x0+c*(cw+gap)).toFixed(1)+'" y="'+y.toFixed(1)+'" width="'+cw.toFixed(1)+'" height="'+ch.toFixed(1)+'" rx="2"/>'; return; }
        HEATTIPS[ri][c]=r.date+' · '+pad2(c)+':00';
        var op=Math.round(7+93*Math.pow(Math.min(val/maxCell,1),.8));
        out+='<rect class="hc" data-r="'+ri+'" data-c="'+c+'" x="'+(x0+c*(cw+gap)).toFixed(1)+'" y="'+y.toFixed(1)+'" width="'+cw.toFixed(1)+'" height="'+ch.toFixed(1)+'" rx="2" style="fill:color-mix(in srgb,var(--accent) '+op+'%,transparent)"/>';
      });
    });
    [0,6,12,18].forEach(function(c){out+='<text class="ax ta-c" x="'+(x0+c*(cw+gap)+cw/2).toFixed(1)+'" y="'+(H-4)+'">'+c+ui('时',':00')+'</text>';});
    sub2=days===1?ui('当天 · 小时分布','Single day · hourly'):ui('日期 × 小时','Date × hour');
  }else if(days<=31){
    /* 8–31 天：横轴保留每一天，纵轴压缩成 4 小时时段；本月预设保留完整自然月 */
    var blocks=['00–04','04–08','08–12','12–16','16–20','20–24'];
    var vals=blocks.map(function(){return [];});
    var tips=blocks.map(function(){return [];});
    var dates=[], monthFuture=false, ds0=ymd(b.s), ds1=ymd(b.e), displayDs1=ds1;
    if(state.range===2){
      var monthEnd=new Date(b.e.getFullYear(),b.e.getMonth()+1,0);
      displayDs1=ymd(monthEnd); monthFuture=displayDs1>ds1;
    }
    var cur=new Date(dateOf(ds0));
    while(ymd(cur)<=displayDs1){
      var dstr=ymd(cur);
      dates.push(dstr);
      var rec2=map[dstr], active=!!rec2 && !isFutureDate(dstr) && dstr>=ds0 && dstr<=ds1;
      if(rec2 && isFutureDate(dstr)) hasBlank=true;
      if(monthFuture && dstr>ds1) hasBlank=true;
      for(var bi=0;bi<6;bi++){
        var v=null;
        if(active){
          v=0;
          for(var h=bi*4;h<bi*4+4;h++) v += +((rec2.hours||[])[h])||0;
        }
        vals[bi].push(v); tips[bi].push(active?dstr+' · '+blocks[bi]:'');
        if(v!==null&&v>maxCell)maxCell=v;
      }
      cur.setDate(cur.getDate()+1);
    }
    var pT=8,pB=20,pL=52,pR=8,g=2;
    var cW=Math.min((W-pL-pR-(dates.length-1)*g)/dates.length,64),cH=(H-pT-pB-5*g)/6;
    var gw=dates.length*cW+(dates.length-1)*g,xStart=pL+Math.max((W-pL-pR-gw)/2,0);
    vals.forEach(function(row,ri){
      HEATVALS.push(row); HEATLAB.push(blocks[ri]); HEATTIPS.push(tips[ri]);
      var y=pT+ri*(cH+g);
      out+='<text class="ax ta-e" x="'+(xStart-8)+'" y="'+(y+cH/2+3).toFixed(1)+'">'+blocks[ri]+'</text>';
      row.forEach(function(val,c){
        if(val===null){
          out+='<rect class="hc-empty" x="'+(xStart+c*(cW+g)).toFixed(1)+'" y="'+y.toFixed(1)+'" width="'+cW.toFixed(1)+'" height="'+cH.toFixed(1)+'" rx="2"/>';
          return;
        }
        var op=Math.round(7+93*Math.pow(Math.min(val/maxCell,1),.8));
        out+='<rect class="hc" data-r="'+ri+'" data-c="'+c+'" x="'+(xStart+c*(cW+g)).toFixed(1)+'" y="'+y.toFixed(1)+'" width="'+cW.toFixed(1)+'" height="'+cH.toFixed(1)+'" rx="2" style="fill:color-mix(in srgb,var(--accent) '+op+'%,transparent)"/>';
      });
    });
    var step=Math.max(1,Math.ceil(dates.length/6));
    dates.forEach(function(dsx,c){if(c%step===0||c===dates.length-1)out+='<text class="ax ta-c" x="'+(xStart+c*(cW+g)+cW/2).toFixed(1)+'" y="'+(H-4)+'">'+dsx.slice(5)+'</text>';});
    sub2=ui('日期 × 4 小时时段','Date × 4-hour block');
  }else{
    /* 32 天以上：GitHub 式日历热力图；本年保留完整自然年的周列 */
    var start=new Date(b.s), displayEnd=state.range===3?new Date(b.e.getFullYear(),11,31):new Date(b.e);
    var displayDays=Math.round((displayEnd-start)/86400000)+1, yearFuture=displayDays>days;
    var offset=(start.getDay()+6)%7, weeks=Math.ceil((offset+displayDays)/7);
    var pt=24,pb=8,pl=34,pr=8,gp=2;
    var cellW=Math.min((W-pl-pr-(weeks-1)*gp)/weeks,34),cellH=Math.min((H-pt-pb-6*gp)/7,24);
    var calW=weeks*cellW+(weeks-1)*gp,calH=7*cellH+6*gp;
    var sx=pl+Math.max((W-pl-pr-calW)/2,0),sy=pt+Math.max((H-pt-pb-calH)/2,0);
    var rI,cI;
    HEATVALS=[]; for(rI=0;rI<7;rI++){ HEATVALS.push([]); for(cI=0;cI<weeks;cI++) HEATVALS[rI].push(null); }
    HEATLAB=(LOCALE==='en'?WD_EN:WD_ZH.map(function(w){return '周'+w;}));
    HEATTIPS=[]; for(rI=0;rI<7;rI++){ HEATTIPS.push([]); for(cI=0;cI<weeks;cI++) HEATTIPS[rI].push(''); }
    var cells=[],months={},dailyMax=1;
    for(var n2=0;n2<displayDays;n2++){
      var cd=new Date(start); cd.setDate(cd.getDate()+n2);
      var cds=ymd(cd),pos=offset+n2,row=pos%7,col=Math.floor(pos/7);
      var rec3=map[cds], active2=!!rec3 && !isFutureDate(cds);
      var val=active2?(+rec3.total||0):null;
      if(rec3 && isFutureDate(cds)) hasBlank=true;
      if(yearFuture && n2>=days) hasBlank=true;
      HEATVALS[row][col]=val; HEATTIPS[row][col]=active2?cds:'';
      if(val!==null&&val>dailyMax)dailyMax=val;
      cells.push({row:row,col:col,val:val,date:cd});
      var mk=cd.getFullYear()+'-'+cd.getMonth(); if(months[mk]===undefined)months[mk]=col;
    }
    HEATLAB.forEach(function(label,r){
      out+='<text class="ax ta-e" x="'+(sx-8)+'" y="'+(sy+r*(cellH+gp)+cellH/2+3).toFixed(1)+'">'+label+'</text>';
    });
    cells.forEach(function(c){
      if(c.val===null){
        out+='<rect class="hc-empty" x="'+(sx+c.col*(cellW+gp)).toFixed(1)+'" y="'+(sy+c.row*(cellH+gp)).toFixed(1)+'" width="'+cellW.toFixed(1)+'" height="'+cellH.toFixed(1)+'" rx="2"/>';
        return;
      }
      var op=Math.round(7+93*Math.pow(Math.min(c.val/dailyMax,1),.8));
      out+='<rect class="hc" data-r="'+c.row+'" data-c="'+c.col+'" x="'+(sx+c.col*(cellW+gp)).toFixed(1)+'" y="'+(sy+c.row*(cellH+gp)).toFixed(1)+'" width="'+cellW.toFixed(1)+'" height="'+cellH.toFixed(1)+'" rx="2" style="fill:color-mix(in srgb,var(--accent) '+op+'%,transparent)"/>';
    });
    Object.keys(months).forEach(function(key){
      var mp=key.split('-'),label=LOCALE==='en'?new Date(+mp[0],+mp[1],1).toLocaleDateString('en-US',{month:'short'}):(+mp[1]+1)+'月';
      out+='<text class="ax" x="'+(sx+months[key]*(cellW+gp)).toFixed(1)+'" y="14">'+label+'</text>';
    });
    maxCell=dailyMax; sub2=ui('日历热力图 · 每格 1 天','Calendar heatmap · 1 day per cell');
  }
  svg.setAttribute('viewBox','0 0 '+W+' '+H);
  svg.setAttribute('aria-label',ui('用量热力图：','Usage heatmap: ')+rangeDateText()+' · '+sub2);
  svg.innerHTML=out;
  decorateChartTargets();
  el('chart-title-h').textContent=ui('用量热力图','Usage heatmap');
  if(hasBlank) sub2+=' · '+ui('空白为尚未发生','Blank = not yet');
  el('chart-sub').innerHTML=rangeName(state.range)+' · '+rangeDateText()+' · '+sub2+' · <span class="heat-scale">'+ui('低','Low')+'<i></i>'+ui('高','High')+'</span>';
}
function chartTargetLabel(tg){
  if(!tg || !tg.dataset) return '';
  if(tg.dataset.i!==undefined){
    var i=+tg.dataset.i;
    return CUR.labels[i]+ui('，用量 ',', tokens ')+fmtTok(CUR.t[i]);
  }
  if(tg.dataset.r!==undefined){
    var rr=+tg.dataset.r,cc=+tg.dataset.c;
    var v=(HEATVALS[rr]&&HEATVALS[rr][cc]);
    return ((HEATTIPS[rr]&&HEATTIPS[rr][cc])||HEATLAB[rr]||'')+ui('，用量 ',', tokens ')+fmtTok(v);
  }
  return '';
}
function decorateChartTargets(){
  svg.querySelectorAll('[data-i],[data-r]').forEach(function(tg){
    tg.setAttribute('tabindex','0'); tg.setAttribute('role','img'); tg.setAttribute('aria-label',chartTargetLabel(tg));
  });
}
function showChartTarget(tg){
  if(!tg || !tg.dataset) return;
  var f = frame.getBoundingClientRect(), r = tg.getBoundingClientRect();
  var x = r.left-f.left+r.width/2, y = r.top-f.top;
  if(tg.dataset.i!==undefined){
    var i = +tg.dataset.i;
    showTip('<b>'+CUR.labels[i]+'</b><span>'+fmtTok(CUR.t[i])+ui(' tokens',' tokens')+'</span>', x, y);
  }else if(tg.dataset.r!==undefined){
    var rr = +tg.dataset.r, cc = +tg.dataset.c;
    showTip('<b>'+((HEATTIPS[rr]&&HEATTIPS[rr][cc])||HEATLAB[rr]||'')+'</b><span>'+HEATSCOPE+' · '+fmtTok(HEATVALS[rr]&&HEATVALS[rr][cc])+'</span>', x, y);
  }
}
svg.addEventListener('mouseover', function(e){
  showChartTarget(e.target);
});
svg.addEventListener('focusin',function(e){ showChartTarget(e.target); });
svg.addEventListener('focusout',function(){ tip.hidden=true; });
svg.addEventListener('mouseleave', function(){ tip.hidden = true; });
function showTip(html, x, y){
  tip.innerHTML = html;
  tip.hidden = false;
  var tw = tip.offsetWidth, fw = frame.getBoundingClientRect().width;
  tip.style.left = Math.min(Math.max(x-tw/2, 6), Math.max(fw-tw-6, 6))+'px';
  tip.style.top = Math.max(y-tip.offsetHeight-10, 4)+'px';
}
var rzT = null;
window.addEventListener('resize', function(){
  clearTimeout(rzT);
  rzT = setTimeout(function(){
    if(el('page-dash').getAttribute('data-active')==='true' && state.data) drawChart();
  }, 120);
});

/* ---------- 数据获取：/api/meta 与 /api/dashboard ---------- */
function api(url, opts){
  return fetch(url, opts).then(function(res){
    return res.json().catch(function(){ return null; }).then(function(body){
      return { ok:res.ok, status:res.status, body:body };
    });
  });
}
function renderMeta(){
  var m = state.meta||{};
  var v = m.version||'';
  var badge = el('version-badge');
  if(badge){
    if(v){
      badge.textContent = v;
      badge.title = v;
      badge.setAttribute('aria-label', ui('版本 ','Version ')+v);
      badge.hidden = false;
    }else{
      badge.hidden = true;
    }
  }
  /* 在线状态只来自「本页已成功取得 /api/meta」这一事实;请求失败一律
     显示离线态,不保留任何写死的在线字样。 */
  var online = !!state.meta;
  var last = (state.meta && m.last_collection)||'';
  var dot = document.querySelector('.asof .dot');
  var asof = document.querySelector('.asof');
  if(asof) asof.classList.toggle('offline', !online);
  if(dot && dot!==asof) dot.classList.toggle('offline', !online);
  var ft, st;
  if(!online){
    ft = ui('离线 · 无法连接本地服务','Offline · cannot reach the local service');
    st = ui('离线','Offline');
  }else if(last){
    ft = ui('在线 · 最近同步 ','Online · Last synced ')+last;
    st = ui('在线 · ','Online · ')+(last.length>=16 ? last.slice(5,16) : last);
  }else{
    ft = ui('在线','Online');
    st = ui('在线','Online');
  }
  var full = el('asof-t'), short = document.querySelector('.asof-short');
  if(full) full.textContent = ft;
  if(short) short.textContent = st;
}
function fetchMeta(){
  return api('/api/meta').then(function(r){
    state.meta = (r.ok && r.body) ? r.body : null;
    renderMeta();
  }, function(){
    state.meta = null;
    renderMeta();
  });
}
function setDashLoading(){
  if(state.data) return; /* 已有内容：区间切换保持旧画面直到新数据到达，不闪烁 */
  el('kpis').innerHTML = '<p class="page-note">'+ui('加载中','Loading')+'</p>';
  el('groups').innerHTML = '<p class="page-note">'+ui('加载中','Loading')+'</p>';
  el('cviews').innerHTML = '';
  el('sessions-body').innerHTML = '';
  el('sessions-sub').textContent = rangeName(state.range)+' · '+rangeDateText();
  var b=rangeBounds();
  svg.setAttribute('viewBox','0 0 600 200');
  svg.innerHTML='<text class="ax ta-c" x="300" y="100">'+ui('加载中','Loading')+'</text>';
  el('chart-title-h').textContent = ui('用量趋势','Usage trend');
  el('chart-sub').textContent = ui('加载中','Loading')+' · '+ymd(b.s)+' ~ '+ymd(b.e);
}
function showDashError(msg){
  /* 失败态覆盖整块仪表板:清掉旧区间数据,避免「新区间标签+旧区间统计」
     的误读;首次失败也不停留在「加载中」。恢复只需重新切换区间或刷新。 */
  state.data = null;
  state.heat = [];
  var note = '<p class="page-note">'+esc(msg)+'</p>';
  el('kpis').innerHTML = note;
  el('groups').innerHTML = note;
  el('cviews').innerHTML = note;
  el('sessions-body').innerHTML = '';
  el('sessions-sub').textContent = msg;
  el('chart-sub').textContent = msg;
  svg.setAttribute('viewBox','0 0 600 200');
  svg.innerHTML = '<text class="ax ta-c" x="300" y="100">'+esc(msg)+'</text>';
  toast(msg);
}
function loadDashboard(){
  var b = rangeBounds(), from = ymd(b.s), to = ymd(b.e);
  var seq = ++fetchSeq;
  setDashLoading();
  api('/api/dashboard?from='+from+'&to='+to).then(function(r){
    if(seq!==fetchSeq) return;
    if(!r.ok){
      showDashError((r.body && r.body.error && r.body.error.message) || ui('数据加载失败','Failed to load data'));
      return;
    }
    state.data = r.body||{};
    state.heat = (state.data.heatmap && state.data.heatmap.days) || [];
    renderAll();
  }, function(){
    if(seq!==fetchSeq) return;
    showDashError(ui('数据加载失败','Failed to load data'));
  });
}

/* ---------- 用量最高的会话：真实区间会话，直接渲染 ---------- */
function renderSessions(){
  var body = el('sessions-body');
  if(!body) return;
  var rows = (state.data && state.data.sessions) || [];
  if(!rows.length){
    body.innerHTML = '<tr class="session-row"><td colspan="6" class="tl">'+ui('该区间暂无会话','No sessions in this range')+'</td></tr>';
  }else{
    body.innerHTML = rows.map(function(r){
      var full=esc(r.title||'');
      return '<tr class="session-row"><th scope="row" class="tname"><div class="session-title-wrap"><span class="session-title-text" title="'+full+'">'+full+'</span>'+
        '<button type="button" class="session-title-detail" data-full="'+full+'" aria-label="'+ui('查看完整会话标题','View full session title')+'" title="'+ui('查看完整会话标题','View full session title')+'"><svg class="ic"><use href="#i-info"/></svg></button></div></th>' +
        '<td class="num tl" data-label="'+dimLabel('client')+'">'+esc(r.client||'')+'</td><td class="num tl" data-label="'+dimLabel('project')+'">'+esc(r.project||'')+'</td>' +
        '<td class="num" data-label="'+ui('时长','Duration')+'">'+fmtDur(r.duration_ms)+'</td><td class="num" data-label="'+colLabel(colById('requests'))+'">'+fmtInt(r.requests)+'</td>' +
        '<td class="num tot" data-label="'+colLabel(colById('total'))+'"><span class="nv">'+fmtTok(r.total)+'</span></td></tr>';
    }).join('');
  }
  el('sessions-sub').textContent = rangeName(state.range)+' · '+rangeDateText();
}
var sessionTitleModal=el('session-title-modal'), sessionTitleOpener=null;
function openSessionTitle(button){
  sessionTitleOpener=button;
  el('session-title-modal-text').textContent=button.getAttribute('data-full')||'';
  sessionTitleModal.hidden=false;
  el('session-title-modal-close').focus();
}
function closeSessionTitle(){
  sessionTitleModal.hidden=true;
  if(sessionTitleOpener && document.contains(sessionTitleOpener)) sessionTitleOpener.focus();
  sessionTitleOpener=null;
}
el('sessions-body').addEventListener('click',function(e){
  var button=e.target.closest('.session-title-detail');
  if(button) openSessionTitle(button);
});
el('session-title-modal-close').addEventListener('click',closeSessionTitle);
sessionTitleModal.addEventListener('click',function(e){
  if(e.target.classList.contains('modal-scrim')) closeSessionTitle();
});
sessionTitleModal.addEventListener('keydown',function(e){
  if(sessionTitleModal.hidden) return;
  if(e.key==='Escape'){ e.preventDefault(); closeSessionTitle(); return; }
  if(e.key==='Tab'){ e.preventDefault(); el('session-title-modal-close').focus(); }
});

/* ---------- 自定义视图：/api/dashboard.custom_views，Top N + 唯一“其他组合”守恒行 ---------- */
function cellCv(k, r, extra){
  if(k==='cache_hit') return '<span class="nv">'+pctText(r.hit,2)+'</span>';
  if(k==='total') return '<span class="nv">'+fmtTok(rowVal(r,k))+'</span><i class="mb mb-a" style="width:'+extra+'%"></i>';
  if(k==='requests') return fmtInt(rowVal(r,k));
  return '<span class="nv">'+fmtTok(rowVal(r,k))+'</span>';
}
function cellAggCv(k, T){
  if(k==='cache_hit') return pctText(T.hit,2);
  if(k==='requests') return fmtInt(T.requests);
  return fmtTok(rowVal(T,k));
}
var CV_TOP_N = 8;
function renderCustomViews(){
  var host = el('cviews');
  if(!host) return;
  if(!state.data){ host.innerHTML=''; return; }
  var views = state.data.custom_views||[];
  if(!views.length){
    host.innerHTML = '<section class="sessions-card"><div class="sessions-head"><div><h3>'+ui('自定义视图','Custom views')+'</h3>' +
      '<p>'+ui('尚未配置自定义视图','No custom views configured')+'</p></div></div></section>';
    return;
  }
  var ids = colIds();
  host.innerHTML = views.map(function(v){
    var dims = v.dimensions||[];
    var rows = (v.rows||[]).map(function(r){
      var o = {
        keys:(r.keys||[]).slice(),
        requests:+r.requests||0, fresh_input:+r.fresh_input||0, output:+r.output||0,
        cache_read:+r.cache_read||0, cache_create:+r.cache_create||0, reasoning:+r.reasoning||0,
        total:+r.total||0, hit:null, isOther:false
      };
      o.hit = hitOf(o.fresh_input, o.cache_read, o.cache_create);
      return o;
    }).sort(function(a,b){ return b.total-a.total; });
    /* 尾部收拢:从最小项向内并入,直到并入会使「其他组合」总量不再严格
       小于紧邻的独立项为止(交接合同:其他必须小于每个独立项,不满足时
       多展示或全部展示,不生成会排到中间的假其他行);CV_TOP_N 为展示上限。 */
    var shownCount = Math.min(rows.length, CV_TOP_N), hidden = [];
    while(shownCount < rows.length){
      var cand = rows[shownCount];
      var prevShownTotal = rows[shownCount-1] ? rows[shownCount-1].total : Infinity;
      var futureSum = cand.total;
      for(var hi2=shownCount+1; hi2<rows.length; hi2++) futureSum += rows[hi2].total;
      if(futureSum < prevShownTotal) break; /* 已满足:剩余全部并入 */
      shownCount++; /* 并入会越位:该行独立展示 */
    }
    var shown = rows.slice(0, shownCount), hidden = rows.slice(shownCount);
    if(hidden.length){
      /* 其他组合行 = 全部行求和 − 已展示行求和（逐项守恒，防负） */
      var sumShown = aggRows(shown), sumAll = aggRows(rows);
      var other = {
        keys: dims.map(function(d,i){ return i===0 ? ui('其他组合（'+hidden.length+' 项）','Other combos ('+hidden.length+' items)') : '—'; }),
        requests:Math.max(0,sumAll.requests-sumShown.requests),
        fresh_input:Math.max(0,sumAll.fresh_input-sumShown.fresh_input),
        output:Math.max(0,sumAll.output-sumShown.output),
        cache_read:Math.max(0,sumAll.cache_read-sumShown.cache_read),
        cache_create:Math.max(0,sumAll.cache_create-sumShown.cache_create),
        reasoning:Math.max(0,sumAll.reasoning-sumShown.reasoning),
        total:Math.max(0,sumAll.total-sumShown.total),
        hit:null, isOther:true, hiddenCount:hidden.length
      };
      other.hit = hitOf(other.fresh_input, other.cache_read, other.cache_create);
      shown.push(other);
    }
    var T = aggRows(shown), maxTotal = shown.length && shown[0].total>0 ? shown[0].total : 1;
    var dimHeads = dims.map(function(d){ return '<th scope="col">'+dimLabel(d)+'</th>'; }).join('');
    var colHeads = ids.map(function(k){
      var c = colById(k);
      return '<th scope="col" title="'+colLabel(c)+'">'+colShort(c)+'</th>';
    }).join('');
    var viewTitle = dims.map(dimLabel).join(' × ');
    var h = '<section class="sessions-card" aria-label="'+esc(viewTitle)+'">' +
      '<div class="sessions-head"><div><h3>'+esc(viewTitle)+'</h3></div><span class="toml-tag">'+esc(v.name||'')+'</span></div>' +
      '<div class="table-wrap"><table class="cvx"><thead><tr>' + dimHeads + colHeads +
      '</tr></thead><tbody>';
    shown.forEach(function(r){
      h += '<tr class="cv-row" tabindex="0" aria-expanded="false">' + dims.map(function(d,i){
        var val = String((r.keys||[])[i]!=null ? r.keys[i] : '');
        var safe=esc(val);
        var disp = val.length>26 ? val.slice(0,12)+'…'+val.slice(-8) : val;
        return '<th scope="row" class="tname" title="'+safe+'" data-label="'+dimLabel(d)+'">'+esc(disp)+'</th>';
      }).join('') +
      ids.map(function(k){
        return '<td class="num'+(k==='total'?' tot':'')+'" data-k="'+k+'" data-label="'+colLabel(colById(k))+'">'+cellCv(k, r, (r.total/maxTotal*100).toFixed(1))+'</td>';
      }).join('') + '</tr>';
    });
    h += '<tr class="trow"><th scope="row" class="tname" colspan="'+dims.length+'">'+ui('总计','Total')+'</th>' +
      ids.map(function(k){ return '<td class="num'+(k==='total'?' tot':'')+'" data-k="'+k+'" data-label="'+colLabel(colById(k))+'">'+cellAggCv(k, T)+'</td>'; }).join('') +
      '</tr></tbody></table></div></section>';
    return h;
  }).join('');
}
function toggleCustomRow(row){
  var on = !row.classList.contains('expanded');
  row.classList.toggle('expanded', on);
  row.setAttribute('aria-expanded', on?'true':'false');
}
el('cviews').addEventListener('click', function(e){
  var row = e.target.closest('.cv-row');
  if(row && matchMedia('(max-width:768px)').matches) toggleCustomRow(row);
});
el('cviews').addEventListener('keydown', function(e){
  var row = e.target.closest('.cv-row');
  if(row && matchMedia('(max-width:768px)').matches && (e.key==='Enter'||e.key===' ')){
    e.preventDefault(); toggleCustomRow(row);
  }
});

/* ---------- 仪表板整体渲染 ---------- */
function renderAll(){
  syncRangeSeg();
  syncModeSeg();
  renderKPIs();
  renderGroups();
  drawChart();
  renderCustomViews();
  renderSessions();
}

/* ---------- 配置页：草稿对象为唯一真相源，GET/PUT 真实接线 ---------- */
var CFG = {
  loaded:false, loading:false, saving:false,
  saved:null,        /* 最近一次已保存（或初始 GET）的草稿深拷贝 */
  savedRevision:'',
  draft:null,        /* 当前编辑中的草稿深拷贝 */
  diag:[],           /* GET 下发的 query 解析诊断（不回传 PUT） */
  /* 恢复全部默认值时置位:即使用户没有手工编辑 query(问题态下 GET 是
     回退草稿,queryTouched 为 false),保存也必须携带 query 覆盖磁盘问题态,
     否则「全部默认值」无法修复坏掉的查询配置。 */
  forceQueryRewrite:false
};
var Q_RESERVED = ['client','model','provider','project','session','summary','day','month','hour','weekday','heatmap','custom','list'];
var Q_DIMS = ['client','model','provider','project','day','month','hour','weekday'];
/* query.default 的合法内置值域=全部 8 个内置维度(querydef.parseDefault 同域):
   只列 4 个业务维度会把 CLI 配的 day/month 等静默改写为 client。 */
var BUILTIN_VIEWS = ['client','model','provider','project','day','month','hour','weekday'];
var ROUTER_CAPABLE = ['claude','codex'];
function sanitizeCols(list){
  var out=[], seen={};
  (Array.isArray(list)?list:[]).forEach(function(k){ if(colById(k) && !seen[k]){ seen[k]=1; out.push(k); } });
  return out.length ? out : DEFAULT_OUT.slice();
}
function normalizeDraft(d){
  d = d||{};
  var q = d.query||{};
  function str(v){ return v==null ? '' : String(v); }
  function num(v, def){ var n=+v; return isNaN(n) ? (def||0) : n; }
  return {
    daemon:{ poll_interval:num(d.daemon&&d.daemon.poll_interval,0), autostart:!!(d.daemon&&d.daemon.autostart) },
    log:{ level:str(d.log&&d.log.level), dir:str(d.log&&d.log.dir), max_days:num(d.log&&d.log.max_days,0) },
    clients:(d.clients||[]).map(function(c){ return { name:str(c.name), enabled:!!c.enabled, router:str(c.router), paths:clone(c.paths)||{} }; }),
    routers:(d.routers||[]).map(function(r){ return { name:str(r.name), db_path:str(r.db_path) }; }),
    provider_aliases:(d.provider_aliases||[]).map(function(a){ return { key:str(a.key), value:str(a.value) }; }),
    query:{
      default:str(q.default)||'client',
      subqueries:clone(q.subqueries)||{},
      groups:clone(q.groups)||{},
      output_columns:sanitizeCols(q.output_columns)
    }
  };
}
/* 脏状态：两个草稿展平为标量映射后比对——clients/routers 按名对齐，aliases 保序 */
function mapEntries(mp){
  return Object.keys(mp||{}).sort().map(function(k){ return k+'='+mp[k]; }).join('\u0001');
}
function flattenDraft(d){
  var m={};
  m['daemon.poll_interval']=String(+d.daemon.poll_interval||0);
  m['daemon.autostart']=!!d.daemon.autostart;
  m['log.level']=String(d.log.level||'');
  m['log.dir']=String(d.log.dir||'');
  m['log.max_days']=String(+d.log.max_days||0);
  var cs=(d.clients||[]).slice().sort(function(a,b){ return a.name<b.name?-1:a.name>b.name?1:0; });
  cs.forEach(function(c){
    var pk=Object.keys(c.paths||{}).sort().map(function(k){ return k+'='+c.paths[k]; }).join('\u0001');
    m['client.'+c.name+'.enabled']=!!c.enabled;
    m['client.'+c.name+'.router']=String(c.router||'');
    m['client.'+c.name+'.paths']=pk;
  });
  var rs=(d.routers||[]).slice().sort(function(a,b){ return a.name<b.name?-1:a.name>b.name?1:0; });
  rs.forEach(function(r){ m['router.'+r.name+'.db_path']=String(r.db_path||''); });
  m['aliases']=(d.provider_aliases||[]).map(function(a){ return String(a.key)+'\u0001'+String(a.value); }).join('|');
  m['query.default']=String(d.query.default||'');
  m['query.subqueries']=mapEntries(d.query.subqueries);
  m['query.groups']=mapEntries(d.query.groups);
  m['query.output_columns']=(d.query.output_columns||[]).join(',');
  return m;
}
function diffDraft(a, b){
  var fa=flattenDraft(a), fb=flattenDraft(b), n=0, k;
  for(k in fa){ if(fa[k]!==fb[k]) n++; }
  for(k in fb){ if(!(k in fa)) n++; }
  return n;
}
function recomputeDirty(){
  if(!CFG.saved) return;
  var n = diffDraft(CFG.saved, CFG.draft);
  if(n===0 && CFG.forceQueryRewrite) n=1; /* 待重写的 query 段计入修改 */
  var b = el('cfg-save');
  el('dirty-pill').textContent = n>0 ? ui(n+' 项修改', n+' change'+(n===1?'':'s')) : ui('已保存','Saved');
  el('dirty-pill').setAttribute('data-show', n>0 ? 'true' : 'false');
  b.disabled = n===0 || CFG.saving;
  b.textContent = CFG.saving ? ui('保存中…','Saving…') : (n>0 ? ui('保存','Save') : ui('已保存','Saved'));
  b.classList.toggle('primary', n>0);
  el('cfg-actions').setAttribute('data-dirty', n>0?'true':'false');
}
function markDirty(){
  recomputeDirty();
  /* 只有值实际变化时才提示:改回原值立即恢复「已保存」,不再叠加修改提示 */
  if(el('cfg-save').disabled) return;
  toast(ui('已修改（未保存）','Modified (unsaved)'));
}
/* 拉取配置并填充 CFG 状态(不渲染配置页控件)。返回 Promise:
   成功 resolve(true);配置接口未启用(501)或网络失败 resolve(false)——
   仪表板明细列回退默认七列,不视为错误。 */
function fetchConfigState(){
  if(CFG.loaded) return Promise.resolve(true);
  if(CFG.loading) return CFG.pending || Promise.resolve(false);
  CFG.loading = true;
  CFG.pending = api('/api/config').then(function(r){
    CFG.loading = false; CFG.pending = null;
    if(!r.ok){
      /* 501=服务未装配配置能力;400/500 才值得提示(且仅在用户主动进入配置页时) */
      return false;
    }
    CFG.savedRevision = (r.body && r.body.revision) || '';
    CFG.draft = normalizeDraft(r.body && r.body.config);
    CFG.diag = (r.body && r.body.config && r.body.config.query && r.body.config.query.diagnostics) || [];
    CFG.saved = clone(CFG.draft);
    CFG.querySnapshot = JSON.stringify(serializeQueryDraft());
    CFG.forceQueryRewrite = false;
    CFG.loaded = true;
    /* 预载即渲染配置页控件:nav 切换的 !CFG.loaded 短路不会再触发 loadConfig,
       不在这里渲染的话,预载成功(生产常态)时进入配置页会是空表单 */
    renderAllConfig();
    recomputeDirty();
    return true;
  }, function(){
    CFG.loading = false; CFG.pending = null;
    return false;
  });
  return CFG.pending;
}
function loadConfig(){
  fetchConfigState().then(function(ok){
    if(!ok){
      toast(ui('读取配置失败','Failed to load config'));
      return;
    }
    renderAllConfig();
    recomputeDirty();
    /* 输出列可能随配置变化：刷新仪表板明细表 */
    if(state.data){
      if(state.view==='list') renderGroups();
      renderCustomViews();
    }
  });
}
function renderAllConfig(){
  if(!CFG.draft) return;
  renderDaemonCfg();
  renderLogCfg();
  renderClients();
  renderDefaultView();
  buildColsEditor();
  renderQDefs();
  renderAliases(false);
  renderDiag();
}
function renderDiag(){
  var title = el('config-query-title');
  if(!title || !title.closest) return;
  var host = title.closest('.config-section');
  if(!host) return;
  var old = host.querySelector('.query-diag');
  if(old) host.removeChild(old);
  if(!CFG.diag.length) return;
  var box = document.createElement('div');
  box.className = 'config-card query-diag';
  box.style.borderColor = 'color-mix(in srgb, #e0a43a 60%, var(--line))';
  var p = document.createElement('p');
  p.className = 'page-note';
  p.title = CFG.diag.join('\n');
  p.textContent = ui('查询配置存在解析问题，页面按默认值运行；悬停查看诊断','Query config has parse issues; defaults are in effect. Hover for details');
  box.appendChild(p);
  host.insertBefore(box, host.firstChild);
}

/* 守护进程 / 日志卡片 */
function renderDaemonCfg(){
  el('cfg-poll-interval').value = String(CFG.draft.daemon.poll_interval);
  el('cfg-autostart').setAttribute('aria-checked', CFG.draft.daemon.autostart?'true':'false');
}
function renderLogCfg(){
  var level = CFG.draft.log.level||'default';
  el('cfg-log-level').querySelectorAll('.chip').forEach(function(c){
    c.setAttribute('aria-pressed', c.textContent.trim()===level ? 'true':'false');
  });
  el('cfg-max-days').value = String(CFG.draft.log.max_days);
  el('cfg-log-dir').value = CFG.draft.log.dir;
}

/* 客户端表：四列（客户端/启用/数据来源/路由中间件） */
function clientPathCell(c){
  var keys = Object.keys(c.paths||{});
  if(!keys.length) return { html:'<span class="ct-na">—</span>', title:'' };
  var full = keys.map(function(k){ return k+' = '+c.paths[k]; }).join(' / ');
  var lines = keys.map(function(k){ return esc(String(c.paths[k])); });
  var html = lines.slice(0,2).join('<br>') + (lines.length>2 ? '<br>…' : '');
  return { html:'<span class="ct-path-value">'+html+'</span>', title:full };
}
function renderClients(){
  el('client-table').innerHTML =
    '<thead><tr><th scope="col">'+ui('客户端','Client')+'</th><th scope="col">'+ui('启用','Enabled')+'</th><th scope="col">'+ui('数据来源','Data source')+'</th><th scope="col">'+ui('路由中间件','Routing middleware')+'</th></tr></thead><tbody>' +
    CFG.draft.clients.map(function(c){
      var router;
      if(ROUTER_CAPABLE.indexOf(c.name)<0){
        router = '<span class="ct-na" title="'+ui('该客户端不支持路由归因','This client does not support routing attribution')+'">-</span>';
      }else if(c.router){
        router = '<span class="rt-cur">'+esc(c.router)+'</span><button type="button" class="rt-edit" data-edit="'+esc(c.name)+'" aria-label="'+esc(ui('修改 ','Change ')+c.name+ui(' 的路由中间件',' routing middleware'))+'">'+ui('修改','Change')+'</button>';
      }else{
        router = '<span class="rt-cur rt-off">'+ui('未对接','None')+'</span><button type="button" class="rt-edit" data-edit="'+esc(c.name)+'" aria-label="'+esc(ui('修改 ','Change ')+c.name+ui(' 的路由中间件',' routing middleware'))+'">'+ui('修改','Change')+'</button>';
      }
      var pn = clientPathCell(c);
      return '<tr><th scope="row" class="ct-name"><b>'+esc(c.name)+'</b></th>' +
        '<td class="ct-enabled" data-label="'+ui('启用','Enabled')+'"><button type="button" class="toggle" role="switch" aria-checked="'+(c.enabled?'true':'false')+'" aria-label="'+esc(ui('启用 ','Enable ')+c.name)+'" data-enable="'+esc(c.name)+'"></button></td>' +
        '<td class="ct-path" data-label="'+ui('数据来源','Data source')+'"'+(pn.title?' title="'+esc(pn.title)+'"':'')+'>'+pn.html+'</td>' +
        '<td class="ct-router-cell" data-label="'+ui('路由中间件','Routing middleware')+'"><span class="ct-router">'+router+'</span></td></tr>';
    }).join('') + '</tbody>';
}

/* 路由选择弹窗：单选（未对接 + 已配置路由）；应用写入草稿 */
var rtModal = el('rt-modal'), rtEditing = null, rtChoice = '', rtOpener = null;
function openRouterModal(name, opener){
  var c = null;
  CFG.draft.clients.forEach(function(x){ if(x.name===name) c=x; });
  if(!c || ROUTER_CAPABLE.indexOf(name)<0) return;
  el('toast').setAttribute('data-show','false');
  rtEditing = name; rtChoice = c.router; rtOpener = opener || null;
  el('rt-modal-title').textContent = ui('修改 '+name+' 的路由中间件','Change routing middleware for '+name);
  var opts = [{ v:'', label:ui('未对接','None'), note:ui('不绑定任何路由中间件','Do not bind routing middleware') }].concat(
    CFG.draft.routers.map(function(r){ return { v:r.name, label:r.name, note:r.db_path }; })
  );
  el('rt-opts').innerHTML = opts.map(function(o){
    var on = rtChoice===o.v;
    return '<button type="button" class="rt-opt" role="radio" aria-checked="'+on+'" data-v="'+esc(o.v)+'"><b>'+esc(o.label)+'</b><small>'+esc(o.note||'')+'</small></button>';
  }).join('');
  rtModal.hidden = false;
  var cur = el('rt-opts').querySelector('[aria-checked="true"]') || el('rt-opts').querySelector('.rt-opt');
  if(cur) cur.focus();
}
function closeRouterModal(){
  rtModal.hidden = true;
  if(rtOpener){ rtOpener.focus(); rtOpener = null; }
}
el('rt-opts').addEventListener('click', function(e){
  var o = e.target.closest('.rt-opt'); if(!o) return;
  rtChoice = o.dataset.v;
  el('rt-opts').querySelectorAll('.rt-opt').forEach(function(x){
    x.setAttribute('aria-checked', x===o ? 'true' : 'false');
  });
});
rtModal.addEventListener('keydown', function(e){
  if(rtModal.hidden) return;
  var opt=e.target.closest('.rt-opt');
  if(opt && /^(ArrowDown|ArrowRight|ArrowUp|ArrowLeft)$/.test(e.key)){
    e.preventDefault();
    var opts=[].slice.call(el('rt-opts').querySelectorAll('.rt-opt'));
    var i=opts.indexOf(opt), step=(e.key==='ArrowDown'||e.key==='ArrowRight')?1:-1;
    var next=opts[(i+step+opts.length)%opts.length];
    rtChoice=next.dataset.v;
    opts.forEach(function(x){ x.setAttribute('aria-checked',x===next?'true':'false'); });
    next.focus();
    return;
  }
  if(e.key==='Tab'){
    var focusable=[].slice.call(rtModal.querySelectorAll('button:not([disabled])'));
    if(!focusable.length) return;
    var first=focusable[0], last=focusable[focusable.length-1];
    if(e.shiftKey && document.activeElement===first){ e.preventDefault(); last.focus(); }
    else if(!e.shiftKey && document.activeElement===last){ e.preventDefault(); first.focus(); }
  }
});
el('rt-save').addEventListener('click', function(){
  var c = null;
  CFG.draft.clients.forEach(function(x){ if(x.name===rtEditing) c=x; });
  var changed = false;
  if(c && c.router !== rtChoice){
    c.router = rtChoice;
    changed = true;
    renderClients();
    markDirty();
  }
  closeRouterModal();
  if(c && !changed) toast(ui('路由中间件未发生变化','Routing middleware unchanged'));
});
el('rt-cancel').addEventListener('click', closeRouterModal);
rtModal.addEventListener('click', function(e){
  if(e.target.classList.contains('modal-scrim')) closeRouterModal();
});
document.addEventListener('keydown', function(e){
  if(e.key==='Escape' && !rtModal.hidden) closeRouterModal();
});

/* 默认视图 listbox：内置视图 + 自定义视图 + 组合查询 */
function subNames(){ return Object.keys(CFG.draft.query.subqueries); }
function groupNames(){ return Object.keys(CFG.draft.query.groups); }
function dvValue(){ return (CFG.draft && CFG.draft.query.default) || 'client'; }
/* 默认视图候选的显示名:内置维度用本地化标签,自定义视图与组合查询显示原始 key */
function dvLabel(v){ return Q_DIMS.indexOf(v)>=0 ? dimLabel(v) : v; }
var DV = { open:false, active:-1, opts:[] };
function dvRender(){
  if(!CFG.draft) return;
  var cur = dvValue();
  var g = groupNames(), s = subNames();
  var opt = function(v){ return '<div class="dv-opt" role="option" data-v="'+esc(v)+'" aria-selected="'+(v===cur)+'" tabindex="-1"><svg class="ic"><use href="#i-check"/></svg>'+esc(dvLabel(v))+'</div>'; };
  el('dv-pop').innerHTML =
    '<div class="dv-grp">'+ui('内置视图','Built-in views')+'</div>' + BUILTIN_VIEWS.map(opt).join('') +
    (g.length ? '<div class="dv-grp">'+ui('组合查询','Groups')+'</div>' + g.map(opt).join('') : '') +
    (s.length ? '<div class="dv-grp">'+ui('自定义视图','Custom views')+'</div>' + s.map(opt).join('') : '');
  DV.opts = [].slice.call(el('dv-pop').querySelectorAll('.dv-opt'));
  DV.active = -1;
  DV.opts.forEach(function(o,i){ if(o.dataset.v===cur) DV.active = i; });
  el('dv-cur').textContent = dvLabel(cur);
}
function renderDefaultView(){
  buildDefaultSelect();
  if(DV.open) dvRender();
}
function dvSetActive(i, scroll){
  if(!DV.opts.length) return;
  if(DV.active>=0) DV.opts[DV.active].classList.remove('active');
  DV.active = Math.max(0, Math.min(DV.opts.length-1, i));
  DV.opts[DV.active].classList.add('active');
  if(scroll!==false && DV.opts[DV.active].scrollIntoView) DV.opts[DV.active].scrollIntoView({block:'nearest'});
}
function dvOpen(){
  DV.open = true;
  dvRender();
  el('q-default').closest('.config-card').classList.add('dv-open');
  el('dv-pop').hidden = false;
  el('q-default').setAttribute('aria-expanded','true');
  dvSetActive(DV.active>=0 ? DV.active : 0, false);
}
function dvClose(refocus){
  if(!DV.open) return;
  DV.open = false;
  el('q-default').closest('.config-card').classList.remove('dv-open');
  el('dv-pop').hidden = true;
  el('q-default').setAttribute('aria-expanded','false');
  if(refocus) el('q-default').focus();
}
function dvPick(v){
  var changed = v !== dvValue();
  CFG.draft.query.default = v;
  el('q-default').dataset.value = v;
  el('dv-cur').textContent = dvLabel(v);
  dvClose(true);
  if(changed) markDirty();
}
/* 候选变化（组合查询/自定义视图增删）后校验当前默认视图仍有效 */
function buildDefaultSelect(){
  var cur = dvValue();
  var names = BUILTIN_VIEWS.concat(subNames()).concat(groupNames());
  if(names.indexOf(cur)<0) cur = 'client';
  CFG.draft.query.default = cur;
  el('q-default').dataset.value = cur;
  el('dv-cur').textContent = dvLabel(cur);
}
el('q-default').addEventListener('click', function(){
  if(!CFG.draft) return;
  setPalettePop(false);
  if(!calpop.hidden) closeCal();
  DV.open ? dvClose(true) : dvOpen();
});
el('q-default').addEventListener('keydown', function(e){
  if(!CFG.draft) return;
  if(e.key==='ArrowDown' || e.key==='ArrowUp'){
    e.preventDefault();
    if(!DV.open){ dvOpen(); return; }
    dvSetActive(DV.active + (e.key==='ArrowDown' ? 1 : -1));
  }else if(e.key==='Enter'){
    e.preventDefault();
    if(!DV.open){ dvOpen(); return; }
    if(DV.active>=0) dvPick(DV.opts[DV.active].dataset.v);
  }else if(e.key==='Escape' && DV.open){
    e.stopPropagation();
    dvClose(true);
  }
});
el('dv-pop').addEventListener('keydown', function(e){
  if(e.key==='ArrowDown' || e.key==='ArrowUp'){
    e.preventDefault();
    dvSetActive(DV.active + (e.key==='ArrowDown' ? 1 : -1));
  }else if(e.key==='Enter'){
    e.preventDefault();
    if(DV.active>=0) dvPick(DV.opts[DV.active].dataset.v);
  }else if(e.key==='Escape'){
    dvClose(true);
  }
});
el('dv-pop').addEventListener('click', function(e){
  var o = e.target.closest('.dv-opt'); if(!o) return;
  dvPick(o.dataset.v);
});
document.addEventListener('click', function(e){
  if(DV.open && !e.target.closest('.selwrap')) dvClose(false);
});
el('q-default-reset').addEventListener('click', function(){
  if(!CFG.draft) return;
  CFG.draft.query.default = 'client';
  el('q-default').dataset.value = 'client';
  el('dv-cur').textContent = 'client';
  if(DV.open) dvRender();
  markDirty();
  toast(ui('默认视图已恢复为 client','Default view restored to client'));
});

/* 组合查询 / 自定义视图编辑器：行 = 成员 pick 控件；草稿 map 为真相源 */
function qdefRowHtml(kind, name){
  return '<div class="qdef-row" data-key="'+esc(name)+'" data-kind="'+kind+'">' +
    '<div class="qdef-head"><span class="qdef-name">'+esc(name)+'</span>' +
    '<button class="icon-button qdef-del" aria-label="'+esc(ui('删除 ','Delete ')+name)+'">×</button></div>' +
    '<div class="pick"></div></div>';
}
function mountRowPick(row){
  var kind = row.dataset.kind, name = row.dataset.key;
  var map = kind==='group' ? CFG.draft.query.groups : CFG.draft.query.subqueries;
  var initial = String(map[name]||'').split(',').map(function(s){ return s.trim(); }).filter(Boolean);
  row._pick = makeOrderedPick(row.querySelector('.pick'), {
    values: kind==='group' ? Q_DIMS.concat(subNames()) : Q_DIMS.slice(),
    selected: initial,
    labelOn:ui('已选成员','Selected members'), labelOff:ui('可选视图','Available views'),
    min:2, minMsg: kind==='group' ? ui('组合查询至少需要 2 个成员','A group needs at least two members') : ui('自定义视图至少需要 2 个维度','A custom view needs at least two dimensions'),
    text: function(v){ return v; },
    onChange: function(){ map[name] = row._pick.get().join(','); recomputeDirty(); }
  });
}
function renderQDefs(){
  el('q-groups').innerHTML = groupNames().map(function(name){ return qdefRowHtml('group', name); }).join('');
  el('q-subs').innerHTML = subNames().map(function(name){ return qdefRowHtml('sub', name); }).join('');
  el('q-groups').querySelectorAll('.qdef-row').forEach(mountRowPick);
  el('q-subs').querySelectorAll('.qdef-row').forEach(mountRowPick);
  buildDefaultSelect();
}
function groupRefs(name){
  var refs = [];
  Object.keys(CFG.draft.query.groups).forEach(function(g){
    var members=String(CFG.draft.query.groups[g]||'').split(',').map(function(s){ return s.trim(); });
    if(members.indexOf(name)>=0) refs.push(g);
  });
  return refs;
}
el('q-groups').addEventListener('click', function(e){
  if(!CFG.draft) return;
  var del = e.target.closest('.qdef-del'); if(!del) return;
  var row = del.closest('.qdef-row'), name = row.dataset.key;
  if(dvValue()===name){ toast(ui('组合查询 '+name+' 被默认视图引用，无法删除','Grouped query '+name+' is the default view and cannot be deleted')); return; }
  delete CFG.draft.query.groups[name];
  renderQDefs(); markDirty();
  toast(ui('已删除组合查询 '+name,'Grouped query '+name+' deleted'));
});
el('q-subs').addEventListener('click', function(e){
  if(!CFG.draft) return;
  var del = e.target.closest('.qdef-del'); if(!del) return;
  var row = del.closest('.qdef-row'), name = row.dataset.key;
  var refs = groupRefs(name);
  if(refs.length){ toast(ui('自定义视图 '+name+' 被组合查询引用: ','Custom view '+name+' is referenced by: ')+refs.join(', ')); return; }
  if(dvValue()===name){ toast(ui('自定义视图 '+name+' 被默认视图引用，无法删除','Custom view '+name+' is the default view and cannot be deleted')); return; }
  delete CFG.draft.query.subqueries[name];
  renderQDefs(); markDirty();
  toast(ui('已删除自定义视图 '+name,'Custom view '+name+' deleted'));
});
document.querySelectorAll('.qdef-addbar [data-add]').forEach(function(btn){
  btn.addEventListener('click', function(){
    if(!CFG.draft) return;
    var listId = btn.dataset.add;
    var name = btn.closest('.qdef-addbar').querySelector('input').value.trim();
    if(!/^[a-z][a-z0-9_-]*$/.test(name)){ toast(ui('名称须为小写标识符（字母开头）','Name must be a lowercase identifier beginning with a letter')); return; }
    if(Q_RESERVED.indexOf(name)>=0){ toast(ui('保留名不可用','Reserved names cannot be used')); return; }
    if(listId==='q-groups' ? (CFG.draft.query.groups[name]!==undefined || CFG.draft.query.subqueries[name]!==undefined)
                            : (CFG.draft.query.subqueries[name]!==undefined || CFG.draft.query.groups[name]!==undefined)){
      toast(ui('名称已存在','Name already exists')); return;
    }
    var kind = listId==='q-groups' ? 'group' : 'sub';
    var map = kind==='group' ? CFG.draft.query.groups : CFG.draft.query.subqueries;
    map[name] = '';
    renderQDefs();
    btn.closest('.qdef-addbar').querySelector('input').value='';
    markDirty();
    toast(ui('已新增 '+name+'，点选至少 2 个成员',name+' added; select at least two members'));
  });
});

/* 供应商别名：数组保序；新增置顶高亮，删除即从草稿移除 */
function aliasKeyDisp(k){ return k.length>26 ? k.slice(0,12)+'…'+k.slice(-8) : k; }
function renderAliases(flashFirst){
  var list = el('alias-list');
  list.innerHTML = CFG.draft.provider_aliases.map(function(a){
    return '<div class="alias-row" data-key="'+esc(a.key)+'"><span class="alias-key" title="'+esc(a.key)+'">'+esc(aliasKeyDisp(a.key))+'</span><b class="alias-val">'+esc(a.value)+'</b>' +
      '<button class="alias-del" data-key="'+esc(a.key)+'" aria-label="'+esc(ui('删除 ','Delete ')+a.key)+'" title="'+esc(ui('删除 ','Delete ')+a.key)+'">'+ui('删除','Delete')+'</button></div>';
  }).join('');
  if(flashFirst && list.firstChild){
    list.firstChild.classList.add('flash');
    if(list.firstChild.scrollIntoView) list.firstChild.scrollIntoView({block:'nearest'});
    var del=list.firstChild.querySelector('.alias-del');
    if(del) del.focus();
  }
}
el('alias-add-btn').addEventListener('click', function(){
  if(!CFG.draft) return;
  var k = el('alias-add-key').value.trim(), v = el('alias-add-val').value.trim();
  if(!k || !v){ toast(ui('key 和 value 不能为空','key and value are required')); return; }
  var dup = CFG.draft.provider_aliases.some(function(a){ return a.key===k; });
  if(dup){ toast(ui('key 已存在','key already exists')); return; }
  CFG.draft.provider_aliases.unshift({ key:k, value:v });
  renderAliases(true);
  el('alias-add-key').value=''; el('alias-add-val').value='';
  markDirty();
  toast(ui('已新增供应商别名 '+k,'Provider alias '+k+' added'));
});
el('alias-list').addEventListener('click', function(e){
  if(!CFG.draft) return;
  var del = e.target.closest('.alias-del'); if(!del) return;
  var key = del.dataset.key, idx = -1;
  CFG.draft.provider_aliases.forEach(function(a,i){ if(idx<0 && a.key===key) idx=i; });
  if(idx>=0) CFG.draft.provider_aliases.splice(idx,1);
  renderAliases(false);
  markDirty();
  toast(ui('已删除供应商别名 '+key,'Provider alias '+key+' deleted'));
});

/* 配置页事件委托：开关 / 级别 chips / 文本输入；打开弹层与聚焦不产生任何草稿效果 */
var cfgPage = el('page-config');
cfgPage.addEventListener('click', function(e){
  if(!CFG.draft || CFG.saving) return;
  if(e.target.closest('.cfg-actions')) return;
  var chip = e.target.closest('#cfg-log-level .chip');
  if(chip){
    var level = chip.textContent.trim();
    CFG.draft.log.level = level==='default' ? '' : level;
    renderLogCfg();
    markDirty();
    return;
  }
  var tgl = e.target.closest('#cfg-autostart');
  if(tgl){
    CFG.draft.daemon.autostart = !CFG.draft.daemon.autostart;
    tgl.setAttribute('aria-checked', CFG.draft.daemon.autostart?'true':'false');
    markDirty();
    return;
  }
  var en = e.target.closest('[data-enable]');
  if(en){
    var name = en.dataset.enable;
    CFG.draft.clients.forEach(function(c){ if(c.name===name) c.enabled = !c.enabled; });
    renderClients();
    markDirty();
    return;
  }
  var ed = e.target.closest('[data-edit]');
  if(ed){ openRouterModal(ed.dataset.edit, ed); return; }
});
function cfgNum(t){
  var n = parseInt(t.value, 10);
  return isNaN(n) ? 0 : n;
}
cfgPage.addEventListener('input', function(e){
  if(!CFG.draft || CFG.saving) return;
  var t = e.target;
  if(t.id==='cfg-poll-interval'){ CFG.draft.daemon.poll_interval = cfgNum(t); recomputeDirty(); return; }
  if(t.id==='cfg-max-days'){ CFG.draft.log.max_days = cfgNum(t); recomputeDirty(); return; }
  if(t.id==='cfg-log-dir'){ CFG.draft.log.dir = t.value; recomputeDirty(); return; }
});
cfgPage.addEventListener('focusin', function(e){
  var input = e.target.closest('input.ti');
  if(!input || input.closest('.qdef-addbar,.alias-addbar')) return;
  input.dataset.focusValue = input.value;
});
cfgPage.addEventListener('focusout', function(e){
  var input = e.target.closest('input.ti');
  if(!input || input.closest('.qdef-addbar,.alias-addbar') || input.dataset.focusValue===input.value) return;
  /* 与 markDirty 同口径:草稿已回到已保存值(改回原值)时不提示 */
  if(el('cfg-save').disabled) return;
  toast(ui('已修改（未保存）','Modified (unsaved)'));
});

/* 保存：PUT 成功以响应 config+revision 为新真相；409 不覆盖草稿不自动重试 */
function buildPutBody(){
  var config = {
    daemon:{ poll_interval:+CFG.draft.daemon.poll_interval||0, autostart:!!CFG.draft.daemon.autostart },
    log:{ level:CFG.draft.log.level||'', dir:CFG.draft.log.dir||'', max_days:+CFG.draft.log.max_days||0 },
    clients: CFG.draft.clients.map(function(c){ return { name:c.name, enabled:!!c.enabled, router:c.router||'', paths:clone(c.paths)||{} }; }),
    routers: CFG.draft.routers.map(function(r){ return { name:r.name, db_path:r.db_path||'' }; }),
    provider_aliases: CFG.draft.provider_aliases.map(function(a){ return { key:a.key, value:a.value }; })
  };
  /* query 段仅在用户明确编辑过后回传:GET 时 query 处于问题态(解析失败
     附 diagnostics)的话,回退草稿写回会静默清掉既有查询定义——未编辑
     即省略 query,服务端保持磁盘原样。 */
  if(queryTouched() || CFG.forceQueryRewrite){
    config.query = {
      default: CFG.draft.query.default||'',
      subqueries: clone(CFG.draft.query.subqueries)||{},
      groups: clone(CFG.draft.query.groups)||{},
      output_columns: (CFG.draft.query.output_columns||[]).slice()
    };
  }
  return { revision: CFG.savedRevision, config: config };
}
/* query 段是否相对最近一次已接受的服务端快照被修改过 */
function queryTouched(){
  if(!CFG.querySnapshot) return true; /* 无快照(理论上不发生)时按修改处理 */
  return JSON.stringify(serializeQueryDraft()) !== CFG.querySnapshot;
}
function serializeQueryDraft(){
  return {
    default: CFG.draft.query.default||'',
    subqueries: clone(CFG.draft.query.subqueries)||{},
    groups: clone(CFG.draft.query.groups)||{},
    output_columns: (CFG.draft.query.output_columns||[]).slice()
  };
}
function saveConfig(){
  if(!CFG.saved || CFG.saving) return;
  CFG.saving = true;
  recomputeDirty();
  api('/api/config', { method:'PUT', headers:{'Content-Type':'application/json'}, body:JSON.stringify(buildPutBody()) }).then(function(r){
    CFG.saving = false;
    if(r.status===200 && r.body){
      /* 保存成功：以响应里的 config 与 revision 替换本地已保存快照 */
      CFG.savedRevision = r.body.revision || CFG.savedRevision;
      CFG.saved = normalizeDraft(r.body.config);
      CFG.draft = clone(CFG.saved);
      CFG.diag = (r.body.config && r.body.config.query && r.body.config.query.diagnostics) || [];
      CFG.querySnapshot = JSON.stringify(serializeQueryDraft());
      CFG.forceQueryRewrite = false;
      renderAllConfig();
      recomputeDirty();
      var w = r.body.warnings||[];
      if(w.length){
        toast(ui('已保存，但需要注意','Saved, but attention needed')+'：'+w.join(ui('；','; ')));
      }else if(r.body.changed){
        toast(ui('配置已保存','Config saved'));
      }else{
        toast(ui('配置未变化','No changes'));
      }
      /* 保存会即时改变 provider 别名与自定义视图定义：重新拉取仪表板 */
      if(state.data) loadDashboard();
      return;
    }
    if(r.status===409){
      /* revision 冲突：不覆盖他处修改、不自动重试；保留本地草稿，由用户刷新 */
      toast(ui('配置已在别处被修改，请刷新页面后重试；当前修改已保留','Config was changed elsewhere; reload the page and retry. Local edits are kept.'));
    }else if(r.status===400){
      toast((r.body && r.body.error && r.body.error.message) || ui('保存失败','Save failed'));
    }else{
      toast(ui('保存失败','Save failed'));
    }
    recomputeDirty();
  }, function(){
    CFG.saving = false;
    toast(ui('保存失败','Save failed'));
    recomputeDirty();
  });
}
el('cfg-save').addEventListener('click', saveConfig);

/* 恢复全部默认值:从服务端拉取全默认编辑模型作为待保存草稿(默认值定义
   只在后端,前端不复制第二套);不直接落盘,需再点保存才生效。 */
function resetAllConfig(){
  if(!CFG.saved) return;
  api('/api/config?defaults=1').then(function(r){
    if(!r.ok || !r.body || !r.body.config){
      toast((r.body && r.body.error && r.body.error.message) || ui('读取默认配置失败','Failed to load defaults'));
      return;
    }
    CFG.draft = normalizeDraft(r.body.config);
    /* 默认草稿的 query 与问题态下的 GET 回退草稿可能完全相同:显式标记
       待重写,保证保存携带 query、dirty 不清零 */
    CFG.forceQueryRewrite = true;
    renderAllConfig();
    recomputeDirty();
    toast(ui('已载入全部默认值（未保存）','Defaults loaded (unsaved)'));
  }, function(){
    toast(ui('读取默认配置失败','Failed to load defaults'));
  });
}
var resetModal=el('reset-modal'), resetOpener=null;
function closeResetModal(){
  resetModal.hidden=true;
  if(resetOpener){ resetOpener.focus(); resetOpener=null; }
}
el('cfg-reset').addEventListener('click',function(e){
  if(!CFG.saved) return;
  el('toast').setAttribute('data-show','false');
  resetOpener=e.currentTarget;
  resetModal.hidden=false;
  el('reset-cancel').focus();
});
el('reset-cancel').addEventListener('click',closeResetModal);
el('reset-confirm').addEventListener('click',function(){ closeResetModal(); resetAllConfig(); });
resetModal.addEventListener('click',function(e){ if(e.target.classList.contains('modal-scrim')) closeResetModal(); });
resetModal.addEventListener('keydown',function(e){
  if(resetModal.hidden) return;
  if(e.key==='Escape'){ e.preventDefault(); closeResetModal(); return; }
  if(e.key==='Tab'){
    var a=[el('reset-cancel'),el('reset-confirm')], first=a[0], last=a[1];
    if(e.shiftKey&&document.activeElement===first){ e.preventDefault(); last.focus(); }
    else if(!e.shiftKey&&document.activeElement===last){ e.preventDefault(); first.focus(); }
  }
});

/* ---------- 语言切换：静态文案 TreeWalker + 动态区重建 ---------- */
function syncLocaleUI(){
  root.lang=LOCALE;
  root.setAttribute('data-locale',LOCALE);
  document.title=LOCALE==='en' ? 'Token Usage Dashboard' : 'Token Usage 仪表盘';
  el('appearance-control').setAttribute('aria-label',ui('语言与外观','Language and appearance'));
  el('locale-switch').setAttribute('aria-label',ui('界面语言','Interface language'));
  el('locale-switch').querySelectorAll('button').forEach(function(b){
    var on=b.dataset.locale===LOCALE;
    b.setAttribute('aria-pressed',on?'true':'false');
    var label=b.dataset.locale==='zh-CN' ? ui('切换到中文','Switch to Chinese') : ui('切换到英文','Switch to English');
    b.setAttribute('aria-label',label); b.title=label;
  });
  var sth=document.querySelectorAll('.top-sessions thead th');
  [ui('会话','Session'),dimLabel('client'),dimLabel('project'),ui('时长','Duration'),colLabel(colById('requests')),colLabel(colById('total'))].forEach(function(label,i){ if(sth[i]) sth[i].textContent=label; });
  applyStaticLocale(document.body);
  syncThemeSeg();
  syncPalette();
  syncSide();
  renderMeta();
  renderCal();
  buildColsEditor();
  if(CFG.draft){
    el('q-groups').querySelectorAll('.qdef-row').forEach(mountRowPick);
    el('q-subs').querySelectorAll('.qdef-row').forEach(mountRowPick);
    document.querySelectorAll('.qdef-row').forEach(function(row){
      var del=row.querySelector('.qdef-del');
      if(del) del.setAttribute('aria-label',ui('删除 ','Delete ')+row.dataset.key);
    });
    el('alias-list').querySelectorAll('.alias-row').forEach(function(row){
      var del=row.querySelector('.alias-del'), k=row.dataset.key;
      del.textContent=ui('删除','Delete'); del.setAttribute('aria-label',ui('删除 ','Delete ')+k); del.title=ui('删除 ','Delete ')+k;
    });
    renderDiag();
    renderClients();
  }
  if(DV.open) dvRender();
  renderAll();
  recomputeDirty();
}
el('locale-switch').addEventListener('click',function(e){
  var b=e.target.closest('[data-locale]');
  if(!b || b.dataset.locale===LOCALE) return;
  LOCALE=b.dataset.locale;
  try{ localStorage.setItem('tu-locale',LOCALE); }catch(_){}
  syncLocaleUI();
});

/* ---------- 初始引导：恢复选区 → 日历预选 → 静态本地化 → 拉取 meta 与仪表板 ---------- */
restoreSavedRange();
CAL.view = (function(){ var t=startToday(); return new Date(t.getFullYear(), t.getMonth(), 1); })();
var b0 = rangeBounds();
setCalSel(ymd(b0.s), ymd(b0.e));
syncLocaleUI();
renderCal();
fetchMeta().then(function(){
  /* 配置预加载(输出列首开生效);失败不阻塞仪表板数据 */
  return fetchConfigState();
}, function(){
  return fetchConfigState();
}).then(loadDashboard, loadDashboard);
})();
