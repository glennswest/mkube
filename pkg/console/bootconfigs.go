package console

import "net/http"

func (c *Console) handleBootConfigs(w http.ResponseWriter, r *http.Request) {
	body := `<h1>Boot Configs</h1>
<table><thead><tr><th>Name</th><th>Kernel</th><th>Initrd</th><th>iPXE</th><th>Age</th><th></th></tr></thead>
<tbody id="tbl"><tr><td colspan="6" class="loading">Loading...</td></tr></tbody></table>`

	js := `
async function load(){
  const data=await apiGet(API+'/api/v1/bootconfigs');
  const tb=document.getElementById('tbl');
  tb.innerHTML='';
  const items=data?.items||[];
  if(items.length===0){tb.innerHTML='<tr><td colspan="6" class="muted" style="text-align:center;padding:16px">No boot configs</td></tr>';return;}
  items.forEach(b=>{
    const s=b.spec||{};
    const ipxe=s.ipxeScript?'Yes':'—';
    tb.innerHTML+='<tr><td><a href="bootconfigs/'+encodeURIComponent(b.metadata.name)+'">'+escapeHtml(b.metadata.name)+'</a></td>'
      +'<td>'+shortImage(s.kernel||'')+'</td>'
      +'<td>'+shortImage(s.initrd||'')+'</td>'
      +'<td>'+escapeHtml(ipxe)+'</td>'
      +'<td>'+timeSince(b.metadata?.creationTimestamp)+'</td>'
      +'<td><button class="btn btn-danger" onclick="del(\''+escapeHtml(b.metadata.name)+'\')">Delete</button></td></tr>';
  });
  initSort('tbl');reapplySort('tbl');
}
async function del(name){ if(confirm('Delete bootconfig '+name+'?')){ await apiDelete(API+'/api/v1/bootconfigs/'+name); load(); }}
load(); setInterval(load,15000);
`
	write(w, c.pageWithJS("Boot Configs", "BootConfigs", body, js))
}

func (c *Console) handleBootConfigDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	body := `<h1>BootConfig: ` + name + `</h1>
<div class="card"><h3>Spec</h3><div class="kv" id="spec"><div class="loading">Loading...</div></div></div>
<div class="card"><h3>Labels</h3><div class="kv" id="labels"></div></div>
<div class="card"><h3>Referencing Hosts</h3>
<table><thead><tr><th>BMH</th><th>Namespace</th><th>State</th><th>Power</th></tr></thead><tbody id="refs"></tbody></table></div>`

	js := `
const bcName=` + jsStr(name) + `;
async function load(){
  const bc=await apiGet(API+'/api/v1/bootconfigs/'+bcName);
  if(!bc){document.getElementById('spec').innerHTML='<div class="muted">Not found</div>';return;}
  const s=bc.spec||{};
  document.getElementById('spec').innerHTML=
    kv('Kernel',s.kernel)+kv('Initrd',s.initrd)+kv('Cmdline',s.cmdline)
    +kv('iPXE Script',s.ipxeScript?'(set)':'—')+kv('Disk Image',s.diskImage)
    +kv('Install Script',s.installScript?'(set)':'—');
  document.getElementById('labels').innerHTML=Object.entries(bc.metadata?.labels||{}).map(([k,v])=>kv(k,v)).join('')||'<div class="muted">None</div>';

  // Find BMHs referencing this boot config
  const bmhs=await apiGet(API+'/api/v1/baremetalhosts');
  const refs=(bmhs?.items||[]).filter(b=>b.spec?.bootConfigRef===bcName);
  const tb=document.getElementById('refs');
  tb.innerHTML='';
  if(refs.length===0){tb.innerHTML='<tr><td colspan="4" class="muted" style="text-align:center;padding:8px">No hosts reference this config</td></tr>';return;}
  refs.forEach(b=>{
    const ns=b.metadata.namespace||'default';
    tb.innerHTML+='<tr><td><a href="bmh/'+encodeURIComponent(ns)+'/'+encodeURIComponent(b.metadata.name)+'">'+escapeHtml(b.metadata.name)+'</a></td>'
      +'<td>'+escapeHtml(ns)+'</td><td>'+statusBadge(b.spec?.state||'—')+'</td><td>'+statusBadge(b.spec?.online?'ON':'OFF')+'</td></tr>';
  });
  initSort('refs');reapplySort('refs');
}
function kv(k,v){ return '<div class="k">'+escapeHtml(k)+'</div><div class="v">'+escapeHtml(String(v||'—'))+'</div>'; }
load(); setInterval(load,15000);
`
	write(w, c.pageWithJS("BootConfig: "+name, "BootConfigs", body, js))
}
