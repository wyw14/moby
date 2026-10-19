//go:build linux

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	containertypes "github.com/moby/moby/api/types/container"
	swarmtypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/errdefs"
	"github.com/moby/swarmkit/v2/agent/exec"
	swarmapi "github.com/moby/swarmkit/v2/api"
	"github.com/moby/sys/mountinfo"
	"github.com/moby/sys/user"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/skip"
)

const sharedSecretID = "shared-secret"

type testSecrets struct {
	m map[string]*swarmapi.Secret
}

func (s *testSecrets) Get(secretID string) (*swarmapi.Secret, error) {
	if secret, ok := s.m[secretID]; ok {
		return secret, nil
	}
	return nil, fmt.Errorf("secret %s not found", secretID)
}

type testConfigs struct {
	m map[string]*swarmapi.Config
}

func (c *testConfigs) Get(configID string) (*swarmapi.Config, error) {
	if config, ok := c.m[configID]; ok {
		return config, nil
	}
	return nil, fmt.Errorf("config %s not found", configID)
}

type testVolumes struct{}

func (testVolumes) Get(volumeID string) (string, error) {
	return "", fmt.Errorf("volume %s not found", volumeID)
}

type testDependencyStore struct {
	secrets exec.SecretGetter
	configs exec.ConfigGetter
}

func (d *testDependencyStore) Secrets() exec.SecretGetter { return d.secrets }
func (d *testDependencyStore) Configs() exec.ConfigGetter { return d.configs }
func (d *testDependencyStore) Volumes() exec.VolumeGetter { return testVolumes{} }

func newTestDependencyStore(secrets map[string]string, configs map[string]string) *testDependencyStore {
	sm := &testSecrets{m: make(map[string]*swarmapi.Secret, len(secrets))}
	for id, data := range secrets {
		sm.m[id] = &swarmapi.Secret{ID: id, Spec: swarmapi.SecretSpec{Data: []byte(data)}}
	}
	cm := &testConfigs{m: make(map[string]*swarmapi.Config, len(configs))}
	for id, data := range configs {
		cm.m[id] = &swarmapi.Config{ID: id, Spec: swarmapi.ConfigSpec{Data: []byte(data)}}
	}
	return &testDependencyStore{secrets: sm, configs: cm}
}

func newSecretsTestContainer(t *testing.T, root string, refs []*swarmtypes.SecretReference, store exec.DependencyGetter) *container.Container {
	t.Helper()
	ctr := container.NewBaseContainer(filepath.Base(root), root)
	// Private IPC mode keeps setupContainerDirs free of shm mounts, so the
	// tests only exercise the secret/config setup paths.
	ctr.HostConfig = &containertypes.HostConfig{IpcMode: containertypes.IPCModePrivate}
	ctr.SecretReferences = refs
	ctr.DependencyStore = store
	return ctr
}

// assertMountedAt asserts whether path is a mount point. A path that no
// longer exists after cleanup is considered not mounted.
func assertMountedAt(t *testing.T, path string, want bool) {
	t.Helper()
	mounted, err := mountinfo.Mounted(path)
	if os.IsNotExist(err) {
		mounted, err = false, nil
	}
	assert.NilError(t, err)
	assert.Equal(t, mounted, want)
}

func secretFileRef(id, name, uid, gid string, mode os.FileMode) *swarmtypes.SecretReference {
	return &swarmtypes.SecretReference{
		SecretID:   id,
		SecretName: "secret-" + id,
		File: &swarmtypes.SecretReferenceFileTarget{
			Name: name,
			UID:  uid,
			GID:  gid,
			Mode: mode,
		},
	}
}

