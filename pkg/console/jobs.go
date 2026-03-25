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
    <select id="job-phase" onchange="loadJobs()"><option value="">All Phases</option><option value="active">Active</option><option>Pending</option><option>Provisioning</option><option>Running</option><option>Completed</option><option>Failed</option><option>Cancelled</option><option>TimedOut</option></select>
    <button class="btn btn-danger" onclick="deleteSelected()">Delete Selected</button>
  </div>
  <table><thead><tr><th><input type="checkbox" id="select-all" onchange="toggleSelectAll()"></th><th>Name</th><th>Namespace</th><th>Phase</th><th>Pool</th><th>Priority</th><th>BMH</th><th>Duration</th><th>Age</th><th>Actions</th></tr></thead>
  <tbody id="jobs-tbl"><tr><td colspan="10" class="loading">Loading...</td></tr></tbody></table>
  <div id="job-detail-panel" style="display:none;margin-top:12px">
    <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px">
      <div class="card" style="margin-bottom:0">
        <h3 id="detail-title">Job Details</h3>
        <div class="kv" id="detail-spec"></div>
      </div>
      <div class="card" style="margin-bottom:0">
        <h3>Status</h3>
        <div class="kv" id="detail-status"></div>
      </div>
    </div>
    <div class="card mt" style="margin-bottom:0">
      <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:8px">
        <h3 style="margin:0" id="log-title">Logs</h3>
        <div style="display:flex;gap:8px;align-items:center">
          <label style="color:#888;font-size:11px"><input type="checkbox" id="log-follow" checked> Follow</label>
          <span id="log-line-count" style="color:#666;font-size:11px"></span>
        </div>
      </div>
      <div class="terminal" id="job-logs" style="max-height:500px;min-height:200px"><span class="muted">Select a job to view logs</span></div>
    </div>
  </div>
</div>

<div id="runners" class="tab-content">
  <div class="flex mb">
    <button class="btn btn-primary" onclick="showCreateRunner()">Create Runner</button>
  </div>
  <table><thead><tr><th>Name</th><th>Pool</th><th>Template</th><th>Max Concurrent</th><th>Idle Timeout</th><th>Reclaim</th><th>Active</th><th>Completed</th><th>Failed</th><th>Status</th><th>Actions</th></tr></thead>
  <tbody id="runners-tbl"></tbody></table>
  <div id="runner-detail-panel" style="display:none;margin-top:12px">
    <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px">
      <div class="card" style="margin-bottom:0">
        <h3 id="runner-detail-title">Runner Details</h3>
        <div class="kv" id="runner-detail-spec"></div>
      </div>
      <div class="card" style="margin-bottom:0">
        <h3>Active Jobs</h3>
        <div id="runner-active-jobs"><span class="muted">No active jobs</span></div>
      </div>
    </div>
    <div id="runner-env-panel" style="display:none;margin-top:12px">
      <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px">
        <div class="card" style="margin-bottom:0">
          <h3>Runtime Environment</h3>
          <div class="kv" id="runner-env-info"></div>
        </div>
        <div class="card" style="margin-bottom:0">
          <h3>Container Images</h3>
          <div id="runner-env-images"><span class="muted">No images reported</span></div>
        </div>
      </div>
    </div>
    <div class="card mt" style="margin-bottom:0">
      <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:8px">
        <h3 style="margin:0" id="runner-log-title">Runner Activity Log</h3>
        <div style="display:flex;gap:8px;align-items:center">
          <label style="color:#888;font-size:11px"><input type="checkbox" id="runner-log-follow" checked> Follow</label>
          <span id="runner-log-line-count" style="color:#666;font-size:11px"></span>
        </div>
      </div>
      <div class="terminal" id="runner-logs" style="max-height:500px;min-height:200px"><span class="muted">Select a runner to view activity log</span></div>
    </div>
  </div>
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
function humanDuration(startedAt,completedAt){
  if(!startedAt) return '—';
  const start=new Date(startedAt).getTime();
  if(isNaN(start)) return '—';
  const end=completedAt?new Date(completedAt).getTime():Date.now();
  if(isNaN(end)) return '—';
  let secs=Math.floor((end-start)/1000);
  if(secs<0) secs=0;
  const h=Math.floor(secs/3600);
  const m=Math.floor((secs%3600)/60);
  const s=secs%60;
  const parts=[];
  if(h>0) parts.push(h+(h===1?' hour':' hours'));
  if(m>0) parts.push(m+(m===1?' minute':' minutes'));
  if(s>0||parts.length===0) parts.push(s+(s===1?' second':' seconds'));
  if(parts.length>2) return parts.slice(0,-1).join(', ')+' and '+parts[parts.length-1];
  return parts.join(' and ');
}

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
    await apiDelete(API+'/api/v1/namespaces/'+cb.dataset.ns+'/jobs/'+cb.dataset.name);
  }
  document.getElementById('select-all').checked=false;
  loadJobs();
}

