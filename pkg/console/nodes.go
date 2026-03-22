package console

import "net/http"

func (c *Console) handleNodes(w http.ResponseWriter, r *http.Request) {
	body := `<h1>Nodes</h1>
<table><thead><tr><th>Name</th><th>Architecture</th><th>IP</th><th>OS</th><th>Version</th><th>Status</th><th>Heartbeat</th><th>stormd</th></tr></thead>
<tbody id="tbl"><tr><td colspan="8" class="loading">Loading...</td></tr></tbody></table>`

	js := `
async function load(){
  const data=await apiGet(API+'/api/v1/nodes');
  const tb=document.getElementById('tbl');
  const items=data?.items||[];
  if(items.length===0){tb.innerHTML='<tr><td colspan="8" class="muted" style="text-align:center;padding:16px">No nodes</td></tr>';return;}
  const rows=[];
  items.forEach(n=>{
    const addr=n.status?.addresses?.find(a=>a.type==='InternalIP')?.address||'—';
    const info=n.status?.nodeInfo||{};
    const ready=n.status?.conditions?.find(c=>c.type==='Ready');
    const hb=ready?.lastHeartbeatTime;
    rows.push('<tr><td><a href="nodes/'+encodeURIComponent(n.metadata.name)+'">'+escapeHtml(n.metadata.name)+'</a></td>'
      +'<td>'+escapeHtml(info.architecture||'—')+'</td>'
      +'<td>'+escapeHtml(addr)+'</td>'
      +'<td>'+escapeHtml(info.operatingSystem||'—')+'</td>'
      +'<td>'+escapeHtml(info.kubeletVersion||'—')+'</td>'
      +'<td>'+statusBadge(ready?.status==='True'?'Ready':'NotReady')+'</td>'
      +'<td>'+timeSince(hb)+'</td>'
      +'<td><a href="http://'+addr+':9080/ui/" target="_blank" class="btn btn-primary">UI</a></td></tr>');
  });
  tb.innerHTML=rows.join('');
  initSort('tbl');
  reapplySort('tbl');
}
load(); _uiInterval(load,15000);
`
	write(w, c.pageWithJS("Nodes", "Nodes", body, js))
}

