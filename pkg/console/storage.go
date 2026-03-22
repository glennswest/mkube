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
  <table><thead><tr><th>Name</th><th>Namespace</th><th>Status</th><th>Used</th><th>Size</th><th>Created</th></tr></thead><tbody id="pvcs-tbl"><tr><td colspan="6" class="loading">Loading...</td></tr></tbody></table>
</div>
<div id="cdroms" class="tab-content">
  <table><thead><tr><th>Name</th><th>Version</th><th>Phase</th><th>Size</th><th>Subscribers</th><th>Created</th></tr></thead><tbody id="cdroms-tbl"></tbody></table>
</div>
<div id="disks" class="tab-content">
  <table><thead><tr><th>Name</th><th>Host</th><th>Pool</th><th>Size</th><th>Phase</th><th>Source</th><th>Created</th><th>Last Used</th></tr></thead><tbody id="disks-tbl"></tbody></table>
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
  const u=['B','KiB','MiB','GiB','TiB'];
  let i=0; let v=b;
  while(v>=1024&&i<u.length-1){v/=1024;i++;}
  return v.toFixed(1)+' '+u[i];
}

function pctUsed(used,total){
  if(!total||total<=0) return 0;
  return Math.round((used/total)*100);
}

async function load(){
  const [pools,pvcs,cdroms,disks]=await Promise.all([
    apiGet(API+'/api/v1/storagepools'),
    apiGet(API+'/api/v1/persistentvolumeclaims'),
    apiGet(API+'/api/v1/iscsi-cdroms'),
    apiGet(API+'/api/v1/iscsi-disks'),
  ]);

  // Pools — capacity cards
  const pc=document.getElementById('pool-cards');
  const poolItems=pools?.items||[];
  if(poolItems.length===0){
    pc.innerHTML='<div class="muted" style="padding:8px">No storage pools discovered</div>';
  } else {
    pc.innerHTML='<div style="display:flex;gap:12px;flex-wrap:wrap;margin-bottom:12px">'+poolItems.map(p=>{
      const st=p.status||{};
      const pct=pctUsed(st.usedBytes,st.totalBytes);
      const color=pct>90?'var(--red)':pct>75?'var(--orange)':'var(--green)';
      const def=p.spec?.default?' (default)':'';
      return '<div class="card" style="min-width:200px;flex:1;max-width:300px">'+
        '<h4>'+escapeHtml(p.metadata.name)+def+'</h4>'+
        '<div style="font-size:12px;color:var(--comment)">'+escapeHtml(st.deviceType||'')+' '+escapeHtml(st.interface||'')+'</div>'+
        '<div style="margin:8px 0;background:var(--selection);border-radius:4px;height:16px;overflow:hidden">'+
        '<div style="width:'+pct+'%;height:100%;background:'+color+';border-radius:4px;transition:width 0.3s"></div></div>'+
        '<div style="font-size:12px">'+fmtPoolBytes(st.usedBytes)+' / '+fmtPoolBytes(st.totalBytes)+' ('+pct+'%)</div>'+
        '<div style="font-size:11px;color:var(--comment);margin-top:4px">'+st.diskCount+' disks, '+st.pvcCount+' PVCs</div>'+
        '</div>';
    }).join('')+'</div>';
  }

  // Pools table
  const plt=document.getElementById('pools-tbl');
  if(poolItems.length===0){plt.innerHTML='<tr><td colspan="11" class="muted" style="text-align:center;padding:8px">No pools</td></tr>';}
  else{
    const poolRows=[];
    poolItems.forEach(p=>{
      const sp=p.spec||{};const st=p.status||{};
      const devType=st.raidType?('RAID-'+st.raidType):(st.deviceType||'—');
      poolRows.push('<tr><td>'+escapeHtml(p.metadata.name)+'</td><td>'+escapeHtml(devType)+'</td><td>'+escapeHtml(st.interface||'—')+'</td><td>'+escapeHtml(sp.mountPoint||'—')+'</td><td>'+fmtPoolBytes(st.totalBytes)+'</td><td>'+fmtPoolBytes(st.usedBytes)+'</td><td>'+fmtPoolBytes(st.availBytes)+'</td><td>'+statusBadge(st.phase||'—')+'</td><td>'+(sp.default?'Yes':'')+'</td><td>'+(st.diskCount||0)+'</td><td>'+(st.pvcCount||0)+'</td></tr>');
    });
    plt.innerHTML=poolRows.join('');
  }
  initSort('pools-tbl');reapplySort('pools-tbl');

  // PVCs
  const pt=document.getElementById('pvcs-tbl');
  const pvcItems=pvcs?.items||[];
  if(pvcItems.length===0){pt.innerHTML='<tr><td colspan="6" class="muted" style="text-align:center;padding:8px">No PVCs</td></tr>';}
  else{
    const pvcRows=[];
    pvcItems.forEach(p=>{
      const s=p.spec||{};
      const st=p.status||{};
      const ann=p.metadata?.annotations||{};
      const usedBytes=ann['vkube.io/used-bytes'];
      const used=usedBytes?fmtBytes(parseInt(usedBytes)):'—';
      const cap=st.capacity?.storage||s.resources?.requests?.storage||'—';
      pvcRows.push('<tr><td>'+escapeHtml(p.metadata.name)+'</td><td>'+escapeHtml(p.metadata.namespace||'default')+'</td><td>'+statusBadge(st.phase||'Bound')+'</td><td>'+used+'</td><td>'+escapeHtml(String(cap))+'</td><td>'+timeSince(p.metadata?.creationTimestamp)+'</td></tr>');
    });
    pt.innerHTML=pvcRows.join('');
  }
  initSort('pvcs-tbl');reapplySort('pvcs-tbl');

  // CDROMs
  const ct=document.getElementById('cdroms-tbl');
  const cdromItems=cdroms?.items||[];
  if(cdromItems.length===0){ct.innerHTML='<tr><td colspan="6" class="muted" style="text-align:center;padding:8px">No CDROMs</td></tr>';}
  else{
    const cdRows=[];
    cdromItems.forEach(c=>{
      const s=c.spec||{};
      const st=c.status||{};
      cdRows.push('<tr><td>'+escapeHtml(c.metadata.name)+'</td><td>'+escapeHtml(s.version||'—')+'</td><td>'+statusBadge(s.phase||st.phase||'—')+'</td><td>'+fmtSize(s.size||st.size)+'</td><td>'+(s.subscribers?.length||0)+'</td><td>'+timeSince(c.metadata?.creationTimestamp)+'</td></tr>');
    });
    ct.innerHTML=cdRows.join('');
  }
  initSort('cdroms-tbl');reapplySort('cdroms-tbl');

  // Disks
  const dt=document.getElementById('disks-tbl');
  const diskItems=disks?.items||[];
  if(diskItems.length===0){dt.innerHTML='<tr><td colspan="8" class="muted" style="text-align:center;padding:8px">No disks</td></tr>';}
  else{
    const diskRows=[];
    diskItems.forEach(d=>{
      const s=d.spec||{};
      const st=d.status||{};
      diskRows.push('<tr><td>'+escapeHtml(d.metadata.name)+'</td><td>'+escapeHtml(s.host||'—')+'</td><td>'+escapeHtml(s.storagePool||'—')+'</td><td>'+fmtSize(s.size||st.size)+'</td><td>'+statusBadge(s.phase||st.phase||'—')+'</td><td>'+escapeHtml(s.source||'—')+'</td><td>'+timeSince(d.metadata?.creationTimestamp)+'</td><td>'+timeSince(st.lastUsed||s.lastUsed)+'</td></tr>');
    });
    dt.innerHTML=diskRows.join('');
  }
  initSort('disks-tbl');reapplySort('disks-tbl');
}
load(); _uiInterval(load,30000);
`
	write(w, c.pageWithJS("Storage", "Storage", body, js))
}
