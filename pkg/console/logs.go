package console

import "net/http"

func (c *Console) handleLogs(w http.ResponseWriter, r *http.Request) {
	body := `<h1>Logs</h1>
<div class="toolbar">
  <select id="node-filter"><option value="">All Nodes</option></select>
  <select id="pod-filter" onchange="loadLogs()"><option value="">Select a pod...</option></select>
  <input type="text" id="log-search" placeholder="Search..." oninput="filterLogs()">
  <label style="color:#888;font-size:12px"><input type="checkbox" id="follow" checked> Follow</label>
  <button class="btn btn-primary" onclick="loadLogs()">Refresh</button>
</div>
<div class="terminal" id="logs" style="max-height:600px"></div>`

	js := `
let allLogLines='';

async function init(){
  const [nodes,pods]=await Promise.all([
    apiGet(API+'/api/v1/nodes'),
    apiGet(API+'/api/v1/pods'),
  ]);

  const nf=document.getElementById('node-filter');
  (nodes?.items||[]).forEach(n=>{
    const o=document.createElement('option');o.value=n.metadata.name;o.text=n.metadata.name;nf.add(o);
  });
  nf.onchange=function(){
    const pf=document.getElementById('pod-filter');
    pf.innerHTML='<option value="">Select a pod...</option>';
    const nodeVal=nf.value;
    (pods?.items||[]).forEach(p=>{
      const pNode=p.metadata?.annotations?.['vkube.io/node']||'';
      if(nodeVal && pNode!==nodeVal) return;
      const ns=p.metadata.namespace||'default';
      const o=document.createElement('option');
      o.value=ns+'/'+p.metadata.name;
      o.text=ns+'/'+p.metadata.name;
      pf.add(o);
    });
  };
  nf.onchange();
}

async function loadLogs(){
  const sel=document.getElementById('pod-filter').value;
  if(!sel){ document.getElementById('logs').innerHTML=''; return; }
  const [ns,name]=sel.split('/');
  const logs=await fetch(API+'/api/v1/namespaces/'+ns+'/pods/'+name+'/log').then(r=>r.text()).catch(()=>'');
  allLogLines=logs;
  filterLogs();
}

function filterLogs(){
  const search=document.getElementById('log-search').value.toLowerCase();
  const el=document.getElementById('logs');
  if(!allLogLines){ el.innerHTML=''; return; }
  let lines=allLogLines;
  if(search){
    lines=allLogLines.split('\n').filter(l=>l.toLowerCase().includes(search)).join('\n');
  }
  el.innerHTML=ansiToHtml(lines);
  if(document.getElementById('follow').checked) el.scrollTop=el.scrollHeight;
}

init();
`
	write(w, c.pageWithJS("Logs", "Logs", body, js))
}
