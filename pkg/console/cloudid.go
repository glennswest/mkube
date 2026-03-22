package console

import "net/http"

func (c *Console) handleCloudID(w http.ResponseWriter, r *http.Request) {
	body := `<h1>CloudID — Template Management</h1>
<div class="tabs">
  <div class="tab active" onclick="switchTab('templates')">Templates</div>
  <div class="tab" onclick="switchTab('assignments')">Assignments</div>
  <div class="tab" onclick="switchTab('oneshot')">Oneshot State</div>
  <div class="tab" onclick="switchTab('backup')">Backup / Restore</div>
</div>

<!-- Templates Tab -->
<div id="templates" class="tab-content active">
  <div class="flex mb">
    <button class="btn btn-primary" onclick="showCreateTemplate()">Create Template</button>
  </div>
  <table><thead><tr><th>Image Type</th><th>Name</th><th>Format</th><th>Mode</th><th>Updated</th><th>Actions</th></tr></thead>
  <tbody id="tpl-tbl"><tr><td colspan="6" class="loading">Loading...</td></tr></tbody></table>
</div>

<!-- Assignments Tab -->
<div id="assignments" class="tab-content">
  <div class="flex mb">
    <button class="btn btn-primary" onclick="showCreateAssignment()">Assign Template</button>
  </div>
  <table><thead><tr><th>Hostname</th><th>Image Type</th><th>Template</th><th>Actions</th></tr></thead>
  <tbody id="assign-tbl"></tbody></table>
</div>

<!-- Oneshot Tab -->
<div id="oneshot" class="tab-content">
  <table><thead><tr><th>Hostname</th><th>Completed At</th><th>Actions</th></tr></thead>
  <tbody id="oneshot-tbl"></tbody></table>
</div>

<!-- Backup/Restore Tab -->
<div id="backup" class="tab-content">
  <div class="flex mb">
    <button class="btn btn-primary" onclick="backupTemplates()">Download Backup</button>
    <button class="btn btn-success" onclick="document.getElementById('restore-file').click()">Restore from File</button>
    <input type="file" id="restore-file" style="display:none" accept=".json" onchange="restoreTemplates(event)">
  </div>
  <div id="backup-status" class="mt"></div>
</div>

<!-- Create Template Modal -->
<div class="modal-overlay" id="tpl-modal">
<div class="modal">
  <h3 id="tpl-modal-title">Create Template</h3>
  <div class="kv mb">
    <div class="k">Image Type</div><div class="v"><input type="text" id="tpl-type" placeholder="fcos" style="width:100%"></div>
    <div class="k">Name</div><div class="v"><input type="text" id="tpl-name" placeholder="agent-runner.ign.json" style="width:100%"></div>
    <div class="k">Mode</div><div class="v"><select id="tpl-mode" style="width:100%"><option value="forever">Forever</option><option value="oneshot">Oneshot</option></select></div>
  </div>
  <h3>Content</h3>
  <textarea id="tpl-content"></textarea>
  <div class="actions">
    <button class="btn" onclick="hideModal('tpl-modal')">Cancel</button>
    <button class="btn btn-primary" id="tpl-save-btn" onclick="saveTemplate()">Create</button>
  </div>
</div></div>

<!-- Create Assignment Modal -->
<div class="modal-overlay" id="assign-modal">
<div class="modal">
  <h3>Assign Template to Host</h3>
  <div class="kv mb">
    <div class="k">Hostname</div><div class="v"><input type="text" id="assign-host" placeholder="server1" style="width:100%"></div>
    <div class="k">Image Type</div><div class="v"><input type="text" id="assign-type" placeholder="fcos" style="width:100%"></div>
    <div class="k">Template</div><div class="v"><input type="text" id="assign-tpl" placeholder="agent-runner.ign.json" style="width:100%"></div>
  </div>
  <div class="actions">
    <button class="btn" onclick="hideModal('assign-modal')">Cancel</button>
    <button class="btn btn-primary" onclick="createAssignment()">Assign</button>
  </div>
</div></div>`

	js := `
let editingTemplate=null;

function switchTab(id){
  document.querySelectorAll('.tab-content').forEach(e=>e.classList.remove('active'));
  document.querySelectorAll('.tab').forEach(e=>e.classList.remove('active'));
  document.getElementById(id).classList.add('active');
  event.target.classList.add('active');
}

function showCreateTemplate(){
  editingTemplate=null;
  document.getElementById('tpl-modal-title').textContent='Create Template';
  document.getElementById('tpl-save-btn').textContent='Create';
  document.getElementById('tpl-type').value='';
  document.getElementById('tpl-name').value='';
  document.getElementById('tpl-mode').value='forever';
  document.getElementById('tpl-content').value='';
  document.getElementById('tpl-type').disabled=false;
  document.getElementById('tpl-name').disabled=false;
  document.getElementById('tpl-modal').classList.add('show');
}

function showEditTemplate(imageType,name,mode,content){
  editingTemplate={imageType,name};
  document.getElementById('tpl-modal-title').textContent='Edit Template';
  document.getElementById('tpl-save-btn').textContent='Save';
  document.getElementById('tpl-type').value=imageType;
  document.getElementById('tpl-name').value=name;
  document.getElementById('tpl-mode').value=mode||'forever';
  document.getElementById('tpl-content').value=content||'';
  document.getElementById('tpl-type').disabled=true;
  document.getElementById('tpl-name').disabled=true;
  document.getElementById('tpl-modal').classList.add('show');
}

function showCreateAssignment(){
  document.getElementById('assign-host').value='';
  document.getElementById('assign-type').value='';
  document.getElementById('assign-tpl').value='';
  document.getElementById('assign-modal').classList.add('show');
}

function hideModal(id){ document.getElementById(id).classList.remove('show'); }

// ── Templates ──
async function loadTemplates(){
  const data=await fetch(CLOUDID+'/api/v1/templates').then(r=>r.json()).catch(()=>null);
  const tb=document.getElementById('tpl-tbl');
  tb.innerHTML='';
  const items=Array.isArray(data)?data:[];
  if(items.length===0){tb.innerHTML='<tr><td colspan="6" class="muted" style="text-align:center;padding:16px">'+(data===null?'CloudID unavailable — check connection':'No templates')+'</td></tr>';return;}
  items.forEach(t=>{
    tb.innerHTML+='<tr><td>'+escapeHtml(t.image_type||'—')+'</td>'
      +'<td><a href="cloudid/'+encodeURIComponent(t.image_type)+'/'+encodeURIComponent(t.name)+'">'+escapeHtml(t.name||'—')+'</a></td>'
      +'<td>'+escapeHtml(t.format||'—')+'</td>'
      +'<td>'+statusBadge(t.mode||'forever')+'</td>'
      +'<td>'+timeSince(t.updated_at)+'</td>'
      +'<td><button class="btn btn-primary" onclick="editTpl(\''+escapeHtml(t.image_type)+'\',\''+escapeHtml(t.name)+'\')">Edit</button> '
      +'<button class="btn btn-danger" onclick="delTpl(\''+escapeHtml(t.image_type)+'\',\''+escapeHtml(t.name)+'\')">Delete</button></td></tr>';
  });
  initSort('tpl-tbl');reapplySort('tpl-tbl');
}

async function editTpl(imageType,name){
  const data=await fetch(CLOUDID+'/api/v1/templates/'+encodeURIComponent(imageType)+'/'+encodeURIComponent(name)).then(r=>r.json()).catch(()=>null);
  if(data) showEditTemplate(imageType,name,data.mode,data.content);
}

async function saveTemplate(){
  const imageType=document.getElementById('tpl-type').value;
  const name=document.getElementById('tpl-name').value;
  const mode=document.getElementById('tpl-mode').value;
  const content=document.getElementById('tpl-content').value;
  if(!imageType||!name){ alert('Image type and name are required'); return; }
  await fetch(CLOUDID+'/api/v1/templates/'+encodeURIComponent(imageType)+'/'+encodeURIComponent(name),{
    method:'PUT',
    headers:{'Content-Type':'application/json'},
    body:JSON.stringify({mode,content})
  });
  hideModal('tpl-modal');
  loadTemplates();
}

async function delTpl(imageType,name){
  if(!confirm('Delete template '+imageType+'/'+name+'?')) return;
  await fetch(CLOUDID+'/api/v1/templates/'+encodeURIComponent(imageType)+'/'+encodeURIComponent(name),{method:'DELETE'});
  loadTemplates();
}

// ── Assignments ──
async function loadAssignments(){
  const data=await fetch(CLOUDID+'/api/v1/assignments').then(r=>r.json()).catch(()=>null);
  const tb=document.getElementById('assign-tbl');
  tb.innerHTML='';
  if(!data||Object.keys(data).length===0){tb.innerHTML='<tr><td colspan="4" class="muted" style="text-align:center;padding:8px">No assignments</td></tr>';return;}
  for(const[host,val] of Object.entries(data||{})){
    const imageType=typeof val==='object'?val.image_type||'—':'—';
    const tpl=typeof val==='object'?val.template||'—':String(val);
    tb.innerHTML+='<tr><td>'+escapeHtml(host)+'</td><td>'+escapeHtml(imageType)+'</td><td>'+escapeHtml(tpl)+'</td>'
      +'<td><button class="btn btn-danger" onclick="delAssign(\''+escapeHtml(host)+'\')">Remove</button></td></tr>';
  }
  initSort('assign-tbl');reapplySort('assign-tbl');
}

async function createAssignment(){
  const host=document.getElementById('assign-host').value;
  const imageType=document.getElementById('assign-type').value;
  const tpl=document.getElementById('assign-tpl').value;
  if(!host){ alert('Hostname required'); return; }
  await fetch(CLOUDID+'/api/v1/assignments/'+encodeURIComponent(host),{
    method:'PUT',
    headers:{'Content-Type':'application/json'},
    body:JSON.stringify({image_type:imageType,template:tpl})
  });
  hideModal('assign-modal');
  loadAssignments();
}

async function delAssign(host){
  if(!confirm('Remove assignment for '+host+'?')) return;
  await fetch(CLOUDID+'/api/v1/assignments/'+encodeURIComponent(host),{method:'DELETE'});
  loadAssignments();
}

// ── Oneshot ──
async function loadOneshot(){
  const data=await fetch(CLOUDID+'/api/v1/oneshot').then(r=>r.json()).catch(()=>null);
  const tb=document.getElementById('oneshot-tbl');
  tb.innerHTML='';
  if(!data||Object.keys(data).length===0){tb.innerHTML='<tr><td colspan="3" class="muted" style="text-align:center;padding:8px">No oneshot entries</td></tr>';return;}
  for(const[host,ts] of Object.entries(data||{})){
    tb.innerHTML+='<tr><td>'+escapeHtml(host)+'</td><td>'+escapeHtml(String(ts))+'</td>'
      +'<td><button class="btn btn-danger" onclick="resetOneshot(\''+escapeHtml(host)+'\')">Reset</button></td></tr>';
  }
  initSort('oneshot-tbl');reapplySort('oneshot-tbl');
}

async function resetOneshot(host){
  if(!confirm('Reset oneshot for '+host+'? This will allow re-provisioning.')) return;
  await fetch(CLOUDID+'/api/v1/oneshot/'+encodeURIComponent(host),{method:'DELETE'});
  loadOneshot();
}

// ── Backup/Restore ──
async function backupTemplates(){
  document.getElementById('backup-status').innerHTML='<span class="badge badge-yellow">Downloading...</span>';
  try{
    const data=await fetch(CLOUDID+'/api/v1/templates/backup').then(r=>r.json());
    const blob=new Blob([JSON.stringify(data,null,2)],{type:'application/json'});
    const url=URL.createObjectURL(blob);
    const a=document.createElement('a');
    a.href=url;
    a.download='cloudid-backup-'+new Date().toISOString().slice(0,10)+'.json';
    a.click();
    URL.revokeObjectURL(url);
    document.getElementById('backup-status').innerHTML='<span class="badge badge-green">Backup downloaded ('+((data.templates||[]).length)+' templates)</span>';
  }catch(e){
    document.getElementById('backup-status').innerHTML='<span class="badge badge-red">Backup failed: '+escapeHtml(e.message)+'</span>';
  }
}

async function restoreTemplates(event){
  const file=event.target.files[0];
  if(!file) return;
  if(!confirm('Restore templates from '+file.name+'? This will overwrite existing templates with the same names.')) return;
  const text=await file.text();
  document.getElementById('backup-status').innerHTML='<span class="badge badge-yellow">Restoring...</span>';
  try{
    const data=JSON.parse(text);
    await fetch(CLOUDID+'/api/v1/templates/restore',{
      method:'POST',
      headers:{'Content-Type':'application/json'},
      body:JSON.stringify(data)
    });
    document.getElementById('backup-status').innerHTML='<span class="badge badge-green">Restore complete</span>';
    loadTemplates();
  }catch(e){
    document.getElementById('backup-status').innerHTML='<span class="badge badge-red">Restore failed: '+escapeHtml(e.message)+'</span>';
  }
  event.target.value='';
}

async function loadAll(){
  await Promise.all([loadTemplates(),loadAssignments(),loadOneshot()]);
}
loadAll(); setInterval(loadAll,30000);
`
	write(w, c.pageWithJS("CloudID", "CloudID", body, js))
}

