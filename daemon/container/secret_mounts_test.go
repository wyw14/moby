package container

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	containertypes "github.com/moby/moby/api/types/container"
	mounttypes "github.com/moby/moby/api/types/mount"
	swarmtypes "github.com/moby/moby/api/types/swarm"
	volumemounts "github.com/moby/moby/v2/daemon/volume/mounts"
	"github.com/moby/swarmkit/v2/agent/exec"
	swarmapi "github.com/moby/swarmkit/v2/api"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func secretReference(id, name string) *swarmtypes.SecretReference {
	return &swarmtypes.SecretReference{
		SecretID:   id,
		SecretName: "secret-" + id,
		File: &swarmtypes.SecretReferenceFileTarget{
			Name: name,
			UID:  "0",
			GID:  "0",
			Mode: 0o444,
		},
	}
}

func configReference(id, name string) *swarmtypes.ConfigReference {
	return &swarmtypes.ConfigReference{
		ConfigID:   id,
		ConfigName: "config-" + id,
		File: &swarmtypes.ConfigReferenceFileTarget{
			Name: name,
			UID:  "0",
			GID:  "0",
			Mode: 0o444,
		},
	}
}

type fakeSecrets struct {
	m map[string]*swarmapi.Secret
}

func (s *fakeSecrets) Get(secretID string) (*swarmapi.Secret, error) {
	if secret, ok := s.m[secretID]; ok {
		return secret, nil
	}
	return nil, fmt.Errorf("secret %s not found", secretID)
}

type fakeConfigs struct {
	m map[string]*swarmapi.Config
}

func (c *fakeConfigs) Get(configID string) (*swarmapi.Config, error) {
	if config, ok := c.m[configID]; ok {
		return config, nil
	}
	return nil, fmt.Errorf("config %s not found", configID)
}

type fakeVolumes struct{}

func (fakeVolumes) Get(volumeID string) (string, error) {
	return "", fmt.Errorf("volume %s not found", volumeID)
}

type fakeDependencyStore struct {
	secrets exec.SecretGetter
	configs exec.ConfigGetter
}

func (d *fakeDependencyStore) Secrets() exec.SecretGetter { return d.secrets }
func (d *fakeDependencyStore) Configs() exec.ConfigGetter { return d.configs }
func (d *fakeDependencyStore) Volumes() exec.VolumeGetter { return fakeVolumes{} }

func newFakeDependencyStore(secretIDs, configIDs []string) *fakeDependencyStore {
	secrets := &fakeSecrets{m: make(map[string]*swarmapi.Secret)}
	for _, id := range secretIDs {
		secrets.m[id] = &swarmapi.Secret{ID: id}
	}
	configs := &fakeConfigs{m: make(map[string]*swarmapi.Config)}
	for _, id := range configIDs {
		configs.m[id] = &swarmapi.Config{ID: id}
	}
	return &fakeDependencyStore{secrets: secrets, configs: configs}
}

func newPrecheckContainer(t *testing.T) *Container {
	t.Helper()
	ctr := NewBaseContainer("precheck-container", t.TempDir())
	ctr.HostConfig = &containertypes.HostConfig{}
	return ctr
}

func TestValidateSecretConfigReferencesValid(t *testing.T) {
	ctr := newPrecheckContainer(t)
	ctr.SecretReferences = []*swarmtypes.SecretReference{
		secretReference("s1", "app"),
		nonFileSecretReference(),
	}
	ctr.ConfigReferences = []*swarmtypes.ConfigReference{
		configReference("c1", "app.conf"),
		{ConfigID: "c2", ConfigName: "config-c2", Runtime: &swarmtypes.ConfigReferenceRuntimeTarget{}},
	}
	ctr.DependencyStore = newFakeDependencyStore([]string{"s1"}, []string{"c1"})

	assert.NilError(t, ctr.ValidateSecretConfigReferences())
}

func TestValidateSecretConfigReferencesNone(t *testing.T) {
	ctr := newPrecheckContainer(t)
	// No references at all must succeed even without a dependency store.
	assert.NilError(t, ctr.ValidateSecretConfigReferences())
}

func TestValidateSecretConfigReferencesMissingDependencyStore(t *testing.T) {
	ctr := newPrecheckContainer(t)
	ctr.SecretReferences = []*swarmtypes.SecretReference{secretReference("s1", "app")}

	err := ctr.ValidateSecretConfigReferences()
	assert.ErrorContains(t, err, "dependency store is not initialized")
}

func TestValidateSecretConfigReferencesSecretUnavailable(t *testing.T) {
	ctr := newPrecheckContainer(t)
	ctr.SecretReferences = []*swarmtypes.SecretReference{secretReference("missing", "app")}
	ctr.DependencyStore = newFakeDependencyStore(nil, nil)

	err := ctr.ValidateSecretConfigReferences()
	assert.ErrorContains(t, err, `secret "secret-missing" (id: missing) is not available`)
}