// ── Inline job detail + real-time logs ──
var _selectedJob=null;
var _selectedJobData=null;
var _logPollTimer=null;
var _lastLogText='';
var _allJobData=[];

function selectJob(ns,name){
  ns=decodeURIComponent(ns);
  name=decodeURIComponent(name);
  const key=ns+'/'+name;
  if(_selectedJob===key){
    _selectedJob=null;
    _selectedJobData=null;
    document.getElementById('job-detail-panel').style.display='none';
    document.querySelectorAll('#jobs-tbl tr').forEach(r=>r.classList.remove('selected'));
    stopLogPoll();
    return;
  }
  _selectedJob=key;
  // Find full job data
  _selectedJobData=_allJobData.find(j=>(j.metadata.namespace||'default')+'/'+j.metadata.name===key)||null;
  // Highlight selected row
  document.querySelectorAll('#jobs-tbl tr').forEach(r=>r.classList.remove('selected'));
  document.querySelectorAll('#jobs-tbl tr').forEach(r=>{if(r.dataset.jobKey===key) r.classList.add('selected');});
  // Show detail panel
  renderJobDetail();
  _lastLogText=null;
  document.getElementById('job-logs').innerHTML='<span class="muted">Loading logs...</span>';
  document.getElementById('log-line-count').textContent='';
  startLogPoll();
}

