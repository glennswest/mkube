package routeros

import (
	"context"
	"strings"
	"testing"

	rosapi "github.com/go-routeros/routeros/v3"
	"github.com/go-routeros/routeros/v3/proto"
	"github.com/glennswest/mkube/pkg/config"
)

// newTestClient creates a Client with a mock exec function.
// Tests override c.exec to provide expected responses.
func newTestClient(t *testing.T) *Client {
	t.Helper()
	return &Client{
		cfg: config.RouterOSConfig{
			RESTURL:  "https://192.168.200.1",
			User:     "admin",
			Password: "",
			Address:  "192.168.200.1:8728",
		},
		exec: func(ctx context.Context, words ...string) (*rosapi.Reply, error) {
			t.Fatalf("unexpected exec call: %v", words)
			return nil, nil
		},
	}
}

// makeReply constructs a rosapi.Reply with the given !re sentence maps.
func makeReply(sentences ...map[string]string) *rosapi.Reply {
	reply := &rosapi.Reply{
		Done: &proto.Sentence{Word: "!done"},
	}
	for _, m := range sentences {
		reply.Re = append(reply.Re, &proto.Sentence{
			Word: "!re",
			Map:  m,
		})
	}
	return reply
}

// makeAddReply constructs a rosapi.Reply for /add commands with a ret value.
func makeAddReply(ret string) *rosapi.Reply {
	return &rosapi.Reply{
		Done: &proto.Sentence{
			Word: "!done",
			Map:  map[string]string{"ret": ret},
		},
	}
}

// makeDoneReply constructs a rosapi.Reply with just !done (no data).
func makeDoneReply() *rosapi.Reply {
	return &rosapi.Reply{
		Done: &proto.Sentence{Word: "!done"},
	}
}

func TestListContainers(t *testing.T) {
	client := newTestClient(t)
	client.exec = func(ctx context.Context, words ...string) (*rosapi.Reply, error) {
		if words[0] != "/container/print" {
			t.Errorf("unexpected command: %v", words)
		}
		return makeReply(
			map[string]string{".id": "*1", "name": "test-container", "running": "true"},
			map[string]string{".id": "*2", "name": "another", "stopped": "true"},
		), nil
	}

	result, err := client.ListContainers(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(result))
	}
	if result[0].Name != "test-container" {
		t.Errorf("expected name 'test-container', got %q", result[0].Name)
	}
}

func TestGetContainer(t *testing.T) {
	client := newTestClient(t)
	client.exec = func(ctx context.Context, words ...string) (*rosapi.Reply, error) {
		return makeReply(
			map[string]string{".id": "*1", "name": "target", "running": "true"},
			map[string]string{".id": "*2", "name": "other", "stopped": "true"},
		), nil
	}

	ct, err := client.GetContainer(context.Background(), "target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ct.ID != "*1" {
		t.Errorf("expected ID '*1', got %q", ct.ID)
	}

	_, err = client.GetContainer(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent container")
	}
}

