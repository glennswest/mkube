package console

// css returns the Dracula-themed CSS for all console pages.
func css() string {
	return `
* { margin:0; padding:0; box-sizing:border-box; }
body { background:#0f0f1a; color:#e0e0e0; font-family:'SF Mono','Cascadia Code','Fira Code',monospace; font-size:13px; }
a { color:#8be9fd; text-decoration:none; }
a:hover { text-decoration:underline; }

/* Nav */
nav { display:flex; align-items:center; background:#16192e; border-bottom:1px solid #2a2d45; padding:0 16px; height:44px; }
nav .brand { font-weight:700; font-size:15px; color:#e94560; margin-right:24px; }
nav .links { display:flex; gap:2px; }
nav .links a { padding:8px 12px; border-radius:6px 6px 0 0; color:#8899aa; font-size:12px; font-weight:600; }
nav .links a:hover { background:#1e2140; color:#e0e0e0; text-decoration:none; }
nav .links a.active { background:#0f0f1a; color:#8be9fd; border-bottom:2px solid #8be9fd; }

/* Layout */
.container { padding:16px 20px; max-width:1600px; margin:0 auto; }
h1 { font-size:18px; font-weight:700; color:#e0e0e0; margin-bottom:12px; }
h2 { font-size:15px; font-weight:600; color:#ccc; margin:16px 0 8px; }

/* Cards */
.card { background:#16192e; border:1px solid #2a2d45; border-radius:8px; padding:16px; margin-bottom:16px; }
.card h3 { font-size:14px; font-weight:600; margin-bottom:10px; color:#8be9fd; }
.card-grid { display:grid; grid-template-columns:repeat(auto-fill,minmax(300px,1fr)); gap:12px; }

/* Stats grid */
.stats { display:grid; grid-template-columns:repeat(auto-fill,minmax(140px,1fr)); gap:10px; margin-bottom:16px; }
.stat { background:#16192e; border:1px solid #2a2d45; border-radius:8px; padding:12px; text-align:center; }
.stat .val { font-size:22px; font-weight:700; color:#50fa7b; }
.stat .lbl { font-size:11px; color:#888; margin-top:4px; }

/* Tables */
table { width:100%; border-collapse:collapse; font-size:12px; }
th { text-align:left; padding:8px 10px; background:#16192e; color:#8899aa; font-weight:600; border-bottom:1px solid #2a2d45; position:sticky; top:0; }
td { padding:7px 10px; border-bottom:1px solid #1e2140; }
tr:hover td { background:#1a1d32; }

/* Badges */
.badge { display:inline-block; padding:2px 8px; border-radius:10px; font-size:11px; font-weight:600; }
.badge-green { background:rgba(80,250,123,0.15); color:#50fa7b; }
.badge-red { background:rgba(233,69,96,0.15); color:#e94560; }
.badge-yellow { background:rgba(241,250,140,0.15); color:#f1fa8c; }
.badge-cyan { background:rgba(139,233,253,0.15); color:#8be9fd; }
.badge-gray { background:rgba(136,153,170,0.15); color:#8899aa; }
.badge-magenta { background:rgba(255,121,198,0.15); color:#ff79c6; }

/* Buttons */
.btn { display:inline-block; padding:4px 12px; border-radius:4px; font-size:11px; font-weight:600; border:1px solid #2a2d45; background:#1a1d32; color:#e0e0e0; cursor:pointer; }
.btn:hover { background:#2a2d45; text-decoration:none; }
.btn-danger { border-color:#e94560; color:#e94560; }
.btn-danger:hover { background:rgba(233,69,96,0.2); }
.btn-primary { border-color:#8be9fd; color:#8be9fd; }
.btn-primary:hover { background:rgba(139,233,253,0.2); }
.btn-success { border-color:#50fa7b; color:#50fa7b; }
.btn-success:hover { background:rgba(80,250,123,0.2); }

/* Tabs */
.tabs { display:flex; gap:2px; margin-bottom:12px; border-bottom:1px solid #2a2d45; }
.tab { padding:8px 16px; cursor:pointer; color:#8899aa; font-size:12px; font-weight:600; border-bottom:2px solid transparent; }
.tab:hover { color:#e0e0e0; }
.tab.active { color:#8be9fd; border-bottom-color:#8be9fd; }
.tab-content { display:none; }
.tab-content.active { display:block; }

/* Terminal / logs */
.terminal { background:#0a0a14; border:1px solid #2a2d45; border-radius:6px; padding:12px; font-size:12px; line-height:1.6; white-space:pre-wrap; word-break:break-all; max-height:500px; overflow-y:auto; }

/* Toolbar */
.toolbar { display:flex; gap:8px; align-items:center; margin-bottom:12px; flex-wrap:wrap; }
.toolbar select, .toolbar input[type=text] { background:#1a1d32; border:1px solid #2a2d45; color:#e0e0e0; padding:5px 10px; border-radius:4px; font-size:12px; font-family:inherit; }
.toolbar select:focus, .toolbar input:focus { outline:none; border-color:#8be9fd; }

/* Usage bar */
.usage-bar { width:100%; height:8px; background:#1a1d32; border-radius:4px; overflow:hidden; }
.usage-bar .fill { height:100%; border-radius:4px; }

/* KV list */
.kv { display:grid; grid-template-columns:180px 1fr; gap:4px 12px; font-size:12px; }
.kv .k { color:#8899aa; font-weight:600; }
.kv .v { color:#e0e0e0; word-break:break-all; }

/* Modal */
.modal-overlay { display:none; position:fixed; top:0;left:0;right:0;bottom:0; background:rgba(0,0,0,0.6); z-index:1000; align-items:center; justify-content:center; }
.modal-overlay.show { display:flex; }
.modal { background:#16192e; border:1px solid #2a2d45; border-radius:8px; padding:20px; max-width:800px; width:90%; max-height:80vh; overflow-y:auto; }
.modal h3 { margin-bottom:12px; color:#8be9fd; }
.modal textarea { width:100%; min-height:300px; background:#0a0a14; border:1px solid #2a2d45; color:#e0e0e0; font-family:inherit; font-size:12px; padding:10px; border-radius:4px; resize:vertical; }
.modal .actions { display:flex; gap:8px; margin-top:12px; justify-content:flex-end; }

/* Misc */
.muted { color:#666; }
.mono { font-family:inherit; }
.mt { margin-top:12px; }
.mb { margin-bottom:12px; }
.flex { display:flex; gap:8px; align-items:center; }
`
}

