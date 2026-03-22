package console

import "net/http"

func (c *Console) handleDeployments(w http.ResponseWriter, r *http.Request) {
	body := `<h1>Deployments</h1>
<table><thead><tr><th>Name</th><th>Namespace</th><th>Replicas</th><th>Ready</th><th>Image</th><th>Age</th><th></th></tr></thead>
<tbody id="tbl"></tbody></table>`

	js := `
async function load(){
  const data=await apiGet(API+'/api/v1/deployments');
  const tb=document.getElementById('tbl');
  tb.innerHTML='';
  (data?.items||[]).forEach(d=>{
    const ns=d.metadata.namespace||'default';
    const rep=d.spec?.replicas||1;
    const ready=d.status?.readyReplicas||0;
    const img=d.spec?.template?.spec?.containers?.[0]?.image||'—';
    const cls=ready>=rep?'badge-green':ready>0?'badge-yellow':'badge-red';
    tb.innerHTML+='<tr><td><a href="/ui/deployments/'+ns+'/'+escapeHtml(d.metadata.name)+'">'+escapeHtml(d.metadata.name)+'</a></td>'
      +'<td>'+escapeHtml(ns)+'</td>'
      +'<td>'+rep+'</td>'
      +'<td><span class="badge '+cls+'">'+ready+'/'+rep+'</span></td>'
      +'<td>'+shortImage(img)+'</td>'
      +'<td>'+timeSince(d.metadata?.creationTimestamp)+'</td>'
      +'<td><button class="btn btn-danger" onclick="del(\''+ns+'\',\''+escapeHtml(d.metadata.name)+'\')">Delete</button></td></tr>';
  });
}
async function del(ns,name){ if(confirm('Delete deployment '+name+'?')){ await apiDelete(API+'/api/v1/namespaces/'+ns+'/deployments/'+name); load(); } }
load(); setInterval(load,15000);
`
	write(w, c.pageWithJS("Deployments", "Deployments", body, js))
}

func (c *Console) handleDeploymentDetail(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	name := r.PathValue("name")

	body := `<h1>Deployment: ` + name + `</h1>
<div class="card"><h3>Info</h3><div class="kv" id="info"></div></div>
<div class="card"><h3>Owned Pods</h3><table><thead><tr><th>Name</th><th>Status</th><th>IP</th><th>Restarts</th><th>Age</th></tr></thead><tbody id="pods"></tbody></table></div>`

	js := `
const dNs=` + jsStr(ns) + `,dName=` + jsStr(name) + `;
async function load(){
  const dep=await apiGet(API+'/api/v1/namespaces/'+dNs+'/deployments/'+dName);
  if(!dep) return;
  const rep=dep.spec?.replicas||1;
  const ready=dep.status?.readyReplicas||0;
  const img=dep.spec?.template?.spec?.containers?.[0]?.image||'—';
  document.getElementById('info').innerHTML=
    kv('Replicas',rep)+kv('Ready',ready)+kv('Image',shortImage(img))+kv('Age',timeSince(dep.metadata?.creationTimestamp));

  const allPods=await apiGet(API+'/api/v1/namespaces/'+dNs+'/pods');
  const owned=(allPods?.items||[]).filter(p=>p.metadata.name.startsWith(dName));
  const tb=document.getElementById('pods');
  tb.innerHTML='';
  owned.forEach(p=>{
    tb.innerHTML+='<tr><td><a href="/ui/pods/'+dNs+'/'+escapeHtml(p.metadata.name)+'">'+escapeHtml(p.metadata.name)+'</a></td>'
      +'<td>'+statusBadge(p.status?.phase)+'</td>'
      +'<td>'+escapeHtml(p.status?.podIP||'—')+'</td>'
      +'<td>'+(p.status?.containerStatuses?.[0]?.restartCount||0)+'</td>'
      +'<td>'+timeSince(p.status?.startTime)+'</td></tr>';
  });
}
function kv(k,v){ return '<div class="k">'+escapeHtml(k)+'</div><div class="v">'+escapeHtml(String(v||'—'))+'</div>'; }
load(); setInterval(load,15000);
`
	write(w, c.pageWithJS("Deployment: "+name, "Deployments", body, js))
}
