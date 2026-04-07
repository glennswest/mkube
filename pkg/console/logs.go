package console

import "net/http"

func (c *Console) handleLogs(w http.ResponseWriter, r *http.Request) {
	body := `<h1>Logs</h1>
<div id="log-list">
  <div class="toolbar">
    <select id="node-filter"><option value="">All Nodes</option></select>
    <input type="text" id="source-search" placeholder="Filter sources..." oninput="filterSources()">
  </div>
  <table><thead><tr><th>Source</th><th>Namespace</th><th>Status</th><th>Last Log</th></tr></thead>
  <tbody id="sources-tbl"><tr><td colspan="4" class="loading">Loading...</td></tr></tbody></table>
</div>
<div id="log-detail" style="display:none">
  <div class="toolbar">
    <button class="btn" onclick="showList()">&larr; Back</button>
    <span id="log-detail-title" style="font-weight:600;font-size:14px"></span>
    <div style="flex:1"></div>
    <select id="level-filter" onchange="filterLogs()" style="min-width:80px">
      <option value="">All Levels</option>
      <option value="ERROR">Error</option>
      <option value="WARN">Warn+</option>
      <option value="INFO" selected>Info+</option>
      <option value="DEBUG">Debug</option>
    </select>
    <input type="text" id="log-search" placeholder="Search..." oninput="debounce(filterLogs,200)">
    <label style="color:#888;font-size:12px"><input type="checkbox" id="follow" checked> Follow</label>
    <button class="btn btn-primary" onclick="loadDetail()">Refresh</button>
  </div>
  <div style="margin-bottom:8px;display:flex;gap:12px;flex-wrap:wrap;font-size:11px;color:var(--comment)">
    <span style="color:var(--fg);font-weight:600">Fields:</span>
    <label><input type="checkbox" id="fld-timestamp" checked onchange="filterLogs()"> timestamp</label>
    <label><input type="checkbox" id="fld-level" checked onchange="filterLogs()"> level</label>
    <label><input type="checkbox" id="fld-message" checked onchange="filterLogs()" disabled> message</label>
    <label><input type="checkbox" id="fld-target" onchange="filterLogs()"> target</label>
    <label><input type="checkbox" id="fld-extras" checked onchange="filterLogs()"> extra fields</label>
    <label><input type="checkbox" id="fld-path" onchange="filterLogs()"> path</label>
    <label><input type="checkbox" id="fld-pid" onchange="filterLogs()"> pid</label>
  </div>
  <div class="terminal" id="logs" style="max-height:600px"></div>
</div>`

	js := `
var _pods=[];
var _rawLines=[];
var _detailKey='';

var _levelOrder={ERROR:0,WARN:1,INFO:2,DEBUG:3,TRACE:4};

async function init(){
  var [nodes,pods]=await Promise.all([
    apiGet(API+'/api/v1/nodes'),
    apiGet(API+'/api/v1/pods'),
  ]);
  _pods=pods?.items||[];

  var nf=document.getElementById('node-filter');
  (nodes?.items||[]).forEach(function(n){
    var o=document.createElement('option');o.value=n.metadata.name;o.text=n.metadata.name;nf.add(o);
  });
  nf.onchange=function(){ renderSources(); };

  await fetchLastLines();
  renderSources();
}

var _lastLines={};
async function fetchLastLines(){
  var fetches=_pods.map(function(p){
    var ns=p.metadata.namespace||'default';
    var name=p.metadata.name;
    var key=ns+'/'+name;
    return fetch(API+'/api/v1/namespaces/'+ns+'/pods/'+name+'/log')
      .then(function(r){return r.text();})
      .then(function(txt){
        var lines=txt.trim().split('\n').filter(function(l){return l.length>0;});
        _lastLines[key]=lines.length>0?lines[lines.length-1]:'';
      })
      .catch(function(){_lastLines[key]='';});
  });
  await Promise.all(fetches);
}

function formatPreview(raw){
  var parsed=parseLogLine(raw);
  if(!parsed.json) return raw.length>120?raw.substring(0,120)+'...':raw;
  var msg=parsed.message||'';
  if(msg.length>100) msg=msg.substring(0,100)+'...';
  var lvl=parsed.level||'';
  var ts=shortTime(parsed.timestamp);
  var parts=[];
  if(ts) parts.push(ts);
  if(lvl) parts.push(lvl);
  parts.push(msg);
  return parts.join(' ');
}

function renderSources(){
  var nodeVal=document.getElementById('node-filter').value;
  var search=(document.getElementById('source-search').value||'').toLowerCase();
  var tbl=document.getElementById('sources-tbl');
  var rows=[];

  _pods.forEach(function(p){
    var ns=p.metadata.namespace||'default';
    var name=p.metadata.name;
    var pNode=p.metadata?.annotations?.['vkube.io/node']||'';
    if(nodeVal && pNode!==nodeVal) return;
    var key=ns+'/'+name;
    if(search && key.toLowerCase().indexOf(search)<0) return;

    var phase=p.status?.phase||'Unknown';
    var last=_lastLines[key]||'';
    var displayLast=formatPreview(last);

    rows.push('<tr>'+
      '<td><a href="#" onclick="showDetail(\''+escapeHtml(key)+'\');return false" style="color:var(--cyan);text-decoration:none;font-weight:600">'+escapeHtml(name)+'</a></td>'+
      '<td>'+escapeHtml(ns)+'</td>'+
      '<td>'+statusBadge(phase)+'</td>'+
      '<td style="font-size:11px;color:var(--comment);max-width:500px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">'+escapeHtml(displayLast)+'</td>'+
      '</tr>');
  });

  if(rows.length===0){
    tbl.innerHTML='<tr><td colspan="4" class="muted" style="text-align:center;padding:12px">No log sources found</td></tr>';
  } else {
    tbl.innerHTML=rows.join('');
  }
  initSort('sources-tbl');
}

function filterSources(){ renderSources(); }

function showDetail(key){
  _detailKey=key;
  document.getElementById('log-list').style.display='none';
  document.getElementById('log-detail').style.display='block';
  document.getElementById('log-detail-title').textContent=key;
  document.getElementById('logs').innerHTML='<span class="muted">Loading...</span>';
  loadDetail();
}

function showList(){
  document.getElementById('log-detail').style.display='none';
  document.getElementById('log-list').style.display='block';
  _detailKey='';
  _rawLines=[];
}

async function loadDetail(){
  if(!_detailKey) return;
  var parts=_detailKey.split('/');
  var ns=parts[0],name=parts[1];
  var txt=await fetch(API+'/api/v1/namespaces/'+ns+'/pods/'+name+'/log').then(function(r){return r.text();}).catch(function(){return '';});
  _rawLines=txt.split('\n').filter(function(l){return l.length>0;});
  filterLogs();
}

// Parse a raw log line. Handles:
// - Pure JSON: {"timestamp":..., "level":..., "fields":{"message":...}, "target":...}
// - RouterOS prefix + JSON: "2026-04-06 15:51:47  gt_dns_microdns: {json...}"
// - Plain text
function parseLogLine(raw){
  // Try to extract JSON from the line
  var jsonIdx=raw.indexOf('{');
  if(jsonIdx<0) return {json:false,raw:raw};
  var prefix=raw.substring(0,jsonIdx).trim();
  var jsonStr=raw.substring(jsonIdx);
  try{
    var obj=JSON.parse(jsonStr);
    return {
      json:true,
      prefix:prefix,
      timestamp:obj.timestamp||'',
      level:(obj.level||'').toUpperCase(),
      message:(obj.fields&&obj.fields.message)||obj.msg||obj.message||'',
      target:obj.target||'',
      fields:obj.fields||{},
      raw:raw
    };
  }catch(e){
    return {json:false,raw:raw};
  }
}

// Simplify ISO timestamp to HH:MM:SS
function shortTime(ts){
  if(!ts) return '';
  // "2026-04-06T20:51:47.606406Z" -> "20:51:47"
  var m=ts.match(/T(\d{2}:\d{2}:\d{2})/);
  return m?m[1]:ts;
}

var _levelColors={ERROR:'var(--red)',WARN:'var(--orange)',INFO:'var(--green)',DEBUG:'var(--comment)',TRACE:'var(--comment)'};

function formatLogLine(parsed){
  if(!parsed.json) return escapeHtml(parsed.raw);

  var showTs=document.getElementById('fld-timestamp').checked;
  var showLevel=document.getElementById('fld-level').checked;
  var showTarget=document.getElementById('fld-target').checked;
  var showExtras=document.getElementById('fld-extras').checked;
  var showPath=document.getElementById('fld-path').checked;
  var showPid=document.getElementById('fld-pid').checked;

  var parts=[];

  if(showTs){
    var ts=shortTime(parsed.timestamp);
    if(ts) parts.push('<span style="color:var(--comment)">'+escapeHtml(ts)+'</span>');
  }

  if(showLevel&&parsed.level){
    var lc=_levelColors[parsed.level]||'var(--fg)';
    parts.push('<span style="color:'+lc+';font-weight:600">'+escapeHtml(parsed.level.substring(0,5).padEnd(5))+'</span>');
  }

  parts.push('<span style="color:var(--fg)">'+escapeHtml(parsed.message)+'</span>');

  if(showTarget&&parsed.target){
    parts.push('<span style="color:var(--purple)">['+escapeHtml(parsed.target)+']</span>');
  }

  // Extra fields from fields object (excluding message, path, pid which have their own toggles)
  var skipKeys={message:1};
  if(!showPath) skipKeys.path=1;
  if(!showPid) skipKeys.pid=1;
  var extras=[];
  if(parsed.fields){
    Object.keys(parsed.fields).forEach(function(k){
      if(skipKeys[k]) return;
      if(!showExtras&&k!=='path'&&k!=='pid') return;
      if(k==='path'&&!showPath) return;
      if(k==='pid'&&!showPid) return;
      extras.push(escapeHtml(k)+'='+escapeHtml(String(parsed.fields[k])));
    });
  }
  if(extras.length>0){
    parts.push('<span style="color:var(--comment)">'+extras.join(' ')+'</span>');
  }

  return parts.join(' ');
}

function filterLogs(){
  var search=(document.getElementById('log-search').value||'').toLowerCase();
  var levelFilter=document.getElementById('level-filter').value;
  var minLevel=levelFilter?(_levelOrder[levelFilter]!=null?_levelOrder[levelFilter]:99):99;
  var el=document.getElementById('logs');

  if(_rawLines.length===0){ el.innerHTML='<span class="muted">No logs</span>'; return; }

  var html=[];
  _rawLines.forEach(function(raw){
    if(search && raw.toLowerCase().indexOf(search)<0) return;

    var parsed=parseLogLine(raw);

    // Level filter
    if(parsed.json && parsed.level && levelFilter){
      var lineLevel=_levelOrder[parsed.level];
      if(lineLevel!=null && lineLevel>minLevel) return;
    }

    html.push(formatLogLine(parsed));
  });

  el.innerHTML=html.length>0?html.join('\n'):'<span class="muted">No matching lines</span>';
  if(document.getElementById('follow').checked) el.scrollTop=el.scrollHeight;
}

init();
`
	write(w, c.pageWithJS("Logs", "Logs", body, js))
}
