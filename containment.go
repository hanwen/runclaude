package main

import (
	"fmt"
	"os"
)

// checkContainment refuses to start the sandbox when any mount inside rootfs
// re-exposes a protected mount via aliasing on the same backing filesystem.
//
// The threat: a btrfs (or any) volume that backs an in-container mount (e.g.
// $HOME, /tmp, /run) can also be mounted somewhere else in the host tree
// (e.g. /mnt/data is subvol=/ of the same btrfs whose subvol=/cache/.../home
// is bound at $HOME). A naive recursive bind of "everything under /" then
// plants an alias inside the container at <rootfs>/mnt/data/cache/.../home,
// letting the sandboxed process read the contents that the protected mount
// was supposed to hide or replace.
//
// Two mounts alias each other (or one contains the other) when they share
// a (major:minor) device and one's root-in-filesystem path is a prefix of
// the other's. Disjoint subvolumes on the same block device do NOT fire.
func checkContainment(rootfs string, protectedMountPoints []string) error {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("open mountinfo: %w", err)
	}
	defer f.Close()
	entries, err := parseMountinfo(f)
	if err != nil {
		return fmt.Errorf("parse mountinfo: %w", err)
	}
	return containmentLeak(entries, rootfs, protectedMountPoints)
}

func containmentLeak(entries []mountEntry, rootfs string, protectedMountPoints []string) error {
	type prot struct {
		dev      string
		rootInFS string
		mp       string
	}
	protectedByID := map[int]bool{}
	var prots []prot
	for _, want := range protectedMountPoints {
		// Last entry wins: a later bind onto the same mountpoint shadows
		// the earlier one, so its (dev, rootInFS) is what's actually live.
		var found *mountEntry
		for i := range entries {
			if entries[i].mountPoint == want {
				e := entries[i]
				found = &e
			}
		}
		if found == nil {
			return fmt.Errorf("containment check: protected mount %q not present in mountinfo", want)
		}
		protectedByID[found.mountID] = true
		prots = append(prots, prot{found.dev, found.rootInFS, found.mountPoint})
	}
	for _, e := range entries {
		if !pathContains(rootfs, e.mountPoint) {
			continue
		}
		if protectedByID[e.mountID] {
			continue
		}
		for _, p := range prots {
			if e.dev != p.dev {
				continue
			}
			if pathContains(e.rootInFS, p.rootInFS) {
				return fmt.Errorf(
					"refusing to start: mount %q (dev %s, fs-root %q) aliases protected mount %q (fs-root %q); "+
						"the host volume that backs %q is also exposed at %q, which would leak %q's contents into the sandbox. "+
						"Remove the offending bind, or move %q off the shared volume.",
					e.mountPoint, e.dev, e.rootInFS, p.mp, p.rootInFS, p.mp, e.mountPoint, p.mp, p.mp)
			}
		}
	}
	return nil
}
