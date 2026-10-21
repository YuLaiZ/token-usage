/* 截图分享真实浏览器回归。使用独立合成载荷，不读取用户数据库或运行服务。
   PLAYWRIGHT_MODULE 指向已安装的 playwright 模块；可选 BROWSER_EXECUTABLE。
   node internal/web/testdata/screenshot_browser.cjs [素材输出目录]
   独立运行，不由 go test 启动；与 Go 驱动的 vm 纯函数/管线测试互补。
   剪贴板使用页内 sink，系统剪贴板与 Safari 手测证据另行记录。 */
'use strict';
const fs = require('fs');
const path = require('path');
const http = require('http');
const assert = require('assert/strict');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const output = path.resolve(process.argv[2] || '/tmp/tu-share-accept');
const assets = process.env.SCREENSHOT_ASSETS || path.resolve(__dirname, '../static');
const metric = { requests:3, fresh_input:100, output:100, cache_read:30, cache_create:0, reasoning:0, total:230 };
const dims = ['client','provider','model','project'];
const rows = dims.map(d => [d, Array.from({length:20}, (_,i) => ({...metric,key:d==='project'?'SECRET_PROJECT_'+i:d+'_'+i}))]);
const custom = name => ({name,dimensions:['client','project'],rows:Array.from({length:20}, (_,i) => ({...metric,keys:['Codex','SECRET_CUSTOM_'+i]}))});
const payload = {totals:{...metric,total:4600},dimensions:Object.fromEntries(rows),trend:{granularity:'day',buckets:[{key:'2026-10-14',total:230},{key:'2026-10-15',total:460}],peak_index:1,peak_total:460,empty:false},heatmap:{mode:'calendar',days:[{date:'2026-10-14',total:230},{date:'2026-10-15',total:460}]},custom_views:[custom('cp'),custom('pc')],sessions:Array.from({length:20},(_,i)=>({...metric,title:'SECRET_SESSION_'+i,project:'SECRET_PROJECT_'+i,client:'Codex',duration_ms:1000}))};
let cssMode='ok', cssSuffix='', revision=0, customNames=['cp','pc'], dashDelay=0, dashFail=false, dashboardCalls=0, emptyFreshness=false;
const server = http.createServer((req,res)=>{
  if(req.url.startsWith('/api/')){
    res.setHeader('Content-Type','application/json');
    if(req.url.startsWith('/api/meta')) return res.end(JSON.stringify({version:'v0.2.1-dev',data_through:'2026-10-15 08:00:00',last_collection:'2026-10-15 08:00:05'}));
    if(req.url.startsWith('/api/config')) return res.end(JSON.stringify({revision:'fixture',config:{query:{output_columns:['requests','input','output','cache_read','reasoning','total','cache_hit']},refresh:{dashboard_interval:3600}}}));
    dashboardCalls++;
    return setTimeout(()=>{const hour=revision>=20000?'12':revision?'11':'10';res.statusCode=dashFail?503:200;res.end(JSON.stringify(dashFail?{error:{message:'controlled dashboard failure'}}:{...payload,data_through:emptyFreshness?null:'2026-10-15 '+hour+':00:00',last_collection:emptyFreshness?null:'2026-10-15 '+hour+':00:05',custom_views:customNames.map(custom),totals:{...payload.totals,total:4600+revision}}));},dashDelay);
  }
  const name=req.url.startsWith('/assets/')?req.url.slice(8):'index.html';
  if(name==='app.css'&&cssMode!=='ok'){res.statusCode=cssMode==='empty'?200:503;return res.end('');}
  res.setHeader('Content-Type',name.endsWith('.css')?'text/css':name.endsWith('.js')?'application/javascript':'text/html');
  const body=fs.readFileSync(path.join(assets,name));
  res.end(name==='app.css'?Buffer.concat([body,Buffer.from(cssSuffix)]):body);
});
async function main(){
  fs.mkdirSync(output,{recursive:true});
  await new Promise(r=>server.listen(0,'127.0.0.1',r));
  const origin='http://127.0.0.1:'+server.address().port;
  const browser=await chromium.launch({headless:true,...(process.env.BROWSER_EXECUTABLE?{executablePath:process.env.BROWSER_EXECUTABLE}:{})});
  const report={browser:browser.version(),cases:[]};
  const pass=(name,evidence={})=>{report.cases.push({name,...evidence});console.log('PASS '+name);};
  try{
    const context=await browser.newContext({viewport:{width:1440,height:1000},deviceScaleFactor:2,locale:'zh-CN'});
    const page=await context.newPage();
    // 同日时间格式的断言与固定样本日期一致，避免运行日期改变测试结果。
    await page.clock.setFixedTime(new Date('2026-10-15T12:00:00+08:00'));
    const errors=[];page.on('pageerror',e=>errors.push(String(e)));
    await page.addInitScript(()=>{
      localStorage.setItem('tu-theme','dark');
      window.__shotEvidence={svg:'',writes:[],inClick:false,deny:false,toBlobFails:0,canvasCalls:0,imageFail:false};
      const e=window.__shotEvidence;
      const listen=EventTarget.prototype.addEventListener;
      EventTarget.prototype.addEventListener=function(type,fn,opts){
        if(this.id==='shot-copy'&&type==='click') return listen.call(this,type,function(...args){e.inClick=true;try{return fn.apply(this,args);}finally{e.inClick=false;}},opts);
        return listen.call(this,type,fn,opts);
      };
      const desc=Object.getOwnPropertyDescriptor(HTMLImageElement.prototype,'src');
      Object.defineProperty(HTMLImageElement.prototype,'src',{...desc,set(u){
        if(u.startsWith('data:image/svg+xml')){e.svg=decodeURIComponent(u.slice(u.indexOf(',')+1));if(e.imageFail)u='data:image/svg+xml,<invalid';}
        return desc.set.call(this,u);
      }});
      const nativeContext=HTMLCanvasElement.prototype.getContext;HTMLCanvasElement.prototype.getContext=function(...args){if(e.contextFail)throw new Error('controlled context exception');return nativeContext.apply(this,args);};
      const toBlob=HTMLCanvasElement.prototype.toBlob;
      HTMLCanvasElement.prototype.toBlob=function(cb,...args){e.canvasCalls++;if(e.toBlobFails-->0){cb(null);return;}return toBlob.call(this,cb,...args);};
      Object.defineProperty(navigator,'clipboard',{value:{write(items){
        const inClick=e.inClick;
        if(e.deny) return Promise.reject(new Error('controlled clipboard denial'));
        const gate=e.hold ? new Promise(resolve=>{e.release=resolve;}) : Promise.resolve();
        return gate.then(()=>items[0].getType('image/png')).then(async blob=>{
          const image=await createImageBitmap(blob);
          e.writes.push({inClick,blob,width:image.width,height:image.height}); image.close();
        });
      }}});
    });
    await page.goto(origin);
    await page.waitForSelector('#sessions-body .session-row');
    async function pageName(heading,title,nav){
      assert.equal(await page.title(),title+' · Token Usage');
      const active=page.locator('.page[data-active="true"]');
      assert.equal(await active.locator('h2').textContent(),heading);
      const usageNav=page.locator('.nav-button[data-page="dash"]');
      assert.equal(await usageNav.getAttribute('aria-label'),nav);
      assert.equal(await usageNav.locator('.nav-txt').textContent(),nav);
    }
    await pageName('用量统计','用量统计','用量统计');
    await page.locator('.nav-button[data-page="config"]').click();
    await pageName('配置','配置','用量统计');
    await page.locator('#locale-switch button[data-locale="en"]').click();
    await pageName('Config','Config','Usage');
    await page.locator('.nav-button[data-page="dash"]').click();
    await pageName('Usage','Usage','Usage');
    await page.locator('#locale-switch button[data-locale="zh-CN"]').click();
    await pageName('用量统计','用量统计','用量统计');
    pass('page-titles-headings-and-navigation-follow-locale');
    await page.locator('#range-seg button[data-v="6"]').click();
    await page.waitForFunction(()=>document.querySelector('#page-dash').getAttribute('data-refreshing')!=='true');
    const viewBefore=await page.locator('#chart').innerHTML();
    const desktopFont=await page.locator('#groups td.num').first().evaluate(n=>{const s=getComputedStyle(n);return[s.fontFamily,s.fontSize,s.fontWeight,s.lineHeight];});
    await page.screenshot({path:path.join(output,'header-freshness.png'),clip:{x:0,y:0,width:1440,height:240}});
    async function ready(){
      await page.waitForFunction(()=>!document.getElementById('shot-copy').disabled,{},{timeout:30000});
      await page.waitForFunction(()=>document.querySelectorAll('iframe').length===0);
    }
    async function open(){await page.locator('#shot-trigger').click();await ready();}
    async function close(){await page.locator('#shot-close').click();}
    async function change(k,on){await page.locator('[data-shot="'+k+'"]').setChecked(on);await ready();}
    async function inspect(name,save=true){
      const svg=await page.evaluate(()=>window.__shotEvidence.svg);
      assert(svg.includes('font:500 11.5px/1.3 var(--mono)'),name+': raw font shorthand');
      assert(!/font-(?:size|family):\s*;/.test(svg),name+': no corrupted font');
      const replay=await context.newPage();
      await replay.goto('data:image/svg+xml;charset=utf-8,'+encodeURIComponent(svg));
      const data=await replay.evaluate(()=>{
        const root=document.querySelector('.share-sheet'),box=root.getBoundingClientRect();
        const last=root.lastElementChild.getBoundingClientRect();
        const font=sel=>{const n=root.querySelector(sel);if(!n)return null;const s=getComputedStyle(n);return [s.fontFamily,s.fontSize,s.fontWeight,s.lineHeight];};
        return {height:box.height,width:box.width,last:last.bottom,lastRow:root.querySelector('.top-sessions tbody tr:last-child')?.getBoundingClientRect().bottom,overflow:root.scrollWidth>1280,header:root.querySelector('.share-head').textContent,periods:root.querySelectorAll('.share-range-seg .share-static-control').length,selected:root.querySelector('.share-range-seg [data-selected="true"]')?.textContent,date:root.querySelector('.share-date-box')?.textContent,query:root.querySelector('.share-query-time')?.textContent,privacy:root.querySelector('.share-privacy-badge')?.getAttribute('data-masked'),groups:root.querySelectorAll('.group').length,lists:root.querySelectorAll('.group-table').length,compose:root.querySelectorAll('.compose').length,charts:root.querySelectorAll('.chart-card').length,custom:root.querySelectorAll('.cvx').length,sessions:root.querySelectorAll('.session-row').length,controls:root.querySelectorAll('button,.sidebar,.side-toggle,.modal,.toast').length,leaks:root.innerHTML.includes('SECRET_'),font:font('.group td.num'),customFont:font('.cvx td.num'),theme:root.getAttribute('data-theme'),locale:root.getAttribute('data-locale'),palette:root.getAttribute('data-palette'),bg:getComputedStyle(root,'::before').backgroundImage,top:root.querySelector('.group .compose')?.getBoundingClientRect().top,bottom:root.querySelector('.group .group-table')?.getBoundingClientRect().top};
      });
      const dimensions=await page.evaluate(async()=>{const b=await(await fetch(document.getElementById('shot-preview').src)).blob();const i=await createImageBitmap(b);const r={width:i.width,height:i.height};i.close();return r;});
      assert.equal(data.width,1280); assert(data.height<=dimensions.height/2+.1,name+': content fits PNG');
      assert(data.last<=dimensions.height/2+.1,name+': last card fits PNG');if(data.lastRow)assert(data.lastRow<dimensions.height/2,name+': last session row fits PNG');assert(!data.overflow,name+': horizontal fit');assert.equal(data.controls,0);
      assert(data.header.includes('Token Usage')&&data.header.includes('v0.2.1-dev')&&data.header.includes('2026-'),name+': header context');
      if(data.font) assert.deepEqual(data.font,desktopFont,name+': fixed desktop layout font parity');
      if(save){const data=await page.evaluate(async()=>{const b=await(await fetch(document.getElementById('shot-preview').src)).blob();return await new Promise(resolve=>{const f=new FileReader();f.onload=()=>resolve(f.result);f.readAsDataURL(b);});});fs.writeFileSync(path.join(output,name+'.png'),Buffer.from(data.split(',')[1],'base64'));}
      await replay.close();return {...data,png:dimensions};
    }
    const callsBefore=dashboardCalls;
    await open();
    assert((await page.locator('.asof').getAttribute('title')).includes('2026-10-15 10:00:05'));
    assert((await page.locator('.asof').getAttribute('title')).includes('数据截至 · 2026-10-15 10:00:00'));
    const header=await page.locator('#asof-t').textContent();assert(!header.includes('\n'));assert(header.includes('最近同步 10:00:05'));assert(header.includes('数据截至 · 10:00:00'));assert(!header.includes('2026-'));
    assert((await page.locator('.asof').boundingBox()).height<=36,'desktop freshness stays one compact row');
    assert.equal(await page.locator('#privacy-trigger').textContent(),'隐私遮蔽');assert.equal(await page.locator('#privacy-trigger').getAttribute('title'),'隐藏项目名和会话标题');
    pass('header-freshness-comes-from-dashboard-snapshot');
    await page.keyboard.press('Shift+Tab');assert.equal(await page.evaluate(()=>document.activeElement.id),'shot-copy');
    await page.keyboard.press('Tab');assert.equal(await page.evaluate(()=>document.activeElement.id),'shot-privacy-toggle');pass('keyboard-focus-trap');
    assert(await page.locator('[data-shot="list"]').isChecked());assert(await page.locator('[data-shot="trend"]').isChecked());
    assert(!(await page.locator('[data-shot="sessions"]').isChecked()));
    assert.equal(await page.locator('[data-shot-custom]:checked').count(),0);
    const first=await inspect('dark-default');assert.equal(first.sessions,0);assert.equal(first.custom,0);pass('first-open-defaults',first);assert.equal(first.periods,7);assert.equal(first.selected,await page.locator('#range-seg [aria-pressed="true"]').textContent());assert(first.date.includes('2026-'));assert(first.header.includes('用量'));assert(!first.header.includes('仪表盘'));assert.equal(first.query,'数据截至 · 2026-10-15 10:00:00');pass('original-header-and-static-range-preserved');
    for(const k of ['compose','heat','sessions']) await change(k,true);
    await page.locator('[data-shot-all="yes"]').click();await ready();
    const all=await inspect('dark-all');
    assert.equal(all.groups,4);assert.equal(all.lists,4);assert.equal(all.compose,4);assert.equal(all.charts,2);assert.equal(all.custom,2);assert.equal(all.sessions,20);assert(all.top<all.bottom);
    assert.equal(await page.locator('#chart').innerHTML(),viewBefore);assert.equal(dashboardCalls,callsBefore);
    pass('all-modules-and-original-page-unchanged',all);
    await page.screenshot({path:path.join(output,'dialog-dark.png')});
    const mainBefore=await page.locator('#dash-results').innerHTML(),privacyBefore=await page.locator('#privacy-trigger').getAttribute('aria-pressed'),requestsBeforePrivacy=dashboardCalls;
    await page.locator('#shot-privacy-toggle').click();await ready();
    assert.equal(await page.locator('#shot-privacy').textContent(),'本次图片：敏感名称已隐藏');const privateImage=await inspect('dialog-masked');assert(!privateImage.leaks);assert.equal(privateImage.privacy,'true');assert.equal(await page.locator('#privacy-trigger').getAttribute('aria-pressed'),privacyBefore);assert.equal(await page.locator('#dash-results').innerHTML(),mainBefore);assert.equal(dashboardCalls,requestsBeforePrivacy);assert(!JSON.parse(await page.evaluate(()=>localStorage.getItem('tu-shot-options'))).hasOwnProperty('masked'));pass('dialog-mask-is-independent-and-does-not-query');
    await page.locator('#shot-privacy-toggle').click();await ready();assert.equal(await page.locator('#shot-privacy').textContent(),'本次图片：敏感名称可见');const plainAgain=await inspect('dialog-plain-again',false);assert(plainAgain.leaks);assert.equal(plainAgain.privacy,'false');assert.equal(plainAgain.header,all.header);pass('dialog-can-restore-plaintext-from-same-capture');

    const previewHash=await page.evaluate(async()=>Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256',await(await fetch(document.getElementById('shot-preview').src)).arrayBuffer()))).join(','));
    await page.locator('#shot-copy').click();await page.waitForFunction(()=>window.__shotEvidence.writes.length===1);
    const copied=await page.evaluate(async()=>{const w=window.__shotEvidence.writes[0];return{inClick:w.inClick,hash:Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256',await w.blob.arrayBuffer()))).join(',')};});
    assert(copied.inClick);assert.equal(copied.hash,previewHash);pass('clipboard-is-exact-preview-and-called-in-click-stack');
    await page.evaluate(()=>window.__shotEvidence.hold=true);await page.locator('#shot-copy').click();await page.waitForFunction(()=>!!window.__shotEvidence.release);assert(await page.locator('#shot-copy').isDisabled());assert(await page.locator('#shot-privacy-toggle').isDisabled());await page.keyboard.press('Tab');assert(await page.evaluate(()=>!!document.activeElement.closest('#shot-modal')));await page.keyboard.press('Escape');assert(await page.locator('#shot-modal').isVisible());await page.evaluate(()=>{window.__shotEvidence.hold=false;window.__shotEvidence.release();});await page.waitForFunction(()=>!document.getElementById('shot-copy').disabled);pass('pending-clipboard-retains-dialog-focus');
    await page.evaluate(()=>window.__shotEvidence.deny=true);await page.locator('#shot-copy').click();
    await page.waitForFunction(()=>document.getElementById('shot-status').textContent.includes('controlled clipboard denial'));
    assert(!(await page.locator('#shot-copy').isDisabled()));await page.evaluate(()=>window.__shotEvidence.deny=false);pass('clipboard-denial-restores-controls');
    for(const k of ['list','compose','trend','heat','sessions']) await change(k,false);
    await page.locator('[data-shot-all="no"]').click();await ready();
    const summary=await inspect('summary-only');assert.equal(summary.groups+summary.charts+summary.custom+summary.sessions,0);assert(summary.height<500);pass('all-unchecked-is-summary',summary);
    await close();assert.equal(await page.evaluate(()=>document.activeElement.id),'shot-trigger');
    await open();assert.equal(await page.locator('#shot-choices input:checked').count(),0);pass('remember-explicit-empty');
    // 连续改变后只允许最终配置的预览可复制。
    await page.evaluate(()=>{for(const k of ['list','compose','heat','list','compose']){const n=document.querySelector('[data-shot="'+k+'"]');n.checked=!n.checked;n.dispatchEvent(new Event('change',{bubbles:true}));}});await ready();
    const rapid=await inspect('rapid-final',false);assert.equal(rapid.groups,0);assert.equal(rapid.charts,1);pass('rapid-selection-final-result',rapid);
    const frozen=await page.locator('#shot-preview').getAttribute('src');revision=9999;
    await page.evaluate(()=>document.getElementById('dash-refresh').click());
    await page.waitForFunction(()=>document.querySelector('#kpis').textContent.includes('14.6'));
    assert((await page.locator('.asof').getAttribute('title')).includes('2026-10-15 11:00:05'));
    assert((await page.locator('.asof').getAttribute('title')).includes('数据截至 · 2026-10-15 11:00:00'));
    const capturedTime=await inspect('frozen-data-through',false);assert.equal(capturedTime.query,'数据截至 · 2026-10-15 10:00:00');
    pass('manual-refresh-updates-header-but-freezes-open-share-time');
    assert.equal(await page.locator('#shot-preview').getAttribute('src'),frozen);
    await change('trend',true);assert((await page.evaluate(()=>window.__shotEvidence.svg)).includes('4.60'));pass('background-refresh-does-not-change-open-capture');
    await page.locator('#shot-privacy-toggle').click();await ready();assert((await page.evaluate(()=>window.__shotEvidence.svg)).includes('4.60'));assert.equal(dashboardCalls,callsBefore+1);pass('privacy-after-refresh-keeps-frozen-data');

    await close();
    await page.locator('#privacy-trigger').click();await page.locator('#locale-switch button[data-locale="en"]').click();await page.locator('#theme-toggle').click();
    await page.waitForTimeout(400);await open();
    for(const k of ['list','compose','trend','sessions']) await change(k,true);
    await page.locator('[data-shot-all="yes"]').click();await ready();
    assert.equal(await page.locator('#shot-privacy-toggle').getAttribute('aria-pressed'),'true');const masked=await inspect('light-en-masked');assert(!masked.leaks);assert.equal(masked.theme,'light');assert.equal(masked.locale,'en');assert.equal(await page.locator('#shot-privacy-toggle').textContent(),'Privacy masking');assert.equal(await page.locator('#shot-privacy').textContent(),'This image: sensitive names are hidden');
    const bg=await page.evaluate(()=>getComputedStyle(document.body,'::before').backgroundImage);assert.equal(masked.bg,bg);pass('light-en-masked-root-and-fonts',masked);
    await page.locator('#shot-privacy-toggle').click();await ready();const inverse=await inspect('dialog-unmask-outer-masked',false);assert(inverse.leaks);assert.equal(await page.locator('#privacy-trigger').getAttribute('aria-pressed'),'true');await close();await open();assert.equal(await page.locator('#shot-privacy-toggle').getAttribute('aria-pressed'),'true');const reopened=await inspect('dialog-reopened-masked',false);assert(!reopened.leaks);pass('privacy-reopens-following-main-not-last-dialog');

    await close();
    await page.setViewportSize({width:390,height:844});await page.screenshot({path:path.join(output,'header-mobile.png'),clip:{x:0,y:0,width:390,height:230}});await open();
    const mobile=await inspect('mobile-share');assert.equal(mobile.lists,4);assert.equal(mobile.sessions,20);assert.equal(mobile.custom,2);
    assert((await page.locator('.asof-short').textContent()).includes('Through 11:00'));
    const toolsFit=await page.locator('.global-tools').evaluate(n=>n.scrollWidth<=n.clientWidth);assert(toolsFit,'mobile freshness and appearance controls fit');
    assert(await page.locator('#shot-copy').isVisible());const copyBox=await page.locator('#shot-copy').boundingBox();assert(copyBox.y>=0 && copyBox.y+copyBox.height<=844,'mobile copy action stays in viewport');await page.screenshot({path:path.join(output,'dialog-mobile.png')});pass('mobile-config-and-full-desktop-report',mobile);
    await page.keyboard.press('Escape');assert(await page.locator('#shot-modal').isHidden());
    await page.setViewportSize({width:1440,height:1000});
    for(const mode of ['503','empty']){cssMode=mode;await page.locator('#shot-trigger').click();assert(await page.locator('#shot-modal').isHidden());assert((await page.locator('#toast').textContent()).includes('stylesheet fetch failed'));pass('css-'+mode+'-blocks-export');}cssMode='ok';
    await page.evaluate(()=>{window.__nativeXHR=XMLHttpRequest;window.XMLHttpRequest=function(){throw new Error('controlled XHR exception');};});await page.locator('#shot-trigger').click();assert(await page.locator('#shot-modal').isHidden());assert((await page.locator('#toast').textContent()).includes('stylesheet fetch failed'));await page.evaluate(()=>window.XMLHttpRequest=window.__nativeXHR);pass('xhr-exception-blocks-export');
    await open();await page.evaluate(()=>window.__shotEvidence.imageFail=true);await changeFail('image');await page.evaluate(()=>window.__shotEvidence.imageFail=false);await page.locator('#shot-retry').click();await ready();pass('image-failure-and-retry');
    await page.evaluate(()=>{window.__shotEvidence.toBlobFails=1;window.__shotEvidence.canvasCalls=0;});await change('sessions',true);
    assert.equal(await page.evaluate(()=>window.__shotEvidence.canvasCalls),2);pass('canvas-falls-back-to-1x');
    await page.evaluate(()=>window.__shotEvidence.toBlobFails=99);await changeFail('canvas');await page.evaluate(()=>window.__shotEvidence.toBlobFails=0);await page.locator('#shot-retry').click();await ready();pass('canvas-all-fail-and-retry');
    await page.evaluate(()=>window.__shotEvidence.contextFail=true);await changeFail('canvas');await page.evaluate(()=>window.__shotEvidence.contextFail=false);await page.locator('#shot-retry').click();await ready();pass('synchronous-canvas-exception-settles-and-retries');
    async function changeFail(kind){await page.locator('[data-shot="sessions"]').click();await page.waitForFunction(()=>!document.getElementById('shot-retry').hidden);assert(await page.locator('#shot-copy').isDisabled());assert(await page.locator('#shot-preview').isHidden());assert((await page.locator('#shot-status').textContent()).includes(kind==='image'?'SVG image load failed':'canvas export failed'));}
    await close();
    await page.locator('#shot-trigger').click();await page.locator('#shot-close').click();await open();await page.waitForTimeout(100);assert(!(await page.locator('#shot-copy').isDisabled()));pass('close-during-generation-and-reopen');await close();
    await page.evaluate(()=>localStorage.setItem('tu-shot-options',JSON.stringify({list:true,compose:true,trend:true,heat:true,custom:['cp','pc','removed']})));
    customNames=['pc','new'];await page.locator('#dash-refresh').click();await page.waitForFunction(()=>document.querySelector('#cviews').textContent.includes('new'));
    await open();assert.equal(await page.locator('[data-shot-custom]:checked').count(),1);assert(await page.locator('[data-shot-custom="pc"]').isChecked());const partial=await inspect('one-custom',false);assert.equal(partial.custom,1);pass('custom-choices-reconcile-by-name');await close();
    customNames=[];await page.locator('#dash-refresh').click();await page.waitForFunction(()=>!document.querySelector('#cviews .cvx'));await open();assert.equal(await page.locator('[data-shot-custom]').count(),0);const noViews=await inspect('no-custom',false);assert.equal(noViews.custom,0);pass('no-configured-custom-views');await close();
    await page.evaluate(()=>localStorage.setItem('tu-shot-options','{bad json'));await open();assert(await page.locator('[data-shot="list"]').isChecked());pass('malformed-local-preferences-recover');await close();
    await page.locator('#view-seg button[data-v="compose"]').click();await page.locator('#mode-seg button[data-v="heat"]').click();await open();assert(await page.locator('[data-shot="compose"]').isChecked());assert(await page.locator('[data-shot="heat"]').isChecked());assert(!(await page.locator('[data-shot="list"]').isChecked()));assert(!(await page.locator('[data-shot="trend"]').isChecked()));pass('first-use-follows-compose-and-heat');await close();
    await page.locator('#palette-toggle').click();await page.locator('[data-pal="lake"]').click();await page.waitForTimeout(200);await open();const palette=await inspect('lake-palette',false);assert.equal(palette.palette,'lake');assert.equal(palette.bg,await page.evaluate(()=>getComputedStyle(document.body,'::before').backgroundImage));pass('nondefault-palette-preserved');await close();
    const specialText='</style> & ]]>';
    cssSuffix='\n.share-query-time::after{content:'+JSON.stringify(specialText)+';}\n';
    await page.locator('#shot-trigger').click();
    await page.waitForFunction(()=>!document.getElementById('shot-copy').disabled || !document.getElementById('shot-retry').hidden);
    assert(await page.locator('#shot-retry').isHidden(),'CSS XML special characters must export successfully');
    await ready();
    const specialSVG=await page.evaluate(()=>window.__shotEvidence.svg);
    const xmlCSS=await page.evaluate(svg=>{
      const xml=new DOMParser().parseFromString(svg,'image/svg+xml');
      return {valid:!xml.querySelector('parsererror'),css:xml.querySelector('style')?.textContent};
    },specialSVG);
    assert(xmlCSS.valid);assert(xmlCSS.css.includes(cssSuffix),'XML parsing must preserve the original CSS text');
    const specialReplay=await context.newPage();
    await specialReplay.goto('data:image/svg+xml;charset=utf-8,'+encodeURIComponent(specialSVG));
    assert.equal(await specialReplay.locator('.share-query-time').evaluate(n=>getComputedStyle(n,'::after').content),JSON.stringify(specialText));
    await specialReplay.close();
    const special=await inspect('css-special-characters',false);pass('css-xml-special-characters-preserve-text-and-export',special);
    await close();cssSuffix='';
    emptyFreshness=true;await page.locator('#dash-refresh').click();
    await page.waitForFunction(()=>document.querySelector('#asof-t').textContent.includes('Data through · —'));
    await open();const unknown=await inspect('unknown-freshness',false);assert.equal(unknown.query,'Data through · —');pass('null-freshness-shows-dash-without-query-time-fallback');await close();emptyFreshness=false;
    const autoPage=await context.newPage();let autoMetaCalls=0;
    autoPage.on('request',r=>{if(r.url().endsWith('/api/meta'))autoMetaCalls++;});
    await autoPage.route('**/api/config',route=>route.fulfill({json:{revision:'auto-fixture',config:{query:{output_columns:['requests','input','output','cache_read','reasoning','total','cache_hit']},refresh:{dashboard_interval:1}}}}));
    await autoPage.goto(origin);await autoPage.waitForFunction(()=>document.getElementById('asof-t').textContent.includes('11:00:05'));
    revision=20000;await autoPage.waitForFunction(()=>document.getElementById('asof-t').textContent.includes('12:00:05'));
    assert((await autoPage.locator('.asof').getAttribute('title')).includes('Data through · 2026-10-15 12:00:00'));assert.equal(autoMetaCalls,1);
    await autoPage.close();pass('automatic-refresh-updates-both-times-without-meta-request');
    // 同一快照跨日时须补日期；窄屏允许工具组换行，时间本身不得截断。
    const boundary=await context.newPage();await boundary.clock.setFixedTime(new Date('2026-10-16T12:00:00+08:00'));await boundary.goto(origin);
    await boundary.waitForFunction(()=>document.getElementById('asof-t').textContent.includes('10-15 12:00:05'));
    for(const width of [1440,700,520,390,320]){
      await boundary.setViewportSize({width,height:844});
      const layout=await boundary.evaluate(()=>{const n=document.querySelector('.asof'),t=document.querySelector('.global-tools');return {timeFits:n.scrollWidth<=n.clientWidth,toolsFit:t.scrollWidth<=t.clientWidth,height:n.getBoundingClientRect().height,text:n.innerText};});
      assert(layout.timeFits&&layout.toolsFit&&layout.height<=36,'cross-day compact header fits at '+width+': '+JSON.stringify(layout));
      if(width===390)await boundary.screenshot({path:path.join(output,'header-cross-day-mobile.png'),clip:{x:0,y:0,width,height:230}});
    }
    await boundary.close();pass('cross-day-header-keeps-date-and-fits-narrow-widths');
    dashDelay=500;dashFail=true;
    await page.locator('#range-seg button[data-v="5"]').click();await page.locator('#shot-trigger').click();assert(await page.locator('#shot-modal').isHidden());
    await page.waitForFunction(()=>document.querySelector('#page-dash').getAttribute('data-refreshing')!=='true');await page.locator('#shot-trigger').click();assert(await page.locator('#shot-modal').isHidden());pass('loading-and-failed-new-range-block-export');
    assert.deepEqual(errors,[]);assert.equal(await page.locator('iframe').count(),0);pass('no-page-errors-or-leaked-frames');
    report.pass=true;
  }finally{fs.writeFileSync(path.join(output,'report.json'),JSON.stringify(report,null,2));await browser.close();await new Promise(r=>server.close(r));}
  console.log(JSON.stringify(report));
}
main().catch(e=>{console.error(e);process.exitCode=1;});