func TestValidateSecretConfigReferencesConfigUnavailable(t *testing.T) {
	ctr := newPrecheckContainer(t)
	ctr.ConfigReferences = []*swarmtypes.ConfigReference{configReference("missing", "app.conf")}
	ctr.DependencyStore = newFakeDependencyStore(nil, nil)

	err := ctr.ValidateSecretConfigReferences()
	assert.ErrorContains(t, err, `config "config-missing" (id: missing) is not available`)
}

func TestValidateSecretConfigReferencesDuplicateSecret(t *testing.T) {
	ctr := newPrecheckContainer(t)
	ctr.SecretReferences = []*swarmtypes.SecretReference{
		secretReference("s1", "first"),
		secretReference("s1", "second"),
	}
	ctr.DependencyStore = newFakeDependencyStore([]string{"s1"}, nil)

	err := ctr.ValidateSecretConfigReferences()
	assert.ErrorContains(t, err, "duplicate mount of secret")
}

func TestValidateSecretConfigReferencesDuplicateConfig(t *testing.T) {
	ctr := newPrecheckContainer(t)
	ctr.ConfigReferences = []*swarmtypes.ConfigReference{
		configReference("c1", "first.conf"),
		configReference("c1", "second.conf"),
	}
	ctr.DependencyStore = newFakeDependencyStore(nil, []string{"c1"})

	err := ctr.ValidateSecretConfigReferences()
	assert.ErrorContains(t, err, "duplicate mount of config")
}

func TestValidateSecretConfigReferencesDuplicateSecretTarget(t *testing.T) {
	ctr := newPrecheckContainer(t)
	ctr.SecretReferences = []*swarmtypes.SecretReference{
		secretReference("s1", "app"),
		secretReference("s2", "app"),
	}
	ctr.DependencyStore = newFakeDependencyStore([]string{"s1", "s2"}, nil)

	err := ctr.ValidateSecretConfigReferences()
	assert.ErrorContains(t, err, "target path")
	assert.ErrorContains(t, err, "conflicts with secret secret-s1")
}

func TestValidateSecretConfigReferencesSecretConfigTargetConflict(t *testing.T) {
	// Absolute targets make the conflict platform independent: both resolve
	// to the exact same in-container path. The volume prefix is required for
	// the path to be considered absolute on Windows.
	absoluteName := containerAbsolutePath("etc", "shared.conf")
	secretRef := secretReference("s1", absoluteName)
	configRef := configReference("c1", absoluteName)
	ctr := newPrecheckContainer(t)
	ctr.SecretReferences = []*swarmtypes.SecretReference{secretRef}
	ctr.ConfigReferences = []*swarmtypes.ConfigReference{configRef}
	ctr.DependencyStore = newFakeDependencyStore([]string{"s1"}, []string{"c1"})

	err := ctr.ValidateSecretConfigReferences()
	assert.ErrorContains(t, err, "target path")
	assert.ErrorContains(t, err, "conflicts with secret")
}

func TestValidateSecretConfigReferencesMountConflict(t *testing.T) {
	ref := secretReference("s1", "app")
	target := getSecretTargetPath(ref)

	for _, tc := range []struct {
		name     string
		dest     string
		contains string
	}{
		{name: "exact", dest: target, contains: "conflicts with"},
		{name: "ancestor shadows secret", dest: filepath.Dir(target), contains: "conflicts with"},
		{name: "descendant under file", dest: filepath.Join(target, "nested", "path"), contains: "conflicts with"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctr := newPrecheckContainer(t)
			ctr.SecretReferences = []*swarmtypes.SecretReference{ref}
			ctr.DependencyStore = newFakeDependencyStore([]string{"s1"}, nil)
			ctr.MountPoints[tc.dest] = &volumemounts.MountPoint{
				Type:        mounttypes.TypeVolume,
				Name:        "vol",
				Destination: tc.dest,
			}

			err := ctr.ValidateSecretConfigReferences()
			assert.ErrorContains(t, err, tc.contains)
			assert.ErrorContains(t, err, "mount destination")
		})
	}
}

func TestValidateSecretConfigReferencesTmpfsConflict(t *testing.T) {
	ref := secretReference("s1", "app")
	target := getSecretTargetPath(ref)

	ctr := newPrecheckContainer(t)
	ctr.SecretReferences = []*swarmtypes.SecretReference{ref}
	ctr.DependencyStore = newFakeDependencyStore([]string{"s1"}, nil)
	ctr.HostConfig.Tmpfs = map[string]string{target: "size=1m,mode=1777"}

	err := ctr.ValidateSecretConfigReferences()
	assert.ErrorContains(t, err, "conflicts with tmpfs mount destination")
}

