package container

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pkg/errors"
)

// ValidateSecretConfigReferences performs a read-only preflight validation of
// the container's swarm secret and config references before the container is
// started and any mount is created. It does not create, mount or otherwise
// mutate anything on disk, which lets the caller reject invalid configurations
// without leaving partially created mounts behind.
//
// The following conditions are reported:
//
//   - duplicate references to the same secret or config,
//   - file targets that resolve to the same in-container path, including
//     targets that collide with user-provided volume, bind-mount or tmpfs
//     destinations,
//   - invalid file modes, UIDs or GIDs,
//   - referenced secrets or configs that are missing from the dependency
//     store populated by the swarmkit adapter.
func (container *Container) ValidateSecretConfigReferences() error {
	if len(container.SecretReferences) == 0 && len(container.ConfigReferences) == 0 {
		return nil
	}

	// The dependency store holds the actual secret/config payloads and is
	// attached by the swarmkit executor adapter together with the references.
	// It must be present before anything is mounted, otherwise the injection
	// would fail halfway through setting up the secrets tmpfs.
	if container.DependencyStore == nil {
		return errors.New("secret/config dependency store is not initialized")
	}

	secretIDs := make(map[string]struct{}, len(container.SecretReferences))
	configIDs := make(map[string]struct{}, len(container.ConfigReferences))
	// targets maps the cleaned in-container destination path to a human
	// readable description of the reference that owns it.
	targets := make(map[string]string)

	for _, ref := range container.SecretReferences {
		// Only file targets are materialized on disk. Other target types
		// are not supported today and are skipped, matching the behavior of
		// the setup code.
		if ref.File == nil {
			continue
		}

		if _, exists := secretIDs[ref.SecretID]; exists {
			return errors.Errorf("duplicate mount of secret %q (id: %s)", ref.SecretName, ref.SecretID)
		}
		secretIDs[ref.SecretID] = struct{}{}

		if err := validateReferenceFileTarget("secret", ref.SecretName, ref.File.Name, ref.File.UID, ref.File.GID, ref.File.Mode); err != nil {
			return err
		}

		target := getSecretTargetPath(ref)
		if err := container.validateReferenceTargetPath("secret", ref.SecretName, target, targets); err != nil {
			return err
		}

		secret, err := container.DependencyStore.Secrets().Get(ref.SecretID)
		if err != nil {
			return errors.Wrapf(err, "secret %q (id: %s) is not available", ref.SecretName, ref.SecretID)
		}
		if secret == nil {
			return errors.Errorf("secret %q (id: %s) is not available", ref.SecretName, ref.SecretID)
		}
	}

	for _, ref := range container.ConfigReferences {
		// Runtime configs are not mounted into the container, so they have
		// no file target to validate.
		if ref.File == nil {
			continue
		}

		if _, exists := configIDs[ref.ConfigID]; exists {
			return errors.Errorf("duplicate mount of config %q (id: %s)", ref.ConfigName, ref.ConfigID)
		}
		configIDs[ref.ConfigID] = struct{}{}

		if err := validateReferenceFileTarget("config", ref.ConfigName, ref.File.Name, ref.File.UID, ref.File.GID, ref.File.Mode); err != nil {
			return err
		}

		target := getConfigTargetPath(ref)
		if err := container.validateReferenceTargetPath("config", ref.ConfigName, target, targets); err != nil {
			return err
		}

		config, err := container.DependencyStore.Configs().Get(ref.ConfigID)
		if err != nil {
			return errors.Wrapf(err, "config %q (id: %s) is not available", ref.ConfigName, ref.ConfigID)
		}
		if config == nil {
			return errors.Errorf("config %q (id: %s) is not available", ref.ConfigName, ref.ConfigID)
		}
	}

	return nil
}

