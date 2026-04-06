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
    <input type="text" id="log-search" placeholder="Search..." oninput="debounce(filterLogs,200)">
    <label style="color:#888;font-size:12px"><input type="checkbox" id="follow" checked> Follow</label>
    <button class="btn btn-primary" onclick="loadDetail()">Refresh</button>
  </div>
  <div class="terminal" id="logs" style="max-height:600px"></div>
</div>`

	js := `
var _pods=[];
var _allLogLines='';
var _detailKey='';

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

  // Fetch last log line for each pod in parallel
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
    // Trim timestamp prefix if present for display
    var displayLast=last;
    if(displayLast.length>120) displayLast=displayLast.substring(0,120)+'...';

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
  _allLogLines='';
}

async function loadDetail(){
  if(!_detailKey) return;
  var parts=_detailKey.split('/');
  var ns=parts[0],name=parts[1];
  var logs=await fetch(API+'/api/v1/namespaces/'+ns+'/pods/'+name+'/log').then(function(r){return r.text();}).catch(function(){return '';});
  _allLogLines=logs;
  filterLogs();
}

function filterLogs(){
  var search=(document.getElementById('log-search').value||'').toLowerCase();
  var el=document.getElementById('logs');
  if(!_allLogLines){ el.innerHTML='<span class="muted">No logs</span>'; return; }
  var lines=_allLogLines;
  if(search){
    lines=_allLogLines.split('\n').filter(function(l){return l.toLowerCase().indexOf(search)>=0;}).join('\n');
  }
  el.innerHTML=ansiToHtml(lines)||'<span class="muted">No matching lines</span>';
  if(document.getElementById('follow').checked) el.scrollTop=el.scrollHeight;
}

init();
`
	write(w, c.pageWithJS("Logs", "Logs", body, js))
}
