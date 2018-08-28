// Copyright 2018 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package container

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.googlesource.com/gvisor/pkg/log"
	"gvisor.googlesource.com/gvisor/runsc/boot"
	"gvisor.googlesource.com/gvisor/runsc/specutils"
)

type mapping struct {
	set bool
	val uint32
}

var optionsMap = map[string]mapping{
	"acl":           {set: true, val: syscall.MS_POSIXACL},
	"async":         {set: false, val: syscall.MS_SYNCHRONOUS},
	"atime":         {set: false, val: syscall.MS_NOATIME},
	"bind":          {set: true, val: syscall.MS_BIND},
	"defaults":      {set: true, val: 0},
	"dev":           {set: false, val: syscall.MS_NODEV},
	"diratime":      {set: false, val: syscall.MS_NODIRATIME},
	"dirsync":       {set: true, val: syscall.MS_DIRSYNC},
	"exec":          {set: false, val: syscall.MS_NOEXEC},
	"iversion":      {set: true, val: syscall.MS_I_VERSION},
	"loud":          {set: false, val: syscall.MS_SILENT},
	"mand":          {set: true, val: syscall.MS_MANDLOCK},
	"noacl":         {set: false, val: syscall.MS_POSIXACL},
	"noatime":       {set: true, val: syscall.MS_NOATIME},
	"nodev":         {set: true, val: syscall.MS_NODEV},
	"nodiratime":    {set: true, val: syscall.MS_NODIRATIME},
	"noexec":        {set: true, val: syscall.MS_NOEXEC},
	"noiversion":    {set: false, val: syscall.MS_I_VERSION},
	"nomand":        {set: false, val: syscall.MS_MANDLOCK},
	"norelatime":    {set: false, val: syscall.MS_RELATIME},
	"nostrictatime": {set: false, val: syscall.MS_STRICTATIME},
	"nosuid":        {set: true, val: syscall.MS_NOSUID},
	"private":       {set: true, val: syscall.MS_PRIVATE},
	"rbind":         {set: true, val: syscall.MS_BIND | syscall.MS_REC},
	"relatime":      {set: true, val: syscall.MS_RELATIME},
	"remount":       {set: true, val: syscall.MS_REMOUNT},
	"ro":            {set: true, val: syscall.MS_RDONLY},
	"rprivate":      {set: true, val: syscall.MS_PRIVATE | syscall.MS_REC},
	"rw":            {set: false, val: syscall.MS_RDONLY},
	"silent":        {set: true, val: syscall.MS_SILENT},
	"strictatime":   {set: true, val: syscall.MS_STRICTATIME},
	"suid":          {set: false, val: syscall.MS_NOSUID},
	"sync":          {set: true, val: syscall.MS_SYNCHRONOUS},
}

// setupFS creates the container directory structure under 'spec.Root.Path'.
// This allows the gofer serving the containers to be chroot under this
// directory to create an extra layer to security in case the gofer gets
// compromised.
func setupFS(spec *specs.Spec, conf *boot.Config, bundleDir string) error {
	for _, m := range spec.Mounts {
		if m.Type != "bind" || !specutils.IsSupportedDevMount(m) {
			continue
		}
		src := m.Source
		if !filepath.IsAbs(src) {
			src = filepath.Join(bundleDir, src)
		}
		srcfi, err := os.Stat(src)
		if err != nil {
			return fmt.Errorf("failed to stat() mount source: %v", err)
		}

		// It's possible that 'm.Destination' follows symlinks inside the
		// container.
		dst, err := resolveSymlinks(spec.Root.Path, m.Destination)
		if err != nil {
			return fmt.Errorf("failed to resolve symlinks: %v", err)
		}

		// Create mount point if it doesn't exits
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			if srcfi.IsDir() {
				if err := os.MkdirAll(dst, 0755); err != nil {
					return fmt.Errorf("failed to make mount directory %q: %v", dst, err)
				}
			} else {
				if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
					return fmt.Errorf("failed to make mount directory for file %q: %v", filepath.Dir(dst), err)
				}
				f, err := os.OpenFile(dst, os.O_CREATE, 0755)
				if err != nil {
					return fmt.Errorf("failed to open mount file %q: %v", dst, err)
				}
				f.Close()
			}
		}

		flags := optionsToFlags(m.Options)
		flags |= syscall.MS_BIND
		log.Infof("Mounting src: %q, dst: %q, flags: %#x", src, dst, flags)
		if err := syscall.Mount(src, dst, m.Type, uintptr(flags), ""); err != nil {
			return fmt.Errorf("failed to mount src: %q, dst: %q, flags: %#x, err: %v", src, dst, flags, err)
		}
	}

	// Remount root as readonly after setup is done, if requested.
	if spec.Root.Readonly {
		log.Infof("Remounting root as readonly: %q", spec.Root.Path)
		flags := uintptr(syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_RDONLY | syscall.MS_REC)
		src := spec.Root.Path
		if !filepath.IsAbs(src) {
			src = filepath.Join(bundleDir, src)
		}
		if err := syscall.Mount(src, src, "bind", flags, ""); err != nil {
			return fmt.Errorf("failed to remount root as readonly with source: %q, target: %q, flags: %#x, err: %v", spec.Root.Path, spec.Root.Path, flags, err)
		}
	}
	return nil
}

// resolveSymlinks walks 'rel' having 'root' as the root directory. If there are
// symlinks, they are evaluated relative to 'root' to ensure the end result is
// the same as if the process was running inside the container.
func resolveSymlinks(root, rel string) (string, error) {
	return resolveSymlinksImpl(root, root, rel, 255)
}

func resolveSymlinksImpl(root, base, rel string, followCount uint) (string, error) {
	if followCount == 0 {
		return "", fmt.Errorf("too many symlinks to follow, path: %q", filepath.Join(base, rel))
	}

	rel = filepath.Clean(rel)
	for _, name := range strings.Split(rel, string(filepath.Separator)) {
		if name == "" {
			continue
		}
		// Note that Join() resolves things like ".." and returns a clean path.
		path := filepath.Join(base, name)
		if !strings.HasPrefix(path, root) {
			// One cannot '..' their way out of root.
			path = root
			continue
		}
		fi, err := os.Lstat(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return "", err
			}
			// Not found means there is no symlink to check. Just keep walking dirs.
			base = path
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return "", err
			}
			if filepath.IsAbs(link) {
				base = root
			}
			base, err = resolveSymlinksImpl(root, base, link, followCount-1)
			if err != nil {
				return "", err
			}
			continue
		}
		base = path
	}
	return base, nil
}

func optionsToFlags(opts []string) uint32 {
	var rv uint32
	for _, opt := range opts {
		if m, ok := optionsMap[opt]; ok {
			if m.set {
				rv |= m.val
			} else {
				rv ^= m.val
			}
		} else {
			log.Warningf("Ignoring mount option %q", opt)
		}
	}
	return rv
}
