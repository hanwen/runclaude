package main

import (
	"strings"
	"testing"
)

func TestContainmentLeak(t *testing.T) {
	rootfs := "/tmp/rc/rootfs"
	const homeMP = "/tmp/rc/rootfs/home/hanwen"
	const tmpMP = "/tmp/rc/rootfs/tmp"
	const runMP = "/tmp/rc/rootfs/run"
	protected := []string{homeMP, tmpMP, runMP}

	// Cache is on btrfs subvol /hanwen-cache; protected mountpoints map into
	// that subtree.
	cacheHome := mountEntry{6057, 1, "0:37", "/hanwen-cache/rc/abc/home", homeMP}
	cacheTmp := mountEntry{6058, 1, "0:37", "/hanwen-cache/rc/abc/tmp", tmpMP}
	cacheRun := mountEntry{6059, 1, "0:37", "/hanwen-cache/rc/abc/run", runMP}

	// Other mounts on the same btrfs that are disjoint from the cache.
	siblingSubvol := mountEntry{6100, 1, "0:37", "/var-lib-containers", "/tmp/rc/rootfs/var/lib/containers"}
	differentDevice := mountEntry{6101, 1, "252:1", "/", "/tmp/rc/rootfs/etc"}

	// Outside-rootfs mounts must be ignored: only mounts inside the
	// container's view can leak into it.
	outsideRootfsSubvolRoot := mountEntry{5988, 1, "0:37", "/", "/mnt/data"}

	// In-rootfs mount whose root-in-fs is "/" (whole btrfs) — exposes home.
	leakSubvolRoot := mountEntry{6200, 1, "0:37", "/", "/tmp/rc/rootfs/mnt/data"}

	// In-rootfs mount whose root-in-fs is a strict prefix of the cache
	// path — exposes home (e.g. host bind-mounts the user's whole .cache).
	leakAncestor := mountEntry{6201, 1, "0:37", "/hanwen-cache", "/tmp/rc/rootfs/exposed"}

	// Same dev, same prefix, but the protected mount itself: must be
	// skipped via mountID match, not flagged.

	for _, tc := range []struct {
		name    string
		entries []mountEntry
		wantErr string
	}{
		{
			name:    "clean: only protected + disjoint mounts",
			entries: []mountEntry{cacheHome, cacheTmp, cacheRun, siblingSubvol, differentDevice},
			wantErr: "",
		},
		{
			name:    "outside rootfs ignored",
			entries: []mountEntry{cacheHome, cacheTmp, cacheRun, outsideRootfsSubvolRoot},
			wantErr: "",
		},
		{
			name:    "alias: subvol root exposed inside rootfs",
			entries: []mountEntry{cacheHome, cacheTmp, cacheRun, leakSubvolRoot},
			wantErr: `aliases protected mount "/tmp/rc/rootfs/home/hanwen"`,
		},
		{
			name:    "alias: ancestor of cache root exposed inside rootfs",
			entries: []mountEntry{cacheHome, cacheTmp, cacheRun, leakAncestor},
			wantErr: `aliases protected mount "/tmp/rc/rootfs/home/hanwen"`,
		},
		{
			name: "alias of /tmp via subvol root",
			entries: []mountEntry{
				cacheHome, cacheTmp, cacheRun,
				{6300, 1, "0:37", "/", "/tmp/rc/rootfs/srv/data"},
			},
			wantErr: `aliases protected mount`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := containmentLeak(tc.entries, rootfs, protected)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// Repeated bind onto the same mountpoint shadows the earlier mount: the
// containment check must consult the latest entry's (dev, fs-root).
func TestContainmentLeakUsesLatestProtectedMount(t *testing.T) {
	rootfs := "/r"
	homeMP := "/r/home/hanwen"
	entries := []mountEntry{
		// Initial bind, will be shadowed by the second one.
		{1, 0, "0:37", "/old/home", homeMP},
		// Effective bind: /tmp/cache subvol.
		{2, 0, "0:37", "/cache/home", homeMP},
		// Aliases the *latest* effective home (shares prefix /cache).
		{3, 0, "0:37", "/cache", "/r/exposed"},
	}
	err := containmentLeak(entries, rootfs, []string{homeMP})
	if err == nil {
		t.Fatal("expected leak via /cache prefix, got nil")
	}
	if !strings.Contains(err.Error(), `aliases protected mount`) {
		t.Errorf("unexpected error: %v", err)
	}
}

// A protected mountpoint that isn't present in mountinfo is a programmer
// error — the caller passed a path that wasn't actually mounted.
func TestContainmentLeakMissingProtected(t *testing.T) {
	err := containmentLeak(nil, "/r", []string{"/r/home"})
	if err == nil || !strings.Contains(err.Error(), "not present in mountinfo") {
		t.Fatalf("expected missing-mount error, got: %v", err)
	}
}
