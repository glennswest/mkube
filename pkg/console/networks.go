package console

import "net/http"

func (c *Console) handleNetworks(w http.ResponseWriter, r *http.Request) {
	body := `<h1>Networks</h1>
<table><thead><tr><th>Name</th><th>Type</th><th>CIDR</th><th>Gateway</th><th>DNS</th><th>Pods</th><th>DNS Health</th><th>DHCP</th></tr></thead>
<tbody id="tbl"><tr><td colspan="8" class="loading">Loading...</td></tr></tbody></table>`

	js := `
async function load(){
  const data=await apiGet(API+'/api/v1/networks');
  const tb=document.getElementById('tbl');
  const items=data?.items||[];
  if(items.length===0){tb.innerHTML='<tr><td colspan="8" class="muted" style="text-align:center;padding:16px">No networks</td></tr>';return;}
  const rows=[];
  items.forEach(n=>{
    const s=n.spec||{};
    const st=n.status||{};
    const dhcp=s.dhcp?.enabled?'Yes':'—';
    rows.push('<tr><td><a href="networks/'+encodeURIComponent(n.metadata.name)+'">'+escapeHtml(n.metadata.name)+'</a></td>'
      +'<td>'+escapeHtml(s.type||'—')+'</td>'
      +'<td>'+escapeHtml(s.cidr||'—')+'</td>'
      +'<td>'+escapeHtml(s.gateway||'—')+'</td>'
      +'<td>'+escapeHtml(s.dns||'—')+'</td>'
      +'<td>'+(st.podCount||0)+'</td>'
      +'<td>'+statusBadge(st.dnsLiveness||'—')+'</td>'
      +'<td>'+escapeHtml(dhcp)+'</td></tr>');
  });
  tb.innerHTML=rows.join('');
  initSort('tbl');reapplySort('tbl');
}
load(); _uiInterval(load,15000);
`
	write(w, c.pageWithJS("Networks", "Networks", body, js))
}