// TestSetupContainerDirsPrecheckNoPartialMounts makes sure a failed read-only
// precheck aborts the container start before any directory or mount is
// created, so no half-finished mounts can be left behind.
//
// This test intentionally does not require root: the precheck failure must
// happen before the first mount syscall.
func TestSetupContainerDirsPrecheckNoPartialMounts(t *testing.T) {
	d := &Daemon{}

	for _, tc := range []struct {
		name string
		refs []*swarmtypes.SecretReference
	}{
		{
			name: "duplicate secret reference",
			refs: []*swarmtypes.SecretReference{
				secretFileRef(sharedSecretID, "first", "0", "0", 0o444),
				secretFileRef(sharedSecretID, "second", "0", "0", 0o444),
			},
		},
		{
			name: "invalid gid",
			refs: []*swarmtypes.SecretReference{
				secretFileRef(sharedSecretID, "app", "0", "not-a-number", 0o444),
			},
		},
		{
			name: "invalid file mode bits",
			refs: []*swarmtypes.SecretReference{
				secretFileRef(sharedSecretID, "app", "0", "0", os.ModeDir|0o644),
			},
		},
		{
			name: "missing dependency store",
			refs: []*swarmtypes.SecretReference{
				secretFileRef(sharedSecretID, "app", "0", "0", 0o444),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			ctr := newSecretsTestContainer(t, root, tc.refs, nil)
			if tc.name != "missing dependency store" {
				ctr.DependencyStore = newTestDependencyStore(map[string]string{sharedSecretID: "data"}, nil)
			}

			_, err := d.setupContainerDirs(ctr)
			assert.Assert(t, err != nil, "expected precheck to reject the configuration")

			var invalidParameter errdefs.ErrInvalidParameter
			assert.Assert(t, errors.As(err, &invalidParameter), "expected errdefs.ErrInvalidParameter, got %T: %v", err, err)

			for _, path := range []string{
				filepath.Join(root, "mounts"),
				filepath.Join(root, "mounts", "secrets"),
			} {
				_, statErr := os.Stat(path)
				assert.Assert(t, os.IsNotExist(statErr), "expected %s to not exist after failed precheck, got %v", path, statErr)
			}
		})
	}
}

// TestSetupSecretDirSharedSecretIndependentLifecycle verifies that two
// containers referencing the same secret get independent per-container tmpfs
// mounts and cleaning up one container does not affect the other one.
func TestSetupSecretDirSharedSecretIndependentLifecycle(t *testing.T) {
	skip.If(t, os.Getuid() != 0, "skipping test that requires root to mount tmpfs")

	d := &Daemon{}
	store := newTestDependencyStore(map[string]string{sharedSecretID: "topsecret"}, nil)

	newCtr := func(t *testing.T) *container.Container {
		return newSecretsTestContainer(t, t.TempDir(), []*swarmtypes.SecretReference{
			secretFileRef(sharedSecretID, "shared", "0", "0", 0o400),
		}, store)
	}

	ctr1 := newCtr(t)
	ctr2 := newCtr(t)

	_, err := d.setupContainerDirs(ctr1)
	assert.NilError(t, err)
	t.Cleanup(func() { d.cleanupSecretDir(ctr1) })
	_, err = d.setupContainerDirs(ctr2)
	assert.NilError(t, err)
	t.Cleanup(func() { d.cleanupSecretDir(ctr2) })

	secretPath := func(ctr *container.Container) string {
		p, err := ctr.SecretFilePath(*ctr.SecretReferences[0])
		assert.NilError(t, err)
		return p
	}
	secretDir := func(ctr *container.Container) string {
		p, err := ctr.SecretMountPath()
		assert.NilError(t, err)
		return p
	}

	dir1, dir2 := secretDir(ctr1), secretDir(ctr2)
	p1, p2 := secretPath(ctr1), secretPath(ctr2)
	assert.Assert(t, p1 != p2, "containers must not share the on-host secret path: %s", p1)

	// The tmpfs is mounted on the per-container secrets directory; the
	// individual secret files live inside it.
	assertMountedAt(t, dir1, true)
	assertMountedAt(t, dir2, true)

	data1, err := os.ReadFile(p1)
	assert.NilError(t, err)
	data2, err := os.ReadFile(p2)
	assert.NilError(t, err)
	assert.DeepEqual(t, data1, []byte("topsecret"))
	assert.DeepEqual(t, data2, []byte("topsecret"))

	// Tearing down the first container must not touch the second one.
	d.cleanupSecretDir(ctr1)

	_, statErr := os.Stat(dir1)
	assert.Assert(t, os.IsNotExist(statErr), "expected cleaned up %s to be gone", dir1)
	assertMountedAt(t, dir1, false)

	assertMountedAt(t, dir2, true)
	data2, err = os.ReadFile(p2)
	assert.NilError(t, err)
	assert.DeepEqual(t, data2, []byte("topsecret"))
}

