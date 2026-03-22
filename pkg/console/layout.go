package console

import "fmt"

type navItem struct {
	label string
	href  string
}

var navItems = []navItem{
	{"Dashboard", "/ui/"},
	{"Nodes", "/ui/nodes"},
	{"Pods", "/ui/pods"},
	{"Deployments", "/ui/deployments"},
	{"Networks", "/ui/networks"},
	{"BMH", "/ui/bmh"},
	{"BootConfigs", "/ui/bootconfigs"},
	{"Registries", "/ui/registries"},
	{"Storage", "/ui/storage"},
	{"Jobs", "/ui/jobs"},
	{"CloudID", "/ui/cloudid"},
	{"Logs", "/ui/logs"},
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
<style>%s</style>
</head><body>
%s
<div class="container">%s</div>
</body></html>`, title, css(), navHTML(active), body)
}

func (c *Console) pageWithJS(title, active, body, extraJS string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html><head>
<title>mkube — %s</title>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>%s</style>
</head><body>
%s
<div class="container">%s</div>
<script>
const API = %q;
const CLOUDID = %q;
%s
%s
</script>
</body></html>`, title, css(), navHTML(active), body, c.apiBase, c.cloudidURL, js(), extraJS)
}
