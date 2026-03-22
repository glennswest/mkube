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
  <table><thead><tr><th>Name</th><th>Namespace</th><th>Status</th><th>Size</th></tr></thead><tbody id="pvcs-tbl"></tbody></table>
</div>
<div id="cdroms" class="tab-content">
  <table><thead><tr><th>Name</th><th>Version</th><th>Phase</th><th>Subscribers</th></tr></thead><tbody id="cdroms-tbl"></tbody></table>
</div>
<div id="disks" class="tab-content">
  <table><thead><tr><th>Name</th><th>Host</th><th>Size</th><th>Phase</th><th>Source</th></tr></thead><tbody id="disks-tbl"></tbody></table>
</div>
<div class="card mt"><h3>Disk Capacity</h3><div class="kv" id="capacity"></div></div>`

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
  (pvcs?.items||[]).forEach(p=>{
    const s=p.spec||{};
    pt.innerHTML+='<tr><td>'+escapeHtml(p.metadata.name)+'</td><td>'+escapeHtml(p.metadata.namespace||'default')+'</td><td>'+statusBadge(p.status?.phase||'Bound')+'</td><td>'+escapeHtml(s.resources?.requests?.storage||'—')+'</td></tr>';
  });

  // CDROMs
  const ct=document.getElementById('cdroms-tbl');
  ct.innerHTML='';
  (cdroms?.items||[]).forEach(c=>{
    const s=c.spec||{};
    ct.innerHTML+='<tr><td>'+escapeHtml(c.metadata.name)+'</td><td>'+escapeHtml(s.version||'—')+'</td><td>'+statusBadge(s.phase||c.status?.phase||'—')+'</td><td>'+(s.subscribers?.length||0)+'</td></tr>';
  });

  // Disks
  const dt=document.getElementById('disks-tbl');
  dt.innerHTML='';
  (disks?.items||[]).forEach(d=>{
    const s=d.spec||{};
    dt.innerHTML+='<tr><td>'+escapeHtml(d.metadata.name)+'</td><td>'+escapeHtml(s.host||'—')+'</td><td>'+escapeHtml(s.size||'—')+'</td><td>'+statusBadge(s.phase||d.status?.phase||'—')+'</td><td>'+escapeHtml(s.source||'—')+'</td></tr>';
  });

  // Capacity
  const cb=document.getElementById('capacity');
  if(cap){
    cb.innerHTML=Object.entries(cap).map(([k,v])=>'<div class="k">'+escapeHtml(k)+'</div><div class="v">'+escapeHtml(String(v))+'</div>').join('');
  }
}
load(); setInterval(load,30000);
`
	write(w, c.pageWithJS("Storage", "Storage", body, js))
}