func (c *Console) handleNetworkDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	body := `<h1>Network: ` + name + `</h1>
<div class="card"><h3>Spec</h3><div class="kv" id="info"><div class="loading">Loading...</div></div>
<button class="btn btn-primary mt" onclick="smoketest()">Run Smoketest</button>
<div id="smoketest-result" class="mt"></div></div>
<div class="tabs">
  <div class="tab active" onclick="switchTab('dns')">DNS Records</div>
  <div class="tab" onclick="switchTab('pools')">DHCP Pools</div>
  <div class="tab" onclick="switchTab('reservations')">DHCP Reservations</div>
  <div class="tab" onclick="switchTab('leases')">DHCP Leases</div>
  <div class="tab" onclick="switchTab('forwarders')">DNS Forwarders</div>
</div>
<div id="dns" class="tab-content active"><table><thead><tr><th>Name</th><th>Type</th><th>Data</th><th>TTL</th></tr></thead><tbody id="dns-tbl"></tbody></table></div>
<div id="pools" class="tab-content"><table><thead><tr><th>Name</th><th>Range</th><th>Subnet</th><th>Gateway</th><th>Lease</th></tr></thead><tbody id="pools-tbl"></tbody></table></div>
<div id="reservations" class="tab-content"><table><thead><tr><th>Name</th><th>MAC</th><th>IP</th><th>Hostname</th></tr></thead><tbody id="res-tbl"></tbody></table></div>
<div id="leases" class="tab-content"><table><thead><tr><th>MAC</th><th>IP</th><th>Hostname</th><th>Expires</th></tr></thead><tbody id="leases-tbl"></tbody></table></div>
<div id="forwarders" class="tab-content"><table><thead><tr><th>Zone</th><th>Servers</th></tr></thead><tbody id="fwd-tbl"></tbody></table></div>`

	js := `
const netName=` + jsStr(name) + `;

function switchTab(id){
  document.querySelectorAll('.tab-content').forEach(e=>e.classList.remove('active'));
  document.querySelectorAll('.tab').forEach(e=>e.classList.remove('active'));
  document.getElementById(id).classList.add('active');
  event.target.classList.add('active');
}

async function load(){
  const net=await apiGet(API+'/api/v1/networks/'+netName);
  if(!net){document.getElementById('info').innerHTML='<div class="muted">Network not found</div>';return;}
  const s=net.spec||{};
  document.getElementById('info').innerHTML=
    kv('Type',s.type)+kv('CIDR',s.cidr)+kv('Gateway',s.gateway)+kv('DNS',s.dns)
    +kv('Bridge',s.bridge)+kv('VLAN',s.vlan)+kv('Router',s.router)
    +kv('Managed',String(s.managed!==false))+kv('ExternalDNS',String(!!s.externalDNS));

  // DNS records
  const dr=await apiGet(API+'/api/v1/namespaces/'+netName+'/dnsrecords');
  fillTable('dns-tbl',(dr?.items||[]),r=>'<td>'+escapeHtml(r.spec?.name||r.metadata?.name||'—')+'</td><td>'+escapeHtml(r.spec?.type||'—')+'</td><td>'+escapeHtml(r.spec?.data||'—')+'</td><td>'+(r.spec?.ttl||300)+'</td>');

  // DHCP pools
  const dp=await apiGet(API+'/api/v1/namespaces/'+netName+'/dhcppools');
  fillTable('pools-tbl',(dp?.items||[]),p=>'<td>'+escapeHtml(p.metadata?.name||'—')+'</td><td>'+escapeHtml((p.spec?.range_start||'')+' – '+(p.spec?.range_end||''))+'</td><td>'+escapeHtml(p.spec?.subnet||'—')+'</td><td>'+escapeHtml(p.spec?.gateway||'—')+'</td><td>'+(p.spec?.lease_time_secs||3600)+'s</td>');

  // Reservations
  const dhr=await apiGet(API+'/api/v1/namespaces/'+netName+'/dhcpreservations');
  fillTable('res-tbl',(dhr?.items||[]),r=>'<td>'+escapeHtml(r.metadata?.name||'—')+'</td><td>'+escapeHtml(r.spec?.mac||'—')+'</td><td>'+escapeHtml(r.spec?.ip||'—')+'</td><td>'+escapeHtml(r.spec?.hostname||'—')+'</td>');

  // Leases
  const dl=await apiGet(API+'/api/v1/namespaces/'+netName+'/dhcpleases');
  fillTable('leases-tbl',(dl?.items||[]),l=>'<td>'+escapeHtml(l.spec?.mac||l.mac||'—')+'</td><td>'+escapeHtml(l.spec?.ip||l.ip||'—')+'</td><td>'+escapeHtml(l.spec?.hostname||l.hostname||'—')+'</td><td>'+escapeHtml(l.spec?.expires||l.expires||'—')+'</td>');

  // Forwarders
  const df=await apiGet(API+'/api/v1/namespaces/'+netName+'/dnsforwarders');
  fillTable('fwd-tbl',(df?.items||[]),f=>'<td>'+escapeHtml(f.spec?.zone||f.metadata?.name||'—')+'</td><td>'+escapeHtml(JSON.stringify(f.spec?.servers||[]))+'</td>');
}

function fillTable(id,items,rowFn){
  const tb=document.getElementById(id);
  tb.innerHTML=items.map(i=>'<tr>'+rowFn(i)+'</tr>').join('');
  if(items.length===0) tb.innerHTML='<tr><td colspan="10" class="muted" style="text-align:center;padding:8px">None</td></tr>';
  initSort(id);reapplySort(id);
}

function kv(k,v){ return '<div class="k">'+escapeHtml(k)+'</div><div class="v">'+escapeHtml(String(v||'—'))+'</div>'; }

async function smoketest(){
  document.getElementById('smoketest-result').innerHTML='<span class="badge badge-yellow">Running...</span>';
  const r=await apiPost(API+'/api/v1/networks/'+netName+'/smoketest');
  if(r) document.getElementById('smoketest-result').innerHTML=statusBadge(r.result||'unknown')+' <span class="muted">'+escapeHtml(r.message||'')+'</span>';
  else document.getElementById('smoketest-result').innerHTML=statusBadge('error');
}

load(); _uiInterval(load,15000);
`
	write(w, c.pageWithJS("Network: "+name, "Networks", body, js))
}