function renderJobDetail(){
  const j=_selectedJobData;
  if(!j){document.getElementById('job-detail-panel').style.display='none';return;}
  document.getElementById('job-detail-panel').style.display='block';
  const s=j.spec||{};
  const st=j.status||{};

  document.getElementById('detail-title').textContent=j.metadata.name;

  // Spec details
  let specHtml='';
  const specFields=[
    ['Pool',s.pool],
    ['Priority',s.priority||0],
    ['Build Image',s.buildImage],
    ['Repository',s.repo],
    ['Build Script',s.buildScript],
    ['Image',s.image],
    ['Timeout',s.timeout?s.timeout+'s':'none'],
  ];
  specFields.forEach(([k,v])=>{
    if(v!==undefined&&v!==null&&v!==''){
      specHtml+='<div class="k">'+escapeHtml(k)+'</div><div class="v">'+escapeHtml(String(v))+'</div>';
    }
  });
  // Environment
  if(s.env&&Object.keys(s.env).length>0){
    specHtml+='<div class="k">Environment</div><div class="v">';
    Object.entries(s.env).forEach(([ek,ev])=>{specHtml+=escapeHtml(ek)+'='+escapeHtml(ev)+'<br>';});
    specHtml+='</div>';
  }
  // Labels
  if(s.labels&&Object.keys(s.labels).length>0){
    specHtml+='<div class="k">Labels</div><div class="v">';
    Object.entries(s.labels).forEach(([lk,lv])=>{specHtml+=escapeHtml(lk)+'='+escapeHtml(lv)+'<br>';});
    specHtml+='</div>';
  }
  // Artifacts
  if(s.artifacts&&s.artifacts.length>0){
    specHtml+='<div class="k">Artifacts</div><div class="v">';
    s.artifacts.forEach(a=>{specHtml+=escapeHtml(a.name)+': '+escapeHtml(a.path)+'<br>';});
    specHtml+='</div>';
  }
  // Legacy script (show truncated)
  if(s.script&&!s.repo){
    const preview=s.script.length>200?s.script.substring(0,200)+'...':s.script;
    specHtml+='<div class="k">Script</div><div class="v"><pre style="margin:0;white-space:pre-wrap;font-size:11px;max-height:120px;overflow-y:auto">'+escapeHtml(preview)+'</pre></div>';
  }
  document.getElementById('detail-spec').innerHTML=specHtml;

  // Status details
  let statusHtml='';
  statusHtml+='<div class="k">Phase</div><div class="v">'+statusBadge(st.phase||'Pending')+'</div>';
  const dur=st.startedAt?humanDuration(st.startedAt,st.completedAt):null;
  const statusFields=[
    ['Assigned Host',st.bmhRef],
    ['Runner',st.runnerRef],
    ['Host IP',st.hostIP],
    ['Started',st.startedAt],
    ['Completed',st.completedAt],
    ['Duration',dur],
    ['Exit Code',st.exitCode!==undefined&&st.exitCode!==null?String(st.exitCode):null],
    ['Error',st.errorMessage],
    ['Log Lines',st.logLines||0],
    ['Last Heartbeat',st.lastHeartbeat?timeSince(st.lastHeartbeat)+' ago':null],
    ['Created',j.metadata.creationTimestamp],
  ];
  statusFields.forEach(([k,v])=>{
    if(v!==undefined&&v!==null&&v!==''){
      let sv=String(v);
      if(k==='Error') sv='<span style="color:#e94560">'+escapeHtml(sv)+'</span>';
      else if(k==='Exit Code'&&v!=='0') sv='<span style="color:#e94560">'+escapeHtml(sv)+'</span>';
      else if(k==='Exit Code'&&v==='0') sv='<span style="color:#50fa7b">'+escapeHtml(sv)+'</span>';
      else sv=escapeHtml(sv);
      statusHtml+='<div class="k">'+escapeHtml(k)+'</div><div class="v">'+sv+'</div>';
    }
  });
  document.getElementById('detail-status').innerHTML=statusHtml;
}

function startLogPoll(){
  stopLogPoll();
  pollLogs();
  // Poll every 2s for running jobs, 10s for terminal
  const phase=(_selectedJobData?.status?.phase||'').toLowerCase();
  const isActive=['pending','scheduling','provisioning','running'].includes(phase);
  _logPollTimer=_uiInterval(pollLogs,isActive?2000:10000);
}

function stopLogPoll(){
  if(_logPollTimer){clearInterval(_logPollTimer);_logPollTimer=null;}
}

async function pollLogs(){
  if(!_selectedJob) return;
  const [ns,name]=_selectedJob.split('/');
  try{
    const resp=await fetch(API+'/api/v1/namespaces/'+encodeURIComponent(ns)+'/jobs/'+encodeURIComponent(name)+'/logs');
    if(!_selectedJob||_selectedJob!==ns+'/'+name) return;
    const text=await resp.text();
    if(!resp.ok){
      document.getElementById('job-logs').innerHTML='<span class="muted">'+escapeHtml(text||'Failed to load logs ('+resp.status+')')+'</span>';
      return;
    }
    // Only update if logs changed (use sentinel to distinguish initial state from empty response)
    if(text!==_lastLogText||_lastLogText===null){
      _lastLogText=text;
      const el=document.getElementById('job-logs');
      const html=ansiToHtml(text);
      el.innerHTML=html||'<span class="muted">No logs yet</span>';
      const lineCount=(text.match(/\n/g)||[]).length;
      document.getElementById('log-line-count').textContent=lineCount+' lines';
      if(document.getElementById('log-follow').checked) el.scrollTop=el.scrollHeight;
    }
  }catch(e){
    console.error('log poll error',e);
    const el=document.getElementById('job-logs');
    if(el&&el.innerHTML.includes('Loading')) el.innerHTML='<span class="muted">Failed to connect</span>';
  }
}

