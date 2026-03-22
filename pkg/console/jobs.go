package console

import "net/http"

func (c *Console) handleJobs(w http.ResponseWriter, r *http.Request) {
	body := `<h1>Job Scheduling</h1>
<div class="tabs">
  <div class="tab active" onclick="switchTab('jobs')">Jobs</div>
  <div class="tab" onclick="switchTab('runners')">Job Runners</div>
  <div class="tab" onclick="switchTab('reservations')">Host Reservations</div>
  <div class="tab" onclick="switchTab('queue')">Queue</div>
</div>

<div id="jobs" class="tab-content active">
  <div class="toolbar">
    <select id="job-ns" onchange="loadJobs()"><option value="">All Namespaces</option></select>
    <select id="job-phase" onchange="loadJobs()"><option value="">All Phases</option><option>Pending</option><option>Provisioning</option><option>Running</option><option>Completed</option><option>Failed</option><option>Cancelled</option></select>
    <button class="btn btn-danger" onclick="deleteSelected()">Delete Selected</button>
  </div>
  <table><thead><tr><th><input type="checkbox" id="select-all" onchange="toggleSelectAll()"></th><th>Name</th><th>Namespace</th><th>Phase</th><th>Pool</th><th>Priority</th><th>BMH</th><th>Age</th><th>Actions</th></tr></thead>
  <tbody id="jobs-tbl"><tr><td colspan="9" class="loading">Loading...</td></tr></tbody></table>
  <div class="card mt" id="job-log-panel" style="display:none">
    <h3 id="job-log-title">Job Logs</h3>
    <div class="terminal" id="job-logs" style="max-height:400px"></div>
  </div>
</div>

<div id="runners" class="tab-content">
  <div class="flex mb">
    <button class="btn btn-primary" onclick="showCreateRunner()">Create Runner</button>
  </div>
  <table><thead><tr><th>Name</th><th>Pool</th><th>Template</th><th>Max Concurrent</th><th>Idle Timeout</th><th>Reclaim</th><th>Status</th><th>Actions</th></tr></thead>
  <tbody id="runners-tbl"></tbody></table>
</div>

<div id="reservations" class="tab-content">
  <div class="flex mb">
    <button class="btn btn-primary" onclick="showCreateRes()">Create Reservation</button>
  </div>
  <table><thead><tr><th>Name</th><th>Namespace</th><th>BMH Ref</th><th>Pool</th><th>Owner</th><th>Purpose</th><th>Actions</th></tr></thead>
  <tbody id="res-tbl"></tbody></table>
</div>

<div id="queue" class="tab-content">
  <table><thead><tr><th>Position</th><th>Job</th><th>Namespace</th><th>Pool</th><th>Priority</th><th>Phase</th><th>Assigned Host</th><th>Queued</th></tr></thead>
  <tbody id="queue-tbl"></tbody></table>
</div>

<!-- Create Runner Modal -->
<div class="modal-overlay" id="runner-modal">
<div class="modal">
  <h3>Create Job Runner</h3>
  <div class="kv mb">
    <div class="k">Name</div><div class="v"><input type="text" id="jr-name" style="width:100%"></div>
    <div class="k">Pool</div><div class="v"><input type="text" id="jr-pool" value="build" style="width:100%"></div>
    <div class="k">Template</div><div class="v"><input type="text" id="jr-template" placeholder="fcos/agent-runner" style="width:100%"></div>
    <div class="k">Max Concurrent</div><div class="v"><input type="text" id="jr-max" value="10" style="width:100%"></div>
    <div class="k">Idle Timeout (s)</div><div class="v"><input type="text" id="jr-idle" value="300" style="width:100%"></div>
    <div class="k">Reclaim Policy</div><div class="v"><select id="jr-reclaim" style="width:100%"><option>PowerOff</option><option>Delete</option><option>Retain</option></select></div>
  </div>
  <div class="actions">
    <button class="btn" onclick="hideModal('runner-modal')">Cancel</button>
    <button class="btn btn-primary" onclick="createRunner()">Create</button>
  </div>
</div></div>

<!-- Create Reservation Modal -->
<div class="modal-overlay" id="res-modal">
<div class="modal">
  <h3>Create Host Reservation</h3>
  <div class="kv mb">
    <div class="k">Name</div><div class="v"><input type="text" id="hr-name" style="width:100%"></div>
    <div class="k">Namespace</div><div class="v"><input type="text" id="hr-ns" value="default" style="width:100%"></div>
    <div class="k">BMH Ref</div><div class="v"><input type="text" id="hr-bmh" placeholder="server1" style="width:100%"></div>
    <div class="k">Pool</div><div class="v"><input type="text" id="hr-pool" value="build" style="width:100%"></div>
    <div class="k">Owner</div><div class="v"><input type="text" id="hr-owner" value="ci" style="width:100%"></div>
    <div class="k">Purpose</div><div class="v"><input type="text" id="hr-purpose" style="width:100%"></div>
  </div>
  <div class="actions">
    <button class="btn" onclick="hideModal('res-modal')">Cancel</button>
    <button class="btn btn-primary" onclick="createRes()">Create</button>
  </div>
</div></div>`

	js := `
function switchTab(id){
  document.querySelectorAll('.tab-content').forEach(e=>e.classList.remove('active'));
  document.querySelectorAll('.tab').forEach(e=>e.classList.remove('active'));
  document.getElementById(id).classList.add('active');
  event.target.classList.add('active');
}

function showCreateRunner(){ document.getElementById('runner-modal').classList.add('show'); }
function showCreateRes(){ document.getElementById('res-modal').classList.add('show'); }
function hideModal(id){ document.getElementById(id).classList.remove('show'); }

// ── Select all / Delete selected ──
function toggleSelectAll(){
  const checked=document.getElementById('select-all').checked;
  document.querySelectorAll('#jobs-tbl input[type=checkbox]').forEach(cb=>{cb.checked=checked;});
}

async function deleteSelected(){
  const boxes=document.querySelectorAll('#jobs-tbl input[type=checkbox]:checked');
  if(boxes.length===0){alert('No jobs selected');return;}
  if(!confirm('Delete '+boxes.length+' selected job(s)?')) return;
  for(const cb of boxes){
    const ns=cb.dataset.ns;
    const name=cb.dataset.name;
    await apiDelete(API+'/api/v1/namespaces/'+ns+'/jobs/'+name);
  }
  document.getElementById('select-all').checked=false;
  loadJobs();
}

// ── Inline log viewing ──
let _selectedJob=null;

function selectJob(ns,name){
  const key=ns+'/'+name;
  // Toggle off if clicking same row
  if(_selectedJob===key){
    _selectedJob=null;
    document.getElementById('job-log-panel').style.display='none';
    document.querySelectorAll('#jobs-tbl tr').forEach(r=>r.classList.remove('selected'));
    return;
  }
  _selectedJob=key;
  // Highlight selected row
  document.querySelectorAll('#jobs-tbl tr').forEach(r=>r.classList.remove('selected'));
  const rows=document.querySelectorAll('#jobs-tbl tr');
  rows.forEach(r=>{if(r.dataset.jobKey===key) r.classList.add('selected');});
  // Load logs
  document.getElementById('job-log-panel').style.display='block';
  document.getElementById('job-log-title').textContent='Logs: '+key;
  document.getElementById('job-logs').innerHTML='<span class="muted">Loading...</span>';
  fetch(API+'/api/v1/namespaces/'+encodeURIComponent(ns)+'/jobs/'+encodeURIComponent(name)+'/logs')
    .then(r=>r.text())
    .then(text=>{
      if(_selectedJob!==key) return;
      document.getElementById('job-logs').innerHTML=ansiToHtml(text)||'<span class="muted">No logs available</span>';
      const el=document.getElementById('job-logs');
      el.scrollTop=el.scrollHeight;
    })
    .catch(()=>{
      if(_selectedJob!==key) return;
      document.getElementById('job-logs').innerHTML='<span class="muted">Failed to load logs</span>';
    });
}

// ── Jobs ──
async function loadJobs(){
  const data=await apiGet(API+'/api/v1/jobs');
  const nsFilter=document.getElementById('job-ns').value;
  const phaseFilter=document.getElementById('job-phase').value.toLowerCase();
  let items=data?.items||[];
  if(nsFilter) items=items.filter(j=>j.metadata.namespace===nsFilter);
  if(phaseFilter) items=items.filter(j=>(j.status?.phase||j.spec?.phase||'').toLowerCase()===phaseFilter);

  // Populate ns filter
  const nss=[...new Set((data?.items||[]).map(j=>j.metadata.namespace||'default'))].sort();
  const sel=document.getElementById('job-ns');
  if(sel.options.length<=1) nss.forEach(n=>{const o=document.createElement('option');o.value=n;o.text=n;sel.add(o);});

  const tb=document.getElementById('jobs-tbl');
  tb.innerHTML='';
  if(items.length===0){tb.innerHTML='<tr><td colspan="9" class="muted" style="text-align:center;padding:16px">No jobs</td></tr>';return;}
  items.forEach(j=>{
    const ns=j.metadata.namespace||'default';
    const s=j.spec||{};
    const st=j.status||{};
    const phase=st.phase||s.phase||'Pending';
    const bmh=st.assignedHost||s.assignedHost||'—';
    const canCancel=['pending','provisioning','running'].includes(phase.toLowerCase());
    const key=ns+'/'+j.metadata.name;
    const selected=_selectedJob===key?' selected':'';
    tb.innerHTML+='<tr data-job-key="'+escapeHtml(key)+'" class="job-row'+selected+'" onclick="selectJob(\''+encodeURIComponent(ns)+'\',\''+escapeHtml(j.metadata.name)+'\')" style="cursor:pointer"><td onclick="event.stopPropagation()"><input type="checkbox" data-ns="'+escapeHtml(ns)+'" data-name="'+escapeHtml(j.metadata.name)+'"></td><td>'+escapeHtml(j.metadata.name)+'</td><td>'+escapeHtml(ns)+'</td>'
      +'<td>'+statusBadge(phase)+'</td><td>'+escapeHtml(s.pool||'—')+'</td>'
      +'<td>'+(s.priority||0)+'</td><td>'+escapeHtml(bmh)+'</td>'
      +'<td>'+timeSince(j.metadata?.creationTimestamp)+'</td>'
      +'<td onclick="event.stopPropagation()">'+(canCancel?'<button class="btn btn-danger" onclick="cancelJob(\''+encodeURIComponent(ns)+'\',\''+escapeHtml(j.metadata.name)+'\')">Cancel</button> ':'')
      +'<button class="btn btn-danger" onclick="delJob(\''+encodeURIComponent(ns)+'\',\''+escapeHtml(j.metadata.name)+'\')">Delete</button></td></tr>';
  });
  initSort('jobs-tbl');reapplySort('jobs-tbl');
}

async function cancelJob(ns,name){
  if(!confirm('Cancel job '+name+'?')) return;
  await apiPost(API+'/api/v1/namespaces/'+ns+'/jobs/'+name+'/cancel');
  loadJobs();
}

async function delJob(ns,name){
  if(!confirm('Delete job '+name+'?')) return;
  await apiDelete(API+'/api/v1/namespaces/'+ns+'/jobs/'+name);
  if(_selectedJob===decodeURIComponent(ns)+'/'+name){
    _selectedJob=null;
    document.getElementById('job-log-panel').style.display='none';
  }
  loadJobs();
}

// ── Job Runners ──
async function loadRunners(){
  const data=await apiGet(API+'/api/v1/jobrunners');
  const tb=document.getElementById('runners-tbl');
  tb.innerHTML='';
  const items=data?.items||[];
  if(items.length===0){tb.innerHTML='<tr><td colspan="8" class="muted" style="text-align:center;padding:8px">No runners</td></tr>';return;}
  items.forEach(r=>{
    const s=r.spec||{};
    const st=r.status||{};
    tb.innerHTML+='<tr><td>'+escapeHtml(r.metadata.name)+'</td>'
      +'<td>'+escapeHtml(s.pool||'—')+'</td>'
      +'<td>'+escapeHtml(s.template||s.bootConfigRef||'—')+'</td>'
      +'<td>'+(s.maxConcurrent||1)+'</td>'
      +'<td>'+(s.idleTimeout||0)+'s</td>'
      +'<td>'+escapeHtml(s.reclaimPolicy||'PowerOff')+'</td>'
      +'<td>'+statusBadge(st.state||'Active')+'</td>'
      +'<td><button class="btn btn-danger" onclick="delRunner(\''+escapeHtml(r.metadata.name)+'\')">Delete</button></td></tr>';
  });
  initSort('runners-tbl');reapplySort('runners-tbl');
}

async function createRunner(){
  const body={
    apiVersion:'v1',kind:'JobRunner',
    metadata:{name:document.getElementById('jr-name').value},
    spec:{
      pool:document.getElementById('jr-pool').value,
      template:document.getElementById('jr-template').value,
      maxConcurrent:parseInt(document.getElementById('jr-max').value)||10,
      idleTimeout:parseInt(document.getElementById('jr-idle').value)||300,
      reclaimPolicy:document.getElementById('jr-reclaim').value,
    }
  };
  await apiPost(API+'/api/v1/jobrunners',body);
  hideModal('runner-modal');
  loadRunners();
}

async function delRunner(name){
  if(!confirm('Delete runner '+name+'?')) return;
  await apiDelete(API+'/api/v1/jobrunners/'+name);
  loadRunners();
}

// ── Host Reservations ──
async function loadRes(){
  const data=await apiGet(API+'/api/v1/hostreservations');
  const tb=document.getElementById('res-tbl');
  tb.innerHTML='';
  const items=data?.items||[];
  if(items.length===0){tb.innerHTML='<tr><td colspan="7" class="muted" style="text-align:center;padding:8px">No reservations</td></tr>';return;}
  items.forEach(h=>{
    const ns=h.metadata.namespace||'default';
    const s=h.spec||{};
    tb.innerHTML+='<tr><td>'+escapeHtml(h.metadata.name)+'</td><td>'+escapeHtml(ns)+'</td>'
      +'<td>'+escapeHtml(s.bmhRef||'—')+'</td><td>'+escapeHtml(s.pool||'—')+'</td>'
      +'<td>'+escapeHtml(s.owner||'—')+'</td><td>'+escapeHtml(s.purpose||'—')+'</td>'
      +'<td><button class="btn btn-danger" onclick="delRes(\''+encodeURIComponent(ns)+'\',\''+escapeHtml(h.metadata.name)+'\')">Delete</button></td></tr>';
  });
  initSort('res-tbl');reapplySort('res-tbl');
}

async function createRes(){
  const ns=document.getElementById('hr-ns').value||'default';
  const body={
    apiVersion:'v1',kind:'HostReservation',
    metadata:{name:document.getElementById('hr-name').value,namespace:ns},
    spec:{
      bmhRef:document.getElementById('hr-bmh').value,
      pool:document.getElementById('hr-pool').value,
      owner:document.getElementById('hr-owner').value,
      purpose:document.getElementById('hr-purpose').value,
    }
  };
  await apiPost(API+'/api/v1/namespaces/'+ns+'/hostreservations',body);
  hideModal('res-modal');
  loadRes();
}

async function delRes(ns,name){
  if(!confirm('Delete reservation '+name+'?')) return;
  await apiDelete(API+'/api/v1/namespaces/'+ns+'/hostreservations/'+name);
  loadRes();
}

// ── Queue (formatted table) ──
async function loadQueue(){
  const data=await apiGet(API+'/api/v1/jobqueue');
  const tb=document.getElementById('queue-tbl');
  tb.innerHTML='';
  if(!data){tb.innerHTML='<tr><td colspan="8" class="muted" style="text-align:center;padding:8px">Queue unavailable</td></tr>';return;}
  // Queue data may be an array or object with entries
  let entries=[];
  if(Array.isArray(data)) entries=data;
  else if(data.items) entries=data.items;
  else if(data.queue) entries=data.queue;
  else if(data.entries) entries=data.entries;
  else{
    // Try to render as object keys
    for(const[k,v] of Object.entries(data)){
      if(Array.isArray(v)) entries=entries.concat(v.map((e,i)=>({...e,_pool:k,_pos:i+1})));
    }
  }
  if(entries.length===0){tb.innerHTML='<tr><td colspan="8" class="muted" style="text-align:center;padding:8px">Queue empty</td></tr>';return;}
  entries.forEach((e,i)=>{
    const name=e.name||e.metadata?.name||'—';
    const ns=e.namespace||e.metadata?.namespace||'—';
    const pool=e._pool||e.pool||e.spec?.pool||'—';
    const pri=e.priority||e.spec?.priority||0;
    const phase=e.phase||e.status?.phase||e.spec?.phase||'—';
    const host=e.assignedHost||e.status?.assignedHost||'—';
    const queued=e.creationTimestamp||e.metadata?.creationTimestamp||'';
    tb.innerHTML+='<tr><td>'+(e._pos||i+1)+'</td><td>'+escapeHtml(name)+'</td><td>'+escapeHtml(ns)+'</td><td>'+escapeHtml(pool)+'</td><td>'+pri+'</td><td>'+statusBadge(phase)+'</td><td>'+escapeHtml(host)+'</td><td>'+timeSince(queued)+'</td></tr>';
  });
  initSort('queue-tbl');reapplySort('queue-tbl');
}

async function loadAll(){
  await Promise.all([loadJobs(),loadRunners(),loadRes(),loadQueue()]);
}
loadAll(); setInterval(loadAll,15000);
`
	// Add selected row styling
	extraCSS := `<style>
#jobs-tbl tr.selected td { background:#1e2845; }
#jobs-tbl tr.job-row:hover td { background:#1a1d32; }
#jobs-tbl tr.selected:hover td { background:#1e2845; }
</style>`
	write(w, c.pageWithJS("Jobs", "Jobs", body+extraCSS, js))
}