// TestSetupSecretDirRootlessRemappedOwnership exercises the secret setup with
// a non-empty UID/GID mapping as used by rootless/userns-remap daemons: files
// must be owned by the remapped UID/GID pair.
func TestSetupSecretDirRootlessRemappedOwnership(t *testing.T) {
	skip.If(t, os.Getuid() != 0, "skipping test that requires root to mount tmpfs")

	const (
		hostID = 100000
		size   = 65536
	)
	mapping := user.IdentityMapping{
		UIDMaps: []user.IDMap{{ID: 0, ParentID: hostID, Count: size}},
		GIDMaps: []user.IDMap{{ID: 0, ParentID: hostID, Count: size}},
	}
	d := &Daemon{idMapping: mapping}
	store := newTestDependencyStore(map[string]string{"rootless-secret": "data"}, nil)
	ctr := newSecretsTestContainer(t, t.TempDir(), []*swarmtypes.SecretReference{
		secretFileRef("rootless-secret", "rootless", "1000", "10", 0o400),
	}, store)
	t.Cleanup(func() { d.cleanupSecretDir(ctr) })

	if _, err := d.setupContainerDirs(ctr); err != nil {
		// Running inside an unprivileged user namespace whose map does not
		// cover 100000+ makes chown fail; skip in that environment.
		skip.If(t, errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EPERM),
			"skipping: remapped IDs are not available in this user namespace: %v", err)
		assert.NilError(t, err)
	}

	secretDir, err := ctr.SecretMountPath()
	assert.NilError(t, err)
	fi, err := os.Stat(secretDir)
	assert.NilError(t, err)
	assert.Equal(t, fi.Sys().(*syscall.Stat_t).Uid, uint32(hostID))
	assert.Equal(t, fi.Sys().(*syscall.Stat_t).Gid, uint32(hostID))

	secretFile, err := ctr.SecretFilePath(*ctr.SecretReferences[0])
	assert.NilError(t, err)
	fi, err = os.Stat(secretFile)
	assert.NilError(t, err)
	assert.Equal(t, fi.Sys().(*syscall.Stat_t).Uid, uint32(hostID+1000))
	assert.Equal(t, fi.Sys().(*syscall.Stat_t).Gid, uint32(hostID+10))
	assert.Equal(t, fi.Mode().Perm(), os.FileMode(0o400))

	// A bad reference must still be rejected before anything is created when
	// running with a remapped root.
	bad := newSecretsTestContainer(t, t.TempDir(), []*swarmtypes.SecretReference{
		secretFileRef("rootless-secret", "rootless", "0", "not-a-number", 0o400),
	}, store)
	_, err = d.setupContainerDirs(bad)
	assert.Assert(t, err != nil)
	_, statErr := os.Stat(filepath.Join(bad.Root, "mounts", "secrets"))
	assert.Assert(t, os.IsNotExist(statErr))
}

