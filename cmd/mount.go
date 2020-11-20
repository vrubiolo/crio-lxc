package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
)

func configureMounts(spec *specs.Spec) error {
	// excplicitly disable auto-mounting
	if err := clxc.setConfigItem("lxc.mount.auto", ""); err != nil {
		return err
	}

	for _, ms := range spec.Mounts {
		if ms.Type == "cgroup" {
			// TODO check if hieararchy is cgroup v2 only (unified mode)
			ms.Type = "cgroup2"
			ms.Source = "cgroup2"
			// cgroup filesystem is automounted even with lxc.rootfs.managed = 0
			// from 'man lxc.container.conf':
			// If cgroup namespaces are enabled, then any cgroup auto-mounting request will be ignored,
			// since the container can mount the filesystems itself, and automounting can confuse the container.
		}

		// TODO replace with symlink.FollowSymlinkInScope(filepath.Join(rootfs, "/etc/passwd"), rootfs) ?
		// "github.com/docker/docker/pkg/symlink"
		mountDest, err := resolveMountDestination(spec.Root.Path, ms.Destination)
		// Intermediate path resolution failed. This is not an error, since
		// the remaining directories / files are automatically created (create=dir|file)
		log.Trace().Err(err).Str("dst:", ms.Destination).Str("effective:", mountDest).Msg("resolve mount destination")

		// Check whether the resolved destination of the target link escapes the rootfs.
		if !filepath.HasPrefix(mountDest, spec.Root.Path) {
			// refuses mount destinations that escape from rootfs
			return fmt.Errorf("security violation: resolved mount destination path %s escapes from container root %s", mountDest, spec.Root.Path)
		}
		ms.Destination = mountDest

		err = createMountDestination(spec, &ms)
		if err != nil {
			return errors.Wrapf(err, "failed to create mount destination %s", ms.Destination)
		}

		mnt := fmt.Sprintf("%s %s %s %s", ms.Source, ms.Destination, ms.Type, strings.Join(ms.Options, ","))

		if err := clxc.setConfigItem("lxc.mount.entry", mnt); err != nil {
			return err
		}
	}
	return nil
}

// createMountDestination creates non-existent mount destination paths.
// This is required if rootfs is mounted readonly.
// When the source is a file that should be bind mounted a destination file is created.
// In any other case a target directory is created.
// We add 'create=dir' or 'create=file' to mount options because the mount destination
// may be shadowed by a previous mount. In this case lxc will create the mount destination.
// TODO check whether this is  desired behaviour in lxc ?
// Shouldn't the rootfs should be mounted readonly after all mounts destination directories have been created ?
// https://github.com/lxc/lxc/issues/1702
func createMountDestination(spec *specs.Spec, ms *specs.Mount) error {
	info, err := os.Stat(ms.Source)
	if err != nil && ms.Type == "bind" {
		// check if mountpoint is optional ?
		return errors.Wrapf(err, "failed to access source %s for bind mount", ms.Source)
	}

	if err == nil && !info.IsDir() {
		ms.Options = append(ms.Options, "create=file")
		// source exists and is not a directory
		// create a target file that can be used as target for a bind mount
		err := os.MkdirAll(filepath.Dir(ms.Destination), 0750)
		log.Debug().Err(err).Str("dst:", ms.Destination).Msg("create parent directory for file bind mount")
		if err != nil {
			return errors.Wrap(err, "failed to create mount destination dir")
		}
		f, err := os.OpenFile(ms.Destination, os.O_CREATE, 0440)
		log.Debug().Err(err).Str("dst:", ms.Destination).Msg("create file bind mount destination")
		if err != nil {
			return errors.Wrap(err, "failed to create file mountpoint")
		}
		return f.Close()
	}
	ms.Options = append(ms.Options, "create=dir")
	// FIXME exclude all directories that are below other mounts
	// only directories / files on the readonly rootfs must be created
	err = os.MkdirAll(ms.Destination, 0750)
	log.Debug().Err(err).Str("dst:", ms.Destination).Msg("create mount destination directory")
	if err != nil {
		return errors.Wrap(err, "failed to create mount destination")
	}
	return nil
}

func resolvePathRelative(rootfs string, currentPath string, subPath string) (string, error) {
	log.Trace().Str("current:", currentPath).Str("sub:", subPath).Msg("resolve path relative")
	p := filepath.Join(currentPath, subPath)

	stat, err := os.Lstat(p)
	if err != nil {
		// target does not exist, resolution ends here
		return p, err
	}

	if stat.Mode()&os.ModeSymlink == 0 {
		log.Trace().Str("filepath:", p).Msg("is not a symlink")
		return p, nil
	}
	// resolve symlink

	linkDst, err := os.Readlink(p)
	if err != nil {
		return p, err
	}

	log.Trace().Str("link:", p).Str("dst:", linkDst).Msg("symlink detected")

	// The destination of an absolute link must be prefixed with the rootfs
	if filepath.IsAbs(linkDst) {
		if filepath.HasPrefix(linkDst, rootfs) {
			return p, nil
		}
		return filepath.Join(rootfs, linkDst), nil
	}

	// The link target is relative to currentPath.
	return filepath.Clean(filepath.Join(currentPath, linkDst)), nil
}

// resolveMountDestination resolves mount destination paths for LXC.
//
// Symlinks in mount mount destination paths are not allowed in LXC.
// See CVE-2015-1335: Protect container mounts against symlinks
// and https://github.com/lxc/lxc/commit/592fd47a6245508b79fe6ac819fe6d3b2c1289be
// Mount targets that contain symlinks should be resolved relative to the container rootfs.
// e.g k8s service account tokens are mounted to /var/run/secrets/kubernetes.io/serviceaccount
// but /var/run is (mostly) a symlink to /run, so LXC denies to mount the serviceaccount token.
//
// The mount destination must be either relative to the container root or absolute to
// the directory on the host containing the rootfs.
// LXC simply ignores relative mounts paths to an absolute rootfs.
// See man lxc.container.conf #MOUNT POINTS
//
// The mount option `create=dir` should be set when the error os.ErrNotExist is returned.
// The non-existent directories are then automatically created by LXC.

// source /var/run/containers/storage/overlay-containers/51230afad17aa3b42901f6d9efcba406511821b7e18b2223a6b4c43f9327ce97/userdata/resolv.conf
// destination /etc/resolv.conf
func resolveMountDestination(rootfs string, dst string) (dstPath string, err error) {
	// get path entries
	entries := strings.Split(strings.TrimPrefix(dst, "/"), "/")

	currentPath := rootfs
	// start path resolution at rootfs
	for i, entry := range entries {
		currentPath, err = resolvePathRelative(rootfs, currentPath, entry)
		log.Trace().Err(err).Str("dst:", currentPath).Msg("path resolved")
		if err != nil {
			// The already resolved path is concatenated with the remaining path,
			// if resolution of path fails at some point.
			currentPath = filepath.Join(currentPath, filepath.Join(entries[i+1:]...))
			break
		}
	}
	return currentPath, err
}