// ── Jobs ──
async function loadJobs(){
  const data=await apiGet(API+'/api/v1/jobs');
  _allJobData=data?.items||[];
  const nsFilter=document.getElementById('job-ns').value;
  const phaseFilter=document.getElementById('job-phase').value.toLowerCase();
  let items=[..._allJobData];
  if(nsFilter) items=items.filter(j=>j.metadata.namespace===nsFilter);
  if(phaseFilter==='active'){
    // Active = running/scheduling/provisioning/pending + recently finished (last 1h)
    var oneHourAgo=new Date(Date.now()-3600000).toISOString();
    items=items.filter(function(j){
      var phase=(j.status?.phase||j.spec?.phase||'').toLowerCase();
      if(['running','scheduling','provisioning','pending'].includes(phase)) return true;
      // Include recently completed/failed/cancelled
      var completed=j.status?.completedAt||'';
      return completed&&completed>=oneHourAgo;
    });
  } else if(phaseFilter){
    items=items.filter(j=>(j.status?.phase||j.spec?.phase||'').toLowerCase()===phaseFilter);
  }

  // Populate ns filter
  const nss=[...new Set(_allJobData.map(j=>j.metadata.namespace||'default'))].sort();
  const sel=document.getElementById('job-ns');
  if(sel.options.length<=1) nss.forEach(n=>{const o=document.createElement('option');o.value=n;o.text=n;sel.add(o);});

  const tb=document.getElementById('jobs-tbl');
  if(items.length===0){tb.innerHTML='<tr><td colspan="10" class="muted" style="text-align:center;padding:16px">No jobs</td></tr>';return;}
  const jobRows=[];
  items.forEach(j=>{
    const ns=j.metadata.namespace||'default';
    const s=j.spec||{};
    const st=j.status||{};
    const phase=st.phase||s.phase||'Pending';
    const bmh=st.bmhRef||st.assignedHost||'—';
    const canCancel=['pending','scheduling','provisioning','running'].includes(phase.toLowerCase());
    const key=ns+'/'+j.metadata.name;
    const selected=_selectedJob===key?' selected':'';
    jobRows.push('<tr data-job-key="'+escapeHtml(key)+'" class="job-row'+selected+'" onclick="selectJob(\''+encodeURIComponent(ns)+'\',\''+escapeHtml(j.metadata.name)+'\')" style="cursor:pointer">'
      +'<td onclick="event.stopPropagation()"><input type="checkbox" data-ns="'+escapeHtml(ns)+'" data-name="'+escapeHtml(j.metadata.name)+'"></td>'
      +'<td>'+escapeHtml(j.metadata.name)+'</td>'
      +'<td>'+escapeHtml(ns)+'</td>'
      +'<td>'+statusBadge(phase)+'</td>'
      +'<td>'+escapeHtml(s.pool||'—')+'</td>'
      +'<td>'+(s.priority||0)+'</td>'
      +'<td>'+escapeHtml(bmh)+'</td>'
      +'<td>'+humanDuration(st.startedAt,st.completedAt)+'</td>'
      +'<td>'+timeSince(j.metadata?.creationTimestamp)+'</td>'
      +'<td onclick="event.stopPropagation()">'
      +(canCancel?'<button class="btn btn-danger" onclick="cancelJob(\''+encodeURIComponent(ns)+'\',\''+escapeHtml(j.metadata.name)+'\')">Cancel</button> ':'')
      +'<button class="btn btn-danger" onclick="delJob(\''+encodeURIComponent(ns)+'\',\''+escapeHtml(j.metadata.name)+'\')">Delete</button></td></tr>');
  });
  tb.innerHTML=jobRows.join('');
  initSort('jobs-tbl');reapplySort('jobs-tbl');

  // Refresh detail panel if selected job is still in list
  if(_selectedJob){
    _selectedJobData=_allJobData.find(j=>(j.metadata.namespace||'default')+'/'+j.metadata.name===_selectedJob)||null;
    if(_selectedJobData) renderJobDetail();
    else{
      _selectedJob=null;
      document.getElementById('job-detail-panel').style.display='none';
      stopLogPoll();
    }
  }
}

