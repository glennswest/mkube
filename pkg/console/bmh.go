package console

import "net/http"

func (c *Console) handleBMH(w http.ResponseWriter, r *http.Request) {
	body := `<h1>Bare Metal Hosts</h1>
<table><thead><tr><th>Name</th><th>Namespace</th><th>State</th><th>Power</th><th>Network</th><th>IP</th><th>Image/Disk</th><th>Actions</th></tr></thead>
<tbody id="tbl"></tbody></table>`

	js := `
async function load(){
  const data=await apiGet(API+'/api/v1/baremetalhosts');
  const tb=document.getElementById('tbl');
  tb.innerHTML='';
  (data?.items||[]).forEach(b=>{
    const ns=b.metadata.namespace||'default';
    const s=b.spec||{};
    const st=b.status||{};
    const power=s.online?'ON':'OFF';
    const imgDisk=s.disk?shortImage(s.disk):shortImage(s.image);
    tb.innerHTML+='<tr><td><a href="/ui/bmh/'+ns+'/'+escapeHtml(b.metadata.name)+'">'+escapeHtml(b.metadata.name)+'</a></td>'
      +'<td>'+escapeHtml(ns)+'</td>'
      +'<td>'+statusBadge(s.state||st.state||'—')+'</td>'
      +'<td>'+statusBadge(power)+'</td>'
      +'<td>'+escapeHtml(s.network||'—')+'</td>'
      +'<td>'+escapeHtml(s.ip||'—')+'</td>'
      +'<td>'+escapeHtml(imgDisk)+'</td>'
      +'<td><button class="btn btn-success" onclick="setPower(\''+ns+'\',\''+escapeHtml(b.metadata.name)+'\',true)">On</button> '
      +'<button class="btn btn-danger" onclick="setPower(\''+ns+'\',\''+escapeHtml(b.metadata.name)+'\',false)">Off</button></td></tr>';
  });
}

async function setPower(ns,name,on){
  await apiPatch(API+'/api/v1/namespaces/'+ns+'/baremetalhosts/'+name,{spec:{online:on}});
  load();
}

load(); setInterval(load,15000);
`
	write(w, c.pageWithJS("Bare Metal Hosts", "BMH", body, js))
}

func (c *Console) handleBMHDetail(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	name := r.PathValue("name")

	body := `<h1>BMH: ` + name + `</h1>
<div class="card"><h3>Spec</h3><div class="kv" id="spec"></div></div>
<div class="card"><h3>BMC (IPMI)</h3><div class="kv" id="bmc"></div></div>
<div class="card"><h3>Status</h3><div class="kv" id="status"></div></div>
<div class="flex mt">
  <button class="btn btn-success" onclick="setPower(true)">Power On</button>
  <button class="btn btn-danger" onclick="setPower(false)">Power Off</button>
</div>`

	js := `
const bNs=` + jsStr(ns) + `,bName=` + jsStr(name) + `;
async function load(){
  const b=await apiGet(API+'/api/v1/namespaces/'+bNs+'/baremetalhosts/'+bName);
  if(!b) return;
  const s=b.spec||{};
  const bmc=s.bmc||{};
  const st=b.status||{};
  document.getElementById('spec').innerHTML=
    kv('Online',String(!!s.online))+kv('State',s.state||'—')+kv('Network',s.network)
    +kv('IP',s.ip)+kv('Hostname',s.hostname)+kv('MAC',s.mac)
    +kv('Image',s.image)+kv('Disk',s.disk)+kv('Template',s.template)
    +kv('BootConfigRef',s.bootConfigRef);
  document.getElementById('bmc').innerHTML=
    kv('Address',bmc.address)+kv('Network',bmc.network)+kv('MAC',bmc.mac)+kv('Hostname',bmc.hostname);
  document.getElementById('status').innerHTML=Object.entries(st).map(([k,v])=>kv(k,typeof v==='object'?JSON.stringify(v):v)).join('');
}
function kv(k,v){ return '<div class="k">'+escapeHtml(k)+'</div><div class="v">'+escapeHtml(String(v||'—'))+'</div>'; }
async function setPower(on){
  await apiPatch(API+'/api/v1/namespaces/'+bNs+'/baremetalhosts/'+bName,{spec:{online:on}});
  load();
}
load(); setInterval(load,15000);
`
	write(w, c.pageWithJS("BMH: "+name, "BMH", body, js))
}
