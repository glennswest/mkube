// Package dockersave builds docker-save format tar archives from flattened
// OCI rootfs data. RouterOS requires this format with uncompressed layers.
package dockersave

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Write writes a docker-save format tar archive to w.
// The structure matches what `docker save` produces:
//   - manifest.json
//   - repositories
//   - <config-sha>.json
//   - <layer-id>/layer.tar
//   - <layer-id>/VERSION
//   - <layer-id>/json
func Write(w io.Writer, rootfsData []byte, imageRef string, imgCfg *v1.ConfigFile) error {
	// Compute layer SHA256
	layerHash := sha256.Sum256(rootfsData)
	layerID := fmt.Sprintf("%x", layerHash)

	// Derive a repo:tag from the image ref
	repoTag := "mkube:latest"
	if ref, err := name.NewTag(imageRef, name.WeakValidation); err == nil {
		repoTag = ref.Context().RepositoryStr() + ":" + ref.TagStr()
	}
	parts := strings.SplitN(repoTag, ":", 2)
	repo, tag := parts[0], "latest"
	if len(parts) == 2 {
		tag = parts[1]
	}

	// Build container config from the original image config
	containerCfg := map[string]interface{}{}
	if imgCfg != nil {
		if len(imgCfg.Config.Entrypoint) > 0 {
			containerCfg["Entrypoint"] = imgCfg.Config.Entrypoint
		}
		if len(imgCfg.Config.Cmd) > 0 {
			containerCfg["Cmd"] = imgCfg.Config.Cmd
		}
		if imgCfg.Config.WorkingDir != "" {
			containerCfg["WorkingDir"] = imgCfg.Config.WorkingDir
		}
		if len(imgCfg.Config.Env) > 0 {
			containerCfg["Env"] = imgCfg.Config.Env
		}
	}

	// Build image config JSON
	configObj := map[string]interface{}{
		"architecture": runtime.GOARCH,
		"os":           "linux",
		"rootfs": map[string]interface{}{
			"type":     "layers",
			"diff_ids": []string{"sha256:" + layerID},
		},
	}
	if len(containerCfg) > 0 {
		configObj["config"] = containerCfg
	}
	configJSON, _ := json.Marshal(configObj)
	configHash := sha256.Sum256(configJSON)
	configName := fmt.Sprintf("%x.json", configHash)

	// Build manifest.json
	manifestJSON, _ := json.Marshal([]map[string]interface{}{
		{
			"Config":   configName,
			"RepoTags": []string{repoTag},
			"Layers":   []string{layerID + "/layer.tar"},
		},
	})

	// Build repositories file
	reposJSON, _ := json.Marshal(map[string]map[string]string{
		repo: {tag: layerID},
	})

	// Build layer json (legacy docker format)
	layerJSON, _ := json.Marshal(map[string]interface{}{
		"id":      layerID,
		"created": "1970-01-01T00:00:00Z",
		"config":  containerCfg,
	})

	tw := tar.NewWriter(w)
	defer tw.Close()

	addFile := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Size: int64(len(data)),
			Mode: 0o644,
		}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}

	if err := addFile("manifest.json", manifestJSON); err != nil {
		return err
	}
	if err := addFile("repositories", reposJSON); err != nil {
		return err
	}
	if err := addFile(configName, configJSON); err != nil {
		return err
	}
	if err := addFile(layerID+"/VERSION", []byte("1.0")); err != nil {
		return err
	}
	if err := addFile(layerID+"/json", layerJSON); err != nil {
		return err
	}
	if err := addFile(layerID+"/layer.tar", rootfsData); err != nil {
		return err
	}

	return nil
}

// AnonymousKeychain is a Keychain that always returns Anonymous auth,
// allowing the transport layer to handle OAuth2 bearer token exchange.
type AnonymousKeychain struct{}

func (AnonymousKeychain) Resolve(authn.Resource) (authn.Authenticator, error) {
	return authn.Anonymous, nil
}

// SanitizeImageRef converts an image reference to a safe filename component.
func SanitizeImageRef(ref string) string {
	result := make([]byte, 0, len(ref))
	for _, c := range ref {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			result = append(result, byte(c))
		} else if c >= 'A' && c <= 'Z' {
			result = append(result, byte(c+32))
		} else {
			result = append(result, '-')
		}
	}
	return string(result)
}
