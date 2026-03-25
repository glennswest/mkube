package console

import "net/http"

func (c *Console) handleStorage(w http.ResponseWriter, r *http.Request) {
	body := `<h1>Storage</h1>
<div class="tabs">
  <div class="tab active" onclick="switchTab('pools')">Pools</div>
  <div class="tab" onclick="switchTab('pvcs')">PVCs</div>
  <div class="tab" onclick="switchTab('cdroms')">iSCSI CDROMs</div>
  <div class="tab" onclick="switchTab('disks')">iSCSI Disks</div>
</div>
<div id="pools" class="tab-content active">
  <div id="pool-cards" class="loading">Loading...</div>
  <table><thead><tr><th>Name</th><th>Type</th><th>Interface</th><th>Mount</th><th>Total</th><th>Used</th><th>Avail</th><th>Phase</th><th>Default</th><th>Disks</th><th>PVCs</th></tr></thead><tbody id="pools-tbl"></tbody></table>
</div>
<div id="pvcs" class="tab-content">
  <table><thead><tr><th>Name</th><th>Namespace</th><th>Type</th><th>Pool</th><th>Status</th><th>Used</th><th>Capacity</th><th>Thin%</th><th>Created</th><th></th></tr></thead><tbody id="pvcs-tbl"><tr><td colspan="10" class="loading">Loading...</td></tr></tbody></table>
</div>
<div id="cdroms" class="tab-content">
  <table><thead><tr><th>Name</th><th>Version</th><th>Phase</th><th>Size</th><th>Subscribers</th><th>Created</th></tr></thead><tbody id="cdroms-tbl"></tbody></table>
</div>
<div id="disks" class="tab-content">
  <div style="margin-bottom:8px"><button class="btn btn-primary" onclick="showCreateDisk()">Create Disk</button></div>
  <table><thead><tr><th>Name</th><th>Host</th><th>Pool</th><th>Size</th><th>Actual</th><th>Thin%</th><th>Phase</th><th>Last Active</th><th>Source</th><th>Created</th><th></th></tr></thead><tbody id="disks-tbl"></tbody></table>
</div>
<div id="move-modal" style="display:none;position:fixed;top:0;left:0;width:100%;height:100%;background:rgba(0,0,0,0.6);z-index:1000;align-items:center;justify-content:center">
  <div class="card" style="min-width:320px;max-width:400px">
    <h3 id="move-title">Move PVC</h3>
    <div style="margin:12px 0">
      <label style="font-size:12px;color:var(--comment)">Target Pool</label>
      <select id="move-target" style="width:100%;padding:6px;margin-top:4px;background:var(--bg);color:var(--fg);border:1px solid var(--selection);border-radius:4px"></select>
    </div>
    <div style="display:flex;gap:8px;justify-content:flex-end">
      <button class="btn" onclick="closeMove()">Cancel</button>
      <button class="btn btn-primary" onclick="doMove()">Move</button>
    </div>
    <div id="move-status" style="font-size:12px;margin-top:8px"></div>
  </div>
</div>
<div id="migrate-type-modal" style="display:none;position:fixed;top:0;left:0;width:100%;height:100%;background:rgba(0,0,0,0.6);z-index:1000;align-items:center;justify-content:center">
  <div class="card" style="min-width:320px;max-width:400px">
    <h3 id="migrate-type-title">Convert PVC</h3>
    <div id="migrate-type-info" style="font-size:12px;color:var(--comment);margin:8px 0"></div>
    <div id="migrate-type-capacity-row" style="margin:12px 0;display:none">
      <label style="font-size:12px;color:var(--comment)">Capacity</label>
      <input type="text" id="migrate-type-capacity" placeholder="10Gi" style="width:100%;padding:6px;margin-top:4px;background:var(--bg);color:var(--fg);border:1px solid var(--selection);border-radius:4px">
    </div>
    <div style="display:flex;gap:8px;justify-content:flex-end">
      <button class="btn" onclick="closeMigrateType()">Cancel</button>
      <button class="btn btn-primary" onclick="doMigrateType()">Convert</button>
    </div>
    <div id="migrate-type-status" style="font-size:12px;margin-top:8px"></div>
  </div>
</div>
<div id="resize-pvc-modal" style="display:none;position:fixed;top:0;left:0;width:100%;height:100%;background:rgba(0,0,0,0.6);z-index:1000;align-items:center;justify-content:center">
  <div class="card" style="min-width:320px;max-width:400px">
    <h3>Resize PVC</h3>
    <div style="margin:12px 0">
      <label style="font-size:12px;color:var(--comment)">New Capacity</label>
      <input type="text" id="resize-pvc-capacity" placeholder="20Gi" style="width:100%;padding:6px;margin-top:4px;background:var(--bg);color:var(--fg);border:1px solid var(--selection);border-radius:4px">
    </div>
    <div style="display:flex;gap:8px;justify-content:flex-end">
      <button class="btn" onclick="closeResizePVC()">Cancel</button>
      <button class="btn btn-primary" onclick="doResizePVC()">Resize</button>
    </div>
    <div id="resize-pvc-status" style="font-size:12px;margin-top:8px"></div>
  </div>
</div>
<div class="modal-overlay" id="disk-modal">
  <div class="modal" style="max-width:420px">
    <h3 id="disk-modal-title">Create iSCSI Disk</h3>
    <div class="kv mb">
      <div class="k">Name</div><div class="v"><input type="text" id="dk-name" placeholder="my-disk" style="width:100%"></div>
      <div class="k">Size (GB)</div><div class="v"><input type="number" id="dk-size" value="20" min="1" style="width:100%"></div>
      <div class="k">Source</div><div class="v"><select id="dk-source" style="width:100%"><option value="">(empty — thin volume)</option></select></div>
      <div class="k">Storage Pool</div><div class="v"><select id="dk-pool" style="width:100%"></select></div>
      <div class="k">Host</div><div class="v"><input type="text" id="dk-host" placeholder="(optional)" style="width:100%"></div>
      <div class="k">Description</div><div class="v"><input type="text" id="dk-desc" placeholder="(optional)" style="width:100%"></div>
    </div>
    <div class="actions">
      <button class="btn" onclick="hideModal('disk-modal')">Cancel</button>
      <button class="btn btn-primary" id="dk-submit" onclick="submitCreateDisk()">Create</button>
    </div>
    <div id="dk-status" style="font-size:12px;margin-top:8px"></div>
  </div>
</div>
<div class="modal-overlay" id="clone-modal">
  <div class="modal" style="max-width:420px">
    <h3>Clone iSCSI Disk</h3>
    <div class="kv mb">
      <div class="k">Source</div><div class="v"><input type="text" id="cl-source" disabled style="width:100%"></div>
      <div class="k">New Name</div><div class="v"><input type="text" id="cl-name" placeholder="my-disk-clone" style="width:100%"></div>
      <div class="k">Host</div><div class="v"><input type="text" id="cl-host" placeholder="(optional)" style="width:100%"></div>
    </div>
    <div class="actions">
      <button class="btn" onclick="hideModal('clone-modal')">Cancel</button>
      <button class="btn btn-primary" onclick="submitCloneDisk()">Clone</button>
    </div>
    <div id="cl-status" style="font-size:12px;margin-top:8px"></div>
  </div>
</div>`

	js := `
function switchTab(id){
  document.querySelectorAll('.tab-content').forEach(e=>e.classList.remove('active'));
  document.querySelectorAll('.tab').forEach(e=>e.classList.remove('active'));
  document.getElementById(id).classList.add('active');
  event.target.classList.add('active');
}

function fmtPoolBytes(b){
  if(!b||b<=0) return '—';
  var u=['B','KiB','MiB','GiB','TiB'];
  var i=0; var v=b;
  while(v>=1024&&i<u.length-1){v/=1024;i++;}
  return v.toFixed(1)+' '+u[i];
}

function pctUsed(used,total){
  if(!total||total<=0) return 0;
  return Math.round((used/total)*100);
}

var _poolNames=[];
var _defaultPool='';
var _cdromNames=[];
var _diskNames=[];

function pvcPool(p){
  var ann=p.metadata?.annotations||{};
  return ann['vkube.io/storage-pool']||_defaultPool||'—';
}

function diskPool(d){
  return d.spec?.storagePool||_defaultPool||'—';
}

// Build index of disks/PVCs per pool for the pool detail view
function buildPoolIndex(poolItems,pvcItems,diskItems){
  var idx={};
  poolItems.forEach(function(p){ idx[p.metadata.name]={disks:[],pvcs:[],actualBytes:0,logicalBytes:0}; });
  pvcItems.forEach(function(p){
    var pool=pvcPool(p);
    if(idx[pool]) idx[pool].pvcs.push(p.metadata.namespace+'/'+p.metadata.name);
    else if(idx[_defaultPool]) idx[_defaultPool].pvcs.push(p.metadata.namespace+'/'+p.metadata.name);
  });
  diskItems.forEach(function(d){
    var pool=diskPool(d);
    var st=d.status||{};
    var s=d.spec||{};
    var target=idx[pool]||idx[_defaultPool];
    if(target){
      target.disks.push(d.metadata.name);
      if(st.actualBytes) target.actualBytes+=st.actualBytes;
      var logical=s.sizeGB?s.sizeGB*1024*1024*1024:(st.diskSize||0);
      target.logicalBytes+=logical;
    }
  });
  return idx;
}

async function load(){
  var [pools,pvcs,cdroms,disks]=await Promise.all([
    apiGet(API+'/api/v1/storagepools'),
    apiGet(API+'/api/v1/persistentvolumeclaims'),
    apiGet(API+'/api/v1/iscsi-cdroms'),
    apiGet(API+'/api/v1/iscsi-disks'),
  ]);

  var poolItems=pools?.items||[];
  var pvcItems=pvcs?.items||[];
  var cdromItems=cdroms?.items||[];
  var diskItems=disks?.items||[];

  // Find default pool and build pool name list
  _poolNames=[];
  _defaultPool='';
  poolItems.forEach(function(p){
    _poolNames.push(p.metadata.name);
    if(p.spec?.default) _defaultPool=p.metadata.name;
  });
  if(!_defaultPool&&_poolNames.length>0) _defaultPool=_poolNames[0];

  // Track source options for create modal
  _cdromNames=cdromItems.filter(function(c){return (c.spec?.phase||c.status?.phase)==='Ready';}).map(function(c){return c.metadata.name;});
  _diskNames=diskItems.filter(function(d){return (d.spec?.phase||d.status?.phase)==='Ready';}).map(function(d){return d.metadata.name;});

  var poolIdx=buildPoolIndex(poolItems,pvcItems,diskItems);

  // Pools — capacity cards
  var pc=document.getElementById('pool-cards');
  if(poolItems.length===0){
    pc.innerHTML='<div class="muted" style="padding:8px">No storage pools discovered</div>';
  } else {
    pc.innerHTML='<div style="display:flex;gap:12px;flex-wrap:wrap;margin-bottom:12px">'+poolItems.map(function(p){
      var st=p.status||{};
      var pct=pctUsed(st.usedBytes,st.totalBytes);
      var color=pct>90?'var(--red)':pct>75?'var(--orange)':'var(--green)';
      var def=p.spec?.default?' (default)':'';
      var pi=poolIdx[p.metadata.name]||{disks:[],pvcs:[]};
      var pvcList=pi.pvcs.length>0?pi.pvcs.map(function(n){return escapeHtml(n);}).join(', '):'none';
      // Format type line: "RAID-0 (2 drives)" or "TEAM TM8PS7001T · SATA 6.0 Gbps"
      var typeStr='';
      if(st.raidType){
        typeStr='RAID-'+st.raidType;
        if(st.raidDevices) typeStr+=' ('+st.raidDevices+' drives)';
      } else {
        var parts=[];
        if(st.deviceModel) parts.push(st.deviceModel);
        if(st.interface) parts.push(st.interface);
        typeStr=parts.join(' \u00b7 ')||st.deviceType||'unknown';
      }
      // Physical drives count
      var driveCount=st.raidDevices||1;
      var driveLabel=driveCount===1?'1 drive':driveCount+' drives';
      // Write rate (only show when > 0)
      var writeRateLine='';
      if(st.writeRateBPS&&st.writeRateBPS>0){
        writeRateLine='<div style="font-size:11px;color:var(--cyan);margin-top:2px"><b>Write Rate</b>: '+fmtPoolBytes(st.writeRateBPS)+'/s</div>';
      }
      // Thin savings (only show if pool has disks with actual bytes tracked)
      var thinLine='';
      if(pi.actualBytes>0&&pi.logicalBytes>0){
        var saved=pi.logicalBytes>0?Math.round((1-pi.actualBytes/pi.logicalBytes)*100):0;
        thinLine='<div style="font-size:11px;color:var(--purple);margin-top:2px"><b>Thin</b>: '+fmtPoolBytes(pi.actualBytes)+' actual / '+fmtPoolBytes(pi.logicalBytes)+' logical ('+saved+'% saved)</div>';
      }
      // iSCSI disks (only show if any)
      var iscsiLine='';
      if(pi.disks.length>0){
        var diskList=pi.disks.map(function(n){return escapeHtml(n);}).join(', ');
        iscsiLine='<div style="font-size:11px;color:var(--comment);margin-top:2px"><b>iSCSI Disks</b> ('+pi.disks.length+'): '+diskList+'</div>';
      }
      return '<div class="card" style="min-width:220px;flex:1;max-width:340px">'+
        '<h4>'+escapeHtml(p.metadata.name)+def+'</h4>'+
        '<div style="font-size:12px;color:var(--comment)">'+escapeHtml(typeStr)+'</div>'+
        '<div style="margin:8px 0;background:var(--selection);border-radius:4px;height:16px;overflow:hidden">'+
        '<div style="width:'+pct+'%;height:100%;background:'+color+';border-radius:4px;transition:width 0.3s"></div></div>'+
        '<div style="font-size:12px">'+fmtPoolBytes(st.usedBytes)+' / '+fmtPoolBytes(st.totalBytes)+' ('+pct+'%)</div>'+
        writeRateLine+
        '<div style="font-size:11px;color:var(--comment);margin-top:6px"><b>Drives</b>: '+driveLabel+'</div>'+
        '<div style="font-size:11px;color:var(--comment);margin-top:2px"><b>PVCs</b> ('+pi.pvcs.length+'): '+pvcList+'</div>'+
        iscsiLine+
        thinLine+
        '</div>';
    }).join('')+'</div>';
  }

  // Pools table
  var plt=document.getElementById('pools-tbl');
  if(poolItems.length===0){plt.innerHTML='<tr><td colspan="11" class="muted" style="text-align:center;padding:8px">No pools</td></tr>';}
  else{
    var poolRows=[];
    poolItems.forEach(function(p){
      var sp=p.spec||{};var st=p.status||{};
      var pi=poolIdx[p.metadata.name]||{disks:[],pvcs:[]};
      var devType=st.raidType?('RAID-'+st.raidType+(st.raidDevices?' ('+st.raidDevices+'x)':'')):(st.deviceModel||st.deviceType||'—');
      poolRows.push('<tr><td>'+escapeHtml(p.metadata.name)+'</td><td>'+escapeHtml(devType)+'</td><td>'+escapeHtml(st.interface||'—')+'</td><td>'+escapeHtml(sp.mountPoint||'—')+'</td><td>'+fmtPoolBytes(st.totalBytes)+'</td><td>'+fmtPoolBytes(st.usedBytes)+'</td><td>'+fmtPoolBytes(st.availBytes)+'</td><td>'+statusBadge(st.phase||'—')+'</td><td>'+(sp.default?'Yes':'')+'</td><td>'+pi.disks.length+'</td><td>'+pi.pvcs.length+'</td></tr>');
    });
    plt.innerHTML=poolRows.join('');
  }
  initSort('pools-tbl');reapplySort('pools-tbl');

  // PVCs
  var pt=document.getElementById('pvcs-tbl');
  if(pvcItems.length===0){pt.innerHTML='<tr><td colspan="10" class="muted" style="text-align:center;padding:8px">No PVCs</td></tr>';}
  else{
    var pvcRows=[];
    pvcItems.forEach(function(p){
      var s=p.spec||{};
      var st=p.status||{};
      var ann=p.metadata?.annotations||{};
      var ptype=ann['vkube.io/pvc-type']||'directory';
      var usedBytes=ann['vkube.io/used-bytes'];
      var used=usedBytes?fmtBytes(parseInt(usedBytes)):'—';
      var capBytes=ann['vkube.io/pvc-capacity'];
      var cap=capBytes?fmtBytes(parseInt(capBytes)):(st.capacity?.storage||s.resources?.requests?.storage||'');
      if(!cap||cap==='0'||String(cap)==='undefined') cap='—';
      var thinRatio=ann['vkube.io/thin-ratio'];
      var thinPct=thinRatio?(parseFloat(thinRatio)*100).toFixed(1)+'%':'—';
      var pool=pvcPool(p);
      var ns=p.metadata.namespace||'default';
      var name=p.metadata.name||'';
      var typeBadge=ptype==='file-backed'?'<span style="color:var(--cyan)">File</span>':'Dir';
      var actions='';
      if(_poolNames.length>1) actions+='<button class="btn" style="font-size:10px;padding:2px 6px;margin-right:4px" onclick="openMove(\''+escapeHtml(ns)+'/'+escapeHtml(name)+'\',\''+escapeHtml(pool)+'\')">Move</button>';
      if(ptype==='directory') actions+='<button class="btn" style="font-size:10px;padding:2px 6px;margin-right:4px" onclick="openMigrateType(\''+escapeHtml(ns)+'\',\''+escapeHtml(name)+'\',\'directory\')">To File</button>';
      else actions+='<button class="btn" style="font-size:10px;padding:2px 6px;margin-right:4px" onclick="openMigrateType(\''+escapeHtml(ns)+'\',\''+escapeHtml(name)+'\',\'file-backed\')">To Dir</button>';
      if(ptype==='file-backed') actions+='<button class="btn" style="font-size:10px;padding:2px 6px" onclick="openResizePVC(\''+escapeHtml(ns)+'\',\''+escapeHtml(name)+'\',\''+escapeHtml(String(capBytes||''))+'\')">Resize</button>';
      pvcRows.push('<tr><td>'+escapeHtml(name)+'</td><td>'+escapeHtml(ns)+'</td><td>'+typeBadge+'</td><td>'+escapeHtml(pool)+'</td><td>'+statusBadge(st.phase||'Bound')+'</td><td>'+used+'</td><td>'+escapeHtml(String(cap))+'</td><td>'+thinPct+'</td><td>'+timeSince(p.metadata?.creationTimestamp)+'</td><td>'+actions+'</td></tr>');
    });
    pt.innerHTML=pvcRows.join('');
  }
  initSort('pvcs-tbl');reapplySort('pvcs-tbl');

  // CDROMs
  var ct=document.getElementById('cdroms-tbl');
  if(cdromItems.length===0){ct.innerHTML='<tr><td colspan="6" class="muted" style="text-align:center;padding:8px">No CDROMs</td></tr>';}
  else{
    var cdRows=[];
    cdromItems.forEach(function(c){
      var s=c.spec||{};
      var st=c.status||{};
      cdRows.push('<tr><td>'+escapeHtml(c.metadata.name)+'</td><td>'+escapeHtml(s.version||'—')+'</td><td>'+statusBadge(s.phase||st.phase||'—')+'</td><td>'+fmtSize(s.size||st.size)+'</td><td>'+(s.subscribers?.length||0)+'</td><td>'+timeSince(c.metadata?.creationTimestamp)+'</td></tr>');
    });
    ct.innerHTML=cdRows.join('');
  }
  initSort('cdroms-tbl');reapplySort('cdroms-tbl');

  // Disks
  var dt=document.getElementById('disks-tbl');
  if(diskItems.length===0){dt.innerHTML='<tr><td colspan="11" class="muted" style="text-align:center;padding:8px">No disks</td></tr>';}
  else{
    var diskRows=[];
    diskItems.forEach(function(d){
      var s=d.spec||{};
      var st=d.status||{};
      var phase=s.phase||st.phase||'—';
      var isReady=phase==='Ready';
      var actions='';
      if(isReady) actions+='<button class="btn" style="font-size:10px;padding:2px 6px;margin-right:4px" onclick="showCloneDisk(\''+escapeHtml(d.metadata.name)+'\')">Clone</button>';
      actions+='<button class="btn btn-danger" style="font-size:10px;padding:2px 6px" onclick="deleteDisk(\''+escapeHtml(d.metadata.name)+'\')">Delete</button>';
      var actualStr=st.actualBytes?fmtPoolBytes(st.actualBytes):'—';
      var thinPct=st.thinRatio?(st.thinRatio*100).toFixed(1)+'%':'—';
      var lastActive=st.lastModified?timeSince(st.lastModified):'—';
      diskRows.push('<tr><td>'+escapeHtml(d.metadata.name)+'</td><td>'+escapeHtml(s.host||'—')+'</td><td>'+escapeHtml(s.storagePool||_defaultPool||'—')+'</td><td>'+fmtSize(s.sizeGB?s.sizeGB*1024*1024*1024:(s.size||st.diskSize||st.size))+'</td><td>'+actualStr+'</td><td>'+thinPct+'</td><td>'+statusBadge(phase)+'</td><td>'+lastActive+'</td><td>'+escapeHtml(s.source||'—')+'</td><td>'+timeSince(d.metadata?.creationTimestamp)+'</td><td>'+actions+'</td></tr>');
    });
    dt.innerHTML=diskRows.join('');
  }
  initSort('disks-tbl');reapplySort('disks-tbl');
}

// ── Create Disk ──
function showCreateDisk(){
  document.getElementById('dk-name').value='';
  document.getElementById('dk-size').value='20';
  document.getElementById('dk-host').value='';
  document.getElementById('dk-desc').value='';
  document.getElementById('dk-status').textContent='';
  // Populate source options
  var sel=document.getElementById('dk-source');
  sel.innerHTML='<option value="">(empty — thin volume)</option>';
  _cdromNames.forEach(function(n){
    sel.innerHTML+='<option value="'+escapeHtml(n)+'">cdrom: '+escapeHtml(n)+'</option>';
  });
  _diskNames.forEach(function(n){
    sel.innerHTML+='<option value="'+escapeHtml(n)+'">disk: '+escapeHtml(n)+'</option>';
  });
  // Populate pool options
  var psel=document.getElementById('dk-pool');
  psel.innerHTML='';
  _poolNames.forEach(function(n){
    var opt=document.createElement('option');
    opt.value=n;opt.textContent=n;
    if(n===_defaultPool) opt.selected=true;
    psel.appendChild(opt);
  });
  if(_poolNames.length===0) psel.innerHTML='<option value="">default</option>';
  document.getElementById('disk-modal').classList.add('show');
}

async function submitCreateDisk(){
  var name=document.getElementById('dk-name').value.trim();
  var sizeGB=parseInt(document.getElementById('dk-size').value);
  var source=document.getElementById('dk-source').value;
  var pool=document.getElementById('dk-pool').value;
  var host=document.getElementById('dk-host').value.trim();
  var desc=document.getElementById('dk-desc').value.trim();
  var st=document.getElementById('dk-status');
  if(!name){st.textContent='Name is required';st.style.color='var(--red)';return;}
  if(!sizeGB||sizeGB<1){st.textContent='Size must be at least 1 GB';st.style.color='var(--red)';return;}
  st.textContent='Creating...';st.style.color='var(--comment)';
  var body={metadata:{name:name},spec:{sizeGB:sizeGB}};
  if(source) body.spec.source=source;
  if(pool) body.spec.storagePool=pool;
  if(host) body.spec.host=host;
  if(desc) body.spec.description=desc;
  try{
    var res=await fetch(API+'/api/v1/iscsi-disks',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
    if(res.ok){
      st.textContent='Created successfully';st.style.color='var(--green)';
      setTimeout(function(){hideModal('disk-modal');load();},1000);
    } else {
      var txt=await res.text();
      st.textContent='Error: '+txt;st.style.color='var(--red)';
    }
  }catch(e){st.textContent='Error: '+e;st.style.color='var(--red)';}
}

// ── Clone Disk ──
function showCloneDisk(sourceName){
  document.getElementById('cl-source').value=sourceName;
  document.getElementById('cl-name').value=sourceName+'-clone';
  document.getElementById('cl-host').value='';
  document.getElementById('cl-status').textContent='';
  document.getElementById('clone-modal').classList.add('show');
}

async function submitCloneDisk(){
  var source=document.getElementById('cl-source').value;
  var newName=document.getElementById('cl-name').value.trim();
  var host=document.getElementById('cl-host').value.trim();
  var st=document.getElementById('cl-status');
  if(!newName){st.textContent='New name is required';st.style.color='var(--red)';return;}
  st.textContent='Cloning...';st.style.color='var(--comment)';
  var body={newName:newName};
  if(host) body.host=host;
  try{
    var res=await fetch(API+'/api/v1/iscsi-disks/'+encodeURIComponent(source)+'/clone',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
    if(res.ok){
      st.textContent='Clone started';st.style.color='var(--green)';
      setTimeout(function(){hideModal('clone-modal');load();},1000);
    } else {
      var txt=await res.text();
      st.textContent='Error: '+txt;st.style.color='var(--red)';
    }
  }catch(e){st.textContent='Error: '+e;st.style.color='var(--red)';}
}

// ── Delete Disk ──
async function deleteDisk(name){
  if(!confirm('Delete iSCSI disk "'+name+'"? This removes the disk file and iSCSI target.')) return;
  try{
    var res=await fetch(API+'/api/v1/iscsi-disks/'+encodeURIComponent(name),{method:'DELETE'});
    if(res.ok){load();}
    else{var txt=await res.text();alert('Delete failed: '+txt);}
  }catch(e){alert('Delete failed: '+e);}
}

var _moveResource='';
function openMove(resource,currentPool){
  _moveResource=resource;
  document.getElementById('move-title').textContent='Move '+resource;
  var sel=document.getElementById('move-target');
  sel.innerHTML='';
  _poolNames.forEach(function(n){
    if(n!==currentPool){
      var opt=document.createElement('option');
      opt.value=n;opt.textContent=n;
      sel.appendChild(opt);
    }
  });
  document.getElementById('move-status').textContent='';
  document.getElementById('move-modal').style.display='flex';
}
function closeMove(){
  document.getElementById('move-modal').style.display='none';
}
async function doMove(){
  var target=document.getElementById('move-target').value;
  if(!target) return;
  var st=document.getElementById('move-status');
  st.textContent='Moving...';
  st.style.color='var(--comment)';
  var res=await fetch(API+'/api/v1/storagepools/'+encodeURIComponent(target)+'/migrate',{
    method:'POST',
    headers:{'Content-Type':'application/json'},
    body:JSON.stringify({resourceType:'pvc',resourceName:_moveResource,targetPool:target,purgeSource:true})
  }).then(function(r){return r.json();}).catch(function(e){return {status:'error',message:String(e)};});
  if(res.status==='success'){
    st.textContent='Moved successfully ('+fmtBytes(res.bytesCopied||0)+')';
    st.style.color='var(--green)';
    setTimeout(function(){closeMove();load();},1500);
  } else {
    st.textContent='Error: '+(res.message||'unknown');
    st.style.color='var(--red)';
  }
}

// ── Migrate PVC Type ──
var _migrateNs='',_migrateName='',_migrateFrom='';
function openMigrateType(ns,name,currentType){
  _migrateNs=ns;_migrateName=name;_migrateFrom=currentType;
  var target=currentType==='directory'?'file-backed':'directory';
  document.getElementById('migrate-type-title').textContent='Convert '+ns+'/'+name+' to '+target;
  document.getElementById('migrate-type-info').textContent='Current type: '+currentType+'. Pods using this PVC will be stopped during conversion.';
  document.getElementById('migrate-type-status').textContent='';
  var capRow=document.getElementById('migrate-type-capacity-row');
  if(target==='file-backed'){capRow.style.display='block';document.getElementById('migrate-type-capacity').value='10Gi';}
  else{capRow.style.display='none';}
  document.getElementById('migrate-type-modal').style.display='flex';
}
function closeMigrateType(){document.getElementById('migrate-type-modal').style.display='none';}
async function doMigrateType(){
  var target=_migrateFrom==='directory'?'file-backed':'directory';
  var body={targetType:target};
  if(target==='file-backed'){
    var cap=document.getElementById('migrate-type-capacity').value.trim();
    if(!cap){document.getElementById('migrate-type-status').textContent='Capacity is required';document.getElementById('migrate-type-status').style.color='var(--red)';return;}
    body.capacity=cap;
  }
  var st=document.getElementById('migrate-type-status');
  st.textContent='Converting...';st.style.color='var(--comment)';
  try{
    var res=await fetch(API+'/api/v1/namespaces/'+encodeURIComponent(_migrateNs)+'/persistentvolumeclaims/'+encodeURIComponent(_migrateName)+'/migrate',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
    if(res.ok){st.textContent='Converted successfully';st.style.color='var(--green)';setTimeout(function(){closeMigrateType();load();},1500);}
    else{var txt=await res.text();st.textContent='Error: '+txt;st.style.color='var(--red)';}
  }catch(e){st.textContent='Error: '+e;st.style.color='var(--red)';}
}

// ── Resize PVC ──
var _resizeNs='',_resizeName='';
function openResizePVC(ns,name,currentBytes){
  _resizeNs=ns;_resizeName=name;
  document.getElementById('resize-pvc-status').textContent='';
  var gb=currentBytes?Math.ceil(parseInt(currentBytes)/(1024*1024*1024)):10;
  document.getElementById('resize-pvc-capacity').value=gb+'Gi';
  document.getElementById('resize-pvc-modal').style.display='flex';
}
function closeResizePVC(){document.getElementById('resize-pvc-modal').style.display='none';}
async function doResizePVC(){
  var cap=document.getElementById('resize-pvc-capacity').value.trim();
  if(!cap){document.getElementById('resize-pvc-status').textContent='Capacity is required';document.getElementById('resize-pvc-status').style.color='var(--red)';return;}
  var st=document.getElementById('resize-pvc-status');
  st.textContent='Resizing...';st.style.color='var(--comment)';
  try{
    var res=await fetch(API+'/api/v1/namespaces/'+encodeURIComponent(_resizeNs)+'/persistentvolumeclaims/'+encodeURIComponent(_resizeName)+'/resize',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({capacity:cap})});
    if(res.ok){st.textContent='Resized successfully';st.style.color='var(--green)';setTimeout(function(){closeResizePVC();load();},1500);}
    else{var txt=await res.text();st.textContent='Error: '+txt;st.style.color='var(--red)';}
  }catch(e){st.textContent='Error: '+e;st.style.color='var(--red)';}
}

load(); _uiInterval(load,30000);
`
	write(w, c.pageWithJS("Storage", "Storage", body, js))
}
