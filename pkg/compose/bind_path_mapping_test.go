package compose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/mount"
	"gotest.tools/v3/assert"
)

func TestBindPathMapping(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "app", "data")
	assert.NilError(t, os.MkdirAll(filepath.Dir(inside), 0o700))
	outside := t.TempDir()
	assert.NilError(t, os.Symlink(outside, filepath.Join(root, "escape")))

	service := &composeService{}
	assert.NilError(t, WithBindPathMapping(root, "/stacks")(service))
	mapping := service.bindPathMapping

	for _, tc := range []struct {
		source string
		want   string
	}{
		{root, "/stacks"},
		{inside, "/stacks/app/data"},
		{"/etc/localtime", "/etc/localtime"},
		{"/var/run/docker.sock", "/var/run/docker.sock"},
		{"/var/lib/docker/containers", "/var/lib/docker/containers"},
	} {
		got, err := mapping.guestSource(tc.source)
		assert.NilError(t, err)
		assert.Equal(t, got, tc.want)
	}
	for _, source := range []string{outside, root + "-other/file", filepath.Join(root, "escape", "new"), filepath.Join(root, "..", "outside"), "/var/lib/docker/../outside"} {
		_, err := mapping.guestSource(source)
		assert.Assert(t, err != nil, source)
		assert.Assert(t, strings.Contains(err.Error(), "outside the stacks folder"), err.Error())
	}
}

func TestBindMappingDoesNotChangeDefaultCompose(t *testing.T) {
	var mapping *bindPathMapping
	got, err := mapping.guestSource("/other/host/path")
	assert.NilError(t, err)
	assert.Equal(t, got, "/other/host/path")
}

func TestBindMappingFileConfigsAndSecrets(t *testing.T) {
	root := t.TempDir()
	service := &composeService{}
	assert.NilError(t, WithBindPathMapping(root, "/stacks")(service))
	configFile := filepath.Join(root, "web.conf")
	secretFile := filepath.Join(root, "password")
	assert.NilError(t, os.WriteFile(configFile, []byte("config"), 0o600))
	assert.NilError(t, os.WriteFile(secretFile, []byte("secret"), 0o600))
	project := types.Project{
		WorkingDir: root,
		Configs:    types.Configs{"web": {File: configFile}},
		Secrets:    types.Secrets{"password": {File: secretFile}},
	}
	config := types.ServiceConfig{
		Configs: []types.ServiceConfigObjConfig{{Source: "web"}},
		Secrets: []types.ServiceSecretConfig{{Source: "password"}},
	}
	binds, mounts, err := service.buildContainerVolumes(t.Context(), project, config, nil)
	assert.NilError(t, err)
	assert.Equal(t, len(binds), 0)
	assert.Equal(t, len(mounts), 2)
	for _, item := range mounts {
		assert.Assert(t, item.ReadOnly)
		assert.Equal(t, item.Type, mount.TypeBind)
		if item.Target == "/web" {
			assert.Equal(t, item.Source, "/stacks/web.conf")
		} else {
			assert.Equal(t, item.Target, "/run/secrets/password")
			assert.Equal(t, item.Source, "/stacks/password")
		}
	}
}

func TestBuildContainerVolumesMapsBindSources(t *testing.T) {
	root := t.TempDir()
	service := &composeService{}
	assert.NilError(t, WithBindPathMapping(root, "/stacks")(service))
	project := types.Project{WorkingDir: root}
	config := types.ServiceConfig{
		Name:    "web",
		Volumes: []types.ServiceVolumeConfig{{Type: types.VolumeTypeBind, Source: filepath.Join(root, "data"), Target: "/data"}},
	}
	binds, mounts, err := service.buildContainerVolumes(t.Context(), project, config, nil)
	assert.NilError(t, err)
	assert.DeepEqual(t, binds, []string{"/stacks/data:/data:rw"})
	assert.Equal(t, len(mounts), 0)

	config.Volumes[0].Bind = &types.ServiceVolumeBind{CreateHostPath: false}
	binds, mounts, err = service.buildContainerVolumes(t.Context(), project, config, nil)
	assert.NilError(t, err)
	assert.Equal(t, len(binds), 0)
	assert.DeepEqual(t, mounts, []mount.Mount{{Type: mount.TypeBind, Source: "/stacks/data", Target: "/data", BindOptions: &mount.BindOptions{CreateMountpoint: false}}})
}