async function cancelJob(ns,name){
  if(!confirm('Cancel job '+name+'?')) return;
  await apiPost(API+'/api/v1/namespaces/'+ns+'/jobs/'+name+'/cancel');
  loadJobs();
}

async function delJob(ns,name){
  if(!confirm('Delete job '+decodeURIComponent(name)+'?')) return;
  await apiDelete(API+'/api/v1/namespaces/'+ns+'/jobs/'+name);
  const key=decodeURIComponent(ns)+'/'+decodeURIComponent(name);
  if(_selectedJob===key){
    _selectedJob=null;
    _selectedJobData=null;
    document.getElementById('job-detail-panel').style.display='none';
    stopLogPoll();
  }
  loadJobs();
}

// ── Job Runners ──
var _selectedRunner=null;
var _selectedRunnerData=null;
var _allRunnerData=[];
var _runnerLogPollTimer=null;
var _runnerLastLogText='';

function selectRunner(name){
  if(_selectedRunner===name){
    _selectedRunner=null;
    _selectedRunnerData=null;
    document.getElementById('runner-detail-panel').style.display='none';
    document.querySelectorAll('#runners-tbl tr').forEach(r=>r.classList.remove('selected'));
    stopRunnerLogPoll();
    return;
  }
  _selectedRunner=name;
  _selectedRunnerData=_allRunnerData.find(r=>r.metadata.name===name)||null;
  document.querySelectorAll('#runners-tbl tr').forEach(r=>r.classList.remove('selected'));
  document.querySelectorAll('#runners-tbl tr').forEach(r=>{if(r.dataset.runnerName===name) r.classList.add('selected');});
  renderRunnerDetail();
}

function renderRunnerDetail(){
  const r=_selectedRunnerData;
  if(!r){document.getElementById('runner-detail-panel').style.display='none';return;}
  document.getElementById('runner-detail-panel').style.display='block';
  const s=r.spec||{};
  const st=r.status||{};
  document.getElementById('runner-detail-title').textContent=r.metadata.name;

  let html='';
  const fields=[
    ['Pool',s.pool],['Template',s.template||s.bootConfigRef],['Boot Config',s.bootConfigRef],
    ['Max Concurrent',s.maxConcurrent||1],['Idle Timeout',(s.idleTimeout||0)+'s'],
    ['Reclaim Policy',s.reclaimPolicy||'PowerOff'],['Phase',st.phase||'Active'],
    ['Reserved Hosts',st.reservedHosts||0],['Active Jobs',st.activeJobs||0],
    ['Total Completed',st.totalCompleted||0],['Total Failed',st.totalFailed||0],
    ['Created',r.metadata.creationTimestamp],
  ];
  fields.forEach(([k,v])=>{
    if(v!==undefined&&v!==null&&v!=='') html+='<div class="k">'+escapeHtml(k)+'</div><div class="v">'+escapeHtml(String(v))+'</div>';
  });
  document.getElementById('runner-detail-spec').innerHTML=html;

  // Find jobs assigned to this runner
  const runnerJobs=_allJobData.filter(j=>(j.status?.runnerRef||'')=== _selectedRunner);
  const activeJobs=runnerJobs.filter(j=>['pending','scheduling','provisioning','running'].includes((j.status?.phase||'').toLowerCase()));
  const recentJobs=runnerJobs.sort((a,b)=>(b.metadata?.creationTimestamp||'').localeCompare(a.metadata?.creationTimestamp||'')).slice(0,10);

  // Active jobs list
  const ajDiv=document.getElementById('runner-active-jobs');
  if(recentJobs.length===0){
    ajDiv.innerHTML='<span class="muted">No jobs for this runner</span>';
  } else {
    let ajHtml='<table style="font-size:11px"><thead><tr><th>Job</th><th>Phase</th><th>Host</th><th>Age</th></tr></thead><tbody>';
    recentJobs.forEach(j=>{
      const ns=j.metadata.namespace||'default';
      const phase=j.status?.phase||'Pending';
      ajHtml+='<tr><td>'+escapeHtml(ns+'/'+j.metadata.name)+'</td><td>'+statusBadge(phase)+'</td><td>'+escapeHtml(j.status?.bmhRef||'—')+'</td><td>'+timeSince(j.metadata?.creationTimestamp)+'</td></tr>';
    });
    ajHtml+='</tbody></table>';
    ajDiv.innerHTML=ajHtml;
  }

  // Load runtime environment from host reservations
  loadRunnerEnv();

  // Start polling runner activity log
  _runnerLastLogText='';
  document.getElementById('runner-logs').innerHTML='<span class="muted">Loading activity log...</span>';
  document.getElementById('runner-log-line-count').textContent='';
  document.getElementById('runner-log-title').textContent='Activity Log: '+_selectedRunner;
  startRunnerLogPoll();
}

