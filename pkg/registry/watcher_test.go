package registry

import (
	"testing"
)

func TestParseUpstreamRef(t *testing.T) {
	tests := []struct {
		ref      string
		wantHost string
		wantRepo string
		wantTag  string
	}{
		{"ghcr.io/glenneth/microdns:latest", "ghcr.io", "glenneth/microdns", "latest"},
		{"ghcr.io/glenneth/microdns:v2.1", "ghcr.io", "glenneth/microdns", "v2.1"},
		{"docker.io/library/nginx:1.25", "docker.io", "library/nginx", "1.25"},
		{"ghcr.io/org/repo", "ghcr.io", "org/repo", "latest"},
		{"registry.example.com:5000/app:v1", "registry.example.com:5000", "app", "v1"},
	}

	for _, tt := range tests {
		host, repo, tag := parseUpstreamRef(tt.ref)
		if host != tt.wantHost || repo != tt.wantRepo || tag != tt.wantTag {
			t.Errorf("parseUpstreamRef(%q) = (%q, %q, %q), want (%q, %q, %q)",
				tt.ref, host, repo, tag, tt.wantHost, tt.wantRepo, tt.wantTag)
		}
	}
}

func TestParseWWWAuthenticate(t *testing.T) {
	header := `Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:glenneth/microdns:pull"`
	params := parseWWWAuthenticate(header)

	if params["realm"] != "https://ghcr.io/token" {
		t.Errorf("realm = %q", params["realm"])
	}
	if params["service"] != "ghcr.io" {
		t.Errorf("service = %q", params["service"])
	}
	if params["scope"] != "repository:glenneth/microdns:pull" {
		t.Errorf("scope = %q", params["scope"])
	}
}

func TestExtractJSONString(t *testing.T) {
	data := []byte(`{"token":"abc123","expires_in":300}`)
	if got := extractJSONString(data, "token"); got != "abc123" {
		t.Errorf("expected 'abc123', got %q", got)
	}

	data2 := []byte(`{"access_token": "xyz789", "type": "bearer"}`)
	if got := extractJSONString(data2, "access_token"); got != "xyz789" {
		t.Errorf("expected 'xyz789', got %q", got)
	}

	if got := extractJSONString(data, "missing"); got != "" {
		t.Errorf("expected empty for missing key, got %q", got)
	}
}

func TestTruncDigest(t *testing.T) {
	short := "sha256:abc"
	if got := truncDigest(short); got != short {
		t.Errorf("expected %q, got %q", short, got)
	}

	long := "sha256:a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"
	got := truncDigest(long)
	if len(got) != 22 { // 19 + "..."
		t.Errorf("expected truncated digest, got %q (len %d)", got, len(got))
	}
}
