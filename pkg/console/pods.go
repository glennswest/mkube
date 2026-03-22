package console

import "net/http"

func (c *Console) handlePods(w http.ResponseWriter, r *http.Request) {
	body := `<h1>Pods</h1>
<div class="toolbar">
  <select id="ns-filter" onchange="load()"><option value="">All Namespaces</option></select>
  <select id="status-filter" onchange="load()"><option value="">All Status</option><option>Running</option><option>Pending</option><option>Failed</option><option>Succeeded</option></select>
  <input type="text" id="search" placeholder="Search..." oninput="load()">
</div>
<table><thead><tr><th>Name</th><th>Namespace</th><th>Status</th><th>Node</th><th>IP</th><th>Image</th><th>Restarts</th><th>Age</th></tr></thead>
<tbody id="tbl"><tr><td colspan="8" class="loading">Loading...</td></tr></tbody></table>`

	js := `
async function load(){
  const data=await apiGet(API+'/api/v1/pods');
  const items=data?.items||[];
  const nsFilter=document.getElementById('ns-filter').value;
  const stFilter=document.getElementById('status-filter').value.toLowerCase();
  const search=document.getElementById('search').value.toLowerCase();

  // populate namespace filter
  const nss=[...new Set(items.map(p=>p.metadata.namespace||'default'))].sort();
  const sel=document.getElementById('ns-filter');
  if(sel.options.length<=1){
    nss.forEach(n=>{const o=document.createElement('option');o.value=n;o.text=n;sel.add(o);});
  }

  const filtered=items.filter(p=>{
    const ns=p.metadata.namespace||'default';
    const st=(p.status?.phase||'').toLowerCase();
    const nm=(p.metadata.name||'').toLowerCase();
    if(nsFilter && ns!==nsFilter) return false;
    if(stFilter && st!==stFilter) return false;
    if(search && !nm.includes(search) && !ns.includes(search)) return false;
    return true;
  });

  const tb=document.getElementById('tbl');
  tb.innerHTML='';
  if(filtered.length===0){
    tb.innerHTML='<tr><td colspan="8" class="muted" style="text-align:center;padding:16px">No pods found</td></tr>';
    return;
  }
  filtered.forEach(p=>{
    const ns=p.metadata.namespace||'default';
    const restarts=p.status?.containerStatuses?.[0]?.restartCount||0;
    const node=p.metadata?.annotations?.['vkube.io/node']||'—';
    tb.innerHTML+='<tr><td><a href="pods/'+encodeURIComponent(ns)+'/'+encodeURIComponent(p.metadata.name)+'">'+escapeHtml(p.metadata.name)+'</a></td>'
      +'<td>'+escapeHtml(ns)+'</td>'
      +'<td>'+statusBadge(p.status?.phase)+'</td>'
      +'<td>'+escapeHtml(node)+'</td>'
      +'<td>'+escapeHtml(p.status?.podIP||'—')+'</td>'
      +'<td>'+shortImage(p.spec?.containers?.[0]?.image)+'</td>'
      +'<td>'+restarts+'</td>'
      +'<td>'+timeSince(p.status?.startTime||p.metadata?.creationTimestamp)+'</td></tr>';
  });
  initSort('tbl');
  reapplySort('tbl');
}
load(); setInterval(load,15000);
`
	write(w, c.pageWithJS("Pods", "Pods", body, js))
}

func (c *Console) handlePodDetail(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	name := r.PathValue("name")

	body := `<h1>Pod: ` + name + `</h1>
<div class="card"><h3>Info</h3><div class="kv" id="info"><div class="loading">Loading...</div></div></div>
<div class="card"><h3>Containers</h3><div id="containers"></div></div>
<div class="card"><h3>Labels</h3><div class="kv" id="labels"></div></div>
<div class="card"><h3>Annotations</h3><div class="kv" id="annotations"></div></div>
<div class="flex mt">
  <button class="btn btn-danger" onclick="deletePod()">Delete Pod</button>
</div>
<div class="card mt"><h3>Logs</h3><div class="terminal" id="logs"></div></div>`

	js := `
const podNs=` + jsStr(ns) + `,podName=` + jsStr(name) + `;
async function load(){
  const pod=await apiGet(API+'/api/v1/namespaces/'+podNs+'/pods/'+podName);
  if(!pod){document.getElementById('info').innerHTML='<div class="muted">Pod not found</div>';return;}
  const s=pod.status||{};
  document.getElementById('info').innerHTML=
    kv('Status',s.phase)+kv('Node',pod.metadata?.annotations?.['vkube.io/node']||'—')
    +kv('IP',s.podIP||'—')+kv('Started',s.startTime||'—')
    +kv('Age',timeSince(s.startTime||pod.metadata?.creationTimestamp));

  const cb=document.getElementById('containers');
  cb.innerHTML='';
  (pod.spec?.containers||[]).forEach((c,i)=>{
    const cs=s.containerStatuses?.[i]||{};
    cb.innerHTML+='<div style="margin-bottom:8px"><strong>'+escapeHtml(c.name)+'</strong> — '+shortImage(c.image)
      +' '+statusBadge(cs.ready?'Ready':'NotReady')+' restarts: '+(cs.restartCount||0)+'</div>';
  });

  const lb=document.getElementById('labels');
  lb.innerHTML=Object.entries(pod.metadata?.labels||{}).map(([k,v])=>'<div class="k">'+escapeHtml(k)+'</div><div class="v">'+escapeHtml(v)+'</div>').join('')||'<div class="muted">None</div>';

  const ab=document.getElementById('annotations');
  ab.innerHTML=Object.entries(pod.metadata?.annotations||{}).map(([k,v])=>'<div class="k">'+escapeHtml(k)+'</div><div class="v">'+escapeHtml(v)+'</div>').join('')||'<div class="muted">None</div>';

  const logs=await fetch(API+'/api/v1/namespaces/'+podNs+'/pods/'+podName+'/log').then(r=>r.text()).catch(()=>'');
  document.getElementById('logs').innerHTML=ansiToHtml(logs)||'<span class="muted">No logs</span>';
}

function kv(k,v){ return '<div class="k">'+escapeHtml(k)+'</div><div class="v">'+(typeof v==='string'&&v.startsWith('<')?v:escapeHtml(String(v||'—')))+'</div>'; }

async function deletePod(){
  if(!confirm('Delete pod '+podName+'?')) return;
  await apiDelete(API+'/api/v1/namespaces/'+podNs+'/pods/'+podName);
  location.href='pods';
}
load(); setInterval(load,15000);
`
	write(w, c.pageWithJS("Pod: "+name, "Pods", body, js))
}
