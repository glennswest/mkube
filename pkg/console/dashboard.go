package console

import "net/http"

func (c *Console) handleDashboard(w http.ResponseWriter, r *http.Request) {
	body := `<h1>Cluster Dashboard</h1>
<div class="stats" id="stats"><div class="loading">Loading...</div></div>
<div class="card-grid">
  <div class="card"><h3>Nodes</h3><table><thead><tr><th>Name</th><th>Arch</th><th>IP</th><th>Status</th></tr></thead><tbody id="nodes"></tbody></table></div>
  <div class="card"><h3>Recent Events</h3><div id="events" style="max-height:300px;overflow-y:auto"><div class="loading">Loading...</div></div></div>
</div>
<div class="card"><h3>Consistency</h3><div class="kv" id="consistency"><div class="loading">Loading...</div></div></div>`

	js := `
async function load(){
  const [healthText,nodes,events,consist,pods]=await Promise.all([
    apiGetText(API+'/healthz'),
    apiGet(API+'/api/v1/nodes'),
    apiGet(API+'/api/v1/events'),
    apiGet(API+'/api/v1/consistency'),
    apiGet(API+'/api/v1/pods'),
  ]);

  // Parse healthz plaintext
  let ver='—';
  if(healthText){
    const m=healthText.match(/version:\s*(.+)/);
    if(m) ver=m[1].trim();
  }

  // Stats
  const nl=nodes?.items?.length||0;
  const pl=pods?.items?.length||0;
  const running=pods?.items?.filter(p=>p.status?.phase==='Running').length||0;
  document.getElementById('stats').innerHTML=
    statBox(nl,'Nodes')+statBox(pl,'Pods')+statBox(running,'Running')+statBox(ver,'Version');

  // Nodes table
  const nb=document.getElementById('nodes');
  nb.innerHTML='';
  (nodes?.items||[]).forEach(n=>{
    const addr=n.status?.addresses?.find(a=>a.type==='InternalIP')?.address||'—';
    const arch=n.status?.nodeInfo?.architecture||'—';
    const ready=n.status?.conditions?.find(c=>c.type==='Ready');
    nb.innerHTML+='<tr><td><a href="nodes/'+escapeHtml(n.metadata.name)+'">'+escapeHtml(n.metadata.name)+'</a></td><td>'+escapeHtml(arch)+'</td><td>'+escapeHtml(addr)+'</td><td>'+statusBadge(ready?.status==='True'?'Ready':'NotReady')+'</td></tr>';
  });
  initSort('nodes');
  reapplySort('nodes');

  // Events
  const eb=document.getElementById('events');
  const evts=(events?.items||[]).sort((a,b)=>new Date(b.lastTimestamp||0)-new Date(a.lastTimestamp||0)).slice(0,20);
  eb.innerHTML=evts.length?evts.map(e=>{
    const cls=e.type==='Warning'?'color:#f1fa8c':e.type==='Error'?'color:#e94560':'color:#888';
    return '<div style="padding:3px 0;font-size:12px"><span style="'+cls+';font-weight:600">'+escapeHtml(e.type||'Normal')+'</span> <span class="muted">'+timeSince(e.lastTimestamp)+'</span> '+escapeHtml(e.message||'')+'</div>';
  }).join(''):'<div class="muted" style="padding:8px">No events</div>';

  // Consistency
  const cb=document.getElementById('consistency');
  if(consist){
    let html='';
    for(const[k,v] of Object.entries(consist)){
      if(typeof v==='object') continue;
      html+='<div class="k">'+escapeHtml(k)+'</div><div class="v">'+escapeHtml(String(v))+'</div>';
    }
    cb.innerHTML=html||'<div class="muted">No issues</div>';
  } else {
    cb.innerHTML='<div class="muted">Unavailable</div>';
  }
}

function statBox(val,label){
  return '<div class="stat"><div class="val">'+escapeHtml(String(val))+'</div><div class="lbl">'+escapeHtml(label)+'</div></div>';
}

load(); setInterval(load,30000);
`
	write(w, c.pageWithJS("Dashboard", "Dashboard", body, js))
}