// js returns shared JavaScript utility functions.
func js() string {
	return `
function escapeHtml(t){
  return t?t.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;'):'';
}

function ansiToHtml(text){
  if(!text) return '';
  let html='';
  const parts=text.split(/(\x1b\[[0-9;]*m)/);
  let styles=[];
  const colorMap={'30':'#555','31':'#e94560','32':'#50fa7b','33':'#f1fa8c','34':'#8be9fd','35':'#ff79c6','36':'#8be9fd','37':'#e0e0e0',
    '90':'#888','91':'#ff6b81','92':'#69ff94','93':'#ffffa5','94':'#a4d4ff','95':'#ff92d0','96':'#a4ffff','97':'#ffffff'};
  const bgMap={'40':'#555','41':'#e94560','42':'#50fa7b','43':'#f1fa8c','44':'#8be9fd','45':'#ff79c6','46':'#8be9fd','47':'#e0e0e0'};
  for(const p of parts){
    if(p.startsWith('\x1b[')){
      const codes=p.slice(2,-1).split(';');
      for(const c of codes){
        if(c==='0'||c==='') styles=[];
        else if(c==='1') styles.push('font-weight:bold');
        else if(c==='3') styles.push('font-style:italic');
        else if(c==='4') styles.push('text-decoration:underline');
        else if(colorMap[c]) styles.push('color:'+colorMap[c]);
        else if(bgMap[c]) styles.push('background:'+bgMap[c]);
      }
    } else {
      if(styles.length>0) html+='<span style="'+styles.join(';')+'">'+escapeHtml(p)+'</span>';
      else html+=escapeHtml(p);
    }
  }
  return html;
}

function statusBadge(s){
  if(!s) return '<span class="badge badge-gray">—</span>';
  const l=s.toLowerCase();
  if(['running','ready','healthy','pass','bound','completed','online','true'].includes(l))
    return '<span class="badge badge-green">'+escapeHtml(s)+'</span>';
  if(['failed','error','crash','false','offline'].includes(l))
    return '<span class="badge badge-red">'+escapeHtml(s)+'</span>';
  if(['stopped','pending','warning','terminating','unknown','provisioning','cloning'].includes(l))
    return '<span class="badge badge-yellow">'+escapeHtml(s)+'</span>';
  if(['succeeded','poweroff'].includes(l))
    return '<span class="badge badge-cyan">'+escapeHtml(s)+'</span>';
  return '<span class="badge badge-gray">'+escapeHtml(s)+'</span>';
}

function timeSince(dateStr){
  if(!dateStr) return '—';
  const d=new Date(dateStr);
  if(isNaN(d)) return dateStr;
  const s=Math.floor((Date.now()-d)/1000);
  if(s<60) return s+'s';
  if(s<3600) return Math.floor(s/60)+'m';
  if(s<86400) return Math.floor(s/3600)+'h';
  return Math.floor(s/86400)+'d';
}

function shortImage(img){
  if(!img) return '—';
  const p=img.split('/');
  return p[p.length-1];
}

function apiGet(url){
  return fetch(url).then(r=>{if(!r.ok)throw new Error(r.status);return r.json()}).catch(e=>{console.error('fetch',url,e);return null});
}

function apiPost(url,body){
  return fetch(url,{method:'POST',headers:{'Content-Type':'application/json'},body:body?JSON.stringify(body):undefined}).then(r=>r.json()).catch(e=>null);
}

function apiDelete(url){
  return fetch(url,{method:'DELETE'}).then(r=>r.ok).catch(e=>false);
}

function apiPatch(url,body){
  return fetch(url,{method:'PATCH',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)}).then(r=>r.json()).catch(e=>null);
}

function apiPut(url,body){
  return fetch(url,{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)}).then(r=>r.json()).catch(e=>null);
}
`
}