func (c *Console) handleCloudIDDetail(w http.ResponseWriter, r *http.Request) {
	imageType := r.PathValue("imageType")
	name := r.PathValue("name")

	body := `<h1>Template: ` + imageType + `/` + name + `</h1>
<div class="card"><h3>Metadata</h3><div class="kv" id="meta"><div class="loading">Loading...</div></div></div>
<div class="card"><h3>Content</h3>
  <div class="flex mb">
    <button class="btn btn-primary" onclick="editMode()">Edit</button>
    <button class="btn btn-success" id="save-btn" onclick="saveContent()" style="display:none">Save</button>
    <button class="btn" id="cancel-btn" onclick="cancelEdit()" style="display:none">Cancel</button>
  </div>
  <div class="terminal" id="content-view"></div>
  <textarea id="content-edit" style="display:none;width:100%;min-height:400px;background:#0a0a14;border:1px solid #2a2d45;color:#e0e0e0;font-family:inherit;font-size:12px;padding:10px;border-radius:4px;resize:vertical"></textarea>
</div>
<div class="card mt"><h3>Template Resolution</h3>
<p class="muted mb">Enter a hostname to preview how this template resolves with variable substitution:</p>
<div class="toolbar">
  <input type="text" id="preview-host" placeholder="server1.g10.lo">
  <button class="btn btn-primary" onclick="preview()">Preview</button>
</div>
<div class="terminal" id="preview-output" style="max-height:400px"></div></div>`

	js := `
const tplType=` + jsStr(imageType) + `,tplName=` + jsStr(name) + `;
let tplData=null;

async function load(){
  tplData=await fetch(CLOUDID+'/api/v1/templates/'+encodeURIComponent(tplType)+'/'+encodeURIComponent(tplName)).then(r=>r.json()).catch(()=>null);
  if(!tplData){document.getElementById('meta').innerHTML='<div class="muted">Template not found — check CloudID connection</div>';return;}
  document.getElementById('meta').innerHTML=
    kv('Image Type',tplType)+kv('Name',tplName)+kv('Format',tplData.format)
    +kv('Mode',tplData.mode)+kv('Created',tplData.created_at)+kv('Updated',tplData.updated_at);
  document.getElementById('content-view').textContent=tplData.content||'';
}

function kv(k,v){ return '<div class="k">'+escapeHtml(k)+'</div><div class="v">'+escapeHtml(String(v||'—'))+'</div>'; }

function editMode(){
  document.getElementById('content-view').style.display='none';
  document.getElementById('content-edit').style.display='block';
  document.getElementById('content-edit').value=tplData?.content||'';
  document.getElementById('save-btn').style.display='inline-block';
  document.getElementById('cancel-btn').style.display='inline-block';
}

function cancelEdit(){
  document.getElementById('content-view').style.display='block';
  document.getElementById('content-edit').style.display='none';
  document.getElementById('save-btn').style.display='none';
  document.getElementById('cancel-btn').style.display='none';
}

async function saveContent(){
  const content=document.getElementById('content-edit').value;
  await fetch(CLOUDID+'/api/v1/templates/'+encodeURIComponent(tplType)+'/'+encodeURIComponent(tplName),{
    method:'PUT',
    headers:{'Content-Type':'application/json'},
    body:JSON.stringify({mode:tplData?.mode||'forever',content})
  });
  cancelEdit();
  load();
}

async function preview(){
  const host=document.getElementById('preview-host').value||'example.g10.lo';
  let content=tplData?.content||'';
  const short=host.split('.')[0];
  const domain=host.includes('.')?'.'+host.split('.').slice(1).join('.'):'';
  content=content.replace(/\{\{HOSTNAME\}\}/g,host);
  content=content.replace(/\{\{SHORT_HOSTNAME\}\}/g,short);
  content=content.replace(/\{\{DOMAIN_SUFFIX\}\}/g,domain);
  content=content.replace(/\{\{INSTANCE_ID\}\}/g,short);
  content=content.replace(/\{\{TEMPLATE_NAME\}\}/g,tplName);
  content=content.replace(/\{\{IP\}\}/g,'(resolved at serve time)');
  content=content.replace(/\{\{AVAILABILITY_ZONE\}\}/g,'(resolved at serve time)');
  document.getElementById('preview-output').textContent=content;
}

load();
`
	write(w, c.pageWithJS("Template: "+imageType+"/"+name, "CloudID", body, js))
}