func (c *Console) handleNodeDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	body := `<h1>Node: ` + name + `</h1>
<div class="card"><h3>Info</h3><div class="kv" id="info"><div class="loading">Loading...</div></div></div>
<div class="tabs">
  <div class="tab active" onclick="switchTab('procs')">Processes</div>
  <div class="tab" onclick="switchTab('mounts')">Disk Mounts</div>
  <div class="tab" onclick="switchTab('pods')">Pods</div>
</div>
<div id="procs" class="tab-content active"><table><thead><tr><th>Name</th><th>State</th><th>PID</th><th>Restarts</th><th>Uptime</th><th>Actions</th></tr></thead><tbody id="procs-tbl"></tbody></table></div>
<div id="mounts" class="tab-content"><table><thead><tr><th>Mount</th><th>Device</th><th>FS</th><th>Usage</th><th>Used/Total</th></tr></thead><tbody id="mounts-tbl"></tbody></table></div>
<div id="pods" class="tab-content"><table><thead><tr><th>Name</th><th>Namespace</th><th>Status</th><th>IP</th><th>Image</th></tr></thead><tbody id="pods-tbl"></tbody></table></div>`

	js := `
const nodeName=` + jsStr(name) + `;
let stormdAddr='';

function switchTab(id){
  document.querySelectorAll('.tab-content').forEach(e=>e.classList.remove('active'));
  document.querySelectorAll('.tab').forEach(e=>e.classList.remove('active'));
  document.getElementById(id).classList.add('active');
  event.target.classList.add('active');
}

async function load(){
  const nodes=await apiGet(API+'/api/v1/nodes');
  const node=(nodes?.items||[]).find(n=>n.metadata.name===nodeName);
  if(!node){document.getElementById('info').innerHTML='<div class="muted">Node not found</div>';return;}
  const addr=node.status?.addresses?.find(a=>a.type==='InternalIP')?.address||'';
  stormdAddr=addr;
  const info=node.status?.nodeInfo||{};
  document.getElementById('info').innerHTML=
    '<div class="k">Architecture</div><div class="v">'+escapeHtml(info.architecture||'—')+'</div>'
    +'<div class="k">OS</div><div class="v">'+escapeHtml(info.operatingSystem||'—')+'</div>'
    +'<div class="k">Version</div><div class="v">'+escapeHtml(info.kubeletVersion||'—')+'</div>'
    +'<div class="k">IP</div><div class="v">'+escapeHtml(addr)+'</div>';

  // stormd processes
  if(addr){
    try{
      const procs=await fetch('http://'+addr+':9080/api/v1/processes').then(r=>r.json()).catch(()=>[]);
      const tb=document.getElementById('procs-tbl');
      const procRows=[];
      (procs||[]).forEach(p=>{
        procRows.push('<tr><td>'+escapeHtml(p.name)+'</td><td>'+statusBadge(p.state)+'</td><td>'+escapeHtml(String(p.pid||'—'))+'</td><td>'+escapeHtml(String(p.restarts||0))+'</td><td>'+escapeHtml(p.uptime||'—')+'</td>'
          +'<td><button class="btn btn-primary" onclick="procAction(\'restart\',\''+escapeHtml(p.name)+'\')">Restart</button> '
          +'<button class="btn btn-success" onclick="procAction(\'start\',\''+escapeHtml(p.name)+'\')">Start</button> '
          +'<button class="btn btn-danger" onclick="procAction(\'stop\',\''+escapeHtml(p.name)+'\')">Stop</button></td></tr>');
      });
      tb.innerHTML=procRows.join('');
      initSort('procs-tbl');reapplySort('procs-tbl');
      // mounts
      const mnts=await fetch('http://'+addr+':9080/api/v1/mounts').then(r=>r.json()).catch(()=>[]);
      const mb=document.getElementById('mounts-tbl');
      const mntRows=[];
      (mnts||[]).forEach(m=>{
        const pct=m.total>0?Math.round(m.used/m.total*100):0;
        const color=pct>90?'#e94560':pct>70?'#f1fa8c':'#50fa7b';
        mntRows.push('<tr><td>'+escapeHtml(m.mount_point||m.mountPoint||'—')+'</td><td>'+escapeHtml(m.device||'—')+'</td><td>'+escapeHtml(m.fs_type||m.fsType||'—')+'</td>'
          +'<td><div class="usage-bar"><div class="fill" style="width:'+pct+'%;background:'+color+'"></div></div> '+pct+'%</td>'
          +'<td>'+fmtBytes(m.used)+' / '+fmtBytes(m.total)+'</td></tr>');
      });
      mb.innerHTML=mntRows.join('');
      initSort('mounts-tbl');reapplySort('mounts-tbl');
    }catch(e){}
  }

  // Pods on this node
  const allPods=await apiGet(API+'/api/v1/pods');
  const nodePods=(allPods?.items||[]).filter(p=>p.metadata?.annotations?.['vkube.io/node']===nodeName);
  const pb=document.getElementById('pods-tbl');
  const podRows=[];
  nodePods.forEach(p=>{
    const ns=p.metadata.namespace||'default';
    podRows.push('<tr><td><a href="pods/'+encodeURIComponent(ns)+'/'+encodeURIComponent(p.metadata.name)+'">'+escapeHtml(p.metadata.name)+'</a></td><td>'+escapeHtml(ns)+'</td><td>'+statusBadge(p.status?.phase)+'</td><td>'+escapeHtml(p.status?.podIP||'—')+'</td><td>'+shortImage(p.spec?.containers?.[0]?.image)+'</td></tr>');
  });
  pb.innerHTML=podRows.join('');
  initSort('pods-tbl');reapplySort('pods-tbl');
}

async function procAction(action,name){
  if(stormdAddr) await fetch('http://'+stormdAddr+':9080/api/v1/processes/'+name+'/'+action,{method:'POST'});
  load();
}

load(); _uiInterval(load,15000);
`
	write(w, c.pageWithJS("Node: "+name, "Nodes", body, js))
}

func jsStr(s string) string {
	return "'" + s + "'"
}