async function loadRunnerEnv(){
  const envPanel=document.getElementById('runner-env-panel');
  const envInfo=document.getElementById('runner-env-info');
  const envImages=document.getElementById('runner-env-images');
  if(!_selectedRunnerData){envPanel.style.display='none';return;}
  const pool=_selectedRunnerData.spec?.pool||'';
  if(!pool){envPanel.style.display='none';return;}

  // Fetch host reservations and find one with agentEnv for this pool
  const hrData=await apiGet(API+'/api/v1/hostreservations');
  const hrs=(hrData?.items||[]).filter(h=>h.spec?.pool===pool&&h.status?.agentEnv);
  if(hrs.length===0){envPanel.style.display='none';return;}

  envPanel.style.display='block';
  // Show env from first host that has it (most hosts in same pool have same config)
  const env=hrs[0].status.agentEnv;
  const hostName=hrs[0].spec?.bmhRef||hrs[0].metadata?.name||'?';

  let infoHtml='';
  const fields=[
    ['Host',hostName],
    ['Agent Version',env.agentVersion&&env.agentVersion!=='dev'?env.agentVersion+(env.agentCommit?' ('+env.agentCommit.substring(0,8)+')':''):null],
    ['Agent Commit',!env.agentVersion||env.agentVersion==='dev'?env.agentCommit:null],
    ['Podman',env.podmanVersion],
    ['Storage Driver',env.storageDriver],
    ['Storage Path',env.storagePath],
    ['Cgroups',env.cgroupVersion],
    ['OS / Arch',env.os+'/'+env.arch],
    ['Reported',env.reportedAt?timeSince(env.reportedAt)+' ago':'—'],
  ];
  fields.forEach(([k,v])=>{
    if(v) infoHtml+='<div class="k">'+escapeHtml(k)+'</div><div class="v">'+escapeHtml(String(v))+'</div>';
  });
  if(hrs.length>1) infoHtml+='<div class="k">Hosts</div><div class="v">'+hrs.length+' hosts reporting</div>';
  envInfo.innerHTML=infoHtml;

  // Images table
  const imgs=env.images||[];
  if(imgs.length===0){
    envImages.innerHTML='<span class="muted">No images on host</span>';
  } else {
    let imgHtml='<table style="font-size:11px"><thead><tr><th>Image</th><th>Arch</th><th>Size</th></tr></thead><tbody>';
    imgs.forEach(img=>{
      // Shorten image name for display
      let name=img.name||'<none>';
      imgHtml+='<tr><td>'+escapeHtml(name)+'</td><td>'+escapeHtml(img.arch||'—')+'</td><td>'+escapeHtml(img.size||'—')+'</td></tr>';
    });
    imgHtml+='</tbody></table>';
    envImages.innerHTML=imgHtml;
  }
}

