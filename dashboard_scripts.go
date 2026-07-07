package main

const dashboardScripts = `
const managementApi='/v0/management/plugins/__PLUGIN_ID__/summary';
const managementExportApi='/v0/management/plugins/__PLUGIN_ID__/export';
const managementAutobanReleaseApi='/v0/management/plugins/__PLUGIN_ID__/autobans/release';
const managementAuthFilesApi='/v0/management/auth-files';
const managementAuthFieldsApi='/v0/management/auth-files/fields';
const managementCodexAuthUrlApi='/v0/management/codex-auth-url';
const managementAuthStatusApi='/v0/management/get-auth-status';
const keyEl=document.getElementById('key');
const languageEl=document.getElementById('language');
const batchProxyModal=document.getElementById('batch-proxy-modal');
const batchProxyUrlEl=document.getElementById('batch-proxy-url');
const batchProxyStatusEl=document.getElementById('batch-proxy-status');
const invalidAuthModal=document.getElementById('invalid-auth-modal');
const invalidAuthStatusEl=document.getElementById('invalid-auth-status');
const invalidAuthOAuthUrlEl=document.getElementById('invalid-auth-oauth-url');
const workspaceDeactivatedModal=document.getElementById('workspace-deactivated-modal');
const workspaceDeactivatedStatusEl=document.getElementById('workspace-deactivated-status');
const autobanReleaseModal=document.getElementById('autoban-release-modal');
const autobanReleaseStatusEl=document.getElementById('autoban-release-status');
const logExportModal=document.getElementById('log-export-modal');
const logExportStatusEl=document.getElementById('log-export-status-text');
const cpaStoragePrefix='enc::v1::';
const cpaSecureStorageSalt='cli-proxy-api-webui::secure-storage';
let lastData=null;
let accountPage=1;
let accountPageSize=25;
let autobanPage=1;
let autobanPageSize=10;
let invalidAuthPage=1;
const invalidAuthPageSize=10;
let invalidAuthSelected=new Set();
let invalidAuthOAuthTimer=null;
let invalidAuthOAuthKey='';
let workspaceDeactivatedPage=1;
const workspaceDeactivatedPageSize=10;
let workspaceDeactivatedSelected=new Set();
let autobanReleasePage=1;
const autobanReleasePageSize=10;
let autobanReleaseSelected=new Set();
let invalidAuthDeleting=false;
let workspaceDeactivatedDeleting=false;
let autobanReleaseBusy=false;
let activePage='codex';
let selectedProviders=[];
let providerSelectionSaved=false;
let currentLang=effectiveLanguage();
let loading=false;
let summaryAbortController=null;
let summaryStaleRefreshTimer=null;
const summaryWindowCache=new Map();
const saved=managementKey(); if(saved) keyEl.value=saved;
selectedProviders=loadSelectedProviders();
initLanguageControl();
initBatchProxyControl();
initInvalidAuthControl();
initWorkspaceDeactivatedControl();
initAutobanReleaseControl();
initLogExportControl();
initUIPreferences();
applyHostTheme();
observeHostTheme();
document.getElementById('refresh').onclick=load;
document.getElementById('window').onchange=e=>{safeStorageSet(safeLocalStorage(),'cpa_token_usage_window',e.target.value);showCachedSummaryForWindow(e.target.value);load(false,false,{keepExisting:true,abortPrevious:true})};
document.getElementById('export-logs').onclick=openLogExportModal;
document.getElementById('tab-strip').addEventListener('click',e=>{const btn=e.target.closest('.tab[data-target]');if(btn)switchPage(btn.dataset.target)});
document.getElementById('provider-picker-button').onclick=()=>document.getElementById('provider-picker').classList.toggle('open');
document.addEventListener('click',e=>{const picker=document.getElementById('provider-picker');if(!picker.contains(e.target))picker.classList.remove('open')});
document.getElementById('account-filter').oninput=()=>{accountPage=1;renderAccounts()};
document.getElementById('account-sort').onchange=e=>{safeStorageSet(safeLocalStorage(),'cpa_token_usage_account_sort',e.target.value);accountPage=1;renderAccounts()};
document.getElementById('account-page-size').onchange=(e)=>{accountPageSize=Number(e.target.value)||25;safeStorageSet(safeLocalStorage(),'cpa_token_usage_account_page_size',String(accountPageSize));accountPage=1;renderAccounts()};
document.getElementById('account-prev').onclick=()=>{accountPage=Math.max(1,accountPage-1);renderAccounts()};
document.getElementById('account-next').onclick=()=>{accountPage=accountPage+1;renderAccounts()};
document.getElementById('autoban-page-size').onchange=(e)=>{autobanPageSize=Number(e.target.value)||10;safeStorageSet(safeLocalStorage(),'cpa_token_usage_autoban_page_size',String(autobanPageSize));autobanPage=1;renderAutobans((lastData&&lastData.autobans)||[])};
document.getElementById('autoban-prev').onclick=()=>{autobanPage=Math.max(1,autobanPage-1);renderAutobans((lastData&&lastData.autobans)||[])};
document.getElementById('autoban-next').onclick=()=>{autobanPage=autobanPage+1;renderAutobans((lastData&&lastData.autobans)||[])};
setInterval(()=>{if(!document.hidden&&!loading&&!document.getElementById('provider-picker').classList.contains('open'))load()},15000);
function initUIPreferences(){
  const savedWindow=safeStorageGet(safeLocalStorage(),'cpa_token_usage_window'); if(savedWindow&&selectHasValue('window',savedWindow))document.getElementById('window').value=savedWindow;
  const savedSort=safeStorageGet(safeLocalStorage(),'cpa_token_usage_account_sort'); if(savedSort&&selectHasValue('account-sort',savedSort))document.getElementById('account-sort').value=savedSort;
  const savedPage=Number(safeStorageGet(safeLocalStorage(),'cpa_token_usage_account_page_size')||25); if(savedPage){accountPageSize=savedPage;document.getElementById('account-page-size').value=String(savedPage)}
  const savedAutobanPage=Number(safeStorageGet(safeLocalStorage(),'cpa_token_usage_autoban_page_size')||10); if(savedAutobanPage&&selectHasValue('autoban-page-size',String(savedAutobanPage))){autobanPageSize=savedAutobanPage;document.getElementById('autoban-page-size').value=String(savedAutobanPage)}
  initAccountColumns();
}
function selectHasValue(id,value){return Array.from(document.getElementById(id).options||[]).some(o=>o.value===value)}
function accountColumnsKey(){return 'cpa_token_usage_account_columns'}
function loadAccountColumns(){try{return JSON.parse(safeStorageGet(safeLocalStorage(),accountColumnsKey())||'{}')}catch(e){return {}}}
function saveAccountColumns(values){safeStorageSet(safeLocalStorage(),accountColumnsKey(),JSON.stringify(values))}
function initAccountColumns(){
  const values=loadAccountColumns();
  document.querySelectorAll('#account-columns input[data-col]').forEach(input=>{
    if(Object.prototype.hasOwnProperty.call(values,input.dataset.col))input.checked=!!values[input.dataset.col];
    input.onchange=()=>{const next=loadAccountColumns();next[input.dataset.col]=input.checked;saveAccountColumns(next);applyAccountColumns()};
  });
  applyAccountColumns();
}
function applyAccountColumns(){
  const values=loadAccountColumns();
  document.querySelectorAll('#account-columns input[data-col]').forEach(input=>{
    const on=Object.prototype.hasOwnProperty.call(values,input.dataset.col)?!!values[input.dataset.col]:input.checked;
    input.checked=on;
    document.body.classList.toggle('hide-account-'+input.dataset.col,!on);
  });
}
function currentLogExportScope(){
  if(activePage==='codex')return {scope:'codex',provider:''};
  if(activePage==='providers')return {scope:'providers',provider:''};
  const page=document.querySelector('.tab[data-target="'+activePage+'"]');
  return {scope:'provider',provider:page?firstText(page.childNodes[0]&&page.childNodes[0].nodeValue,page.textContent).replace(/\\s*[0-9.,KMB]+$/,''):''};
}
function initLogExportControl(){
  document.getElementById('log-export-close').onclick=closeLogExportModal;
  document.getElementById('log-export-close-bottom').onclick=closeLogExportModal;
  document.getElementById('log-export-apply').onclick=downloadLogExport;
  logExportModal.addEventListener('click',e=>{if(e.target===logExportModal)closeLogExportModal()});
  document.addEventListener('keydown',e=>{if(e.key==='Escape'&&!logExportModal.hidden)closeLogExportModal()});
}
function openLogExportModal(){
  populateLogExportFilters();
  setLogExportStatus('按当前页面范围导出请求日志。','');
  logExportModal.hidden=false;
  setTimeout(()=>document.getElementById('log-export-account').focus(),0);
}
function closeLogExportModal(){logExportModal.hidden=true}
function setLogExportStatus(text,tone){logExportStatusEl.classList.remove('ok','warn','bad');if(tone)logExportStatusEl.classList.add(tone);logExportStatusEl.textContent=tr(text)}
function populateSelect(id,values,allLabel){
  const el=document.getElementById(id);
  const current=el.value;
  const opts=['<option value="">'+esc(tr(allLabel))+'</option>'].concat([...new Set(values.filter(Boolean))].sort((a,b)=>a.localeCompare(b)).map(v=>'<option value="'+esc(v)+'">'+esc(v)+'</option>'));
  el.innerHTML=opts.join('');
  if(values.includes(current))el.value=current;
}
function logExportContextRows(){
  const data=lastData||{};
  const ctx=currentLogExportScope();
  if(ctx.scope==='codex')return {accounts:data.accounts||[],recent:data.recent||[],providers:[],models:data.models||[]};
  if(ctx.scope==='provider'){
    const name=ctx.provider;
    return {
      accounts:(data.key_summaries||[]).filter(r=>(r.provider_names||r.provider||'').includes(name)),
      recent:(data.provider_recent||[]).filter(r=>providerEquals(r,name)),
      providers:(data.providers||[]).filter(r=>providerEquals(r,name)),
      models:(data.provider_models||[]).filter(r=>providerEquals(r,name))
    };
  }
  return {accounts:data.key_summaries||[],recent:data.provider_recent||[],providers:data.providers||[],models:data.provider_models||[]};
}
function populateLogExportFilters(){
  const rows=logExportContextRows();
  populateSelect('log-export-account',rows.accounts.map(r=>firstText(r.key_id,r.auth_file,r.auth_index,r.email,r.source,r.auth_id,r.name)), '全部账号');
  populateSelect('log-export-provider',rows.providers.map(r=>r.provider).concat(rows.recent.map(r=>r.provider)), '全部接入点');
  populateSelect('log-export-model',rows.models.map(r=>r.model).concat(rows.recent.map(r=>firstText(r.alias,r.model))), '全部模型');
  const ctx=currentLogExportScope();
  if(ctx.scope==='provider'&&ctx.provider)document.getElementById('log-export-provider').value=ctx.provider;
}
async function downloadLogExport(){
  const ctx=currentLogExportScope();
  const format=document.getElementById('log-export-format').value||'csv';
  const params=new URLSearchParams();
  params.set('window',document.getElementById('window').value);
  params.set('limit','20000');
  params.set('type','logs');
  params.set('format',format);
  params.set('scope',ctx.scope);
  const provider=firstText(document.getElementById('log-export-provider').value,ctx.provider);
  if(provider)params.set('provider',provider);
  for(const pair of [['account','log-export-account'],['date','log-export-date'],['model','log-export-model'],['status','log-export-status']]){
    const value=document.getElementById(pair[1]).value;
    if(value)params.set(pair[0],value);
  }
  const key=managementKey();
  if(!key){showFallbackKeyInput();setLogExportStatus('请填写备用 CPA 管理密钥后导出。','warn');return}
  safeStorageSet(safeSessionStorage(),'cpa_token_usage_key',key);
  setLogExportStatus('正在导出日志...','');
  const res=await fetch(managementExportApi+'?'+params.toString(),{headers:{Authorization:'Bearer '+key}});
  if(!res.ok){
    if(res.status===401)rejectManagementKey(key);
    setLogExportStatus('导出失败：'+res.status,'bad');
    return;
  }
  const blob=await res.blob();
  const url=URL.createObjectURL(blob);
  const a=document.createElement('a');
  const date=document.getElementById('log-export-date').value||document.getElementById('window').value;
  a.href=url; a.download='codex-token-usage-logs-'+ctx.scope+'-'+date+'.'+format; document.body.appendChild(a); a.click(); a.remove();
  URL.revokeObjectURL(url);
  setLogExportStatus('导出完成。','ok');
}
function languageStorageKey(){return 'cpa_token_usage_language'}
function safeLocalStorage(){try{return window.localStorage}catch(e){return null}}
function safeSessionStorage(){try{return window.sessionStorage}catch(e){return null}}
function safeStorageGet(storage,key){try{return storage?storage.getItem(key):null}catch(e){return null}}
function safeStorageSet(storage,key,value){try{if(storage)storage.setItem(key,value)}catch(e){}}
function safeStorageRemove(storage,key){try{if(storage)storage.removeItem(key)}catch(e){}}
function languageMode(){
  const mode=safeStorageGet(safeLocalStorage(),languageStorageKey())||safeStorageGet(safeSessionStorage(),languageStorageKey())||(languageEl&&languageEl.value)||'zh';
  return mode==='en'?'en':'zh';
}
function effectiveLanguage(){
  return languageMode();
}
function initLanguageControl(){
  if(!languageEl)return;
  languageEl.value=languageMode();
  languageEl.onchange=()=>{
    const value=languageEl.value||'zh';
    safeStorageSet(safeLocalStorage(),languageStorageKey(),value);safeStorageSet(safeSessionStorage(),languageStorageKey(),value);
    refreshLanguage(true);
  };
}
function initBatchProxyControl(){
  document.getElementById('batch-proxy-open').onclick=openBatchProxyModal;
  document.getElementById('batch-proxy-close').onclick=closeBatchProxyModal;
  document.getElementById('batch-proxy-clear').onclick=clearBatchProxy;
  document.getElementById('batch-proxy-preview').onclick=previewBatchProxyTargets;
  document.getElementById('batch-proxy-apply').onclick=applyBatchProxy;
  batchProxyModal.addEventListener('click',e=>{if(e.target===batchProxyModal)closeBatchProxyModal()});
  document.addEventListener('keydown',e=>{if(e.key==='Escape'&&!batchProxyModal.hidden)closeBatchProxyModal()});
}
function initInvalidAuthControl(){
  document.getElementById('invalid-auth-card').onclick=openInvalidAuthModal;
  document.getElementById('invalid-auth-close').onclick=closeInvalidAuthModal;
  document.getElementById('invalid-auth-close-bottom').onclick=closeInvalidAuthModal;
  document.getElementById('invalid-auth-refresh').onclick=async()=>{setInvalidAuthStatus('正在刷新 401 账号...','');await load(true,true);renderInvalidAuthModal()};
  document.getElementById('invalid-auth-delete-all').onclick=deleteAllInvalidAuths;
  document.getElementById('invalid-auth-select-page').onclick=selectCurrentInvalidAuthPage;
  document.getElementById('invalid-auth-delete-selected').onclick=deleteSelectedInvalidAuths;
  document.getElementById('invalid-auth-prev').onclick=()=>{invalidAuthPage=Math.max(1,invalidAuthPage-1);renderInvalidAuthModal()};
  document.getElementById('invalid-auth-next').onclick=()=>{invalidAuthPage=invalidAuthPage+1;renderInvalidAuthModal()};
  document.getElementById('invalid-auth-list').addEventListener('click',handleInvalidAuthListClick);
  invalidAuthOAuthUrlEl.addEventListener('click',handleInvalidAuthOAuthLinkClick);
  invalidAuthModal.addEventListener('click',e=>{if(e.target===invalidAuthModal)closeInvalidAuthModal()});
  document.addEventListener('keydown',e=>{if(e.key==='Escape'&&!invalidAuthModal.hidden)closeInvalidAuthModal()});
}
function initWorkspaceDeactivatedControl(){
  document.getElementById('workspace-deactivated-card').onclick=openWorkspaceDeactivatedModal;
  document.getElementById('workspace-deactivated-close').onclick=closeWorkspaceDeactivatedModal;
  document.getElementById('workspace-deactivated-close-bottom').onclick=closeWorkspaceDeactivatedModal;
  document.getElementById('workspace-deactivated-refresh').onclick=async()=>{setWorkspaceDeactivatedStatus('正在刷新 402 账号...','');await load(true,true);renderWorkspaceDeactivatedModal()};
  document.getElementById('workspace-deactivated-delete-all').onclick=deleteAllWorkspaceDeactivatedAuths;
  document.getElementById('workspace-deactivated-select-page').onclick=selectCurrentWorkspaceDeactivatedPage;
  document.getElementById('workspace-deactivated-delete-selected').onclick=deleteSelectedWorkspaceDeactivatedAuths;
  document.getElementById('workspace-deactivated-prev').onclick=()=>{workspaceDeactivatedPage=Math.max(1,workspaceDeactivatedPage-1);renderWorkspaceDeactivatedModal()};
  document.getElementById('workspace-deactivated-next').onclick=()=>{workspaceDeactivatedPage=workspaceDeactivatedPage+1;renderWorkspaceDeactivatedModal()};
  document.getElementById('workspace-deactivated-list').addEventListener('click',handleWorkspaceDeactivatedListClick);
  workspaceDeactivatedModal.addEventListener('click',e=>{if(e.target===workspaceDeactivatedModal)closeWorkspaceDeactivatedModal()});
  document.addEventListener('keydown',e=>{if(e.key==='Escape'&&!workspaceDeactivatedModal.hidden)closeWorkspaceDeactivatedModal()});
}
function initAutobanReleaseControl(){
  document.getElementById('autoban-release-card').onclick=openAutobanReleaseModal;
  document.getElementById('autoban-release-close').onclick=closeAutobanReleaseModal;
  document.getElementById('autoban-release-close-bottom').onclick=closeAutobanReleaseModal;
  document.getElementById('autoban-release-refresh').onclick=async()=>{setAutobanReleaseStatus('正在刷新 429 账号...','');await load(true,true);renderAutobanReleaseModal()};
  document.getElementById('autoban-release-all').onclick=releaseAllAutobans;
  document.getElementById('autoban-release-select-page').onclick=selectCurrentAutobanReleasePage;
  document.getElementById('autoban-release-selected').onclick=releaseSelectedAutobans;
  document.getElementById('autoban-release-prev').onclick=()=>{autobanReleasePage=Math.max(1,autobanReleasePage-1);renderAutobanReleaseModal()};
  document.getElementById('autoban-release-next').onclick=()=>{autobanReleasePage=autobanReleasePage+1;renderAutobanReleaseModal()};
  document.getElementById('autoban-release-list').addEventListener('click',handleAutobanReleaseListClick);
  autobanReleaseModal.addEventListener('click',e=>{if(e.target===autobanReleaseModal)closeAutobanReleaseModal()});
  document.addEventListener('keydown',e=>{if(e.key==='Escape'&&!autobanReleaseModal.hidden)closeAutobanReleaseModal()});
}
function openBatchProxyModal(){
  setBatchProxyStatus('等待输入代理地址。','');
  batchProxyModal.hidden=false;
  setTimeout(()=>batchProxyUrlEl.focus(),0);
}
function closeBatchProxyModal(){batchProxyModal.hidden=true}
function openInvalidAuthModal(){
  invalidAuthPage=1;
  invalidAuthOAuthUrlEl.hidden=true;
  renderInvalidAuthModal();
  invalidAuthModal.hidden=false;
  setTimeout(()=>document.getElementById('invalid-auth-delete-selected').focus(),0);
}
function closeInvalidAuthModal(){invalidAuthModal.hidden=true}
function openWorkspaceDeactivatedModal(){
  workspaceDeactivatedPage=1;
  renderWorkspaceDeactivatedModal();
  workspaceDeactivatedModal.hidden=false;
  setTimeout(()=>document.getElementById('workspace-deactivated-delete-selected').focus(),0);
}
function closeWorkspaceDeactivatedModal(){workspaceDeactivatedModal.hidden=true}
function openAutobanReleaseModal(){
  autobanReleasePage=1;
  renderAutobanReleaseModal();
  autobanReleaseModal.hidden=false;
  setTimeout(()=>document.getElementById('autoban-release-selected').focus(),0);
}
function closeAutobanReleaseModal(){autobanReleaseModal.hidden=true}
function batchProxyKey(){
  const key=managementKey();
  if(key){keyEl.value=key;safeStorageSet(safeSessionStorage(),'cpa_token_usage_key',key)}
  return key;
}
function showFallbackKeyInput(){
  keyEl.classList.add('on');
  keyEl.focus();
}
function missingBatchProxyKey(){
  showFallbackKeyInput();
  setBatchProxyStatus('请在页面顶部填写 CPA 管理密钥后重试。','warn');
}
function missingInvalidAuthKey(){
  showFallbackKeyInput();
  setInvalidAuthStatus('请在页面顶部填写 CPA 管理密钥后重试。','warn');
}
function managementKey(){
  const typed=firstText(keyEl.value);
  const rejected=recentRejectedManagementKey();
  let key=typed;
  if(!key){
    for(const candidate of [cpaStoredManagementKey(),safeStorageGet(safeSessionStorage(),'cpa_token_usage_key')]){
      const value=firstText(candidate);
      if(value&&value!==rejected){key=value;break}
    }
  }
  if(key){
    keyEl.value=key;
    safeStorageSet(safeSessionStorage(),'cpa_token_usage_key',key);
    safeStorageRemove(safeSessionStorage(),'cpa_token_usage_rejected_key');
    safeStorageRemove(safeSessionStorage(),'cpa_token_usage_rejected_at');
    keyEl.classList.remove('on');
  }
  return key;
}
function recentRejectedManagementKey(){
  const storage=safeSessionStorage();
  const key=safeStorageGet(storage,'cpa_token_usage_rejected_key')||'';
  if(!key)return '';
  const ts=Number(safeStorageGet(storage,'cpa_token_usage_rejected_at')||0);
  if(!ts||Date.now()-ts>5*60*1000){
    safeStorageRemove(storage,'cpa_token_usage_rejected_key');
    safeStorageRemove(storage,'cpa_token_usage_rejected_at');
    return '';
  }
  return key;
}
function rejectManagementKey(key){
  if(key)safeStorageSet(safeSessionStorage(),'cpa_token_usage_rejected_key',key);
  if(key)safeStorageSet(safeSessionStorage(),'cpa_token_usage_rejected_at',String(Date.now()));
  safeStorageRemove(safeSessionStorage(),'cpa_token_usage_key');
  keyEl.value='';
  showFallbackKeyInput();
}
function cpaStoredManagementKey(){
  return firstText(
    readCPAStorageValue(safeSessionStorage(),'managementKey'),
    readCPAStorageValue(safeLocalStorage(),'managementKey'),
    readCPAAuthStoreKey(safeSessionStorage()),
    readCPAAuthStoreKey(safeLocalStorage())
  );
}
function readCPAAuthStoreKey(storage){
  const auth=readCPAStorageValue(storage,'cli-proxy-auth');
  if(!auth||typeof auth!=='object')return '';
  return firstText(auth.state&&auth.state.managementKey,auth.managementKey);
}
function readCPAStorageValue(storage,name){
  if(!storage)return null;
  const raw=readStorageText(storage,name);
  if(!raw)return null;
  const decoded=decodeCPAStorage(raw);
  return parseStoredValue(decoded||raw);
}
function readStorageText(storage,name){
  try{return String(storage.getItem(name)||'').trim()}catch(e){return ''}
}
function parseStoredValue(value){
  if(!value)return null;
  try{return JSON.parse(value)}catch(e){return value}
}
function decodeCPAStorage(value){
  if(!value||!value.startsWith(cpaStoragePrefix))return value;
  try{
    const data=Uint8Array.from(atob(value.slice(cpaStoragePrefix.length)),c=>c.charCodeAt(0));
    const key=new TextEncoder().encode(cpaSecureStorageSalt+'|'+window.location.host+'|'+navigator.userAgent);
    const out=new Uint8Array(data.length);
    for(let i=0;i<data.length;i++)out[i]=data[i]^key[i%key.length];
    return new TextDecoder().decode(out);
  }catch(e){return ''}
}
function setBatchProxyStatus(text,tone){
  batchProxyStatusEl.textContent=tr(text);
  batchProxyStatusEl.classList.remove('ok','warn','bad');
  if(tone)batchProxyStatusEl.classList.add(tone);
}
function setInvalidAuthStatus(text,tone){
  invalidAuthStatusEl.textContent=tr(text);
  invalidAuthStatusEl.classList.remove('ok','warn','bad');
  if(tone)invalidAuthStatusEl.classList.add(tone);
}
function setWorkspaceDeactivatedStatus(text,tone){
  workspaceDeactivatedStatusEl.textContent=tr(text);
  workspaceDeactivatedStatusEl.classList.remove('ok','warn','bad');
  if(tone)workspaceDeactivatedStatusEl.classList.add(tone);
}
function setAutobanReleaseStatus(text,tone){
  autobanReleaseStatusEl.textContent=tr(text);
  autobanReleaseStatusEl.classList.remove('ok','warn','bad');
  if(tone)autobanReleaseStatusEl.classList.add(tone);
}
function setAuthDeleteProgress(el,text,tone,percent){
  const pct=Math.max(0,Math.min(100,Number(percent)||0));
  el.classList.remove('ok','warn','bad');
  if(tone)el.classList.add(tone);
  el.innerHTML='<div class="modal-progress-status"><span>'+esc(tr(text))+'</span><b>'+pct+'%</b></div><div class="modal-progress" aria-hidden="true"><span style="width:'+pct+'%"></span></div>';
}
function setAuthDeleteButtonsDisabled(prefix,disabled){
  ['delete-all','delete-selected','all','selected','select-page','refresh','prev','next','close','close-bottom'].forEach(suffix=>{
    const el=document.getElementById(prefix+'-'+suffix);
    if(el)el.disabled=disabled;
  });
}
function setAuthDeleteBusy(prefix,busy){
  if(prefix==='invalid-auth')invalidAuthDeleting=busy;
  if(prefix==='workspace-deactivated')workspaceDeactivatedDeleting=busy;
  if(prefix==='autoban-release')autobanReleaseBusy=busy;
  setAuthDeleteButtonsDisabled(prefix,busy);
  document.querySelectorAll('#'+prefix+'-list button,#'+prefix+'-list input').forEach(el=>{el.disabled=busy});
}
function invalidAuthRows(){
  const accounts=(lastData&&lastData.accounts)||[];
  const invalids=(lastData&&lastData.invalid_auths)||[];
  const out=[];
  const seen=new Set();
  const add=(row,account)=>{
    const merged=Object.assign({},row||{},account||{});
    merged.invalid_auth=true;
    merged.invalid_auth_at=firstText(row&&row.invalidated_at_text,row&&row.invalid_auth_at,account&&account.invalid_auth_at);
    merged.invalid_auth_reason=firstText(row&&row.reason,row&&row.invalid_auth_reason,account&&account.invalid_auth_reason,'401 unauthorized');
    merged.auth_file=firstText(row&&row.auth_file,account&&account.auth_file,row&&row.auth_id,row&&row.auth_index);
    const key=invalidAuthKey(merged);
    if(seen.has(key))return;
    seen.add(key);
    out.push(merged);
  };
  invalids.forEach(row=>{
    const account=accounts.find(acc=>sameAuthIdentity(row,acc));
    add(row,account);
  });
  accounts.filter(r=>r.invalid_auth).forEach(row=>add(row,row));
  out.sort((a,b)=>Date.parse(firstText(b.invalid_auth_at,b.invalidated_at_text,0))-Date.parse(firstText(a.invalid_auth_at,a.invalidated_at_text,0)));
  return out;
}
function workspaceDeactivatedRows(){
  const accounts=(lastData&&lastData.accounts)||[];
  const rows=(lastData&&lastData.workspace_deactivated_auths)||[];
  const out=[];
  const seen=new Set();
  const add=(row,account)=>{
    const merged=Object.assign({},row||{},account||{});
    merged.workspace_deactivated=true;
    merged.workspace_deactivated_at=firstText(row&&row.invalidated_at_text,row&&row.workspace_deactivated_at,account&&account.workspace_deactivated_at);
    merged.workspace_deactivated_reason=firstText(row&&row.reason,row&&row.workspace_deactivated_reason,account&&account.workspace_deactivated_reason,'402 deactivated_workspace');
    merged.auth_file=firstText(row&&row.auth_file,account&&account.auth_file,row&&row.auth_id,row&&row.auth_index);
    const key=workspaceDeactivatedKey(merged);
    if(seen.has(key))return;
    seen.add(key);
    out.push(merged);
  };
  rows.forEach(row=>{
    const account=accounts.find(acc=>sameAuthIdentity(row,acc));
    add(row,account);
  });
  accounts.filter(r=>r.workspace_deactivated).forEach(row=>add(row,row));
  out.sort((a,b)=>Date.parse(firstText(b.workspace_deactivated_at,b.invalidated_at_text,0))-Date.parse(firstText(a.workspace_deactivated_at,a.invalidated_at_text,0)));
  return out;
}
function autobanReleaseRows(){
  return sortAutobansByRemaining(((lastData&&lastData.autobans)||[]).filter(is429Autoban));
}
function sameAuthIdentity(a,b){
  const strictA=strictAuthAliases(a);
  const strictB=strictAuthAliases(b);
  if(strictA.length||strictB.length){
    const set=new Set(strictA);
    return strictB.some(v=>set.has(v));
  }
  const aliases=new Set(accountAliases(a));
  return accountAliases(b).some(v=>aliases.has(v));
}
function accountAliases(r){
  return [r&&r.auth_id,r&&r.auth_index,r&&r.source,r&&r.email,r&&r.name,r&&r.auth_file].flatMap(authAliasVariants).filter(Boolean);
}
function strictAuthAliases(r){
  const values=[r&&r.auth_file,r&&r.chatgpt_account_id];
  ['auth_id','auth_index','source'].forEach(k=>{
    const v=String((r&&r[k])||'').trim();
    if(/\.json$/i.test(fileNameOnly(v)))values.push(v);
  });
  if(r&&r.auth_file&&r.auth_index)values.push(r.auth_index);
  return values.flatMap(authAliasVariants).filter(Boolean);
}
function authAliasVariants(value){
  value=String(value||'').trim().toLowerCase();
  if(!value)return [];
  const slash=value.split(/[\\/]/).pop();
  const out=[value,slash];
  for(const v of [value,slash]){
    if(v.endsWith('.json'))out.push(v.slice(0,-5));
    if(v.endsWith('.cpa.json'))out.push(v.slice(0,-9),v.slice(0,-4));
  }
  return [...new Set(out)];
}
function invalidAuthKey(r){return firstText(r.auth_file,r.auth_id,r.auth_index,r.source,r.email,r.name,'invalid-auth')}
function invalidAuthFileName(r){return fileNameOnly(firstText(r.auth_file,r.auth_id,r.auth_index))}
function workspaceDeactivatedKey(r){return firstText(r.auth_file,r.auth_id,r.auth_index,r.source,r.email,r.name,'workspace-deactivated')}
function workspaceDeactivatedFileName(r){return fileNameOnly(firstText(r.auth_file,r.auth_id,r.auth_index))}
function autobanReleaseKey(r){return firstText(r.auth_file,r.auth_id,r.auth_index,r.source,'autoban-release')}
function fileNameOnly(value){value=String(value||'').trim();if(!value)return '';return value.split(/[\\/]/).pop()}
function fileNameSet(names){return new Set((names||[]).map(fileNameOnly).filter(Boolean))}
function removeAuthFilesFromCurrentData(names){
  if(!lastData)return;
  const removed=fileNameSet(names);
  if(!removed.size)return;
  const keep=row=>!removed.has(fileNameOnly(firstText(row&&row.auth_file,row&&row.auth_id,row&&row.auth_index)));
  if(Array.isArray(lastData.invalid_auths))lastData.invalid_auths=lastData.invalid_auths.filter(keep);
  if(Array.isArray(lastData.workspace_deactivated_auths))lastData.workspace_deactivated_auths=lastData.workspace_deactivated_auths.filter(keep);
  if(Array.isArray(lastData.autobans))lastData.autobans=lastData.autobans.filter(keep);
  if(Array.isArray(lastData.accounts)){
    lastData.accounts=lastData.accounts.filter(row=>keep(row)).map(row=>{
      const next=Object.assign({},row);
      if(removed.has(fileNameOnly(next.auth_file))){
        next.invalid_auth=false;
        next.workspace_deactivated=false;
      }
      return next;
    });
  }
}
function removeAutobansFromCurrentData(rows){
  if(!lastData||!Array.isArray(lastData.autobans))return;
  const removed=new Set((rows||[]).map(autobanReleaseKey).filter(Boolean));
  if(!removed.size)return;
  lastData.autobans=lastData.autobans.filter(r=>!removed.has(autobanReleaseKey(r)));
}
function normalizeEmail(value){return String(value||'').trim().toLowerCase()}
function pagedInvalidAuthRows(){
  const rows=invalidAuthRows();
  const pages=Math.max(1,Math.ceil(rows.length/invalidAuthPageSize));
  invalidAuthPage=Math.max(1,Math.min(invalidAuthPage,pages));
  const start=(invalidAuthPage-1)*invalidAuthPageSize;
  return {rows,pages,start,pageRows:rows.slice(start,start+invalidAuthPageSize)};
}
function renderInvalidAuthModal(){
  const page=pagedInvalidAuthRows();
  const rows=page.rows;
  const rowKeys=new Set(rows.map(invalidAuthKey));
  invalidAuthSelected=new Set([...invalidAuthSelected].filter(k=>rowKeys.has(k)));
  document.getElementById('invalid-auth-summary').textContent='已选 '+invalidAuthSelected.size+' / 共 '+rows.length+' 个';
  document.getElementById('invalid-auth-page-label').textContent=invalidAuthPage+' / '+page.pages;
  document.getElementById('invalid-auth-prev').disabled=invalidAuthPage<=1;
  document.getElementById('invalid-auth-next').disabled=invalidAuthPage>=page.pages;
  document.getElementById('invalid-auth-select-page').disabled=rows.length===0;
  document.getElementById('invalid-auth-delete-all').disabled=rows.length===0;
  document.getElementById('invalid-auth-delete-selected').disabled=invalidAuthSelected.size===0;
  if(invalidAuthDeleting)setAuthDeleteButtonsDisabled('invalid-auth',true);
  if(!rows.length){
    document.getElementById('invalid-auth-list').innerHTML='<div class="invalid-auth-empty">'+tr('当前没有 401 失效账号。')+'</div>';
    setInvalidAuthStatus('当前没有 401 失效账号。','ok');
    return;
  }
  document.getElementById('invalid-auth-list').innerHTML=page.pageRows.map(r=>{
    const key=invalidAuthKey(r);
    const file=invalidAuthFileName(r);
    const checked=invalidAuthSelected.has(key)?' checked':'';
    const loginBusy=invalidAuthOAuthKey===key?' busy':'';
    const disabled=file&&!invalidAuthDeleting?'':' disabled';
    return '<div class="invalid-auth-row'+loginBusy+'" data-key="'+esc(key)+'">'+
      '<label class="invalid-auth-check"><input type="checkbox" data-invalid-check="'+esc(key)+'"'+checked+(invalidAuthDeleting?' disabled':'')+'></label>'+
      '<div class="invalid-auth-main"><b title="'+esc(accountName(r))+'">'+esc(accountName(r))+'</b><span title="'+esc(file||'-')+'">'+esc(file||'非文件型记录')+'</span></div>'+
      '<div class="invalid-auth-meta"><span>'+esc(firstText(r.auth_index,'-'))+'</span><span>'+esc(firstText(r.invalid_auth_at,'-'))+'</span></div>'+
      '<div class="invalid-auth-reason" title="'+esc(r.invalid_auth_reason||'401 unauthorized')+'">'+esc(r.invalid_auth_reason||'401 unauthorized')+'</div>'+
      '<button class="ghost" type="button" data-invalid-login="'+esc(key)+'">OAuth 登录</button>'+
      '<button class="ghost danger-ghost" type="button" data-invalid-delete="'+esc(key)+'"'+disabled+'>删除</button>'+
    '</div>';
  }).join('');
}
function pagedWorkspaceDeactivatedRows(){
  const rows=workspaceDeactivatedRows();
  const pages=Math.max(1,Math.ceil(rows.length/workspaceDeactivatedPageSize));
  workspaceDeactivatedPage=Math.max(1,Math.min(workspaceDeactivatedPage,pages));
  const start=(workspaceDeactivatedPage-1)*workspaceDeactivatedPageSize;
  return {rows,pages,start,pageRows:rows.slice(start,start+workspaceDeactivatedPageSize)};
}
function renderWorkspaceDeactivatedModal(){
  const page=pagedWorkspaceDeactivatedRows();
  const rows=page.rows;
  const rowKeys=new Set(rows.map(workspaceDeactivatedKey));
  workspaceDeactivatedSelected=new Set([...workspaceDeactivatedSelected].filter(k=>rowKeys.has(k)));
  document.getElementById('workspace-deactivated-summary').textContent='已选 '+workspaceDeactivatedSelected.size+' / 共 '+rows.length+' 个';
  document.getElementById('workspace-deactivated-page-label').textContent=workspaceDeactivatedPage+' / '+page.pages;
  document.getElementById('workspace-deactivated-prev').disabled=workspaceDeactivatedPage<=1;
  document.getElementById('workspace-deactivated-next').disabled=workspaceDeactivatedPage>=page.pages;
  document.getElementById('workspace-deactivated-select-page').disabled=rows.length===0;
  document.getElementById('workspace-deactivated-delete-all').disabled=rows.length===0;
  document.getElementById('workspace-deactivated-delete-selected').disabled=workspaceDeactivatedSelected.size===0;
  if(workspaceDeactivatedDeleting)setAuthDeleteButtonsDisabled('workspace-deactivated',true);
  if(!rows.length){
    document.getElementById('workspace-deactivated-list').innerHTML='<div class="invalid-auth-empty">'+tr('当前没有 402 工作区失效账号。')+'</div>';
    setWorkspaceDeactivatedStatus('当前没有 402 工作区失效账号。','ok');
    return;
  }
  document.getElementById('workspace-deactivated-list').innerHTML=page.pageRows.map(r=>{
    const key=workspaceDeactivatedKey(r);
    const file=workspaceDeactivatedFileName(r);
    const checked=workspaceDeactivatedSelected.has(key)?' checked':'';
    const disabled=file&&!workspaceDeactivatedDeleting?'':' disabled';
    return '<div class="invalid-auth-row workspace-deactivated-row" data-key="'+esc(key)+'">'+
      '<label class="invalid-auth-check"><input type="checkbox" data-workspace-check="'+esc(key)+'"'+checked+(workspaceDeactivatedDeleting?' disabled':'')+'></label>'+
      '<div class="invalid-auth-main"><b title="'+esc(accountName(r))+'">'+esc(accountName(r))+'</b><span title="'+esc(file||'-')+'">'+esc(file||'非文件型记录')+'</span></div>'+
      '<div class="invalid-auth-meta"><span>'+esc(firstText(r.auth_index,'-'))+'</span><span>'+esc(firstText(r.workspace_deactivated_at,'-'))+'</span></div>'+
      '<div class="invalid-auth-reason" title="'+esc(r.workspace_deactivated_reason||'402 deactivated_workspace')+'">'+esc(r.workspace_deactivated_reason||'402 deactivated_workspace')+'</div>'+
      '<button class="ghost danger-ghost" type="button" data-workspace-delete="'+esc(key)+'"'+disabled+'>删除</button>'+
    '</div>';
  }).join('');
}
function pagedAutobanReleaseRows(){
  const rows=autobanReleaseRows();
  const pages=Math.max(1,Math.ceil(rows.length/autobanReleasePageSize));
  autobanReleasePage=Math.max(1,Math.min(autobanReleasePage,pages));
  const start=(autobanReleasePage-1)*autobanReleasePageSize;
  return {rows,pages,start,pageRows:rows.slice(start,start+autobanReleasePageSize)};
}
function renderAutobanReleaseModal(){
  const page=pagedAutobanReleaseRows();
  const rows=page.rows;
  const rowKeys=new Set(rows.map(autobanReleaseKey));
  autobanReleaseSelected=new Set([...autobanReleaseSelected].filter(k=>rowKeys.has(k)));
  document.getElementById('autoban-release-summary').textContent='已选 '+autobanReleaseSelected.size+' / 共 '+rows.length+' 个';
  document.getElementById('autoban-release-page-label').textContent=autobanReleasePage+' / '+page.pages;
  document.getElementById('autoban-release-prev').disabled=autobanReleasePage<=1;
  document.getElementById('autoban-release-next').disabled=autobanReleasePage>=page.pages;
  document.getElementById('autoban-release-select-page').disabled=rows.length===0;
  document.getElementById('autoban-release-all').disabled=rows.length===0;
  document.getElementById('autoban-release-selected').disabled=autobanReleaseSelected.size===0;
  if(autobanReleaseBusy)setAuthDeleteButtonsDisabled('autoban-release',true);
  if(!rows.length){
    document.getElementById('autoban-release-list').innerHTML='<div class="invalid-auth-empty">'+tr('当前没有 429 禁用账号。')+'</div>';
    setAutobanReleaseStatus('当前没有 429 禁用账号。','ok');
    return;
  }
  document.getElementById('autoban-release-list').innerHTML=page.pageRows.map(r=>{
    const key=autobanReleaseKey(r);
    const file=fileNameOnly(firstText(r.auth_file,r.auth_id,r.auth_index));
    const checked=autobanReleaseSelected.has(key)?' checked':'';
    const title=[firstText(r.window,'-'),firstText(r.reason,'-'),autobanResetText(r),autobanRemainingText(r)].join(' · ');
    return '<div class="invalid-auth-row autoban-release-row" data-key="'+esc(key)+'">'+
      '<label class="invalid-auth-check"><input type="checkbox" data-autoban-release-check="'+esc(key)+'"'+checked+(autobanReleaseBusy?' disabled':'')+'></label>'+
      '<div class="invalid-auth-main"><b title="'+esc(accountName(r))+'">'+esc(accountName(r))+'</b><span title="'+esc(file||'-')+'">'+esc(file||firstText(r.auth_id,r.source,'-'))+'</span></div>'+
      '<div class="invalid-auth-meta"><span>'+esc(firstText(r.auth_index,'-'))+'</span><span>'+esc(firstText(r.banned_at_text,'-'))+'</span></div>'+
      '<div class="invalid-auth-reason" title="'+esc(title)+'"><b>'+esc(firstText(r.window,'-'))+'</b><span>'+esc(autobanRemainingText(r))+' · '+esc(autobanResetText(r))+'</span></div>'+
      '<button class="ghost danger-ghost" type="button" data-autoban-release-one="'+esc(key)+'"'+(autobanReleaseBusy?' disabled':'')+'>'+esc(tr('解除'))+'</button>'+
    '</div>';
  }).join('');
}
function handleInvalidAuthListClick(e){
  const check=e.target.closest('[data-invalid-check]');
  if(check){
    const key=check.dataset.invalidCheck;
    if(check.checked)invalidAuthSelected.add(key);else invalidAuthSelected.delete(key);
    renderInvalidAuthModal();
    return;
  }
  const login=e.target.closest('[data-invalid-login]');
  if(login){startInvalidAuthOAuth(login.dataset.invalidLogin);return}
  const del=e.target.closest('[data-invalid-delete]');
  if(del){invalidAuthSelected=new Set([del.dataset.invalidDelete]);deleteSelectedInvalidAuths();return}
  const row=e.target.closest('.invalid-auth-row[data-key]');
  if(row){
    const key=row.dataset.key;
    if(invalidAuthSelected.has(key))invalidAuthSelected.delete(key);else invalidAuthSelected.add(key);
    renderInvalidAuthModal();
  }
}
function handleWorkspaceDeactivatedListClick(e){
  const check=e.target.closest('[data-workspace-check]');
  if(check){
    const key=check.dataset.workspaceCheck;
    if(check.checked)workspaceDeactivatedSelected.add(key);else workspaceDeactivatedSelected.delete(key);
    renderWorkspaceDeactivatedModal();
    return;
  }
  const del=e.target.closest('[data-workspace-delete]');
  if(del){workspaceDeactivatedSelected=new Set([del.dataset.workspaceDelete]);deleteSelectedWorkspaceDeactivatedAuths();return}
  const row=e.target.closest('.workspace-deactivated-row[data-key]');
  if(row){
    const key=row.dataset.key;
    if(workspaceDeactivatedSelected.has(key))workspaceDeactivatedSelected.delete(key);else workspaceDeactivatedSelected.add(key);
    renderWorkspaceDeactivatedModal();
  }
}
function handleAutobanReleaseListClick(e){
  const check=e.target.closest('[data-autoban-release-check]');
  if(check){
    const key=check.dataset.autobanReleaseCheck;
    if(check.checked)autobanReleaseSelected.add(key);else autobanReleaseSelected.delete(key);
    renderAutobanReleaseModal();
    return;
  }
  const one=e.target.closest('[data-autoban-release-one]');
  if(one){autobanReleaseSelected=new Set([one.dataset.autobanReleaseOne]);releaseSelectedAutobans();return}
  const row=e.target.closest('.autoban-release-row[data-key]');
  if(row){
    const key=row.dataset.key;
    if(autobanReleaseSelected.has(key))autobanReleaseSelected.delete(key);else autobanReleaseSelected.add(key);
    renderAutobanReleaseModal();
  }
}
function selectedInvalidAuthRows(){
  const selected=invalidAuthSelected;
  return invalidAuthRows().filter(r=>selected.has(invalidAuthKey(r)));
}
function selectedWorkspaceDeactivatedRows(){
  const selected=workspaceDeactivatedSelected;
  return workspaceDeactivatedRows().filter(r=>selected.has(workspaceDeactivatedKey(r)));
}
function selectedAutobanReleaseRows(){
  const selected=autobanReleaseSelected;
  return autobanReleaseRows().filter(r=>selected.has(autobanReleaseKey(r)));
}
function selectCurrentInvalidAuthPage(){
  const page=pagedInvalidAuthRows();
  page.pageRows.forEach(r=>invalidAuthSelected.add(invalidAuthKey(r)));
  renderInvalidAuthModal();
  setInvalidAuthStatus('已全选当前页 401 账号。','ok');
}
function selectCurrentWorkspaceDeactivatedPage(){
  const page=pagedWorkspaceDeactivatedRows();
  page.pageRows.forEach(r=>workspaceDeactivatedSelected.add(workspaceDeactivatedKey(r)));
  renderWorkspaceDeactivatedModal();
  setWorkspaceDeactivatedStatus('已全选当前页 402 账号。','ok');
}
function selectCurrentAutobanReleasePage(){
  const page=pagedAutobanReleaseRows();
  page.pageRows.forEach(r=>autobanReleaseSelected.add(autobanReleaseKey(r)));
  renderAutobanReleaseModal();
  setAutobanReleaseStatus('已全选当前页 429 账号。','ok');
}
function deleteAllInvalidAuths(){
  deleteInvalidAuthRows(invalidAuthRows(),'确认删除所有 401 认证文件？','正在删除所有 401 认证文件...');
}
function deleteAllWorkspaceDeactivatedAuths(){
  deleteWorkspaceDeactivatedRows(workspaceDeactivatedRows(),'确认删除所有 402 认证文件？','正在删除所有 402 认证文件...');
}
function releaseAllAutobans(){
  releaseAutobanRows(autobanReleaseRows(),'确认解除所有 429 禁用账号？','正在解除所有 429 禁用账号...','all429');
}
async function deleteSelectedInvalidAuths(){
  return deleteInvalidAuthRows(selectedInvalidAuthRows(),'确认删除选中的 401 认证文件？','正在删除选中的 401 认证文件...');
}
async function deleteSelectedWorkspaceDeactivatedAuths(){
  return deleteWorkspaceDeactivatedRows(selectedWorkspaceDeactivatedRows(),'确认删除选中的 402 认证文件？','正在删除选中的 402 认证文件...');
}
async function releaseSelectedAutobans(){
  return releaseAutobanRows(selectedAutobanReleaseRows(),'确认解除选中的 429 禁用账号？','正在解除选中的 429 禁用账号...','selected');
}
async function deleteInvalidAuthRows(rows,confirmText,runningText){
  const names=[...new Set(rows.map(invalidAuthFileName).filter(Boolean))];
  if(!names.length){setInvalidAuthStatus('没有可删除的认证文件。','warn');return}
  const key=managementKey();
  if(!key){missingInvalidAuthKey();return}
  if(!confirm(tr(confirmText))){return}
  setAuthDeleteBusy('invalid-auth',true);
  setAuthDeleteProgress(invalidAuthStatusEl,runningText,'',18);
  try{
    const res=await fetch(managementAuthFilesApi,{method:'DELETE',headers:{Authorization:'Bearer '+key,'Content-Type':'application/json',Accept:'application/json'},body:JSON.stringify({names:names})});
    const body=await readResponseBody(res);
    if(!res.ok&&res.status!==207&&!authFileDeleteAlreadyApplied(res,body)){
      if(res.status===401)rejectManagementKey(key);
      throw new Error('HTTP '+res.status+' '+body);
    }
    setAuthDeleteProgress(invalidAuthStatusEl,'删除成功，正在刷新统计...','ok',72);
    invalidAuthSelected.clear();
    removeAuthFilesFromCurrentData(names);
    renderInvalidAuthModal();
    await load(true,true);
    setAuthDeleteBusy('invalid-auth',false);
    renderInvalidAuthModal();
    setInvalidAuthStatus('删除成功，统计已刷新。','ok');
  }catch(e){
    setAuthDeleteBusy('invalid-auth',false);
    renderInvalidAuthModal();
    setInvalidAuthStatus('删除失败：'+e.message,'bad');
  }finally{
    if(invalidAuthDeleting)setAuthDeleteBusy('invalid-auth',false);
  }
}
async function deleteWorkspaceDeactivatedRows(rows,confirmText,runningText){
  const names=[...new Set(rows.map(workspaceDeactivatedFileName).filter(Boolean))];
  if(!names.length){setWorkspaceDeactivatedStatus('没有可删除的认证文件。','warn');return}
  const key=managementKey();
  if(!key){showFallbackKeyInput();setWorkspaceDeactivatedStatus('请在页面顶部填写 CPA 管理密钥后重试。','warn');return}
  if(!confirm(tr(confirmText))){return}
  setAuthDeleteBusy('workspace-deactivated',true);
  setAuthDeleteProgress(workspaceDeactivatedStatusEl,runningText,'',18);
  try{
    const res=await fetch(managementAuthFilesApi,{method:'DELETE',headers:{Authorization:'Bearer '+key,'Content-Type':'application/json',Accept:'application/json'},body:JSON.stringify({names:names})});
    const body=await readResponseBody(res);
    if(!res.ok&&res.status!==207&&!authFileDeleteAlreadyApplied(res,body)){
      if(res.status===401)rejectManagementKey(key);
      throw new Error('HTTP '+res.status+' '+body);
    }
    setAuthDeleteProgress(workspaceDeactivatedStatusEl,'删除成功，正在刷新统计...','ok',72);
    workspaceDeactivatedSelected.clear();
    removeAuthFilesFromCurrentData(names);
    renderWorkspaceDeactivatedModal();
    await load(true,true);
    setAuthDeleteBusy('workspace-deactivated',false);
    renderWorkspaceDeactivatedModal();
    setWorkspaceDeactivatedStatus('删除成功，统计已刷新。','ok');
  }catch(e){
    setAuthDeleteBusy('workspace-deactivated',false);
    renderWorkspaceDeactivatedModal();
    setWorkspaceDeactivatedStatus('删除失败：'+e.message,'bad');
  }finally{
    if(workspaceDeactivatedDeleting)setAuthDeleteBusy('workspace-deactivated',false);
  }
}
async function releaseAutobanRows(rows,confirmText,runningText,scope){
  rows=(rows||[]).filter(is429Autoban);
  if(!rows.length){setAutobanReleaseStatus('没有可解除的 429 禁用账号。','warn');return}
  const key=managementKey();
  if(!key){showFallbackKeyInput();setAutobanReleaseStatus('请在页面顶部填写 CPA 管理密钥后重试。','warn');return}
  if(!confirm(tr(confirmText))){return}
  setAuthDeleteBusy('autoban-release',true);
  setAuthDeleteProgress(autobanReleaseStatusEl,runningText,'',18);
  try{
    const body=scope==='all429'?{scope:'all429'}:{scope:'selected',items:rows.map(r=>({auth_id:firstText(r.auth_id),auth_index:firstText(r.auth_index),source:firstText(r.source),auth_file:firstText(r.auth_file)}))};
    const res=await fetch(managementAutobanReleaseApi,{method:'POST',headers:{Authorization:'Bearer '+key,'Content-Type':'application/json',Accept:'application/json'},body:JSON.stringify(body)});
    const response=await readResponseBody(res);
    if(!res.ok){
      if(res.status===401)rejectManagementKey(key);
      throw new Error('HTTP '+res.status+' '+response);
    }
    let parsed={};
    try{parsed=JSON.parse(response||'{}')}catch(e){}
    setAuthDeleteProgress(autobanReleaseStatusEl,'解除成功，正在刷新统计...','ok',72);
    autobanReleaseSelected.clear();
    removeAutobansFromCurrentData(rows);
    renderAutobanReleaseModal();
    await load(true,true);
    setAuthDeleteBusy('autoban-release',false);
    renderAutobanReleaseModal();
    setAutobanReleaseStatus('解除成功：'+fmt(parsed.released||0)+' 个，跳过 '+fmt(parsed.skipped||0)+' 个，未找到 '+fmt(parsed.not_found||0)+' 个。','ok');
  }catch(e){
    setAuthDeleteBusy('autoban-release',false);
    renderAutobanReleaseModal();
    setAutobanReleaseStatus('解除失败：'+e.message,'bad');
  }finally{
    if(autobanReleaseBusy)setAuthDeleteBusy('autoban-release',false);
  }
}
async function handleInvalidAuthOAuthLinkClick(e){
  const copy=e.target.closest('[data-oauth-copy]');
  if(!copy)return;
  e.preventDefault();
  const url=copy.dataset.oauthCopy||'';
  if(!url)return;
  try{
    await copyText(url);
    setInvalidAuthStatus('授权链接已复制。','ok');
  }catch(err){
    setInvalidAuthStatus('复制失败，请右键复制链接。','warn');
  }
}
async function copyText(text){
  if(navigator.clipboard&&window.isSecureContext){
    await navigator.clipboard.writeText(text);
    return;
  }
  const ta=document.createElement('textarea');
  ta.value=text;
  ta.style.position='fixed';
  ta.style.left='-9999px';
  document.body.appendChild(ta);
  ta.focus();
  ta.select();
  const ok=document.execCommand('copy');
  document.body.removeChild(ta);
  if(!ok)throw new Error('copy failed');
}
function shortOAuthUrl(url){
  try{
    const u=new URL(url);
    return u.hostname+u.pathname;
  }catch(e){
    return 'Codex OAuth 链接';
  }
}
async function startInvalidAuthOAuth(key){
  const row=invalidAuthRows().find(r=>invalidAuthKey(r)===key);
  if(!row){setInvalidAuthStatus('未找到这个 401 账号。','warn');return}
  const management=managementKey();
  if(!management){missingInvalidAuthKey();return}
  const oldFile=invalidAuthFileName(row);
  const oldEmail=normalizeEmail(firstText(row.email,row.source));
  const startedAt=Date.now();
  invalidAuthOAuthKey=key;
  invalidAuthOAuthUrlEl.hidden=true;
  renderInvalidAuthModal();
  try{
    const before=await fetchAuthFilesForBatch(management);
    setInvalidAuthStatus('正在启动 Codex OAuth 登录...','');
    const res=await fetch(managementCodexAuthUrlApi+'?is_webui=true',{headers:{Authorization:'Bearer '+management,Accept:'application/json'}});
    const body=await readResponseBody(res);
    if(!res.ok){
      if(res.status===401)rejectManagementKey(management);
      throw new Error('HTTP '+res.status+' '+body);
    }
    const payload=parseJSONBody(body);
    if(!payload.url||!payload.state)throw new Error('OAuth 启动响应缺少 url 或 state');
    invalidAuthOAuthUrlEl.hidden=false;
    invalidAuthOAuthUrlEl.innerHTML='<div class="oauth-link-row"><span>'+tr('授权链接：')+'</span><a class="oauth-open-link" href="'+esc(payload.url)+'" target="_blank" rel="noopener noreferrer">'+esc(tr('打开登录页'))+'</a><button class="ghost oauth-copy-link" type="button" data-oauth-copy="'+esc(payload.url)+'" title="'+esc(payload.url)+'">'+esc(tr('复制授权链接'))+'</button><code title="'+esc(payload.url)+'">'+esc(shortOAuthUrl(payload.url))+'</code></div>';
    window.open(payload.url,'_blank','noopener,noreferrer');
    setInvalidAuthStatus('已打开 Codex OAuth，等待登录完成...','');
    pollInvalidAuthOAuth(management,payload.state,row,before,startedAt,oldFile,oldEmail);
  }catch(e){
    invalidAuthOAuthKey='';
    renderInvalidAuthModal();
    setInvalidAuthStatus('OAuth 启动失败：'+e.message,'bad');
  }
}
function pollInvalidAuthOAuth(management,state,row,before,startedAt,oldFile,oldEmail){
  if(invalidAuthOAuthTimer)clearInterval(invalidAuthOAuthTimer);
  const deadline=Date.now()+5*60*1000;
  invalidAuthOAuthTimer=setInterval(async()=>{
    try{
      if(Date.now()>deadline)throw new Error('OAuth 等待超时');
      const res=await fetch(managementAuthStatusApi+'?state='+encodeURIComponent(state),{headers:{Authorization:'Bearer '+management,Accept:'application/json'}});
      const body=await readResponseBody(res);
      if(!res.ok){
        if(res.status===401)rejectManagementKey(management);
        throw new Error('HTTP '+res.status+' '+body);
      }
      const data=parseJSONBody(body);
      if(data.status==='wait')return;
      clearInterval(invalidAuthOAuthTimer); invalidAuthOAuthTimer=null; invalidAuthOAuthKey='';
      if(data.status==='error')throw new Error(data.error||'OAuth 失败');
      await handleInvalidAuthOAuthSuccess(management,row,before,startedAt,oldFile,oldEmail);
    }catch(e){
      if(invalidAuthOAuthTimer){clearInterval(invalidAuthOAuthTimer);invalidAuthOAuthTimer=null}
      invalidAuthOAuthKey='';
      renderInvalidAuthModal();
      setInvalidAuthStatus('OAuth 登录失败：'+e.message,'bad');
    }
  },3000);
}
async function handleInvalidAuthOAuthSuccess(management,row,before,startedAt,oldFile,oldEmail){
  const after=await fetchAuthFilesForBatch(management);
  const match=findNewAuthForEmail(before,after,oldEmail,oldFile,startedAt);
  await load();
  renderInvalidAuthModal();
  if(match&&fileNameOnly(match.name)===oldFile){
    setInvalidAuthStatus('OAuth 成功：同名认证文件已更新，401 状态会随刷新解除。','ok');
    return;
  }
  if(match&&oldFile){
    const ok=confirm(tr('已找到相同邮箱的新认证文件，是否删除旧的 401 文件？')+'\\n'+oldFile+' -> '+fileNameOnly(match.name));
    if(ok){
      invalidAuthSelected=new Set([invalidAuthKey(row)]);
      await deleteSelectedInvalidAuths();
      return;
    }
    setInvalidAuthStatus('OAuth 成功：已找到同邮箱新文件，旧文件未删除。','ok');
    return;
  }
  setInvalidAuthStatus('OAuth 成功，但没有找到相同邮箱的新文件；不会删除旧认证文件。','warn');
}
function findNewAuthForEmail(before,after,email,oldFile,startedAt){
  if(!email)return null;
  const beforeMap=new Map((before||[]).map(f=>[fileNameOnly(f.name),authFileTimestamp(f)]));
  const candidates=(after||[]).filter(f=>normalizeEmail(f.email)===email);
  return candidates.find(f=>{
    const name=fileNameOnly(f.name);
    const ts=authFileTimestamp(f);
    return name===oldFile || !beforeMap.has(name) || ts>=startedAt-5000 || ts>(beforeMap.get(name)||0);
  })||null;
}
function authFileTimestamp(f){
  const value=firstText(f&&f.modified,f&&f.modtime,f&&f.created_at,f&&f.updated_at,0);
  const n=Number(value);
  if(Number.isFinite(n)&&n>0)return n>1e12?n:n*1000;
  const parsed=Date.parse(value);
  return Number.isFinite(parsed)?parsed:0;
}
function parseJSONBody(body){try{return JSON.parse(body||'{}')}catch(e){return {}}}
async function readResponseBody(res){const text=await res.text();return text}
function authFileDeleteAlreadyApplied(res,body){
  const text=String(body||'').toLowerCase();
  return !!res&&res.status===404&&(text.includes('auth file not found')||text.includes('auth_file_not_found'));
}
function codexAuthFiles(files){
  return (files||[]).filter(f=>{
    if(f.runtime_only)return false;
    const provider=String(f.provider||f.type||'').trim().toLowerCase();
    return provider==='codex' || provider.includes('codex');
  });
}
async function fetchAuthFilesForBatch(key){
  if(!key)throw new Error('请填写 CPA 管理密钥。');
  const res=await fetch(managementAuthFilesApi,{headers:{Authorization:'Bearer '+key,Accept:'application/json'}});
  if(!res.ok){
    const body=await res.text();
    if(res.status===401)rejectManagementKey(key);
    throw new Error('HTTP '+res.status+' '+body);
  }
  const data=await res.json();
  return codexAuthFiles(data.files||[]);
}
async function previewBatchProxyTargets(){
  const key=batchProxyKey();
  if(!key){missingBatchProxyKey();return}
  setBatchProxyStatus('正在读取 Codex 认证文件...','');
  try{
    const files=await fetchAuthFilesForBatch(key);
    setBatchProxyStatus(files.length?('将写入 '+files.length+' 个 Codex 认证文件。'):'没有找到可写入的 Codex 认证文件。',files.length?'ok':'warn');
  }catch(e){
    setBatchProxyStatus('预览失败：'+e.message,'bad');
  }
}
async function applyBatchProxy(){
  const proxy=(batchProxyUrlEl.value||'').trim();
  const key=batchProxyKey();
  if(!key){missingBatchProxyKey();return}
  if(!proxy){setBatchProxyStatus('请先填写代理地址。','warn');return}
  await writeBatchProxy(proxy,'批量写入完成：成功 ','批量写入失败：','正在写入：成功 ');
}
async function clearBatchProxy(){
  const key=batchProxyKey();
  if(!key){missingBatchProxyKey();return}
  await writeBatchProxy('','清除完成：成功 ','清除失败：','正在清除：成功 ');
}
async function writeBatchProxy(proxy,donePrefix,failPrefix,progressPrefix){
  const key=batchProxyKey();
  const applyBtn=document.getElementById('batch-proxy-apply');
  const previewBtn=document.getElementById('batch-proxy-preview');
  const clearBtn=document.getElementById('batch-proxy-clear');
  applyBtn.disabled=true; previewBtn.disabled=true; clearBtn.disabled=true;
  try{
    setBatchProxyStatus('正在读取 Codex 认证文件...','');
    const files=await fetchAuthFilesForBatch(key);
    if(!files.length){setBatchProxyStatus('没有找到可写入的 Codex 认证文件。','warn');return}
    let ok=0,failed=0;
    for(const file of files){
      const name=String(file.name||file.id||'').trim();
      if(!name){failed++;continue}
      const res=await fetch(managementAuthFieldsApi,{
        method:'PATCH',
        headers:{Authorization:'Bearer '+key,'Content-Type':'application/json',Accept:'application/json'},
        body:JSON.stringify({name:name,proxy_url:proxy})
      });
      if(res.ok){ok++}else{failed++}
      setBatchProxyStatus(progressPrefix+ok+' / 失败 '+failed+' / 总计 '+files.length,'');
    }
    setBatchProxyStatus(donePrefix+ok+' 个，失败 '+failed+' 个。',failed?'warn':'ok');
  }catch(e){
    setBatchProxyStatus(failPrefix+e.message,'bad');
  }finally{
    applyBtn.disabled=false; previewBtn.disabled=false; clearBtn.disabled=false;
  }
}
function syncLanguageControl(){if(languageEl)languageEl.value=languageMode()}
function switchPage(page){
  activePage=page||'codex';
  document.querySelectorAll('.tab[data-target]').forEach(btn=>{
    const on=btn.dataset.target===activePage;
    btn.classList.toggle('active',on);
    btn.setAttribute('aria-selected',on?'true':'false');
  });
  document.querySelectorAll('[data-page]').forEach(el=>el.classList.toggle('page-on',el.dataset.page===activePage));
}
function providerStorageKey(){return 'cpa_token_usage_provider_pages'}
function providerKnownStorageKey(){return 'cpa_token_usage_provider_known'}
function loadSelectedProviders(){const raw=safeStorageGet(safeLocalStorage(),providerStorageKey());providerSelectionSaved=raw!==null;try{return JSON.parse(raw||'[]').filter(Boolean)}catch(e){return []}}
function saveSelectedProviders(){providerSelectionSaved=true;safeStorageSet(safeLocalStorage(),providerStorageKey(),JSON.stringify(selectedProviders))}
function loadKnownProviders(){try{return JSON.parse(safeStorageGet(safeLocalStorage(),providerKnownStorageKey())||'[]').filter(Boolean)}catch(e){return []}}
function saveKnownProviders(names){safeStorageSet(safeLocalStorage(),providerKnownStorageKey(),JSON.stringify(names))}
function providerID(name){return 'provider-'+btoa(unescape(encodeURIComponent(name||'unknown'))).replace(/=+$/,'').replace(/[^a-zA-Z0-9_-]/g,'')}
function providerLabel(name){
  name=String(name||'unknown').trim()||'unknown';
  if(name.startsWith('openai-compatible-'))return name.slice('openai-compatible-'.length)||name;
  if(name.startsWith('openai-compatibility-'))return name.slice('openai-compatibility-'.length)||name;
  if(name.startsWith('openai-compatibility:')){
    const parts=name.split(':');
    if(parts[1])return parts[1];
  }
  return name;
}
function ensureProviderSelection(providers){
  const names=[...new Set(providers.map(p=>providerLabel(p.provider)).filter(Boolean))];
  const valid=new Set(names);
  const known=loadKnownProviders().map(providerLabel).filter(name=>valid.has(name));
  const knownSet=new Set(known);
  selectedProviders=selectedProviders.map(providerLabel).filter(name=>valid.has(name));
  if(!providerSelectionSaved&&!selectedProviders.length){
    selectedProviders=names.slice(0,Math.min(4,names.length));
    saveSelectedProviders();
  }else if(providerSelectionSaved&&known.length===0&&names.length<=8){
    selectedProviders=names;
    saveSelectedProviders();
  }else if(known.length>0){
    const added=names.filter(name=>!knownSet.has(name));
    if(added.length&&selectedProviders.length<8){
      selectedProviders=[...new Set(selectedProviders.concat(added.slice(0,8-selectedProviders.length)))];
      saveSelectedProviders();
    }
  }
  saveKnownProviders(names);
}
function renderProviderPicker(providers){
  ensureProviderSelection(providers);
  const selected=new Set(selectedProviders);
  document.getElementById('provider-picker-button').innerHTML='显示接入点 <span class="tab-count">'+fmt(selectedProviders.length)+'/'+fmt(providers.length)+'</span>';
  document.getElementById('provider-picker-panel').innerHTML=providers.map(p=>{
    const name=providerLabel(p.provider);
    return '<label class="picker-row" title="'+esc(name)+'"><input type="checkbox" data-provider="'+esc(name)+'" '+(selected.has(name)?'checked':'')+'><span class="picker-name">'+esc(name)+'</span><span class="picker-meta">'+compact(p.total_tokens)+' tok</span></label>';
  }).join('') || '<div class="muted" style="padding:6px">暂无其他 AI Provider</div>';
  document.querySelectorAll('#provider-picker-panel input[data-provider]').forEach(input=>input.onchange=e=>{
    const name=e.target.dataset.provider;
    if(e.target.checked){
      if(!selectedProviders.includes(name))selectedProviders.push(name);
    }else{
      selectedProviders=selectedProviders.filter(v=>v!==name);
      if(activePage===providerID(name))switchPage('providers');
    }
    saveSelectedProviders();
    renderProviderTabsAndPages(lastData||{});
  });
}
function renderProviderTabsAndPages(data){
  const providers=data.providers||[];
  renderProviderPicker(providers);
  const providerMap=new Map(providers.map(p=>[providerLabel(p.provider),p]));
  const tabs=selectedProviders.filter(name=>providerMap.has(name)).map(name=>{
    const p=providerMap.get(name);
    return '<button class="tab" data-target="'+providerID(name)+'" role="tab" aria-selected="false" title="'+esc(name)+'">'+esc(name)+'<span class="tab-count">'+compact(p.total_tokens)+'</span></button>';
  }).join('');
  document.getElementById('provider-tabs').innerHTML=tabs;
  document.getElementById('provider-pages').innerHTML=selectedProviders.filter(name=>providerMap.has(name)).map(name=>providerPageHTML(name)).join('');
  selectedProviders.filter(name=>providerMap.has(name)).forEach(name=>renderSingleProviderPage(data,name));
  if(activePage!=='codex'&&activePage!=='providers'&&!document.querySelector('[data-page="'+activePage+'"]'))switchPage('providers');
  switchPage(activePage);
}
function providerPageHTML(name){
  const id=providerID(name);
  return '<section data-page="'+id+'">'+
    '<div class="command-grid">'+
      '<section class="section"><h2><span>'+esc(name)+'</span><span class="mini">独立 Provider 统计，不进入 Codex 账号池</span></h2><div class="section-body"><div class="cards">'+
        '<div class="metric" style="--accent:var(--blue)"><div class="label">请求数</div><div class="value" id="'+id+'-requests">-</div><div class="sub" id="'+id+'-success">成功率 -</div></div>'+
        '<div class="metric" style="--accent:var(--cyan)"><div class="label">总 Token</div><div class="value" id="'+id+'-total">-</div><div class="sub">当前 Provider</div></div>'+
        '<div class="metric" style="--accent:var(--blue)"><div class="label">费用估算</div><div class="value" id="'+id+'-cost">-</div><div class="sub" id="'+id+'-cost-sub">按模型价格估算</div></div>'+
        '<div class="metric" style="--accent:var(--cyan)"><div class="label">输入 Token</div><div class="value" id="'+id+'-input">-</div><div class="sub" id="'+id+'-input-share">占比 -</div></div>'+
        '<div class="metric" style="--accent:var(--violet)"><div class="label">输出 Token</div><div class="value" id="'+id+'-output">-</div><div class="sub" id="'+id+'-output-share">占比 -</div></div>'+
        '<div class="metric" style="--accent:var(--orange)"><div class="label">缓存 Token</div><div class="value" id="'+id+'-cache">-</div><div class="sub" id="'+id+'-cache-share">缓存率 -</div></div>'+
        '<div class="metric" style="--accent:var(--red)"><div class="label">429 次数</div><div class="value bad" id="'+id+'-429">-</div><div class="sub">限流次数</div></div>'+
        '<div class="metric" style="--accent:var(--cyan)"><div class="label">模型数</div><div class="value" id="'+id+'-models-count">-</div><div class="sub">此 Provider</div></div>'+
        '<div class="metric" style="--accent:var(--blue)"><div class="label">平均耗时</div><div class="value" id="'+id+'-latency">-</div><div class="sub" id="'+id+'-latency-sub">慢请求 -</div></div>'+
        '<div class="metric" style="--accent:var(--cyan)"><div class="label">首 Token</div><div class="value" id="'+id+'-ttft">-</div><div class="sub" id="'+id+'-ttft-sub">慢首包 -</div></div>'+
        '<div class="metric" style="--accent:var(--violet)"><div class="label">输出速度</div><div class="value" id="'+id+'-throughput">-</div><div class="sub">输出 Token / 秒</div></div>'+
      '</div></div></section>'+
      '<section class="section"><h2><span>Token 结构</span><span class="mini">'+esc(name)+'</span></h2><div class="section-body"><div class="mix" id="'+id+'-mix"></div></div></section>'+
    '</div>'+
    '<section class="section" style="margin-top:8px"><h2><span>模型排行</span><span class="mini">'+esc(name)+'</span></h2><div class="scroll model-table-wrap"><table><thead><tr><th>模型</th><th>别名</th><th>Provider</th><th>请求</th><th>总 Token</th><th>费用</th><th>性能</th><th>输入</th><th>输出</th><th>缓存</th><th>缓存率</th></tr></thead><tbody id="'+id+'-models"></tbody></table></div></section>'+
    '<section class="section" style="margin-top:8px"><h2><span>最近请求</span><span class="mini">'+esc(name)+' 最近 30 条</span></h2><div class="scroll recent-table-wrap"><table><thead><tr><th>模型</th><th>耗时</th><th>Tokens</th><th>费用</th><th>详情</th></tr></thead><tbody id="'+id+'-recent"></tbody></table></div></section>'+
  '</section>';
}
const i18nEn={
  '按账号聚合 CPA usage：Token 消耗、缓存率、请求健康、5h/7d 额度窗口和最近异常。':'Aggregate CPA usage by account: tokens, cache rate, request health, 5h/7d quota windows, and recent anomalies.',
  '语言':'Language',
  '中文':'Chinese',
  '批量写入代理':'Batch proxy',
  '批量写入 Codex 代理':'Batch write Codex proxy',
  '关闭批量写入代理':'Close batch proxy writer',
  '代理地址':'Proxy URL',
  '管理密钥':'Management key',
  'CPA 管理密钥':'CPA management key',
  '只写入 Codex 认证文件的 ':'Only writes the ',
  ' 字段。填写 ':' field of Codex auth files. Enter ',
  ' 可批量直连，留空不会执行。':' for direct connections; blank input will not run.',
  '等待输入代理地址。':'Waiting for a proxy URL.',
  '清除所有代理':'Clear all proxies',
  '预览数量':'Preview count',
  '应用':'Apply',
  '请填写 CPA 管理密钥。':'Enter the CPA management key.',
  '请在页面顶部填写 CPA 管理密钥后重试。':'Enter the CPA management key at the top of the page, then retry.',
  '正在读取 Codex 认证文件...':'Reading Codex auth files...',
  '将写入 ':'Will write ',
  ' 个 Codex 认证文件。':' Codex auth files.',
  '没有找到可写入的 Codex 认证文件。':'No writable Codex auth files found.',
  '预览失败：':'Preview failed: ',
  '请先填写代理地址。':'Enter the proxy URL first.',
  '正在写入：成功 ':'Writing: success ',
  '正在清除：成功 ':'Clearing: success ',
  ' / 失败 ':' / failed ',
  ' / 总计 ':' / total ',
  '批量写入完成：成功 ':'Batch write complete: success ',
  '清除完成：成功 ':'Clear complete: success ',
  ' 个，失败 ':' files, failed ',
  ' 个。':' files.',
  '批量写入失败：':'Batch write failed: ',
  '清除失败：':'Clear failed: ',
  '管理密钥备用输入':'Fallback management key',
  'CPA 管理密码备用输入':'Fallback CPA management key',
  '统计窗口':'Time window',
  '最近 24 小时':'Last 24 hours',
  '今天':'Today',
  '最近 7 天':'Last 7 days',
  '最近 30 天':'Last 30 days',
  '全部':'All',
  '刷新':'Refresh',
  '导出日志':'Export logs',
  '关闭导出日志':'Close log export',
  '账号':'Account',
  '全部账号':'All accounts',
  '接入点':'Endpoint',
  '全部接入点':'All endpoints',
  '日期':'Date',
  '全部模型':'All models',
  '状态':'Status',
  '全部状态':'All statuses',
  '格式':'Format',
  '按当前页面范围导出请求日志。':'Export request logs for the current page scope.',
  '正在导出日志...':'Exporting logs...',
  '导出完成。':'Export complete.',
  'CPA 多 Key 用量':'CPA multi-key usage',
  '按 CPA 对外 Key 聚合模型、协议和 Token 额度':'Aggregated by CPA external key, model, protocol, and token usage',
  '协议':'Protocol',
  'Token / 费用':'Tokens / cost',
  '暂无 Key 数据':'No key data',
  '统计页面':'Usage pages',
  'Codex 账号池':'Codex accounts',
  'AI 总览':'AI overview',
  '显示接入点':'Show endpoints',
  '导出 CSV':'Export CSV',
  '导出 JSON':'Export JSON',
  '运行总览':'Runtime overview',
  '请求 / Token / 缓存 / 限流':'Requests / tokens / cache / limits',
  '请求数':'Requests',
  '成功率 -':'Success -',
  '总 Token':'Total tokens',
  'Codex 账号池合计':'Codex account pool total',
  '费用估算':'Estimated cost',
  '按模型价格估算':'Based on model prices',
  '输入 Token':'Input tokens',
  '占比 -':'Share -',
  '输出 Token':'Output tokens',
  '缓存 Token':'Cache tokens',
  '缓存率 -':'Cache rate -',
  '429 次数':'429s',
  '限流/额度打满':'Rate limit / quota full',
  '自动禁用':'Auto ban',
  '活跃账号':'Active accounts',
  '可识别账号':'Recognized accounts',
  '7d/月剩余额度':'7d/month remaining quota',
  '按账号额度快照估算':'Estimated from account quota snapshots',
  '额度触发':'Quota trigger',
  '默认关闭':'Off by default',
  'Top 账号占比':'Top account share',
  'Token 集中度':'Token concentration',
  '429 最多':'Most 429s',
  '平均耗时':'Avg latency',
  '慢请求 -':'Slow requests -',
  '首 Token':'First token',
  '慢首包 -':'Slow first token -',
  '输出速度':'Output speed',
  '输出 Token / 秒':'Output tokens / sec',
  '风险洞察':'Risk insights',
  '健康 / 异常 / 集中度':'Health / anomalies / concentration',
  '用量趋势':'Usage trend',
  '请求数 / 总 Token / 输出 Token':'Requests / total tokens / output tokens',
  '请求':'Requests',
  'Token 结构':'Token mix',
  '缓存率 = Cached / Input':'Cache rate = cached / input',
  '模型排行':'Model ranking',
  '仅 Codex 账号池':'Codex account pool only',
  '模型':'Model',
  '别名':'Alias',
  '费用':'Cost',
  '输入':'Input',
  '输出':'Output',
  '缓存':'Cache',
  '缓存率':'Cache rate',
  '账号池运营台':'Account operations',
  '搜索、排序、分页承载大量账号':'Search, sort, and page large account pools',
  '搜索账号、邮箱或 AuthIndex':'Search account, email, or AuthIndex',
  '账号排序方式':'Account sort',
  '按 Token':'By tokens',
  '按费用':'By cost',
  '按 7d/月余量':'By 7d/month remaining',
  '按 7d/月总额度':'By 7d/month total quota',
  '按平均耗时':'By avg latency',
  '按 401 失效':'By 401 invalid',
  '按 402 工作区':'By 402 workspace',
  '按外部消耗':'By external use',
  '按触发状态':'By trigger status',
  '按额度已用':'By quota used',
  '按缓存率':'By cache rate',
  '按 429':'By 429s',
  '按成功率':'By success rate',
  '按最近使用':'By recent use',
  '账号每页数量':'Accounts per page',
  '上一页账号':'Previous account page',
  '上一页':'Previous',
  '下一页账号':'Next account page',
  '下一页':'Next',
  '当前结果':'Current results',
  '费用合计':'Total cost',
  '风险账号':'Risk accounts',
  '401 失效':'401 invalid',
  '402 工作区':'402 workspace',
  '429 禁用':'429 banned',
  '点击管理':'Manage',
  '点击处理':'Resolve',
  '点击解除':'Release',
  '无 401':'No 401',
  '无 402':'No 402',
  '无 429':'No 429',
  '已配置':'Configured',
  '管理 401 失效账号':'Manage 401 invalid accounts',
  '管理 402 工作区失效账号':'Manage 402 deactivated workspace accounts',
  '管理 429 禁用账号':'Manage 429 banned accounts',
  '关闭 401 管理':'Close 401 manager',
  '关闭 402 管理':'Close 402 manager',
  '关闭 429 管理':'Close 429 manager',
  '删除所有 401 账号':'Delete all 401 accounts',
  '删除所有 402 账号':'Delete all 402 accounts',
  '解除所有 429':'Release all 429',
  '已选 0 / 共 0 个':'Selected 0 / 0',
  '等待选择 401 账号。':'Waiting for 401 account selection.',
  '等待选择 402 账号。':'Waiting for 402 account selection.',
  '等待选择 429 账号。':'Waiting for 429 account selection.',
  '全选当前页':'Select current page',
  '删除选中':'Delete selected',
  '解除选中':'Release selected',
  '解除':'Release',
  '管理 401 失效账号':'Manage 401 invalid accounts',
  '疑似外部消耗':'Suspected external use',
  '触发异常':'Trigger issues',
  '额度最高':'Highest quota use',
  '缓存最低':'Lowest cache',
  '账号':'Account',
  '接入点':'Endpoint',
  '成功率':'Success rate',
  '性能':'Performance',
  '总 Token / 费用':'Total tokens / cost',
  '5h 窗口':'5h window',
  '7d/月窗口 / 额度预估':'7d/month window / quota estimate',
  '最近':'Recent',
  '状态':'Status',
  '自动禁用状态':'Auto-ban status',
  '429 按 reset_at 恢复，401 重新登录后解除':'429 recovers at reset_at; 401 clears after re-login',
  '429 按 reset_at 恢复，401/402 处理认证文件后解除':'429 recovers at reset_at; 401/402 clear after credential handling',
  '显示 0 / 0 个自动禁用账号':'Showing 0 / 0 auto-banned accounts',
  '自动禁用每页数量':'Auto-bans per page',
  '上一页自动禁用账号':'Previous auto-ban page',
  '下一页自动禁用账号':'Next auto-ban page',
  '窗口':'Window',
  '原因':'Reason',
  '封禁时间':'Banned at',
  '解禁时间':'Release at',
  '剩余':'Remaining',
  '最近请求':'Recent requests',
  'Codex 最近 30 条':'Latest 30 Codex requests',
  '耗时':'Latency',
  '详情':'Details',
  'AI 接入点总览':'AI endpoint overview',
  '不计入 Codex 账号池价格和额度':'Excluded from Codex account costs and quotas',
  '其他 AI Provider 合计':'Other AI provider total',
  'Provider 限流':'Provider rate limits',
  '模型数':'Models',
  '按模型聚合':'Grouped by model',
  'Top 接入点':'Top endpoint',
  '其他 AI Provider':'Other AI providers',
  'Provider / 接入点总览':'Provider / endpoint overview',
  '按 Provider 名称聚合，不进入 Codex 账号池':'Grouped by provider name, excluded from Codex accounts',
  '账号数':'Accounts',
  '其他 AI Provider 最近 30 条':'Latest 30 other AI provider requests',
  '独立 Provider 统计，不进入 Codex 账号池':'Per-provider stats, excluded from Codex accounts',
  '当前 Provider':'Current provider',
  '限流次数':'Rate limits',
  '此 Provider':'This provider',
  '暂无其他 AI Provider':'No other AI providers',
  '暂无趋势数据':'No trend data',
  '没有匹配的账号。':'No matching accounts.',
  '当前没有被自动禁用的 Codex 账号':'No Codex accounts are currently auto-banned.',
  '当前没有 401 失效账号。':'No 401 invalid accounts.',
  '当前没有 402 工作区失效账号。':'No 402 deactivated workspace accounts.',
  '当前没有 429 禁用账号。':'No 429 banned accounts.',
  '正在刷新 401 账号...':'Refreshing 401 accounts...',
  '正在刷新 402 账号...':'Refreshing 402 accounts...',
  '正在刷新 429 账号...':'Refreshing 429 accounts...',
  '请在页面顶部填写 CPA 管理密钥后重试。':'Enter the CPA management key at the top and retry.',
  '没有可删除的认证文件。':'No credential files can be deleted.',
  '已全选当前页 401 账号。':'Selected the current page of 401 accounts.',
  '已全选当前页 402 账号。':'Selected the current page of 402 accounts.',
  '已全选当前页 429 账号。':'Selected the current page of 429 accounts.',
  '确认删除选中的 401 认证文件？':'Delete the selected 401 credential files?',
  '确认删除所有 401 认证文件？':'Delete all 401 credential files?',
  '正在删除选中的 401 认证文件...':'Deleting selected 401 credential files...',
  '正在删除所有 401 认证文件...':'Deleting all 401 credential files...',
  '确认删除选中的 402 认证文件？':'Delete the selected 402 credential files?',
  '确认删除所有 402 认证文件？':'Delete all 402 credential files?',
  '正在删除选中的 402 认证文件...':'Deleting selected 402 credential files...',
  '正在删除所有 402 认证文件...':'Deleting all 402 credential files...',
  '删除完成，正在刷新统计...':'Delete complete, refreshing stats...',
  '删除成功，正在刷新统计...':'Deleted successfully, refreshing stats...',
  '删除成功，统计已刷新。':'Deleted successfully. Stats refreshed.',
  '删除失败：':'Delete failed: ',
  '没有可解除的 429 禁用账号。':'No releasable 429 banned accounts.',
  '确认解除选中的 429 禁用账号？':'Release the selected 429 banned accounts?',
  '确认解除所有 429 禁用账号？':'Release all 429 banned accounts?',
  '正在解除选中的 429 禁用账号...':'Releasing selected 429 banned accounts...',
  '正在解除所有 429 禁用账号...':'Releasing all 429 banned accounts...',
  '解除成功，正在刷新统计...':'Release succeeded, refreshing stats...',
  '解除失败：':'Release failed: ',
  '未找到这个 401 账号。':'This 401 account was not found.',
  '正在启动 Codex OAuth 登录...':'Starting Codex OAuth login...',
  'OAuth 启动响应缺少 url 或 state':'OAuth start response is missing url or state',
  '授权链接：':'Authorization link:',
  '已打开 Codex OAuth，等待登录完成...':'Codex OAuth opened, waiting for login...',
  'OAuth 启动失败：':'OAuth start failed: ',
  'OAuth 等待超时':'OAuth wait timed out',
  'OAuth 失败':'OAuth failed',
  'OAuth 登录失败：':'OAuth login failed: ',
  '打开登录页':'Open login page',
  '复制授权链接':'Copy auth link',
  '授权链接已复制。':'Authorization link copied.',
  '复制失败，请右键复制链接。':'Copy failed; right-click the link to copy it.',
  'OAuth 成功：同名认证文件已更新，401 状态会随刷新解除。':'OAuth succeeded: the same credential file was updated, and the 401 state will clear after refresh.',
  '已找到相同邮箱的新认证文件，是否删除旧的 401 文件？':'A new credential file with the same email was found. Delete the old 401 file?',
  'OAuth 成功：已找到同邮箱新文件，旧文件未删除。':'OAuth succeeded: same-email new file found; old file was not deleted.',
  'OAuth 成功，但没有找到相同邮箱的新文件；不会删除旧认证文件。':'OAuth succeeded, but no new file with the same email was found; the old credential file will not be deleted.',
  '非文件型记录':'Non-file record',
  '删除或替换认证文件后解除':'Clears after deleting or replacing the credential file',
  '需处理':'Needs action',
  'Team 工作区失效，删除或替换 json 后解除':'Team workspace deactivated; delete or replace the JSON to clear',
  '当前没有工作区失效':'No workspace deactivation',
  '暂无模型数据':'No model data',
  '暂无 Provider 数据':'No provider data',
  '暂无请求记录':'No request records',
  '导出失败：':'Export failed: ',
  '已清除备用管理密钥；页面需要 CPA 管理密钥加载统计。':'Cleared fallback management key. The page needs the CPA management key to load usage data.',
  '请填写备用 CPA 管理密钥后刷新。':'Enter the fallback CPA management key and refresh.',
  '请填写备用 CPA 管理密钥后导出。':'Enter the fallback CPA management key before exporting.',
  '备用管理密钥不正确，已清除临时保存值。':'Fallback management key is incorrect. The temporary value was cleared.',
  '管理接口':'Management API',
  '加载中...':'Loading...',
  '正在更新当前窗口...':'Updating current window...',
  '缓存数据，后台更新中':'Cached data, updating in background',
  '失败':'Failed',
  '优先':'Priority',
  '弹性':'Flex',
  '未捕获重置时间':'Reset time not captured',
  '重置':'Reset',
  '天':'d',
  '小时':'h',
  '分':'m',
  '样本不足':'Insufficient sample',
  '缺价格':'No price',
  '模型价格已覆盖':'Model prices covered',
  '部分模型缺价格':'Some model prices missing',
  '占比':'Share',
  '占总':'Total share',
  '覆盖':'Covers',
  '个账号':'accounts',
  '总额':'Total',
  '等待 7d/月额度快照':'Waiting for 7d/month quota snapshots',
  '运行中':'Running',
  '已开启':'On',
  '已关闭':'Off',
  '模式':'Mode',
  '成功':'success',
  '失败':'failed',
  '跳过':'skipped',
  '默认关闭 · quota 查询不保证启动 5h 窗口':'Off by default · quota query does not guarantee starting the 5h window',
  '显示':'Showing',
  '已加载':'loaded',
  '外部消耗':'External use',
  '触发 OK':'Trigger OK',
  '触发跳过':'Trigger skipped',
  '触发失败':'Trigger failed',
  '正常':'Healthy',
  '已禁用':'Disabled',
  '已过期':'Expired',
  '额度高':'High quota',
  '接近额度':'Near quota',
  '未使用':'Unused',
  '成功率低':'Low success',
  '余':'Remaining',
  '总':'Total',
  '已用':'Used',
  '按当前 ':'Based on current ',
  '窗口已用 Token、额度百分比和最近 quota trigger 快照实时估算':' window tokens, quota percentage, and recent quota trigger snapshots',
  '无窗口 Token':'No window tokens',
  '缓存命中':'Cache hits',
  '流':'Stream',
  '推理':'Reasoning',
  '当前没有 429 ban':'No active 429 bans',
  '等待 reset_at 自动放回':'Waiting for reset_at release',
  '429 等待 reset_at，401/402 需处理认证文件':'429 waits for reset_at; 401/402 need credential handling',
  '当前没有自动禁用':'No active auto-bans',
  '已停止使用，替换或删除 json 后解除':'Stopped. Replace or delete the JSON to release.',
  '当前没有失效 json':'No invalid JSON credentials',
  '未发现一号多卖迹象':'No shared-account signal found',
  '暂无账号':'No accounts',
  '暂无额度快照':'No quota snapshots',
  '次':'times'
};
function tr(text){
  const source=String(text??'');
  if(currentLang!=='en')return source;
  if(i18nEn[source])return i18nEn[source];
  let out=source;
  const exact=(zh,en)=>{out=out.split(zh).join(en)};
  [
    ['成功率 ','Success '],['缓存率 ','Cache rate '],['占总 ','total share '],['占 ','share '],
    ['占比 ','share '],['覆盖 ','Covers '],[' 个自动禁用账号',' auto-banned accounts'],[' 个接入点',' endpoints'],[' 个账号',' accounts'],['总额 ','total '],['模式 ','mode '],
    ['成功 ','success '],['失败 ','failed '],['跳过','skipped'],['最近 ','Recent '],
    ['显示 ','Showing '],['，已加载 ',', loaded '],['已加载 ','loaded '],
    ['外部消耗 ','external use '],['本地 ','local '],['输入 ','input '],['输出 ','output '],
    ['缓存 ↓ ','cache ↓ '],['推理 ','reasoning '],['余 ','remaining '],['总 ','total '],['已用 ','used '],
    ['耗时 ','latency '],['慢首包 ','slow TTFT '],['慢TTFT ','slow TTFT '],['首包 ','TTFT '],['慢请求 ','slow req '],
    ['失败 ','failed '],['次',' times'],['天','d'],['小时','h'],['分','m'],['窗口：','Window: '],
    ['数据库：','DB: '],['更新时间：','Updated: '],['请填写备用 CPA 管理密钥后刷新。','Enter the fallback CPA management key and refresh.'],
    ['备用管理密钥不正确，已清除临时保存值。','Fallback management key is incorrect. The temporary value was cleared.'],
    ['将写入 ','Will write '],[' 个 Codex 认证文件。',' Codex auth files.'],['预览失败：','Preview failed: '],
    ['批量写入失败：','Batch write failed: '],['正在写入：成功 ','Writing: success '],[' / 失败 ',' / failed '],
    [' / 总计 ',' / total '],['批量写入完成：成功 ','Batch write complete: success '],['解除成功：','Release succeeded: '],
    [' 个，失败 ',' files, failed '],[' 个，跳过 ',' released, skipped '],[' 个，未找到 ',', not found '],[' 个。',' files.']
  ].forEach(pair=>exact(pair[0],pair[1]));
  return out;
}
function applyLocale(){
  const root=document.documentElement;
  root.lang=currentLang==='en'?'en':'zh-CN';
  translateNode(document.body);
}
function translateNode(root){
  const walk=document.createTreeWalker(root,NodeFilter.SHOW_TEXT,{acceptNode:n=>n.nodeValue.trim()?NodeFilter.FILTER_ACCEPT:NodeFilter.FILTER_REJECT});
  const nodes=[];
  while(walk.nextNode())nodes.push(walk.currentNode);
  nodes.forEach(n=>{if(n.parentElement&&(['SCRIPT','STYLE'].includes(n.parentElement.tagName)||n.parentElement.closest('[data-no-i18n]')))return;if(!n.__i18nSource)n.__i18nSource=n.nodeValue;n.nodeValue=tr(n.__i18nSource)});
  root.querySelectorAll('[placeholder],[aria-label],[title]').forEach(el=>{
    if(el.closest('[data-no-i18n]'))return;
    ['placeholder','aria-label','title'].forEach(attr=>{
      if(!el.hasAttribute(attr))return;
      const key='__i18n_'+attr;
      if(!el[key])el[key]=el.getAttribute(attr);
      el.setAttribute(attr,tr(el[key]));
    });
  });
}
function refreshLanguage(force=false){
  const next=effectiveLanguage();
  if(!force&&next===currentLang)return;
  currentLang=next;
  syncLanguageControl();
  if(lastData)renderAll();else applyLocale();
}
function applyHostTheme(){
  const root=document.documentElement;
  const sources=[document.documentElement,document.body];
  let hostLooksDark=false, hostLooksLight=false;
  try{
    if(window.parent&&window.parent!==window&&window.parent.document){
      sources.unshift(window.parent.document.documentElement,window.parent.document.body);
      const p=window.parent.document.documentElement;
      const theme=(p.getAttribute('data-theme')||p.getAttribute('class')||'').toLowerCase();
      hostLooksDark=theme.includes('dark')||theme.includes('black');
      hostLooksLight=theme.includes('light')||theme.includes('white');
    }
  }catch(e){}
  const pick=(names)=>{for(const el of sources){if(!el)continue;const cs=getComputedStyle(el);for(const n of names){const v=normalizeColor(cs.getPropertyValue(n));if(v)return v;}}return ''};
  const set=(name,names)=>{const v=pick(names);if(v)root.style.setProperty(name,v)};
  const bgNames=['--cpa-bg','--background','--color-background','--body-bg','--el-bg-color-page','--ant-color-bg-layout'];
  const hostBg=pick(bgNames);
  const prefersDark=(()=>{try{return matchMedia('(prefers-color-scheme:dark)').matches}catch(e){return false}})();
  const dark=hostLooksDark||(!hostLooksLight&&(isDarkColor(hostBg)||(!hostBg&&prefersDark)));
  root.dataset.hostTheme=dark?'dark':'light';
  set('--cpa-primary',['--cpa-primary','--primary','--color-primary','--el-color-primary','--ant-color-primary','--primary-color']);
  set('--cpa-info',['--cpa-info','--info','--color-info','--el-color-info','--ant-color-info']);
  set('--cpa-success',['--cpa-success','--success','--color-success','--el-color-success','--ant-color-success']);
  set('--cpa-warning',['--cpa-warning','--warning','--color-warning','--el-color-warning','--ant-color-warning']);
  set('--cpa-danger',['--cpa-danger','--destructive','--danger','--color-destructive','--el-color-danger','--ant-color-error']);
  set('--cpa-accent',['--cpa-accent','--accent','--color-accent','--ring','--color-ring']);
}
function observeHostTheme(){
  const rerun=()=>applyHostTheme();
  try{matchMedia('(prefers-color-scheme:dark)').addEventListener('change',rerun)}catch(e){}
  try{
    if(window.parent&&window.parent!==window&&window.parent.document){
      const target=window.parent.document.documentElement;
      new MutationObserver(rerun).observe(target,{attributes:true,attributeFilter:['class','style','data-theme']});
    }
  }catch(e){}
}
function normalizeColor(raw){
  let v=String(raw||'').trim();
  if(!v)return '';
  if(/^#|^rgb|^hsl|^oklch|^color\(/i.test(v))return v;
  if(/^-?\d+(\.\d+)?\s+\d+(\.\d+)?%\s+\d+(\.\d+)?%/.test(v))return 'hsl('+v+')';
  return '';
}
function isDarkColor(v){
  v=String(v||'').trim().toLowerCase();
  let r,g,b;
  if(v[0]==='#'){
    if(v.length===4){r=parseInt(v[1]+v[1],16);g=parseInt(v[2]+v[2],16);b=parseInt(v[3]+v[3],16)}
    else if(v.length>=7){r=parseInt(v.slice(1,3),16);g=parseInt(v.slice(3,5),16);b=parseInt(v.slice(5,7),16)}
  }else{
    const m=v.match(/rgba?\((\d+)[,\s]+(\d+)[,\s]+(\d+)/);
    if(m){r=+m[1];g=+m[2];b=+m[3]}
  }
  if([r,g,b].some(x=>Number.isNaN(x)||x===undefined))return false;
  return (r*0.2126+g*0.7152+b*0.0722)<96;
}
function fmt(n){return Number(n||0).toLocaleString()}
function compact(n){n=Number(n||0); if(n>=1e9)return(n/1e9).toFixed(2)+'B'; if(n>=1e6)return(n/1e6).toFixed(2)+'M'; if(n>=1e3)return(n/1e3).toFixed(1)+'K'; return String(Math.round(n))}
function money(n){n=Number(n||0); return new Intl.NumberFormat('en-US',{style:'currency',currency:'USD',minimumFractionDigits:n<1?4:2,maximumFractionDigits:n<1?4:2}).format(n).replace(/^US\$/,'$')}
function ratio(part,total){return total>0?part/total*100:0}
function cacheTokens(r){return (r.cached_tokens||0)+(r.cache_read_tokens||0)+(r.cache_creation_tokens||0)}
function cacheRate(r){return ratio(r.cached_tokens||0,r.input_tokens||0)}
function pct(v){return v===undefined||v===null||v===''?'—':Number(v).toFixed(1)+'%'}
function esc(v){return String(v??'').replace(/[&<>"']/g,s=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[s]))}
function td(v,cls='',col=''){return '<td class="'+cls+'"'+(col?' data-col="'+esc(col)+'"':'')+'>'+v+'</td>'}
function colorForPct(v){v=Number(v||0); return v>=90?'var(--red)':v>=70?'var(--orange)':'var(--blue)'}
function health(v){v=Number(v||0); return v>=90?'danger':v>=70?'warn':'ok'}
function successRate(r){return ratio((r.requests||0)-(r.failed||0),r.requests||0)}
function resetText(ts){if(!ts)return '未捕获重置时间'; const n=Number(ts); const ms=n>1e12?n:n*1000; const d=new Date(ms); return isNaN(d.getTime())?'未捕获重置时间':'重置 '+d.toLocaleString()}
function duration(sec){sec=Math.max(0,Number(sec||0)); const d=Math.floor(sec/86400), h=Math.floor(sec%86400/3600), m=Math.floor(sec%3600/60); return d?d+'天 '+h+'小时':h?h+'小时 '+m+'分':m+'分'}
function isInvalidAuthBan(r){const w=String(r&&r.window||'').toLowerCase();const code=Number(r&&r.last_status_code);return w==='401'||w==='403'||code===401||code===403}
function isWorkspaceDeactivatedBan(r){return String(r&&r.window||'').toLowerCase()==='402'||Number(r&&r.last_status_code)===402}
function is429Autoban(r){const w=String(r&&r.window||'').toLowerCase();const code=Number(r&&r.last_status_code);if(w==='401'||w==='402'||w==='403'||code===401||code===402||code===403)return false;return code===429||['429','5h','primary','7d','week','secondary'].includes(w)}
function isPermanentAuthBan(r){return isInvalidAuthBan(r)||isWorkspaceDeactivatedBan(r)}
function autobanResetText(r){return isWorkspaceDeactivatedBan(r)?'删除或替换认证文件后解除':isInvalidAuthBan(r)?'重新登录后解除':(r.reset_at_text||'-')}
function autobanRemainingText(r){return isPermanentAuthBan(r)?'需处理':duration(r.seconds_remaining)}
function fmtLatencyMs(ms){ms=Number(ms||0); if(!ms)return '—'; if(ms>=1000)return (ms/1000).toFixed(1)+'s'; return Math.round(ms)+'ms'}
function latencyTone(ms){ms=Number(ms||0); return ms>=12000?'slow':ms>0?'fast':''}
function reliableThroughputSample(r){const latency=Number(r.latency_ms||0),ttft=Number(r.ttft_ms||0),ms=Math.max(latency,ttft),out=Number(r.output_tokens||0); return out>0&&ms>=1000&&!(latency===ttft&&out>=1000&&ms<5000)}
function throughput(r){const ms=Math.max(Number(r.latency_ms||0),Number(r.ttft_ms||0)); const out=Number(r.output_tokens||0); return reliableThroughputSample(r)?Math.round(out/(ms/1000))+' t/s':'-'}
function avgThroughput(v){v=Number(v||0); return v>0?v.toFixed(v>=10?1:2)+' t/s':'-'}
function perfTone(r){
  const slow=Number(r.slow_requests||0)+Number(r.slow_ttft_requests||0);
  const failRate=ratio(r.failed||0,r.requests||0);
  const latency=Number(r.avg_latency_ms||0);
  const ttft=Number(r.avg_ttft_ms||0);
  if(slow>0||failRate>=5||latency>=12000||ttft>=3000)return 'danger';
  if(failRate>=1||latency>=6000||ttft>=1500)return 'warn';
  return 'ok';
}
function perfCell(r){
  const tone=perfTone(r);
  const title='耗时 '+fmtLatencyMs(r.avg_latency_ms)+' · 首包 '+fmtLatencyMs(r.avg_ttft_ms)+' · 输出 '+avgThroughput(r.output_tokens_per_second)+' · 慢请求 '+fmt(r.slow_requests||0)+' / 慢首包 '+fmt(r.slow_ttft_requests||0);
  return '<span class="metric-stack" title="'+esc(title)+'"><b class="'+tone+'">'+fmtLatencyMs(r.avg_latency_ms)+'</b><span>首包 '+fmtLatencyMs(r.avg_ttft_ms)+'</span><span>'+avgThroughput(r.output_tokens_per_second)+'</span></span>';
}
function tierText(v){v=String(v||'').trim(); if(!v)return ''; const lower=v.toLowerCase(); if(lower==='default'||lower==='standard')return ''; if(lower==='priority')return '优先'; if(lower==='flex')return '弹性'; return v}
function requestStatusText(r){const code=Number(r.status_code||0)||((r.failed||false)?599:200); return (r.failed?('失败 '+code):('HTTP '+code))}
function summaryWindowCacheKey(win){return String(win||'24h')+'|2000'}
function showCachedSummaryForWindow(win){
  const cached=summaryWindowCache.get(summaryWindowCacheKey(win));
  const st=document.getElementById('status');
  if(cached){
    lastData=cached;
    renderAll();
    st.textContent=tr('正在更新当前窗口...');
    return true;
  }
  if(lastData)st.textContent=tr('正在更新当前窗口...');
  return false;
}
function summaryStatusText(data){
  let text=tr('窗口：')+data.window+' · '+tr('数据库：')+data.db_path+' · '+tr('更新时间：')+data.generated_at+' · '+tr('管理接口');
  const pre=data.precompute||{};
  if(pre.stale)text+=' · '+tr('缓存数据，后台更新中');
  return text;
}
function scheduleStaleSummaryRefresh(win){
  clearTimeout(summaryStaleRefreshTimer);
  summaryStaleRefreshTimer=setTimeout(()=>{if(document.getElementById('window').value===win&&!loading)load(true,false,{keepExisting:true,abortPrevious:true})},2500);
}
async function load(forceRefresh=false,syncRefresh=false,options={}){
  if(loading&&!options.abortPrevious)return;
  if(summaryAbortController)summaryAbortController.abort();
  const controller=new AbortController();
  summaryAbortController=controller;
  loading=true;
  const key=managementKey(); if(key)safeStorageSet(safeSessionStorage(),'cpa_token_usage_key',key);
  const win=document.getElementById('window').value;
  const st=document.getElementById('status'); st.textContent=lastData&&options.keepExisting?tr('正在更新当前窗口...'):tr('加载中...');
  try{
    const data=await fetchSummary(win,key,forceRefresh,syncRefresh,controller.signal);
    if(controller!==summaryAbortController)return;
    lastData=data;
    summaryWindowCache.set(summaryWindowCacheKey(win),data);
    renderAll();
    st.textContent=summaryStatusText(lastData);
    if(lastData.precompute&&lastData.precompute.stale)scheduleStaleSummaryRefresh(win);
  }catch(e){if(e.name!=='AbortError')st.textContent=tr('失败')+': '+tr(e.message)}
  finally{if(controller===summaryAbortController){loading=false;summaryAbortController=null}}
}
async function fetchSummary(win,key,forceRefresh=false,syncRefresh=false,signal=null){
  const url='?window='+encodeURIComponent(win)+'&limit=2000'+(forceRefresh?'&refresh=1':'')+(syncRefresh?'&sync=1':'');
  if(!key){showFallbackKeyInput();throw new Error('请填写备用 CPA 管理密钥后刷新。')}
  keyEl.classList.remove('on');
  const res=await fetch(managementApi+url,{headers:{Authorization:'Bearer '+key,Accept:'application/json'},signal});
  if(!res.ok){
    const body=await res.text();
    if(res.status===401)rejectManagementKey(key);
    throw new Error('HTTP '+res.status+' '+body+(res.status===401?' | '+tr('备用管理密钥不正确，已清除临时保存值。'):''));
  }
  const data=await res.json(); data._source='management'; return data;
}
function renderAll(){
  const data=lastData||{}; const t=data.totals||{}; const total=t.total_tokens||0; const okReq=(t.requests||0)-(t.failed||0);
  document.getElementById('tab-codex-count').textContent=fmt((data.accounts||[]).length);
  document.getElementById('tab-provider-count').textContent=fmt((data.providers||[]).length);
  document.getElementById('m-requests').textContent=fmt(t.requests);
  document.getElementById('m-success').textContent='成功率 '+pct(ratio(okReq,t.requests));
  document.getElementById('m-total').textContent=compact(total);
  document.getElementById('m-cost').textContent=money(t.cost_usd);
  document.getElementById('m-cost-sub').textContent=t.cost_available?'模型价格已覆盖':'部分模型缺价格 · '+compact(t.unpriced_tokens||0)+' tok';
  document.getElementById('m-input').textContent=compact(t.input_tokens);
  document.getElementById('m-input-share').textContent='占比 '+pct(ratio(t.input_tokens,total));
  document.getElementById('m-output').textContent=compact(t.output_tokens);
  document.getElementById('m-output-share').textContent='占比 '+pct(ratio(t.output_tokens,total));
  const cache=cacheTokens(t);
  document.getElementById('m-cache').textContent=compact(cache);
  document.getElementById('m-cache-share').textContent='缓存率 '+pct(cacheRate(t))+' · 占总 '+pct(ratio(cache,total));
  document.getElementById('m-429').textContent=fmt(t.rate_limited);
  document.getElementById('m-bans').textContent=fmt((data.autobans||[]).length);
  document.getElementById('m-accounts').textContent=fmt((data.accounts||[]).length);
  document.getElementById('m-7d-remaining').textContent=quotaValue(t.secondary_quota_remaining_estimate);
  document.getElementById('m-7d-remaining-sub').textContent=(t.secondary_quota_estimated_accounts||0)>0?'覆盖 '+fmt(t.secondary_quota_estimated_accounts)+' 个账号 · 总额 '+quotaValue(t.secondary_quota_total_estimate):'等待 7d/月额度快照';
  const qt=data.quota_trigger||{};
  document.getElementById('m-trigger').textContent=qt.enabled?(qt.running?'运行中':'已开启'):'已关闭';
  document.getElementById('m-trigger-sub').textContent=qt.enabled?('模式 '+(qt.mode||'quota')+' · '+(qt.interval_minutes||10)+'m · 成功 '+fmt(qt.last_success||0)+' / 失败 '+fmt(qt.last_failed||0)+' / 跳过 '+fmt(qt.last_skipped||0)):'默认关闭 · quota 查询不保证启动 5h 窗口';
  const top=(data.accounts||[])[0]?.total_tokens||0;
  document.getElementById('m-topshare').textContent=pct(ratio(top,total));
  document.getElementById('m-latency').textContent=fmtLatencyMs(t.avg_latency_ms);
  document.getElementById('m-latency-sub').textContent='慢请求 '+fmt(t.slow_requests||0);
  document.getElementById('m-ttft').textContent=fmtLatencyMs(t.avg_ttft_ms);
  document.getElementById('m-ttft-sub').textContent='慢首包 '+fmt(t.slow_ttft_requests||0);
  document.getElementById('m-throughput').textContent=avgThroughput(t.output_tokens_per_second);
  renderTrend('trend',data.trend||[]);
  renderTokenMix('token-mix',t);
  renderInsights(data);
  renderAutobans(data.autobans||[]);
  renderAccounts();
  renderModels('models',data.models||[]);
  renderRecent('recent',data.recent||[],'codex');
  renderProviderPage(data);
  renderProviderTabsAndPages(data);
  applyLocale();
}
function renderProviderPage(data){
  const t=data.provider_totals||{}; const total=t.total_tokens||0; const okReq=(t.requests||0)-(t.failed||0);
  document.getElementById('pm-requests').textContent=fmt(t.requests);
  document.getElementById('pm-success').textContent='成功率 '+pct(ratio(okReq,t.requests));
  document.getElementById('pm-total').textContent=compact(total);
  document.getElementById('pm-cost').textContent=money(t.cost_usd);
  document.getElementById('pm-cost-sub').textContent=t.cost_available?'模型价格已覆盖':'部分模型缺价格 · '+compact(t.unpriced_tokens||0)+' tok';
  document.getElementById('pm-input').textContent=compact(t.input_tokens);
  document.getElementById('pm-input-share').textContent='占比 '+pct(ratio(t.input_tokens,total));
  document.getElementById('pm-output').textContent=compact(t.output_tokens);
  document.getElementById('pm-output-share').textContent='占比 '+pct(ratio(t.output_tokens,total));
  const cache=cacheTokens(t);
  document.getElementById('pm-cache').textContent=compact(cache);
  document.getElementById('pm-cache-share').textContent='缓存率 '+pct(cacheRate(t))+' · 占总 '+pct(ratio(cache,total));
  document.getElementById('pm-429').textContent=fmt(t.rate_limited);
  document.getElementById('pm-providers').textContent=fmt((data.providers||[]).length);
  document.getElementById('pm-models').textContent=fmt((data.provider_models||[]).length);
  const top=(data.providers||[])[0]?.total_tokens||0;
  document.getElementById('pm-topshare').textContent=pct(ratio(top,total));
  document.getElementById('pm-latency').textContent=fmtLatencyMs(t.avg_latency_ms);
  document.getElementById('pm-latency-sub').textContent='慢请求 '+fmt(t.slow_requests||0);
  document.getElementById('pm-ttft').textContent=fmtLatencyMs(t.avg_ttft_ms);
  document.getElementById('pm-ttft-sub').textContent='慢首包 '+fmt(t.slow_ttft_requests||0);
  document.getElementById('pm-throughput').textContent=avgThroughput(t.output_tokens_per_second);
  renderTokenMix('provider-token-mix',t);
  renderTrend('provider-trend',data.provider_trend||[]);
  renderProviders(data.providers||[],total);
  renderKeySummaries(data.key_summaries||[]);
  renderModels('provider-models',data.provider_models||[]);
  renderRecent('provider-recent',(data.provider_recent||[]).slice(0,30),'provider');
}
function providerEquals(row,name){return providerLabel(row.provider)===name}
function renderSingleProviderPage(data,name){
  const id=providerID(name);
  const p=(data.providers||[]).find(r=>providerEquals(r,name))||{};
  const total=p.total_tokens||0;
  const okReq=(p.requests||0)-(p.failed||0);
  const models=(data.provider_models||[]).filter(r=>providerEquals(r,name));
  const recent=(data.provider_recent||[]).filter(r=>providerEquals(r,name)).slice(0,30);
  const set=(suffix,value)=>{const el=document.getElementById(id+suffix);if(el)el.textContent=value};
  set('-requests',fmt(p.requests));
  set('-success','成功率 '+pct(ratio(okReq,p.requests)));
  set('-total',compact(total));
  set('-cost',money(p.cost_usd));
  set('-cost-sub',p.cost_available?'模型价格已覆盖':'部分模型缺价格 · '+compact(p.unpriced_tokens||0)+' tok');
  set('-input',compact(p.input_tokens));
  set('-input-share','占比 '+pct(ratio(p.input_tokens,total)));
  set('-output',compact(p.output_tokens));
  set('-output-share','占比 '+pct(ratio(p.output_tokens,total)));
  const cache=cacheTokens(p);
  set('-cache',compact(cache));
  set('-cache-share','缓存率 '+pct(cacheRate(p))+' · 占总 '+pct(ratio(cache,total)));
  set('-429',fmt(p.rate_limited));
  set('-models-count',fmt(models.length));
  set('-latency',fmtLatencyMs(p.avg_latency_ms));
  set('-latency-sub','慢请求 '+fmt(p.slow_requests||0));
  set('-ttft',fmtLatencyMs(p.avg_ttft_ms));
  set('-ttft-sub','慢首包 '+fmt(p.slow_ttft_requests||0));
  set('-throughput',avgThroughput(p.output_tokens_per_second));
  renderTokenMix(id+'-mix',p);
  renderModels(id+'-models',models);
  renderRecent(id+'-recent',recent,'provider');
}
function renderTokenMix(target,t){
  const total=t.total_tokens||0;
  const rows=[
    ['输入 Token',t.input_tokens,'var(--cyan)'],
    ['输出 Token',t.output_tokens,'var(--violet)'],
    ['推理 Token',t.reasoning_tokens,'var(--blue)'],
    ['缓存命中',cacheTokens(t),'var(--orange)']
  ];
  document.getElementById(target).innerHTML=rows.map(r=>'<div class="mix-row"><div>'+r[0]+'</div><div class="bar"><span style="--color:'+r[2]+';width:'+Math.min(100,ratio(r[1],total)).toFixed(1)+'%"></span></div><div class="num">'+compact(r[1])+'</div></div>').join('');
}
function renderTrend(target,points){
  const svg=document.getElementById(target); const w=900,h=270,pad=34;
  if(!points.length){svg.innerHTML='<text x="450" y="135" text-anchor="middle" fill="currentColor">暂无趋势数据</text>';return}
  const maxReq=Math.max(1,...points.map(p=>p.requests||0)); const maxTok=Math.max(1,...points.map(p=>p.total_tokens||0),...points.map(p=>p.output_tokens||0));
  const pointX=i=>pad+(w-pad*2)*(points.length===1?0:i/(points.length-1));
  const pointY=(p,key,max)=>h-pad-(h-pad*2)*((p[key]||0)/max);
  const path=(key,max)=>points.map((p,i)=>{const x=pointX(i); const y=pointY(p,key,max); return (i?'L':'M')+x.toFixed(1)+' '+y.toFixed(1)}).join(' ');
  const area=path('total_tokens',maxTok)+' L '+(w-pad)+' '+(h-pad)+' L '+pad+' '+(h-pad)+' Z';
  const ticks=[0,.25,.5,.75,1].map(v=>'<line x1="'+pad+'" x2="'+(w-pad)+'" y1="'+(h-pad-(h-pad*2)*v).toFixed(1)+'" y2="'+(h-pad-(h-pad*2)*v).toFixed(1)+'" stroke="var(--line)" stroke-dasharray="3 5"/>').join('');
  svg.innerHTML=ticks+'<path d="'+area+'" fill="var(--cyan)" opacity=".10"/><path d="'+path('requests',maxReq)+'" fill="none" stroke="var(--blue)" stroke-width="3"/><path d="'+path('total_tokens',maxTok)+'" fill="none" stroke="var(--cyan)" stroke-width="3"/><path d="'+path('output_tokens',maxTok)+'" fill="none" stroke="var(--orange)" stroke-width="3"/><text x="'+pad+'" y="'+(h-8)+'" fill="var(--muted)" font-size="12">'+esc(points[0].bucket)+'</text><text x="'+(w-pad)+'" y="'+(h-8)+'" text-anchor="end" fill="var(--muted)" font-size="12">'+esc(points[points.length-1].bucket)+'</text>'+
    '<g class="trend-tooltip" style="display:none"><line class="trend-guide" x1="0" x2="0" y1="'+pad+'" y2="'+(h-pad)+'"/><circle class="trend-dot trend-dot-req" r="4" fill="var(--blue)"/><circle class="trend-dot trend-dot-total" r="4" fill="var(--cyan)"/><circle class="trend-dot trend-dot-output" r="4" fill="var(--orange)"/><g class="trend-tip"><rect class="trend-tip-box" width="168" height="82" rx="7"/><text class="trend-tip-title" x="10" y="18"></text><text class="trend-tip-line trend-tip-req" x="10" y="38"></text><text class="trend-tip-line trend-tip-total" x="10" y="56"></text><text class="trend-tip-line trend-tip-output" x="10" y="74"></text></g></g><rect class="trend-hit" x="'+pad+'" y="'+pad+'" width="'+(w-pad*2)+'" height="'+(h-pad*2)+'"/>';
  bindTrendTooltip(svg,points,{w,h,pad,maxReq,maxTok,pointX,pointY});
}
function bindTrendTooltip(svg,points,cfg){
  const tip=svg.querySelector('.trend-tooltip'); const hit=svg.querySelector('.trend-hit');
  if(!tip||!hit)return;
  const guide=tip.querySelector('.trend-guide'); const reqDot=tip.querySelector('.trend-dot-req'); const totalDot=tip.querySelector('.trend-dot-total'); const outputDot=tip.querySelector('.trend-dot-output');
  const title=tip.querySelector('.trend-tip-title'); const req=tip.querySelector('.trend-tip-req'); const total=tip.querySelector('.trend-tip-total'); const output=tip.querySelector('.trend-tip-output'); const box=tip.querySelector('.trend-tip');
  const hide=()=>{tip.style.display='none'};
  hit.onmouseleave=hide; hit.onmousemove=e=>{
    const rect=svg.getBoundingClientRect();
    const svgX=(e.clientX-rect.left)/Math.max(1,rect.width)*cfg.w;
    const rel=(svgX-cfg.pad)/Math.max(1,cfg.w-cfg.pad*2);
    const idx=Math.max(0,Math.min(points.length-1,Math.round(rel*(points.length-1))));
    const p=points[idx]||{}; const x=cfg.pointX(idx);
    const yReq=cfg.pointY(p,'requests',cfg.maxReq), yTotal=cfg.pointY(p,'total_tokens',cfg.maxTok), yOutput=cfg.pointY(p,'output_tokens',cfg.maxTok);
    guide.setAttribute('x1',x.toFixed(1)); guide.setAttribute('x2',x.toFixed(1));
    reqDot.setAttribute('cx',x.toFixed(1)); reqDot.setAttribute('cy',yReq.toFixed(1));
    totalDot.setAttribute('cx',x.toFixed(1)); totalDot.setAttribute('cy',yTotal.toFixed(1));
    outputDot.setAttribute('cx',x.toFixed(1)); outputDot.setAttribute('cy',yOutput.toFixed(1));
    title.textContent=p.bucket||'-';
    req.textContent='请求 '+fmt(p.requests||0);
    total.textContent='总 Token '+compact(p.total_tokens||0);
    output.textContent='输出 Token '+compact(p.output_tokens||0);
    const boxX=Math.min(cfg.w-cfg.pad-168,Math.max(cfg.pad,x+12));
    const boxY=Math.max(10,Math.min(cfg.h-cfg.pad-84,Math.min(yReq,yTotal,yOutput)-42));
    box.setAttribute('transform','translate('+boxX.toFixed(1)+' '+boxY.toFixed(1)+')');
    tip.style.display='block';
  };
}
function renderAccounts(){
  const data=lastData||{}; const total=(data.totals||{}).total_tokens||0; const q=(document.getElementById('account-filter').value||'').toLowerCase(); const sort=document.getElementById('account-sort').value;
  let rows=(data.accounts||[]).filter(r=>(r.auth_index+' '+r.auth_id+' '+r.source+' '+r.provider+' '+r.email+' '+r.name+' '+r.auth_file+' '+r.chatgpt_account_id+' '+r.plan_type+' '+r.invalid_auth_reason+' '+r.workspace_deactivated_reason+' '+r.external_use_reason+' '+r.quota_trigger_status+' '+r.quota_trigger_error).toLowerCase().includes(q));
  rows.sort((a,b)=>sort==='cost'?(b.cost_usd||0)-(a.cost_usd||0):sort==='quotaRemain'?quotaRemainingSortValue(a)-quotaRemainingSortValue(b):sort==='quotaTotal'?quotaTotalSortValue(b)-quotaTotalSortValue(a):sort==='latency'?(b.avg_latency_ms||0)-(a.avg_latency_ms||0):sort==='invalid'?(Number(!!b.invalid_auth)-Number(!!a.invalid_auth))||Date.parse(b.invalid_auth_at||0)-Date.parse(a.invalid_auth_at||0):sort==='workspace'?(Number(!!b.workspace_deactivated)-Number(!!a.workspace_deactivated))||Date.parse(b.workspace_deactivated_at||0)-Date.parse(a.workspace_deactivated_at||0):sort==='external'?(Number(!!b.external_use_suspected)-Number(!!a.external_use_suspected))||((b.external_use_delta_percent||0)-(a.external_use_delta_percent||0)):sort==='trigger'?triggerSortScore(b)-triggerSortScore(a):sort==='quota'?maxQuota(b)-maxQuota(a):sort==='cache'?cacheRate(b)-cacheRate(a):sort==='429'?(b.rate_limited||0)-(a.rate_limited||0):sort==='success'?successRate(a)-successRate(b):sort==='recent'?Date.parse(b.last_seen||0)-Date.parse(a.last_seen||0):(b.total_tokens||0)-(a.total_tokens||0));
  const allCount=(data.accounts||[]).length;
  const pages=Math.max(1,Math.ceil(rows.length/accountPageSize));
  accountPage=Math.max(1,Math.min(accountPage,pages));
  const start=(accountPage-1)*accountPageSize;
  const pageRows=rows.slice(start,start+accountPageSize);
  const externalCount=rows.filter(r=>r.external_use_suspected).length;
  const invalidCount=rows.filter(r=>r.invalid_auth).length;
  const workspaceDeactivatedCount=rows.filter(r=>r.workspace_deactivated).length;
  const active429Count=autobanReleaseRows().length;
  const triggerFailed=rows.filter(r=>r.quota_trigger_status&&r.quota_trigger_status!=='success'&&r.quota_trigger_status!=='skipped').length;
  const riskCount=rows.filter(r=>findBan(r)||r.invalid_auth||r.workspace_deactivated||r.external_use_suspected||r.disabled||r.expired||triggerRisk(r)||maxQuota(r)>=90||((r.requests||0)>0&&successRate(r)<80)).length;
  const quotaHot=[...rows].sort((a,b)=>maxQuota(b)-maxQuota(a))[0];
  const lowCache=[...rows].filter(r=>(r.input_tokens||0)>0).sort((a,b)=>cacheRate(a)-cacheRate(b))[0];
  document.getElementById('account-scope').textContent='显示 '+(rows.length?start+1:0)+'-'+Math.min(start+pageRows.length,rows.length)+' / '+rows.length+'，已加载 '+allCount+' 个账号';
  document.getElementById('account-loaded').textContent=fmt(rows.length)+' / '+fmt(allCount);
  document.getElementById('account-cost-total').textContent=money(rows.reduce((sum,r)=>sum+Number(r.cost_usd||0),0));
  document.getElementById('account-risk').textContent=fmt(riskCount);
  document.getElementById('account-invalid-auth').textContent=fmt(invalidCount);
  const invalidCard=document.getElementById('invalid-auth-card');
  invalidCard.classList.toggle('has-invalid',invalidCount>0);
  invalidCard.title=invalidCount?('点击管理 '+invalidCount+' 个 401 失效账号'):'当前没有 401 失效账号，点击查看';
  document.getElementById('account-invalid-auth-hint').textContent=invalidCount?tr('点击处理'):tr('无 401');
  document.getElementById('account-workspace-deactivated').textContent=fmt(workspaceDeactivatedCount);
  const workspaceCard=document.getElementById('workspace-deactivated-card');
  workspaceCard.classList.toggle('has-invalid',workspaceDeactivatedCount>0);
  workspaceCard.title=workspaceDeactivatedCount?('点击管理 '+workspaceDeactivatedCount+' 个 402 工作区失效账号'):'当前没有 402 工作区失效账号，点击查看';
  document.getElementById('account-workspace-deactivated-hint').textContent=workspaceDeactivatedCount?tr('点击处理'):tr('无 402');
  document.getElementById('account-429-bans').textContent=fmt(active429Count);
  const autobanCard=document.getElementById('autoban-release-card');
  autobanCard.classList.toggle('has-invalid',active429Count>0);
  autobanCard.title=active429Count?('点击解除 '+active429Count+' 个 429 禁用账号'):'当前没有 429 禁用账号，点击查看';
  document.getElementById('account-429-bans-hint').textContent=active429Count?tr('点击解除'):tr('无 429');
  document.getElementById('account-external-use').textContent=fmt(externalCount);
  document.getElementById('account-trigger-failed').textContent=fmt(triggerFailed);
  document.getElementById('account-quota-hot').textContent=quotaHot?accountName(quotaHot)+' · '+pct(maxQuota(quotaHot)):'-';
  document.getElementById('account-cache-low').textContent=lowCache?accountName(lowCache)+' · '+pct(cacheRate(lowCache)):'-';
  document.getElementById('account-page-label').textContent=accountPage+' / '+pages;
  document.getElementById('account-prev').disabled=accountPage<=1;
  document.getElementById('account-next').disabled=accountPage>=pages;
  renderAccountTable(pageRows,total);
}
function findBan(r){
  const bans=(lastData&&lastData.autobans)||[];
  return bans.find(b=>sameAuthIdentity(b,r));
}
function accountName(r){return r.email||r.source||r.name||r.auth_id||r.auth_file||r.auth_index||'unknown'}
function maxQuota(r){return Math.max(r.primary_used_percent||0,r.secondary_used_percent||0)}
function triggerRisk(r){return r.quota_trigger_status&&r.quota_trigger_status!=='success'&&r.quota_trigger_status!=='skipped'}
function triggerSortScore(r){return triggerRisk(r)?3:(r.quota_trigger_status==='skipped'?2:(r.quota_trigger_status==='success'?1:0))}
function accountStatus(r){
  const ban=findBan(r);
  if(r.invalid_auth)return '<span class="status-pill danger" title="'+esc(r.invalid_auth_reason||'401 unauthorized')+'">401 失效</span>';
  if(r.workspace_deactivated)return '<span class="status-pill danger" title="'+esc(r.workspace_deactivated_reason||'402 deactivated_workspace')+'">402 工作区</span>';
  if(ban)return '<span class="status-pill danger">自动禁用</span>';
  if(r.external_use_suspected)return '<span class="status-pill danger" title="'+esc(r.external_use_reason||'quota 上升但本地无明显使用')+'">疑似外部消耗</span>';
  if(triggerRisk(r))return '<span class="status-pill warn" title="'+esc(r.quota_trigger_error||'quota trigger failed')+'">触发异常</span>';
  if(r.disabled)return '<span class="status-pill warn">已禁用</span>';
  if(r.expired)return '<span class="status-pill warn">已过期</span>';
  if(maxQuota(r)>=90)return '<span class="status-pill danger">额度高</span>';
  if(maxQuota(r)>=70)return '<span class="status-pill warn">接近额度</span>';
  if((r.requests||0)===0&&r.configured)return '<span class="status-pill warn">未使用</span>';
  if((r.requests||0)>0&&successRate(r)<80)return '<span class="status-pill warn">成功率低</span>';
  return '<span class="status-pill ok">正常</span>';
}
function renderAccountTable(rows,total){
  document.getElementById('account-table').innerHTML=rows.map(r=>'<tr>'+
    td(accountIdentityCell(r),'account-cell')+
    td(metricStack(fmt(r.requests),'失败 '+fmt(r.failed||0)),'num')+
    td(accountSuccessCell(r),'num')+
    td(perfCell(r),'num','perf')+
    td(tokenCostStack(r,total),'num')+
    td(metricStack(pct(cacheRate(r)),compact(cacheTokens(r))),'num','cache')+
    td(quotaCompact('5h',r.primary_used_percent,r.primary_window_tokens,r.primary_reset_at),'num','quota5h')+
    td(quota7dCell(r),'num')+
    td('<span class="'+((r.rate_limited||0)>0?'danger':'ok')+'">'+fmt(r.rate_limited||0)+'</span>','num')+
    td(esc(r.last_seen||'-'))+td(accountStatus(r),'','status')+
  '</tr>').join('') || '<tr><td colspan="11" class="muted">没有匹配的账号。</td></tr>';
}
function accountIdentityCell(r){
  const name=accountName(r);
  const badges=['<span class="pill">'+esc(r.provider||'codex')+'</span>'];
  if(r.plan_type)badges.push('<span class="pill">'+esc(r.plan_type)+'</span>');
  if(r.configured)badges.push('<span class="pill">已配置</span>');
  if(r.invalid_auth)badges.push('<span class="pill danger" title="'+esc(r.invalid_auth_at||'')+'">401 失效</span>');
  if(r.workspace_deactivated)badges.push('<span class="pill danger" title="'+esc(r.workspace_deactivated_at||'')+'">402 工作区</span>');
  if(r.external_use_suspected)badges.push('<span class="pill danger" title="'+esc(r.external_use_reason||'')+'">外部消耗 '+pct(r.external_use_delta_percent)+'</span>');
  if(r.quota_trigger_status)badges.push(triggerBadge(r));
  if(r.disabled)badges.push('<span class="pill">disabled</span>');
  if(r.expired)badges.push('<span class="pill">expired</span>');
  const accountId=firstText(r.chatgpt_account_id,'');
  const fileId=firstText(r.auth_file,r.auth_index,r.auth_id,'-');
  const id=accountId?('id '+accountId+' · '+fileId):fileId;
  return '<span class="account-name" title="'+esc(name)+'">'+esc(name)+'</span><div class="account-meta">'+badges.join('')+'<span class="account-id" title="'+esc(id)+'">'+esc(id)+'</span></div>';
}
function triggerBadge(r){
  const status=r.quota_trigger_status||'';
  const tone=status==='success'?'ok':status==='skipped'?'warn':'danger';
  const text=status==='success'?'触发 OK':status==='skipped'?'触发跳过':'触发失败';
  const detail=(r.quota_trigger_mode||'probe')+' · '+(r.quota_trigger_last_at||'-')+(r.quota_trigger_http_status?' · HTTP '+r.quota_trigger_http_status:'')+(r.quota_trigger_error?' · '+r.quota_trigger_error:'');
  return '<span class="pill '+tone+'" title="'+esc(detail)+'">'+text+'</span>';
}
function firstText(){for(const v of arguments){if(v!==undefined&&v!==null&&String(v).trim())return String(v)}return ''}
function metricStack(value,sub){return '<span class="metric-stack"><b>'+value+'</b><span>'+esc(sub)+'</span></span>'}
function tonePercent(value,tone){return '<span class="'+tone+'">'+pct(value)+'</span>'}
function accountSuccessCell(r){if((r.requests||0)===0)return '<span class="muted">-</span>'; const sr=successRate(r); return tonePercent(sr,sr>=95?'ok':sr>=80?'warn':'danger')}
function quotaRemainingSortValue(r){return Number(r.secondary_quota_total_estimate||0)>0?Number(r.secondary_quota_remaining_estimate||0):Number.MAX_SAFE_INTEGER}
function quotaTotalSortValue(r){return Number(r.secondary_quota_total_estimate||0)}
function quotaValue(v,allowZero=false){const n=Number(v||0);return n>0||allowZero?compact(n):'-'}
function quotaWindowLabel(r){return (r.secondary_quota_window||'7d')==='month'?'月':'7d'}
function quotaEstimateCell(r){
  const total=Number(r.secondary_quota_total_estimate||0), remaining=Number(r.secondary_quota_remaining_estimate||0);
  if(total<=0)return '<span class="metric-stack"><b class="muted">-</b><span>样本不足</span></span>';
  const usedPct=ratio(total-remaining,total);
  const tone=usedPct>=90?'danger':usedPct>=70?'warn':'ok';
  return '<span class="metric-stack" title="按当前 '+quotaWindowLabel(r)+' 窗口已用 Token、额度百分比和最近 quota trigger 快照实时估算"><b class="'+tone+'">余 '+quotaValue(remaining,true)+'</b><span>总 '+quotaValue(total)+' · 已用 '+pct(usedPct)+'</span></span>';
}
function quota7dCell(r){return '<div class="metric-stack">'+quotaCompact(quotaWindowLabel(r),r.secondary_used_percent,r.secondary_window_tokens,r.secondary_reset_at)+quotaEstimateCell(r)+'</div>'}
function tokenCostStack(r,total){const cost=r.cost_available||Number(r.cost_usd||0)>0?money(r.cost_usd):'缺价格'; const cls=r.cost_available?'cost-line':'cost-weak'; return '<span class="metric-stack"><b>'+compact(r.total_tokens)+'</b><span>占 '+pct(ratio(r.total_tokens,total))+'</span><span class="'+cls+'">'+esc(cost)+'</span></span>'}
function renderProviders(rows,total){
  document.getElementById('providers').innerHTML=rows.map(r=>'<tr>'+
    td('<span class="pill">'+esc(r.provider||'unknown')+'</span>')+td(fmt(r.requests),'num')+td(pct(successRate(r)),'num')+
    td(perfCell(r),'num')+td(meterCell(compact(r.total_tokens),ratio(r.total_tokens,total),'var(--blue)'),'num')+td(costCell(r),'num')+td(compact(r.input_tokens),'num')+td(compact(r.output_tokens),'num')+
    td(compact(cacheTokens(r)),'num')+td(pct(cacheRate(r)),'num')+td(fmt(r.accounts||0),'num')+td(fmt(r.models||0),'num')+
    td(fmt(r.rate_limited||0),'num')+td(esc(r.last_seen||'-'))+'</tr>').join('') || '<tr><td colspan="14" class="muted">暂无 Provider 数据</td></tr>';
}
function renderKeySummaries(rows){
  const target=document.getElementById('key-summaries');
  if(!target)return;
  target.innerHTML=rows.map(r=>{
    const ok=(r.requests||0)-(r.failed||0);
    const providers=firstText(r.provider_names,r.provider,'-');
    return '<tr>'+
      td('<span class="pill">'+esc(r.key_id||'-')+'</span>')+
      td('<span class="pill">'+esc(r.protocol||'-')+'</span>')+
      td('<span class="metric-stack"><b title="'+esc(providers)+'">'+esc(providers)+'</b><span>'+fmt(r.providers||0)+' 个接入点</span></span>')+
      td(fmt(r.requests),'num')+
      td(pct(ratio(ok,r.requests)),'num')+
      td('<span class="metric-stack"><b>'+compact(r.total_tokens)+'</b><span>'+money(r.cost_usd)+'</span></span>','num')+
      td(fmt(r.models||0),'num')+
      td('<span class="'+((r.rate_limited||0)>0?'danger':'ok')+'">'+fmt(r.rate_limited||0)+'</span>','num')+
      td(esc(r.last_seen||'-'))+
    '</tr>';
  }).join('') || '<tr><td colspan="9" class="muted">暂无 Key 数据</td></tr>';
}
function costCell(r){return r.cost_available||Number(r.cost_usd||0)>0?'<span class="'+(r.cost_available?'ok':'muted')+'">'+money(r.cost_usd)+'</span>':'<span class="muted">缺价格</span>'}
function renderInsights(data){
  const accounts=[...(data.accounts||[])]; const t=data.totals||{}; const total=t.total_tokens||0; const bans=data.autobans||[];
  const top=accounts[0]; const quota=[...accounts].sort((a,b)=>maxQuota(b)-maxQuota(a))[0]; const lowCache=[...accounts].filter(r=>(r.input_tokens||0)>0).sort((a,b)=>cacheRate(a)-cacheRate(b))[0]; const noisy=[...accounts].sort((a,b)=>(b.rate_limited||0)-(a.rate_limited||0))[0]; const external=[...accounts].filter(r=>r.external_use_suspected).sort((a,b)=>(b.external_use_delta_percent||0)-(a.external_use_delta_percent||0))[0]; const invalid=[...accounts].filter(r=>r.invalid_auth)[0]; const workspace=[...accounts].filter(r=>r.workspace_deactivated)[0];
  const qt=data.quota_trigger||{}; const triggerLine=qt.enabled?('最近 '+fmt(qt.last_success||0)+' 成功 / '+fmt(qt.last_failed||0)+' 失败 / '+fmt(qt.last_skipped||0)+' 跳过'):'默认关闭';
  const items=[
    ['Token 集中度',top?accountName(top):'-',top?'Top 占 '+pct(ratio(top.total_tokens,total)):'暂无账号',ratio(top?.total_tokens||0,total)>50?'tone-orange':''],
    ['额度触发',qt.enabled?((qt.mode||'probe')+' · '+(qt.interval_minutes||10)+'m'):'已关闭',triggerLine,qt.last_failed?'tone-orange':qt.enabled?'tone-green':''],
    ['自动禁用',fmt(bans.length)+' 个账号',bans.length?'429 等待 reset_at，401/402 需处理认证文件':'当前没有自动禁用',bans.length?'tone-red':'tone-green'],
    ['401 失效',invalid?accountName(invalid):'0 个账号',invalid?'已停止使用，替换或删除 json 后解除':'当前没有失效 json',invalid?'tone-red':'tone-green'],
    ['402 工作区',workspace?accountName(workspace):'0 个账号',workspace?'Team 工作区失效，删除或替换 json 后解除':'当前没有工作区失效',workspace?'tone-orange':'tone-green'],
    ['外部消耗',external?accountName(external):'0 个账号',external?external.external_use_window+' +'+pct(external.external_use_delta_percent)+' · 本地 '+compact(external.external_use_local_tokens)+' tok':'未发现一号多卖迹象',external?'tone-red':'tone-green'],
    ['额度最高',quota?accountName(quota):'-',quota?'5h '+pct(quota.primary_used_percent)+' · '+quotaWindowLabel(quota)+' '+pct(quota.secondary_used_percent):'暂无额度快照',maxQuota(quota||{})>=90?'tone-red':maxQuota(quota||{})>=70?'tone-orange':''],
    ['缓存最低',lowCache?accountName(lowCache):'-',lowCache?'缓存率 '+pct(cacheRate(lowCache))+' · 输入 '+compact(lowCache.input_tokens):'暂无输入 Token',lowCache&&cacheRate(lowCache)<30?'tone-orange':'']
  ];
  if(noisy&&(noisy.rate_limited||0)>0){items.push(['429 最多',accountName(noisy),fmt(noisy.rate_limited)+' 次 · 失败 '+fmt(noisy.failed),'tone-red'])}
  document.getElementById('insights').innerHTML=items.map(r=>'<div class="insight '+r[3]+'"><span>'+r[0]+'</span><b title="'+esc(r[1])+'">'+esc(r[1])+'</b><span>'+r[2]+'</span></div>').join('');
}
function meterCell(value,width,color){width=Math.max(0,Math.min(100,Number(width||0)));return '<div class="cell-meter"><b>'+esc(value)+'</b><div class="bar"><span style="--color:'+color+';width:'+width.toFixed(1)+'%"></span></div></div>'}
function quotaText(percent,tokens){const p=pct(percent); const tok=Number(tokens||0)>0?compact(tokens)+' tok':'无窗口 Token'; return p+' · '+tok}
function quotaCompact(label,value,tokens,resetAt){const width=value==null?0:Math.min(100,Number(value));const title=quotaText(value,tokens);return '<div class="quota-compact" title="'+esc(resetText(resetAt))+'"><span>'+label+'</span><div class="bar"><span style="--color:'+colorForPct(width)+';width:'+width.toFixed(1)+'%"></span></div><b class="'+health(width)+'">'+esc(title)+'</b></div>'}
function sortAutobansByRemaining(rows){
  return [...(rows||[])].sort((a,b)=>autobanRemainingSortValue(a)-autobanRemainingSortValue(b)||Number(a.reset_at||0)-Number(b.reset_at||0));
}
function autobanRemainingSortValue(r){
  const seconds=Number(r&&r.seconds_remaining);
  return Number.isFinite(seconds)?Math.max(0,seconds):Number.MAX_SAFE_INTEGER;
}
function renderAutobans(rows){
  rows=sortAutobansByRemaining(rows);
  const pages=Math.max(1,Math.ceil(rows.length/autobanPageSize));
  autobanPage=Math.max(1,Math.min(autobanPage,pages));
  const start=(autobanPage-1)*autobanPageSize;
  const pageRows=rows.slice(start,start+autobanPageSize);
  document.getElementById('autoban-scope').textContent='显示 '+(rows.length?start+1:0)+'-'+Math.min(start+pageRows.length,rows.length)+' / '+rows.length+' 个自动禁用账号';
  document.getElementById('autoban-page-label').textContent=autobanPage+' / '+pages;
  document.getElementById('autoban-prev').disabled=autobanPage<=1;
  document.getElementById('autoban-next').disabled=autobanPage>=pages;
  document.getElementById('autobans').innerHTML=pageRows.map(r=>'<tr>'+
    td(esc(r.source||r.auth_id||'-'))+td(esc(r.auth_index||'-'))+td(esc(r.window||'-'))+td(esc(r.reason||'-'))+
    td(esc(r.banned_at_text||'-'))+td(esc(autobanResetText(r)))+td(esc(autobanRemainingText(r)),'num')+
    td(pct(r.primary_used_percent),'num')+td(pct(r.secondary_used_percent),'num')+'</tr>').join('') || '<tr><td colspan="9" class="muted">当前没有被自动禁用的 Codex 账号</td></tr>';
}
function renderModels(target,rows){
  document.getElementById(target).innerHTML=rows.map(r=>'<tr>'+td(esc(r.model))+td(esc(r.alias))+td(esc(r.provider))+td(fmt(r.requests),'num')+td(compact(r.total_tokens),'num')+td(costCell(r),'num')+td(perfCell(r),'num')+td(compact(r.input_tokens),'num')+td(compact(r.output_tokens),'num')+td(compact(cacheTokens(r)),'num')+td(pct(cacheRate(r)),'num')+'</tr>').join('') || '<tr><td colspan="11" class="muted">暂无模型数据</td></tr>';
}
function renderRecent(target,rows,mode){
  document.getElementById(target).innerHTML=rows.map(r=>{
    const who=mode==='provider'?firstText(r.provider,r.source,r.auth_index,'unknown'):firstText(r.auth_index,r.source,'unknown');
    const model=firstText(r.alias,r.model,'-');
    const cache=cacheTokens(r);
    const price=r.cost_available||Number(r.cost_usd||0)>0?'<span class="cost-pill">'+money(r.cost_usd)+'</span>':'<span class="cost-weak">缺价格</span>';
    const statusClass=r.failed?'danger':((Number(r.status_code||200)>=400)?'warn':'ok');
    const detail=[tierText(r.service_tier),r.price_detail||'缺价格'].filter(Boolean).join(' · ');
    const reasoning=r.reasoning_effort?('推理 '+r.reasoning_effort+' · '):'';
    return '<tr>'+
      td('<div class="recent-model"><div class="recent-primary"><span class="model-chip" title="'+esc(firstText(r.model,model))+'">'+esc(model)+'</span></div><span class="recent-sub" title="'+esc(who+' · '+(r.time||'-'))+'">'+esc(who)+' · '+esc(r.time||'-')+'</span></div>','recent-model')+
      td('<div class="recent-badges"><span class="latency-pill '+latencyTone(r.latency_ms)+'">'+fmtLatencyMs(r.latency_ms)+'</span><span class="latency-pill '+latencyTone(r.ttft_ms)+'">'+fmtLatencyMs(r.ttft_ms)+'</span></div><span class="token-sub">流 · '+esc(throughput(r))+'</span>')+
      td('<span class="token-main">'+fmt(r.input_tokens)+' / '+fmt(r.output_tokens)+'</span><span class="token-sub">缓存 ↓ '+compact(cache)+(r.reasoning_tokens?(' · 推理 '+compact(r.reasoning_tokens)):'')+'</span>','num')+
      td(price,'num')+
      td('<span class="detail-main">'+esc(detail)+'</span><span class="detail-sub">'+reasoning+'<span class="status-pill '+statusClass+'">'+esc(requestStatusText(r))+'</span></span>')+
    '</tr>';
  }).join('') || '<tr><td colspan="5" class="muted">暂无请求记录</td></tr>';
}
applyLocale();
load();

`