func TestValidateSecretConfigReferencesMountNoConflict(t *testing.T) {
	// A mount at an unrelated path must not conflict.
	ctr := newPrecheckContainer(t)
	ctr.SecretReferences = []*swarmtypes.SecretReference{secretReference("s1", "app")}
	ctr.DependencyStore = newFakeDependencyStore([]string{"s1"}, nil)
	unrelated := containerAbsolutePath("srv", "data")
	ctr.MountPoints[unrelated] = &volumemounts.MountPoint{
		Type:        mounttypes.TypeBind,
		Destination: unrelated,
	}

	assert.NilError(t, ctr.ValidateSecretConfigReferences())
}

func TestValidateSecretConfigReferencesInvalidOwnership(t *testing.T) {
	for _, tc := range []struct {
		name   string
		uid    string
		gid    string
		errMsg string
	}{
		{name: "non numeric uid", uid: "root", gid: "0", errMsg: "invalid UID"},
		{name: "negative gid", uid: "0", gid: "-1", errMsg: "invalid GID"},
		{name: "empty uid", uid: "", gid: "0", errMsg: "invalid UID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := secretReference("s1", "app")
			ref.File.UID, ref.File.GID = tc.uid, tc.gid
			ctr := newPrecheckContainer(t)
			ctr.SecretReferences = []*swarmtypes.SecretReference{ref}
			ctr.DependencyStore = newFakeDependencyStore([]string{"s1"}, nil)

			err := ctr.ValidateSecretConfigReferences()
			assert.ErrorContains(t, err, tc.errMsg)
		})
	}
}

func TestValidateSecretConfigReferencesFileMode(t *testing.T) {
	// File-type/attribute bits can never apply to a regular secret/config
	// file and must be reported.
	for _, mode := range []os.FileMode{
		os.ModeDir | 0o644,
		os.ModeSymlink,
		os.ModeDevice | 0o600,
		os.ModeNamedPipe,
		os.ModeSocket,
	} {
		ref := secretReference("s1", "app")
		ref.File.Mode = mode
		ctr := newPrecheckContainer(t)
		ctr.SecretReferences = []*swarmtypes.SecretReference{ref}
		ctr.DependencyStore = newFakeDependencyStore([]string{"s1"}, nil)

		err := ctr.ValidateSecretConfigReferences()
		assert.ErrorContains(t, err, "invalid file mode")
	}

	// All plain permission bits are accepted, including setuid/sticky and
	// world-writable modes: the secret/config is bind-mounted read-only, so
	// the mode is informational. This matches TestCreateServiceSecretFileMode
	// and TestCreateServiceConfigFileMode, which use 0o777.
	for _, mode := range []os.FileMode{0o400, 0o444, 0o640, 0o644, 0o666, 0o777, 0o1777, 0o4755} {
		ref := secretReference("s1", "app")
		ref.File.Mode = mode
		ctr := newPrecheckContainer(t)
		ctr.SecretReferences = []*swarmtypes.SecretReference{ref}
		ctr.DependencyStore = newFakeDependencyStore([]string{"s1"}, nil)

		assert.NilError(t, ctr.ValidateSecretConfigReferences(), "mode %#o", uint32(mode))
	}
}

func TestValidateSecretConfigReferencesEmptyName(t *testing.T) {
	ref := configReference("c1", "")
	ctr := newPrecheckContainer(t)
	ctr.ConfigReferences = []*swarmtypes.ConfigReference{ref}
	ctr.DependencyStore = newFakeDependencyStore(nil, []string{"c1"})

	err := ctr.ValidateSecretConfigReferences()
	assert.ErrorContains(t, err, "has an empty file name")
}

func TestMountConflict(t *testing.T) {
	sep := string(filepath.Separator)
	assert.Check(t, mountConflict(filepath.Join(sep, "run", "secrets", "app"), filepath.Join(sep, "run", "secrets", "app")))
	assert.Check(t, mountConflict(filepath.Join(sep, "run", "secrets", "app"), filepath.Join(sep, "run", "secrets")))
	assert.Check(t, mountConflict(filepath.Join(sep, "run", "secrets"), filepath.Join(sep, "run", "secrets", "app")))
	assert.Check(t, !mountConflict(filepath.Join(sep, "run", "secrets", "app"), filepath.Join(sep, "srv", "data")))
	assert.Check(t, is.Equal(pathWithin(filepath.Join(sep, "a"), filepath.Join(sep, "a")), false))
}

// containerAbsolutePath builds an absolute in-container path, including the
// volume prefix on Windows, so that filepath.IsAbs recognizes it on every
// platform.
func containerAbsolutePath(parts ...string) string {
	p := string(filepath.Separator) + filepath.Join(parts...)
	if vol := filepath.VolumeName(os.Getenv("SystemRoot")); vol != "" {
		p = vol + p
	}
	return filepath.Clean(p)
}

// nonFileSecretReference returns a secret reference without a file target,
// which must be skipped by the validation just like it is during setup.
func nonFileSecretReference() *swarmtypes.SecretReference {
	return &swarmtypes.SecretReference{SecretID: "s2", SecretName: "secret-s2"}
}
