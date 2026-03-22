package console

import "net/http"

func (c *Console) handleRegistries(w http.ResponseWriter, r *http.Request) {
	body := `<h1>Registries</h1>
<table><thead><tr><th>Name</th><th>URL</th><th>Mirrors</th><th>Insecure</th><th>Age</th><th></th></tr></thead>
<tbody id="tbl"></tbody></table>`

	js := `
async function load(){
  const data=await apiGet(API+'/api/v1/registries');
  const tb=document.getElementById('tbl');
  tb.innerHTML='';
  (data?.items||[]).forEach(r=>{
    const s=r.spec||{};
    const mirrors=(s.mirrors||[]).length;
    tb.innerHTML+='<tr><td><a href="/ui/registries/'+escapeHtml(r.metadata.name)+'">'+escapeHtml(r.metadata.name)+'</a></td>'
      +'<td>'+escapeHtml(s.url||s.endpoint||'—')+'</td>'
      +'<td>'+mirrors+'</td>'
      +'<td>'+(s.insecure?'<span class="badge badge-yellow">yes</span>':'<span class="badge badge-green">no</span>')+'</td>'
      +'<td>'+timeSince(r.metadata?.creationTimestamp)+'</td>'
      +'<td><button class="btn btn-danger" onclick="del(\''+escapeHtml(r.metadata.name)+'\')">Delete</button></td></tr>';
  });
}
async function del(name){ if(confirm('Delete registry '+name+'?')){ await apiDelete(API+'/api/v1/registries/'+name); load(); }}
load(); setInterval(load,15000);
`
	write(w, c.pageWithJS("Registries", "Registries", body, js))
}

func (c *Console) handleRegistryDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	body := `<h1>Registry: ` + name + `</h1>
<div class="card"><h3>Spec</h3><div class="kv" id="spec"></div></div>
<div class="card"><h3>Labels</h3><div class="kv" id="labels"></div></div>
<div class="card"><h3>Image Catalog</h3>
<table><thead><tr><th>Repository</th><th>Tags</th></tr></thead><tbody id="images"></tbody></table></div>`

	js := `
const regName=` + jsStr(name) + `;
async function load(){
  const reg=await apiGet(API+'/api/v1/registries/'+regName);
  if(!reg) return;
  const s=reg.spec||{};
  document.getElementById('spec').innerHTML=
    kv('URL',s.url||s.endpoint)+kv('Insecure',String(!!s.insecure))+kv('Interval',s.interval)
    +kv('Push',String(!!s.push))+kv('Mirrors',JSON.stringify(s.mirrors||[]));
  document.getElementById('labels').innerHTML=Object.entries(reg.metadata?.labels||{}).map(([k,v])=>kv(k,v)).join('');

  // Try to load image catalog
  const cfg=await apiGet(API+'/api/v1/registries/'+regName+'/config').catch(()=>null);
  const ib=document.getElementById('images');
  if(cfg?.catalog){
    ib.innerHTML=(cfg.catalog||[]).map(i=>'<tr><td>'+escapeHtml(i.name||'—')+'</td><td>'+escapeHtml((i.tags||[]).join(', '))+'</td></tr>').join('');
  } else {
    ib.innerHTML='<tr><td colspan="2" class="muted">No catalog available</td></tr>';
  }
}
function kv(k,v){ return '<div class="k">'+escapeHtml(k)+'</div><div class="v">'+escapeHtml(String(v||'—'))+'</div>'; }
load(); setInterval(load,15000);
`
	write(w, c.pageWithJS("Registry: "+name, "Registries", body, js))
}
