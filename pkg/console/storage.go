package console

import "net/http"

func (c *Console) handleStorage(w http.ResponseWriter, r *http.Request) {
	body := `<h1>Storage</h1>
<div class="tabs">
  <div class="tab active" onclick="switchTab('pvcs')">PVCs</div>
  <div class="tab" onclick="switchTab('cdroms')">iSCSI CDROMs</div>
  <div class="tab" onclick="switchTab('disks')">iSCSI Disks</div>
</div>
<div id="pvcs" class="tab-content active">
  <table><thead><tr><th>Name</th><th>Namespace</th><th>Status</th><th>Used</th><th>Size</th><th>Created</th></tr></thead><tbody id="pvcs-tbl"><tr><td colspan="6" class="loading">Loading...</td></tr></tbody></table>
</div>
<div id="cdroms" class="tab-content">
  <table><thead><tr><th>Name</th><th>Version</th><th>Phase</th><th>Size</th><th>Subscribers</th><th>Created</th></tr></thead><tbody id="cdroms-tbl"></tbody></table>
</div>
<div id="disks" class="tab-content">
  <table><thead><tr><th>Name</th><th>Host</th><th>Size</th><th>Phase</th><th>Source</th><th>Created</th><th>Last Used</th></tr></thead><tbody id="disks-tbl"></tbody></table>
</div>
<div class="card mt"><h3>Disk Capacity</h3><div class="kv" id="capacity"><div class="loading">Loading...</div></div></div>`

	js := `
function switchTab(id){
  document.querySelectorAll('.tab-content').forEach(e=>e.classList.remove('active'));
  document.querySelectorAll('.tab').forEach(e=>e.classList.remove('active'));
  document.getElementById(id).classList.add('active');
  event.target.classList.add('active');
}

async function load(){
  const [pvcs,cdroms,disks,cap]=await Promise.all([
    apiGet(API+'/api/v1/persistentvolumeclaims'),
    apiGet(API+'/api/v1/iscsi-cdroms'),
    apiGet(API+'/api/v1/iscsi-disks'),
    apiGet(API+'/api/v1/iscsi-disks/capacity'),
  ]);

  // PVCs
  const pt=document.getElementById('pvcs-tbl');
  pt.innerHTML='';
  const pvcItems=pvcs?.items||[];
  if(pvcItems.length===0){pt.innerHTML='<tr><td colspan="6" class="muted" style="text-align:center;padding:8px">No PVCs</td></tr>';}
  else pvcItems.forEach(p=>{
    const s=p.spec||{};
    const st=p.status||{};
    const ann=p.metadata?.annotations||{};
    const usedBytes=ann['vkube.io/used-bytes'];
    const used=usedBytes?fmtBytes(parseInt(usedBytes)):'—';
    const cap=st.capacity?.storage||s.resources?.requests?.storage||'—';
    pt.innerHTML+='<tr><td>'+escapeHtml(p.metadata.name)+'</td><td>'+escapeHtml(p.metadata.namespace||'default')+'</td><td>'+statusBadge(st.phase||'Bound')+'</td><td>'+used+'</td><td>'+escapeHtml(String(cap))+'</td><td>'+timeSince(p.metadata?.creationTimestamp)+'</td></tr>';
  });
  initSort('pvcs-tbl');reapplySort('pvcs-tbl');

  // CDROMs
  const ct=document.getElementById('cdroms-tbl');
  ct.innerHTML='';
  const cdromItems=cdroms?.items||[];
  if(cdromItems.length===0){ct.innerHTML='<tr><td colspan="6" class="muted" style="text-align:center;padding:8px">No CDROMs</td></tr>';}
  else cdromItems.forEach(c=>{
    const s=c.spec||{};
    const st=c.status||{};
    ct.innerHTML+='<tr><td>'+escapeHtml(c.metadata.name)+'</td><td>'+escapeHtml(s.version||'—')+'</td><td>'+statusBadge(s.phase||st.phase||'—')+'</td><td>'+fmtSize(s.size||st.size)+'</td><td>'+(s.subscribers?.length||0)+'</td><td>'+timeSince(c.metadata?.creationTimestamp)+'</td></tr>';
  });
  initSort('cdroms-tbl');reapplySort('cdroms-tbl');

  // Disks
  const dt=document.getElementById('disks-tbl');
  dt.innerHTML='';
  const diskItems=disks?.items||[];
  if(diskItems.length===0){dt.innerHTML='<tr><td colspan="7" class="muted" style="text-align:center;padding:8px">No disks</td></tr>';}
  else diskItems.forEach(d=>{
    const s=d.spec||{};
    const st=d.status||{};
    dt.innerHTML+='<tr><td>'+escapeHtml(d.metadata.name)+'</td><td>'+escapeHtml(s.host||'—')+'</td><td>'+fmtSize(s.size||st.size)+'</td><td>'+statusBadge(s.phase||st.phase||'—')+'</td><td>'+escapeHtml(s.source||'—')+'</td><td>'+timeSince(d.metadata?.creationTimestamp)+'</td><td>'+timeSince(st.lastUsed||s.lastUsed)+'</td></tr>';
  });
  initSort('disks-tbl');reapplySort('disks-tbl');

  // Capacity
  const cb=document.getElementById('capacity');
  if(cap){
    cb.innerHTML=Object.entries(cap).map(([k,v])=>'<div class="k">'+escapeHtml(k)+'</div><div class="v">'+escapeHtml(String(v))+'</div>').join('');
  } else {
    cb.innerHTML='<div class="muted">Unavailable</div>';
  }
}
load(); setInterval(load,30000);
`
	write(w, c.pageWithJS("Storage", "Storage", body, js))
}