function startRunnerLogPoll(){
  stopRunnerLogPoll();
  pollRunnerLogs();
  // Check if runner has active jobs — poll faster if so
  const hasActive=_allJobData.some(j=>(j.status?.runnerRef||'')===_selectedRunner&&['pending','scheduling','provisioning','running'].includes((j.status?.phase||'').toLowerCase()));
  _runnerLogPollTimer=_uiInterval(pollRunnerLogs,hasActive?2000:10000);
}

function stopRunnerLogPoll(){
  if(_runnerLogPollTimer){clearInterval(_runnerLogPollTimer);_runnerLogPollTimer=null;}
}

async function pollRunnerLogs(){
  if(!_selectedRunner) return;
  try{
    const resp=await fetch(API+'/api/v1/jobrunners/'+encodeURIComponent(_selectedRunner)+'/logs');
    if(!_selectedRunner) return;
    const text=await resp.text();
    if(!resp.ok){
      document.getElementById('runner-logs').innerHTML='<span class="muted">'+escapeHtml(text||'No logs ('+resp.status+')')+'</span>';
      return;
    }
    if(text!==_runnerLastLogText){
      _runnerLastLogText=text;
      const el=document.getElementById('runner-logs');
      el.innerHTML=ansiToHtml(text)||'<span class="muted">No activity yet</span>';
      const lineCount=(text.match(/\n/g)||[]).length;
      document.getElementById('runner-log-line-count').textContent=lineCount+' lines';
      if(document.getElementById('runner-log-follow').checked) el.scrollTop=el.scrollHeight;
    }
  }catch(e){
    console.error('runner log poll error',e);
    const el=document.getElementById('runner-logs');
    if(el&&el.innerHTML.includes('Loading')) el.innerHTML='<span class="muted">Failed to connect</span>';
  }
}