// TestSetupSecretDirRestartCycle simulates a stop/start (e.g. triggered by the
// restart manager): the tmpfs is unmounted on stop and must be recreated from
// scratch on the next start, with the precheck running on every start.
func TestSetupSecretDirRestartCycle(t *testing.T) {
	skip.If(t, os.Getuid() != 0, "skipping test that requires root to mount tmpfs")

	d := &Daemon{}
	store := newTestDependencyStore(map[string]string{"restart-secret": "v1"}, nil)
	ctr := newSecretsTestContainer(t, t.TempDir(), []*swarmtypes.SecretReference{
		secretFileRef("restart-secret", "restart", "0", "0", 0o444),
	}, store)
	t.Cleanup(func() { d.cleanupSecretDir(ctr) })

	for range 2 {
		_, err := d.setupContainerDirs(ctr)
		assert.NilError(t, err)

		dir, err := ctr.SecretMountPath()
		assert.NilError(t, err)
		assertMountedAt(t, dir, true)
		p, err := ctr.SecretFilePath(*ctr.SecretReferences[0])
		assert.NilError(t, err)
		data, err := os.ReadFile(p)
		assert.NilError(t, err)
		assert.DeepEqual(t, data, []byte("v1"))

		// Simulate stopping the container, which unmounts (but does not
		// remove) the secrets directory.
		assert.NilError(t, ctr.UnmountSecrets())
		assertMountedAt(t, dir, false)
	}

	// An invalid reference must keep failing on restart attempts without
	// mounting anything.
	bad := newSecretsTestContainer(t, t.TempDir(), []*swarmtypes.SecretReference{
		secretFileRef("restart-secret", "restart", "0", "oops", 0o444),
	}, store)
	for attempt := range 2 {
		_, err := d.setupContainerDirs(bad)
		assert.Assert(t, err != nil, "attempt %d: expected precheck failure", attempt)
		mountsRoot := filepath.Join(bad.Root, "mounts", "secrets")
		assertMountedAt(t, mountsRoot, false)
	}
}

// TestSetupContainerDirsConcurrentStarts starts several containers referencing
// the same secret concurrently. Each must get its own independent mount setup
// without interfering with the others.
func TestSetupContainerDirsConcurrentStarts(t *testing.T) {
	skip.If(t, os.Getuid() != 0, "skipping test that requires root to mount tmpfs")

	const containers = 16

	d := &Daemon{}
	store := newTestDependencyStore(map[string]string{sharedSecretID: "concurrent"}, nil)

	ctrs := make([]*container.Container, containers)
	for i := range ctrs {
		ctrs[i] = newSecretsTestContainer(t, t.TempDir(), []*swarmtypes.SecretReference{
			secretFileRef(sharedSecretID, fmt.Sprintf("secret-%d", i), "0", "0", 0o400),
		}, store)
	}
	t.Cleanup(func() {
		for _, ctr := range ctrs {
			d.cleanupSecretDir(ctr)
		}
	})

	var wg sync.WaitGroup
	errs := make(chan error, containers)
	dirs := make(chan string, containers)

	for _, ctr := range ctrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.setupContainerDirs(ctr); err != nil {
				errs <- err
				return
			}
			p, err := ctr.SecretMountPath()
			if err != nil {
				errs <- err
				return
			}
			dirs <- p
		}()
	}
	wg.Wait()
	close(errs)
	close(dirs)

	for err := range errs {
		assert.NilError(t, err)
	}

	seen := make(map[string]struct{}, containers)
	for dir := range dirs {
		_, dup := seen[dir]
		assert.Assert(t, !dup, "two containers share the same host secrets directory: %s", dir)
		seen[dir] = struct{}{}

		assertMountedAt(t, dir, true)
	}
	assert.Equal(t, len(seen), containers)

	// Every shared secret must be readable from each container's own mount.
	for _, ctr := range ctrs {
		p, err := ctr.SecretFilePath(*ctr.SecretReferences[0])
		assert.NilError(t, err)
		data, err := os.ReadFile(p)
		assert.NilError(t, err)
		assert.DeepEqual(t, data, []byte("concurrent"))
	}

	// Cleanup must be independent too: tearing every container down at the
	// same time must leave no mounts behind.
	var cleanupWg sync.WaitGroup
	cleanupErrs := make(chan error, containers)
	for _, ctr := range ctrs {
		cleanupWg.Add(1)
		go func() {
			defer cleanupWg.Done()
			dir, _ := ctr.SecretMountPath()
			d.cleanupSecretDir(ctr)
			mounted, err := mountinfo.Mounted(dir)
			if os.IsNotExist(err) {
				return
			}
			if err != nil {
				cleanupErrs <- err
				return
			}
			if mounted {
				cleanupErrs <- fmt.Errorf("expected %s to be unmounted after cleanup", dir)
			}
		}()
	}
	cleanupWg.Wait()
	close(cleanupErrs)
	for err := range cleanupErrs {
		assert.NilError(t, err)
	}
}
