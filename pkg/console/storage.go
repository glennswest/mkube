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
  <table><thead><tr><th>Name</th><th>Namespace</th><th>Pool</th><th>Status</th><th>Used</th><th>Size</th><th>Created</th><th></th></tr></thead><tbody id="pvcs-tbl"><tr><td colspan="8" class="loading">Loading...</td></tr></tbody></table>
</div>
<div id="cdroms" class="tab-content">
  <table><thead><tr><th>Name</th><th>Version</th><th>Phase</th><th>Size</th><th>Subscribers</th><th>Created</th></tr></thead><tbody id="cdroms-tbl"></tbody></table>
</div>
<div id="disks" class="tab-content">
  <table><thead><tr><th>Name</th><th>Host</th><th>Pool</th><th>Size</th><th>Phase</th><th>Source</th><th>Created</th><th>Last Used</th></tr></thead><tbody id="disks-tbl"></tbody></table>
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
  poolItems.forEach(function(p){ idx[p.metadata.name]={disks:[],pvcs:[]}; });
  pvcItems.forEach(function(p){
    var pool=pvcPool(p);
    if(idx[pool]) idx[pool].pvcs.push(p.metadata.namespace+'/'+p.metadata.name);
    else if(idx[_defaultPool]) idx[_defaultPool].pvcs.push(p.metadata.namespace+'/'+p.metadata.name);
  });
  diskItems.forEach(function(d){
    var pool=diskPool(d);
    if(idx[pool]) idx[pool].disks.push(d.metadata.name);
    else if(idx[_defaultPool]) idx[_defaultPool].disks.push(d.metadata.name);
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
      var diskList=pi.disks.length>0?pi.disks.map(function(n){return escapeHtml(n);}).join(', '):'none';
      var pvcList=pi.pvcs.length>0?pi.pvcs.map(function(n){return escapeHtml(n);}).join(', '):'none';
      return '<div class="card" style="min-width:220px;flex:1;max-width:340px">'+
        '<h4>'+escapeHtml(p.metadata.name)+def+'</h4>'+
        '<div style="font-size:12px;color:var(--comment)">'+escapeHtml(st.deviceType||'')+' '+escapeHtml(st.interface||'')+'</div>'+
        '<div style="margin:8px 0;background:var(--selection);border-radius:4px;height:16px;overflow:hidden">'+
        '<div style="width:'+pct+'%;height:100%;background:'+color+';border-radius:4px;transition:width 0.3s"></div></div>'+
        '<div style="font-size:12px">'+fmtPoolBytes(st.usedBytes)+' / '+fmtPoolBytes(st.totalBytes)+' ('+pct+'%)</div>'+
        '<div style="font-size:11px;color:var(--comment);margin-top:6px"><b>Disks</b> ('+pi.disks.length+'): '+diskList+'</div>'+
        '<div style="font-size:11px;color:var(--comment);margin-top:2px"><b>PVCs</b> ('+pi.pvcs.length+'): '+pvcList+'</div>'+
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
      var devType=st.raidType?('RAID-'+st.raidType):(st.deviceType||'—');
      poolRows.push('<tr><td>'+escapeHtml(p.metadata.name)+'</td><td>'+escapeHtml(devType)+'</td><td>'+escapeHtml(st.interface||'—')+'</td><td>'+escapeHtml(sp.mountPoint||'—')+'</td><td>'+fmtPoolBytes(st.totalBytes)+'</td><td>'+fmtPoolBytes(st.usedBytes)+'</td><td>'+fmtPoolBytes(st.availBytes)+'</td><td>'+statusBadge(st.phase||'—')+'</td><td>'+(sp.default?'Yes':'')+'</td><td>'+pi.disks.length+'</td><td>'+pi.pvcs.length+'</td></tr>');
    });
    plt.innerHTML=poolRows.join('');
  }
  initSort('pools-tbl');reapplySort('pools-tbl');

  // PVCs
  var pt=document.getElementById('pvcs-tbl');
  if(pvcItems.length===0){pt.innerHTML='<tr><td colspan="8" class="muted" style="text-align:center;padding:8px">No PVCs</td></tr>';}
  else{
    var pvcRows=[];
    pvcItems.forEach(function(p){
      var s=p.spec||{};
      var st=p.status||{};
      var ann=p.metadata?.annotations||{};
      var usedBytes=ann['vkube.io/used-bytes'];
      var used=usedBytes?fmtBytes(parseInt(usedBytes)):'—';
      var cap=st.capacity?.storage||s.resources?.requests?.storage||'';
      if(!cap||cap==='0'||String(cap)==='undefined') cap='—';
      var pool=pvcPool(p);
      var ns=p.metadata.namespace||'default';
      var name=p.metadata.name||'';
      var moveBtn=_poolNames.length>1?'<button class="btn" style="font-size:10px;padding:2px 6px" onclick="openMove(\''+escapeHtml(ns)+'/'+escapeHtml(name)+'\',\''+escapeHtml(pool)+'\')">Move</button>':'';
      pvcRows.push('<tr><td>'+escapeHtml(name)+'</td><td>'+escapeHtml(ns)+'</td><td>'+escapeHtml(pool)+'</td><td>'+statusBadge(st.phase||'Bound')+'</td><td>'+used+'</td><td>'+escapeHtml(String(cap))+'</td><td>'+timeSince(p.metadata?.creationTimestamp)+'</td><td>'+moveBtn+'</td></tr>');
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
  if(diskItems.length===0){dt.innerHTML='<tr><td colspan="8" class="muted" style="text-align:center;padding:8px">No disks</td></tr>';}
  else{
    var diskRows=[];
    diskItems.forEach(function(d){
      var s=d.spec||{};
      var st=d.status||{};
      diskRows.push('<tr><td>'+escapeHtml(d.metadata.name)+'</td><td>'+escapeHtml(s.host||'—')+'</td><td>'+escapeHtml(s.storagePool||_defaultPool||'—')+'</td><td>'+fmtSize(s.size||st.size)+'</td><td>'+statusBadge(s.phase||st.phase||'—')+'</td><td>'+escapeHtml(s.source||'—')+'</td><td>'+timeSince(d.metadata?.creationTimestamp)+'</td><td>'+timeSince(st.lastUsed||s.lastUsed)+'</td></tr>');
    });
    dt.innerHTML=diskRows.join('');
  }
  initSort('disks-tbl');reapplySort('disks-tbl');
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

load(); _uiInterval(load,30000);
`
	write(w, c.pageWithJS("Storage", "Storage", body, js))
}
