package console

import "fmt"

type navItem struct {
	label string
	href  string
}

// Nav links use relative paths so they work both when accessed directly
// at :8082/ui/ and through stormd proxy at :9080/ui/proxy/mkube/.
var navItems = []navItem{
	{"Dashboard", "./"},
	{"Nodes", "nodes"},
	{"Pods", "pods"},
	{"Deployments", "deployments"},
	{"Networks", "networks"},
	{"BMH", "bmh"},
	{"BootConfigs", "bootconfigs"},
	{"Registries", "registries"},
	{"Storage", "storage"},
	{"Jobs", "jobs"},
	{"CloudID", "cloudid"},
	{"Logs", "logs"},
}

func navHTML(active string) string {
	links := ""
	for _, item := range navItems {
		cls := ""
		if item.label == active {
			cls = ` class="active"`
		}
		links += fmt.Sprintf(`<a href="%s"%s>%s</a>`, item.href, cls, item.label)
	}
	return fmt.Sprintf(`<nav><span class="brand">mkube</span><div class="links">%s</div></nav>`, links)
}

func page(title, active, body string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html><head>
<title>mkube — %s</title>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<script>
(function(){
  var p=location.pathname;
  var m=p.match(/^(\/ui\/proxy\/[^\/]+\/)/);
  var b=document.createElement('base');
  b.href=m?m[1]:'/ui/';
  document.head.prepend(b);
})();
</script>
<link rel="stylesheet" href="static/style.css">
</head><body>
%s
<div class="container">%s</div>
</body></html>`, title, navHTML(active), body)
}

func (c *Console) pageWithJS(title, active, body, extraJS string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html><head>
<title>mkube — %s</title>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<script>
(function(){
  var p=location.pathname;
  var m=p.match(/^(\/ui\/proxy\/[^\/]+\/)/);
  var b=document.createElement('base');
  b.href=m?m[1]:'/ui/';
  document.head.prepend(b);
})();
</script>
<link rel="stylesheet" href="static/style.css">
<script src="static/app.js"></script>
</head><body>
%s
<div class="container">%s</div>
<script>
var API_CFG=%q;
var CLOUDID=%q;
var API=(function(){try{return location.origin===new URL(API_CFG).origin?'':API_CFG}catch(e){return API_CFG}})();
%s
</script>
</body></html>`, title, navHTML(active), body, c.apiBase, c.cloudidURL, extraJS)
}