// validateReferenceFileTarget validates the in-container file name, permission
// mode and ownership of a secret/config file target. It mirrors the UID/GID
// parsing and chmod done while injecting the files, but runs before any mount
// is created so that a bad value cannot leave a partially populated secrets
// directory behind.
func validateReferenceFileTarget(kind, name, fileName, uid, gid string, mode os.FileMode) error {
	if fileName == "" {
		return errors.Errorf("%s %q has an empty file name", kind, name)
	}
	if err := validateReferenceFileMode(kind, name, mode); err != nil {
		return err
	}
	if err := validateReferenceID(kind, name, "UID", uid); err != nil {
		return err
	}
	if err := validateReferenceID(kind, name, "GID", gid); err != nil {
		return err
	}
	return nil
}

// invalidReferenceFileModeBits are the os.FileMode bits that describe file
// types and special attributes rather than permissions. A secret/config target
// is always a regular file created by the daemon, so accepting these bits
// would silently drop them when the mode is applied and usually indicates a
// malformed request. Plain permission bits (including setuid/setgid/sticky and
// world-writable bits, which are harmless on the read-only bind mount) are
// accepted.
const invalidReferenceFileModeBits = os.ModeDir |
	os.ModeAppend |
	os.ModeExclusive |
	os.ModeTemporary |
	os.ModeSymlink |
	os.ModeDevice |
	os.ModeNamedPipe |
	os.ModeSocket |
	os.ModeCharDevice |
	os.ModeIrregular

// validateReferenceFileMode rejects file modes carrying file-type or
// attribute bits that cannot apply to a regular secret/config file.
func validateReferenceFileMode(kind, name string, mode os.FileMode) error {
	if mode&invalidReferenceFileModeBits != 0 {
		return errors.Errorf("%s %q has invalid file mode %#o: only permission bits are allowed", kind, name, uint32(mode))
	}
	return nil
}

// validateReferenceID validates a UID/GID value, rejecting empty, negative and
// otherwise non-numeric values. Atoi accepts negative numbers, which are not
// valid ownership specifications.
func validateReferenceID(kind, name, field, value string) error {
	id, err := strconv.Atoi(value)
	if err != nil || id < 0 {
		return errors.Errorf("%s %q has invalid %s %q: must be a non-negative integer", kind, name, field, value)
	}
	return nil
}

// validateReferenceTargetPath ensures the in-container target of a secret or
// config does not collide with another secret/config target or with any of the
// container's user-provided mounts (volumes, bind mounts and tmpfs mounts).
func (container *Container) validateReferenceTargetPath(kind, name, target string, targets map[string]string) error {
	target = filepath.Clean(target)
	description := kind + " " + name

	if other, exists := targets[target]; exists {
		return errors.Errorf("%s target path %q conflicts with %s", description, target, other)
	}

	for dest, mountPoint := range container.MountPoints {
		if mountConflict(target, filepath.Clean(dest)) {
			return errors.Errorf("%s target path %q conflicts with %s mount destination %q", description, target, mountPoint.Type, dest)
		}
	}

	if container.HostConfig != nil {
		for dest := range container.HostConfig.Tmpfs {
			if mountConflict(target, filepath.Clean(dest)) {
				return errors.Errorf("%s target path %q conflicts with tmpfs mount destination %q", description, target, dest)
			}
		}
	}

	targets[target] = description
	return nil
}

// mountConflict reports whether a secret/config file target and a user-provided
// mount destination cannot coexist. Exact matches conflict (a file cannot share
// its path with a mount), and so do ancestor/descendant pairs: a mount above
// the target would shadow the secret, while a mount below a file target could
// never be created because its parent is a regular file.
func mountConflict(target, dest string) bool {
	if target == dest {
		return true
	}
	return pathWithin(target, dest) || pathWithin(dest, target)
}

// pathWithin reports whether child is located strictly below base in the same
// path hierarchy. Both arguments must be cleaned.
func pathWithin(child, base string) bool {
	rel, err := filepath.Rel(base, child)
	if err != nil || rel == "." {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
