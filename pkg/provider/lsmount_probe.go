package provider

// "Use a container as the shell."
//
// Every /file-based listing on this box is a whole-tree walk that takes
// minutes, so inspecting what is actually inside a mounted clone is
// impractical from the API side. But RouterOS will happily run a container
// with that clone mounted — and the nats image ships busybox — so `ls` in a
// throwaway container answers in seconds, with the output landing in the
// device log.
//
// GET /api/v1/probes/lsmount?slot=iscsi15
//
// Answers the open question behind the CoW failure: the clone carries
// 60 MB, the mount entry is correct, and RouterOS mounts the ext4 — so
// WHERE did the seeder's image actually land?

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/glennswest/mkube/pkg/routeros"
)

func (p *MicroKubeProvider) handleLsMount(w http.ResponseWriter, r *http.Request) {
	slot := strings.Trim(r.URL.Query().Get("slot"), "/")
	if slot == "" {
		http.Error(w, "slot query parameter required (e.g. iscsi15)", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	out := map[string]any{"slot": slot}
	steps := []string{}
	step := func(f string, a ...any) {
		s := fmt.Sprintf(f, a...)
		steps = append(steps, s)
		p.deps.Logger.Infow("LSMOUNT: " + s)
	}
	defer func() {
		out["steps"] = steps
		podWriteJSON(w, http.StatusOK, out)
	}()

	ros := p.getRouterOSClient()
	if ros == nil {
		out["error"] = "RouterOS backend required"
		return
	}

	tarball, err := p.deps.StorageMgr.EnsureImage(ctx, "192.168.200.3:5000/nats:edge")
	if err != nil {
		out["error"] = fmt.Sprintf("staging busybox-bearing image: %v", err)
		return
	}

	unguard := p.cowProbeGuardPod()
	defer unguard()

	veth := "veth_gt_cowprobe_0"
	if _, _, _, verr := p.deps.NetworkMgr.AllocateInterface(ctx, veth, "cowprobe.cowprobe", "gt", ""); verr != nil {
		out["error"] = fmt.Sprintf("allocating veth: %v", verr)
		return
	}
	defer func() { _ = p.deps.NetworkMgr.ReleaseInterface(ctx, veth) }()

	name := cowProbeContainer
	list := "lsmount"
	_ = ros.RemoveMountsByList(ctx, list)
	if err := ros.CreateMount(ctx, list, "/"+slot, "/payload"); err != nil {
		out["error"] = fmt.Sprintf("creating mount: %v", err)
		return
	}
	defer func() { _ = ros.RemoveMountsByList(context.Background(), list) }()

	rootDir := "raid1/images/lsmount-probe"
	_ = ros.RemoveDirectory(ctx, rootDir)
	if err := ros.CreateContainer(ctx, routeros.ContainerSpec{
		Name:        name,
		Interface:   veth,
		RootDir:     rootDir,
		Image:       strings.TrimPrefix(tarball, "/"),
		MountLists:  list,
		Entrypoint:  "/bin/ls",
		Cmd:         "-la /payload",
		Logging:     "yes",
		StartOnBoot: "no",
	}); err != nil {
		out["error"] = fmt.Sprintf("creating listing container: %v", err)
		return
	}
	step("listing container created (ls -la /payload)")

	// Let extraction settle, then start it so ls actually runs.
	deadline := time.Now().Add(4 * time.Minute)
	started := false
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		ros.InvalidateContainerCache()
		ct, gerr := ros.GetContainer(ctx, name)
		if gerr != nil {
			continue
		}
		if ct.IsRunning() {
			started = true
			break
		}
		if ct.IsStopped() {
			if serr := ros.StartContainer(ctx, ct.ID); serr == nil {
				started = true
				time.Sleep(5 * time.Second)
			}
			break
		}
	}
	step("listing container started: %v", started)
	time.Sleep(5 * time.Second)

	if entries, lerr := ros.TailLog(ctx, 60); lerr == nil {
		var lines []string
		for _, e := range entries {
			if strings.Contains(e.Message, "cowprobe") || strings.Contains(e.Message, "payload") ||
				strings.Contains(e.Message, "total") || strings.Contains(e.Message, "drw") ||
				strings.Contains(e.Message, "rw-") {
				lines = append(lines, e.Time+" "+e.Message)
			}
		}
		out["listing"] = lines
	}

	ros.InvalidateContainerCache()
	if ct, gerr := ros.GetContainer(ctx, name); gerr == nil {
		_ = ros.StopContainer(ctx, ct.ID)
		for i := 0; i < 6; i++ {
			time.Sleep(2 * time.Second)
			if ros.RemoveContainer(ctx, ct.ID) == nil {
				break
			}
		}
	}
	_ = ros.RemoveDirectory(ctx, rootDir)
}