func TestCreateContainer(t *testing.T) {
	var scriptSource, scriptName string
	var scriptCreated, scriptRun, scriptRemoved bool
	containerReady := false

	client := newTestClient(t)
	client.exec = func(ctx context.Context, words ...string) (*rosapi.Reply, error) {
		cmd := words[0]
		switch {
		case cmd == "/system/script/print":
			// cleanupStaleScripts lists existing scripts
			return makeReply(), nil // empty list
		case cmd == "/system/script/add":
			scriptCreated = true
			for _, w := range words[1:] {
				if strings.HasPrefix(w, "=name=") {
					scriptName = strings.TrimPrefix(w, "=name=")
				}
				if strings.HasPrefix(w, "=source=") {
					scriptSource = strings.TrimPrefix(w, "=source=")
				}
			}
			return makeAddReply("*A1"), nil
		case cmd == "/system/script/run":
			scriptRun = true
			containerReady = true // simulate async extraction completing
			return makeDoneReply(), nil
		case cmd == "/system/script/remove":
			scriptRemoved = true
			return makeDoneReply(), nil
		case cmd == "/container/print":
			if containerReady {
				return makeReply(
					map[string]string{".id": "*1", "name": "new-container", "stopped": "true"},
				), nil
			}
			return makeReply(), nil
		default:
			t.Errorf("unexpected command: %v", words)
			return makeDoneReply(), nil
		}
	}

	spec := ContainerSpec{
		Name:      "new-container",
		File:      "/cache/test.tar",
		Interface: "veth-test-0",
		RootDir:   "/data/test",
		Logging:   "yes",
	}

	err := client.CreateContainer(context.Background(), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !scriptCreated {
		t.Error("expected script to be created")
	}
	if !scriptRun {
		t.Error("expected script to be run")
	}
	if !scriptRemoved {
		t.Error("expected script to be cleaned up")
	}
	if !strings.HasPrefix(scriptName, "mkube-task-") {
		t.Errorf("expected script name with mkube-task- prefix, got %q", scriptName)
	}
	expected := "/container/add name=new-container file=/cache/test.tar interface=veth-test-0 root-dir=/data/test logging=yes"
	if scriptSource != expected {
		t.Errorf("unexpected script source:\n got: %s\nwant: %s", scriptSource, expected)
	}
}

func TestCreateContainerCleansUpStaleScripts(t *testing.T) {
	var removedIDs []string
	containerReady := false

	client := newTestClient(t)
	client.exec = func(ctx context.Context, words ...string) (*rosapi.Reply, error) {
		cmd := words[0]
		switch {
		case cmd == "/system/script/print":
			return makeReply(
				map[string]string{".id": "*S1", "name": "mkube-task-000001"},
				map[string]string{".id": "*S2", "name": "mkube-task-000002"},
				map[string]string{".id": "*S3", "name": "user-script"},
			), nil
		case cmd == "/system/script/add":
			return makeAddReply("*A1"), nil
		case cmd == "/system/script/run":
			containerReady = true
			return makeDoneReply(), nil
		case cmd == "/system/script/remove":
			for _, w := range words[1:] {
				if strings.HasPrefix(w, "=.id=") {
					removedIDs = append(removedIDs, strings.TrimPrefix(w, "=.id="))
				}
			}
			return makeDoneReply(), nil
		case cmd == "/container/print":
			if containerReady {
				return makeReply(
					map[string]string{".id": "*1", "name": "test-ct", "stopped": "true"},
				), nil
			}
			return makeReply(), nil
		default:
			t.Errorf("unexpected command: %v", words)
			return makeDoneReply(), nil
		}
	}

	spec := ContainerSpec{
		Name:      "test-ct",
		File:      "/cache/test.tar",
		Interface: "veth-test-0",
		RootDir:   "/data/test",
	}

	err := client.CreateContainer(context.Background(), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have removed the two stale mkube-task-* scripts, plus the new one after completion
	staleRemoved := 0
	for _, id := range removedIDs {
		if id == "*S1" || id == "*S2" {
			staleRemoved++
		}
		if id == "*S3" {
			t.Error("should not remove non-mkube-task scripts")
		}
	}
	if staleRemoved != 2 {
		t.Errorf("expected 2 stale scripts removed, got %d (removed: %v)", staleRemoved, removedIDs)
	}
}

func TestBuildContainerAddCLI(t *testing.T) {
	spec := ContainerSpec{
		Name:        "test",
		RemoteImage: "192.168.200.3:5000/myapp:latest",
		Interface:   "veth-ns-pod-0",
		RootDir:     "/data/test",
		MountLists:  "mounts-test",
		Cmd:         "/app",
		Entrypoint:  "/bin/sh",
		WorkDir:     "/app",
		Hostname:    "test-host",
		DNS:         "192.168.200.199",
		User:        "nobody",
		Envlist:     "envs-test",
		Logging:     "yes",
		StartOnBoot: "yes",
	}

	result := buildContainerAddCLI(spec)
	expected := "/container/add name=test remote-image=192.168.200.3:5000/myapp:latest" +
		" interface=veth-ns-pod-0 root-dir=/data/test mountlists=mounts-test" +
		" cmd=/app entrypoint=/bin/sh workdir=/app hostname=test-host" +
		" dns=192.168.200.199 user=nobody envlist=envs-test logging=yes start-on-boot=yes"
	if result != expected {
		t.Errorf("unexpected CLI:\n got: %s\nwant: %s", result, expected)
	}
}

func TestBuildContainerAddCLI_QuotesValuesWithSpaces(t *testing.T) {
	// Real-world failure: nats-server cmd has spaces; without quoting, RouterOS
	// CLI parsed `cmd=nats-server` then choked on `-js --store_dir ...`.
	spec := ContainerSpec{
		Name:      "gt_nats_nats",
		File:      "/raid1/cache/nats.tar",
		Interface: "veth_gt_nats_0",
		RootDir:   "/raid1/images/gt_nats_nats",
		Cmd:       "nats-server -js --store_dir /data --addr 0.0.0.0 -m 8222",
	}
	result := buildContainerAddCLI(spec)
	want := `cmd="nats-server -js --store_dir /data --addr 0.0.0.0 -m 8222"`
	if !strings.Contains(result, want) {
		t.Errorf("multi-word cmd not quoted:\n got: %s\nwant substring: %s", result, want)
	}
}

func TestCLIQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"plain", "plain"},
		{"with-dash_and.dot", "with-dash_and.dot"},
		{"has space", `"has space"`},
		{"has\ttab", "\"has\ttab\""},
		{`has"quote`, `"has\"quote"`},
		{`has\backslash`, `"has\\backslash"`},
		{"has;semicolon", `"has;semicolon"`},
	}
	for _, c := range cases {
		got := cliQuote(c.in)
		if got != c.want {
			t.Errorf("cliQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStartStopContainer(t *testing.T) {
	var lastCmd string
	client := newTestClient(t)
	client.exec = func(ctx context.Context, words ...string) (*rosapi.Reply, error) {
		lastCmd = words[0]
		return makeDoneReply(), nil
	}

	if err := client.StartContainer(context.Background(), "*1"); err != nil {
		t.Fatalf("start error: %v", err)
	}
	if lastCmd != "/container/start" {
		t.Errorf("expected /container/start, got %s", lastCmd)
	}

	if err := client.StopContainer(context.Background(), "*1"); err != nil {
		t.Fatalf("stop error: %v", err)
	}
	if lastCmd != "/container/stop" {
		t.Errorf("expected /container/stop, got %s", lastCmd)
	}
}

func TestRemoveContainer(t *testing.T) {
	client := newTestClient(t)
	client.exec = func(ctx context.Context, words ...string) (*rosapi.Reply, error) {
		if words[0] != "/container/remove" {
			t.Errorf("unexpected command: %s", words[0])
		}
		return makeDoneReply(), nil
	}

	if err := client.RemoveContainer(context.Background(), "*1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateVeth(t *testing.T) {
	var addWords []string
	client := newTestClient(t)
	client.exec = func(ctx context.Context, words ...string) (*rosapi.Reply, error) {
		switch words[0] {
		case "/interface/veth/print":
			// ListVeths — return empty list so CreateVeth proceeds to add
			return makeReply(), nil
		case "/interface/veth/add":
			addWords = words
			return makeDoneReply(), nil
		default:
			t.Errorf("unexpected command: %v", words)
			return makeDoneReply(), nil
		}
	}

	if err := client.CreateVeth(context.Background(), "veth0", "172.20.0.5/16", "172.20.0.1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Check attribute words contain expected values
	found := map[string]bool{}
	for _, w := range addWords {
		if w == "=name=veth0" || w == "=address=172.20.0.5/16" || w == "=gateway=172.20.0.1" {
			found[w] = true
		}
	}
	if !found["=name=veth0"] {
		t.Error("missing =name=veth0 in add words")
	}
	if !found["=address=172.20.0.5/16"] {
		t.Error("missing =address=172.20.0.5/16 in add words")
	}
}

func TestCreateVethIdempotent(t *testing.T) {
	client := newTestClient(t)
	client.exec = func(ctx context.Context, words ...string) (*rosapi.Reply, error) {
		switch words[0] {
		case "/interface/veth/print":
			return makeReply(
				map[string]string{".id": "*1", "name": "veth0", "address": "172.20.0.5/16", "gateway": "172.20.0.1"},
			), nil
		case "/interface/veth/add":
			t.Error("should not call add when veth already exists")
			return makeDoneReply(), nil
		default:
			t.Errorf("unexpected command: %v", words)
			return makeDoneReply(), nil
		}
	}

	if err := client.CreateVeth(context.Background(), "veth0", "172.20.0.5/16", "172.20.0.1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateVethUpdate(t *testing.T) {
	var setWords []string
	client := newTestClient(t)
	client.exec = func(ctx context.Context, words ...string) (*rosapi.Reply, error) {
		switch words[0] {
		case "/interface/veth/print":
			return makeReply(
				map[string]string{".id": "*1", "name": "veth0", "address": "172.20.0.99/16", "gateway": "172.20.0.1"},
			), nil
		case "/interface/veth/set":
			setWords = words
			return makeDoneReply(), nil
		default:
			t.Errorf("unexpected command: %v", words)
			return makeDoneReply(), nil
		}
	}

	if err := client.CreateVeth(context.Background(), "veth0", "172.20.0.5/16", "172.20.0.1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Check set words contain .id and new address
	found := map[string]bool{}
	for _, w := range setWords {
		found[w] = true
	}
	if !found["=.id=*1"] {
		t.Error("missing =.id=*1 in set words")
	}
	if !found["=address=172.20.0.5/16"] {
		t.Error("missing =address=172.20.0.5/16 in set words")
	}
}

func TestListVeths(t *testing.T) {
	client := newTestClient(t)
	client.exec = func(ctx context.Context, words ...string) (*rosapi.Reply, error) {
		return makeReply(
			map[string]string{".id": "*1", "name": "veth0", "address": "172.20.0.5/16"},
		), nil
	}

	result, err := client.ListVeths(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 veth, got %d", len(result))
	}
}

func TestErrorHandling(t *testing.T) {
	client := newTestClient(t)
	client.exec = func(ctx context.Context, words ...string) (*rosapi.Reply, error) {
		return nil, &rosapi.DeviceError{
			Sentence: &proto.Sentence{
				Map: map[string]string{"message": "no such command"},
			},
		}
	}

	_, err := client.ListContainers(context.Background())
	if err == nil {
		t.Error("expected error for device error response")
	}
}

func TestClose(t *testing.T) {
	client := newTestClient(t)
	if err := client.Close(); err != nil {
		t.Errorf("unexpected close error: %v", err)
	}
}

func TestParseRESTPath(t *testing.T) {
	tests := []struct {
		path          string
		wantBase      string
		wantQueryLen  int
		wantFirstWord string
	}{
		{"/container", "/container", 0, ""},
		{"/container/mounts?list=foo", "/container/mounts", 1, "?list=foo"},
		{"/interface/bridge/port?interface=veth0", "/interface/bridge/port", 1, "?interface=veth0"},
		{"/file?name=usb1/test", "/file", 1, "?name=usb1/test"},
	}

	for _, tt := range tests {
		base, qw := parseRESTPath(tt.path)
		if base != tt.wantBase {
			t.Errorf("parseRESTPath(%q): base = %q, want %q", tt.path, base, tt.wantBase)
		}
		if len(qw) != tt.wantQueryLen {
			t.Errorf("parseRESTPath(%q): len(queryWords) = %d, want %d", tt.path, len(qw), tt.wantQueryLen)
		}
		if tt.wantQueryLen > 0 && qw[0] != tt.wantFirstWord {
			t.Errorf("parseRESTPath(%q): queryWords[0] = %q, want %q", tt.path, qw[0], tt.wantFirstWord)
		}
	}
}

func TestBodyToWords(t *testing.T) {
	words := bodyToWords(map[string]string{
		"name":    "test",
		"address": "1.2.3.4",
	})
	// Words should be sorted
	if len(words) != 2 {
		t.Fatalf("expected 2 words, got %d", len(words))
	}
	// After sorting: =address=1.2.3.4, =name=test
	if words[0] != "=address=1.2.3.4" {
		t.Errorf("expected =address=1.2.3.4, got %s", words[0])
	}
	if words[1] != "=name=test" {
		t.Errorf("expected =name=test, got %s", words[1])
	}
}

func TestDecodeReplySlice(t *testing.T) {
	reply := makeReply(
		map[string]string{".id": "*1", "name": "a"},
		map[string]string{".id": "*2", "name": "b"},
	)

	var result []struct {
		ID   string `json:".id"`
		Name string `json:"name"`
	}
	if err := decodeReply(reply, &result); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 items, got %d", len(result))
	}
	if result[0].ID != "*1" || result[0].Name != "a" {
		t.Errorf("unexpected first item: %+v", result[0])
	}
}

func TestDecodeReplySingle(t *testing.T) {
	reply := makeReply(
		map[string]string{"uptime": "3d12h", "version": "7.22.2", "cpu-load": "12"},
	)

	var result SystemResource
	if err := decodeReply(reply, &result); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Uptime != "3d12h" {
		t.Errorf("expected uptime '3d12h', got %q", result.Uptime)
	}
	if result.Version != "7.22.2" {
		t.Errorf("expected version '7.22.2', got %q", result.Version)
	}
}

func TestDecodeReplyEmptySlice(t *testing.T) {
	reply := makeReply() // no sentences

	var result []Container
	if err := decodeReply(reply, &result); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		// JSON unmarshals empty array to nil slice, which is fine
	}
}

func TestListMountsByList(t *testing.T) {
	client := newTestClient(t)
	client.exec = func(ctx context.Context, words ...string) (*rosapi.Reply, error) {
		// Verify the query filter is passed correctly
		if words[0] != "/container/mounts/print" {
			t.Errorf("expected /container/mounts/print, got %s", words[0])
		}
		if len(words) < 2 || words[1] != "?list=my-mounts" {
			t.Errorf("expected ?list=my-mounts filter, got %v", words[1:])
		}
		return makeReply(
			map[string]string{".id": "*1", "list": "my-mounts", "src": "/data/vol", "dst": "/mnt"},
		), nil
	}

	mounts, err := client.ListMountsByList(context.Background(), "my-mounts")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}
	if mounts[0].Src != "/data/vol" {
		t.Errorf("expected src '/data/vol', got %q", mounts[0].Src)
	}
}

func TestGetSystemResource(t *testing.T) {
	client := newTestClient(t)
	client.exec = func(ctx context.Context, words ...string) (*rosapi.Reply, error) {
		if words[0] != "/system/resource/print" {
			t.Errorf("expected /system/resource/print, got %s", words[0])
		}
		return makeReply(
			map[string]string{
				"uptime":            "5d3h",
				"cpu-count":         "4",
				"cpu-load":          "15",
				"free-memory":       "512000000",
				"total-memory":      "1024000000",
				"architecture-name": "arm64",
				"board-name":        "RB5009",
				"version":           "7.22.2",
				"platform":          "MikroTik",
			},
		), nil
	}

	res, err := client.GetSystemResource(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Version != "7.22.2" {
		t.Errorf("expected version '7.22.2', got %q", res.Version)
	}
	if res.Architecture != "arm64" {
		t.Errorf("expected arch 'arm64', got %q", res.Architecture)
	}
}
