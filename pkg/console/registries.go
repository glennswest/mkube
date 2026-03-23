package console

import "net/http"

func (c *Console) handleRegistries(w http.ResponseWriter, r *http.Request) {
	body := `<h1>Registries</h1>
<div id="reg-cards" class="loading" style="margin-bottom:12px">Loading...</div>
<table><thead><tr><th>Name</th><th>URL</th><th>Images</th><th>Pods</th><th>Status</th><th>Store Path</th><th>Age</th><th></th></tr></thead>
<tbody id="tbl"><tr><td colspan="8" class="loading">Loading...</td></tr></tbody></table>`

	js := `
async function load(){
  const [data,pools]=await Promise.all([
    apiGet(API+'/api/v1/registries'),
    apiGet(API+'/api/v1/storagepools')
  ]);
  const items=data?.items||[];
  const poolItems=pools?.items||[];

  // Build pool lookup for storage info
  var poolMap={};
  poolItems.forEach(function(p){ poolMap[p.spec?.mountPoint||p.metadata.name]=p; });

  // Registry summary cards
  var rc=document.getElementById('reg-cards');
  if(items.length===0){
    rc.innerHTML='<div class="muted" style="padding:8px">No registries</div>';
  } else {
    rc.innerHTML='<div style="display:flex;gap:12px;flex-wrap:wrap">'+items.map(function(r){
      var s=r.spec||{};
      var st=r.status||{};
      var url=s.hostname?(s.hostname+':'+((s.listenAddr||':5000').replace(':',''))):(s.staticIP?(s.staticIP+':5000'):'—');
      var alive=st.alive;
      var badge=alive?'<span class="badge badge-green">Online</span>':'<span class="badge badge-red">Offline</span>';
      var imgs=st.imageCount||0;
      // Find storage pool from storePath
      var poolInfo='';
      if(s.storePath){
        var mount=s.storePath.split('/')[1]||'';
        var pool=poolMap[mount];
        if(pool&&pool.status){
          var pst=pool.status;
          poolInfo='<div style="font-size:11px;color:var(--comment);margin-top:4px">Pool: '+escapeHtml(pool.metadata.name)+' ('+fmtBytes(pst.availBytes||0)+' avail)</div>';
        }
      }
      return '<div class="card" style="min-width:260px;flex:1;max-width:380px">'+
        '<h4>'+escapeHtml(r.metadata.name)+' '+badge+'</h4>'+
        '<div style="font-size:12px;color:var(--comment)">'+escapeHtml(url)+'</div>'+
        '<div style="font-size:13px;margin:8px 0"><b>'+imgs+'</b> images &middot; <b>'+st.podCount+'</b> pod(s)</div>'+
        poolInfo+
        '</div>';
    }).join('')+'</div>';
  }

  // Table
  const tb=document.getElementById('tbl');
  if(items.length===0){tb.innerHTML='<tr><td colspan="8" class="muted" style="text-align:center;padding:16px">No registries</td></tr>';return;}
  const rows=[];
  items.forEach(r=>{
    const s=r.spec||{};
    const st=r.status||{};
    const url=s.hostname?(s.hostname+':'+(s.listenAddr||':5000').replace(':','')):(s.staticIP?(s.staticIP+':5000'):'—');
    const alive=st.alive;
    const badge=alive?'<span class="badge badge-green">Online</span>':'<span class="badge badge-red">Offline</span>';
    rows.push('<tr><td><a href="registries/'+encodeURIComponent(r.metadata.name)+'">'+escapeHtml(r.metadata.name)+'</a></td>'
      +'<td>'+escapeHtml(url)+'</td>'
      +'<td>'+(st.imageCount||0)+'</td>'
      +'<td>'+(st.podCount||0)+'</td>'
      +'<td>'+badge+'</td>'
      +'<td>'+escapeHtml(s.storePath||'—')+'</td>'
      +'<td>'+timeSince(r.metadata?.creationTimestamp)+'</td>'
      +'<td><button class="btn btn-danger" onclick="del(\''+escapeHtml(r.metadata.name)+'\')">Delete</button></td></tr>');
  });
  tb.innerHTML=rows.join('');
  initSort('tbl');reapplySort('tbl');
}
async function del(name){ if(confirm('Delete registry '+name+'?')){ await apiDelete(API+'/api/v1/registries/'+name); load(); }}
load(); _uiInterval(load,15000);
`
	write(w, c.pageWithJS("Registries", "Registries", body, js))
}

func (c *Console) handleRegistryDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	body := `<h1>Registry: ` + name + `</h1>
<div class="card"><h3>Spec</h3><div class="kv" id="spec"><div class="loading">Loading...</div></div></div>
<div class="card"><h3>Labels</h3><div class="kv" id="labels"></div></div>
<div class="card"><h3>Image Catalog</h3>
<table><thead><tr><th>Repository</th><th>Tags</th></tr></thead><tbody id="images"></tbody></table></div>`

	js := `
const regName=` + jsStr(name) + `;
async function load(){
  const reg=await apiGet(API+'/api/v1/registries/'+regName);
  if(!reg){document.getElementById('spec').innerHTML='<div class="muted">Not found</div>';return;}
  const s=reg.spec||{};
  document.getElementById('spec').innerHTML=
    kv('URL',s.url||s.endpoint)+kv('Insecure',String(!!s.insecure))+kv('Interval',s.interval)
    +kv('Push',String(!!s.push))+kv('Mirrors',JSON.stringify(s.mirrors||[]));
  document.getElementById('labels').innerHTML=Object.entries(reg.metadata?.labels||{}).map(([k,v])=>kv(k,v)).join('')||'<div class="muted">None</div>';

  // Image catalog
  const cfg=await apiGet(API+'/api/v1/registries/'+regName+'/config');
  const ib=document.getElementById('images');
  if(cfg?.catalog){
    ib.innerHTML=(cfg.catalog||[]).map(i=>'<tr><td>'+escapeHtml(i.name||'—')+'</td><td>'+escapeHtml((i.tags||[]).join(', '))+'</td></tr>').join('');
  } else {
    ib.innerHTML='<tr><td colspan="2" class="muted" style="text-align:center;padding:8px">No catalog available</td></tr>';
  }
  initSort('images');reapplySort('images');
}
function kv(k,v){ return '<div class="k">'+escapeHtml(k)+'</div><div class="v">'+escapeHtml(String(v||'—'))+'</div>'; }
load(); _uiInterval(load,15000);
`
	write(w, c.pageWithJS("Registry: "+name, "Registries", body, js))
}
