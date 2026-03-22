// Package console provides a built-in web dashboard for mkube,
// integrated as a stormd UI extension via [process.ui] config.
package console

import (
	"net/http"

	"github.com/glennswest/mkube/pkg/config"
)

// Console serves the web dashboard UI.
type Console struct {
	apiBase    string // mkube API base URL for JS (e.g. "http://192.168.200.2:8082")
	cloudidURL string // cloudid API base URL (e.g. "http://192.168.200.20:8090")
}

// New creates a Console from config.
func New(cfg config.ConsoleConfig) *Console {
	api := cfg.APIBase
	if api == "" {
		api = "http://192.168.200.2:8082"
	}
	cid := cfg.CloudidURL
	if cid == "" {
		cid = "http://192.168.200.20:8090"
	}
	return &Console{apiBase: api, cloudidURL: cid}
}

// RegisterRoutes registers all console UI routes on the mux.
func (c *Console) RegisterRoutes(mux *http.ServeMux) {
	// Static assets (cacheable)
	mux.HandleFunc("GET /ui/static/style.css", handleStyleCSS)
	mux.HandleFunc("GET /ui/static/app.js", handleAppJS)

	// Dashboard
	mux.HandleFunc("GET /ui/", c.handleDashboard)
	mux.HandleFunc("GET /ui/dashboard", c.handleDashboard)

	// Nodes
	mux.HandleFunc("GET /ui/nodes", c.handleNodes)
	mux.HandleFunc("GET /ui/nodes/{name}", c.handleNodeDetail)

	// Pods
	mux.HandleFunc("GET /ui/pods", c.handlePods)
	mux.HandleFunc("GET /ui/pods/{ns}/{name}", c.handlePodDetail)

	// Deployments
	mux.HandleFunc("GET /ui/deployments", c.handleDeployments)
	mux.HandleFunc("GET /ui/deployments/{ns}/{name}", c.handleDeploymentDetail)

	// Networks
	mux.HandleFunc("GET /ui/networks", c.handleNetworks)
	mux.HandleFunc("GET /ui/networks/{name}", c.handleNetworkDetail)

	// Bare Metal Hosts
	mux.HandleFunc("GET /ui/bmh", c.handleBMH)
	mux.HandleFunc("GET /ui/bmh/{ns}/{name}", c.handleBMHDetail)

	// Boot Configs
	mux.HandleFunc("GET /ui/bootconfigs", c.handleBootConfigs)
	mux.HandleFunc("GET /ui/bootconfigs/{name}", c.handleBootConfigDetail)

	// Registries
	mux.HandleFunc("GET /ui/registries", c.handleRegistries)
	mux.HandleFunc("GET /ui/registries/{name}", c.handleRegistryDetail)

	// Storage
	mux.HandleFunc("GET /ui/storage", c.handleStorage)

	// Jobs (runners, reservations, queue, logs)
	mux.HandleFunc("GET /ui/jobs", c.handleJobs)

	// Logs
	mux.HandleFunc("GET /ui/logs", c.handleLogs)

	// CloudID templates
	mux.HandleFunc("GET /ui/cloudid", c.handleCloudID)
	mux.HandleFunc("GET /ui/cloudid/{imageType}/{name}", c.handleCloudIDDetail)
}

// write is a helper to write an HTML response.
func write(w http.ResponseWriter, html string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

func handleStyleCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write([]byte(css()))
}

func handleAppJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write([]byte(js()))
}