async function loadRunners(){
  const data=await apiGet(API+'/api/v1/jobrunners');
  _allRunnerData=data?.items||[];
  const tb=document.getElementById('runners-tbl');
  if(_allRunnerData.length===0){tb.innerHTML='<tr><td colspan="11" class="muted" style="text-align:center;padding:8px">No runners</td></tr>';return;}
  const runnerRows=[];
  _allRunnerData.forEach(r=>{
    const s=r.spec||{};
    const st=r.status||{};
    const selected=_selectedRunner===r.metadata.name?' selected':'';
    runnerRows.push('<tr data-runner-name="'+escapeHtml(r.metadata.name)+'" class="runner-row'+selected+'" onclick="selectRunner(\''+escapeHtml(r.metadata.name)+'\')" style="cursor:pointer">'
      +'<td>'+escapeHtml(r.metadata.name)+'</td>'
      +'<td>'+escapeHtml(s.pool||'—')+'</td>'
      +'<td>'+escapeHtml(s.template||s.bootConfigRef||'—')+'</td>'
      +'<td>'+(s.maxConcurrent||1)+'</td>'
      +'<td>'+(s.idleTimeout||0)+'s</td>'
      +'<td>'+escapeHtml(s.reclaimPolicy||'PowerOff')+'</td>'
      +'<td>'+(st.activeJobs||0)+'</td>'
      +'<td>'+(st.totalCompleted||0)+'</td>'
      +'<td>'+(st.totalFailed||0)+'</td>'
      +'<td>'+statusBadge(st.phase||'Active')+'</td>'
      +'<td onclick="event.stopPropagation()"><button class="btn btn-danger" onclick="delRunner(\''+escapeHtml(r.metadata.name)+'\')">Delete</button></td></tr>');
  });
  tb.innerHTML=runnerRows.join('');
  initSort('runners-tbl');reapplySort('runners-tbl');

  // Refresh runner detail if selected
  if(_selectedRunner){
    _selectedRunnerData=_allRunnerData.find(r=>r.metadata.name===_selectedRunner)||null;
    if(_selectedRunnerData) renderRunnerDetail();
    else{
      _selectedRunner=null;
      document.getElementById('runner-detail-panel').style.display='none';
      stopRunnerLogPoll();
    }
  }
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
  const items=data?.items||[];
  if(items.length===0){tb.innerHTML='<tr><td colspan="7" class="muted" style="text-align:center;padding:8px">No reservations</td></tr>';return;}
  const resRows=[];
  items.forEach(h=>{
    const ns=h.metadata.namespace||'default';
    const s=h.spec||{};
    resRows.push('<tr><td>'+escapeHtml(h.metadata.name)+'</td><td>'+escapeHtml(ns)+'</td>'
      +'<td>'+escapeHtml(s.bmhRef||'—')+'</td><td>'+escapeHtml(s.pool||'—')+'</td>'
      +'<td>'+escapeHtml(s.owner||'—')+'</td><td>'+escapeHtml(s.purpose||'—')+'</td>'
      +'<td><button class="btn btn-danger" onclick="delRes(\''+encodeURIComponent(ns)+'\',\''+escapeHtml(h.metadata.name)+'\')">Delete</button></td></tr>');
  });
  tb.innerHTML=resRows.join('');
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
  let entries=[];
  if(Array.isArray(data)) entries=data;
  else if(data.items) entries=data.items;
  else if(data.queue) entries=data.queue;
  else if(data.entries) entries=data.entries;
  else{
    for(const[k,v] of Object.entries(data)){
      if(Array.isArray(v)) entries=entries.concat(v.map((e,i)=>({...e,_pool:k,_pos:i+1})));
    }
  }
  if(entries.length===0){tb.innerHTML='<tr><td colspan="8" class="muted" style="text-align:center;padding:8px">Queue empty</td></tr>';return;}
  const qRows=[];
  entries.forEach((e,i)=>{
    const name=e.name||e.metadata?.name||'—';
    const ns=e.namespace||e.metadata?.namespace||'—';
    const pool=e._pool||e.pool||e.spec?.pool||'—';
    const pri=e.priority||e.spec?.priority||0;
    const phase=e.phase||e.status?.phase||e.spec?.phase||'—';
    const host=e.assignedHost||e.status?.assignedHost||'—';
    const queued=e.creationTimestamp||e.metadata?.creationTimestamp||'';
    qRows.push('<tr><td>'+(e._pos||i+1)+'</td><td>'+escapeHtml(name)+'</td><td>'+escapeHtml(ns)+'</td><td>'+escapeHtml(pool)+'</td><td>'+pri+'</td><td>'+statusBadge(phase)+'</td><td>'+escapeHtml(host)+'</td><td>'+timeSince(queued)+'</td></tr>');
  });
  tb.innerHTML=qRows.join('');
  initSort('queue-tbl');reapplySort('queue-tbl');
}

async function loadAll(){
  await Promise.all([loadJobs(),loadRunners(),loadRes(),loadQueue()]);
}
loadAll(); _uiInterval(loadAll,15000);
`
	// Add selected row styling
	extraCSS := `<style>
#jobs-tbl tr.selected td, #runners-tbl tr.selected td { background:#1e2845; }
#jobs-tbl tr.job-row:hover td, #runners-tbl tr.runner-row:hover td { background:#1a1d32; }
#jobs-tbl tr.selected:hover td, #runners-tbl tr.selected:hover td { background:#1e2845; }
</style>`
	write(w, c.pageWithJS("Jobs", "Jobs", body+extraCSS, js))
}
