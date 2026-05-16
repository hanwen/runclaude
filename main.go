package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	rspec "github.com/opencontainers/runtime-spec/specs-go"
)

func defaultSpec(rootPath, cwd string) *rspec.Spec {
	caps := []string{
		"CAP_AUDIT_WRITE",
		"CAP_KILL",
		"CAP_NET_BIND_SERVICE",
	}
	spec := &rspec.Spec{
		Version: "1.2.1",
		Process: &rspec.Process{
			Terminal: true,

			User: rspec.User{
				UID: 0,
				GID: 0,
			},
			Args: []string{
				"sh",
			},
			Env: []string{
				"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
				"TERM=xterm",
			},
			Cwd: cwd,
			Capabilities: &rspec.LinuxCapabilities{
				Bounding:  caps,
				Effective: caps,
				Permitted: caps,
			},
			Rlimits: []rspec.POSIXRlimit{
				{Type: "RLIMIT_NOFILE",
					Hard: 1024,
					Soft: 1024},
			},
			NoNewPrivileges: true,
		},
		Root: &rspec.Root{
			Path:     rootPath,
			Readonly: true,
		},
		Hostname: "runclaude",
		Mounts: []rspec.Mount{
			{
				Destination: "/proc",
				Type:        "proc",
				Source:      "proc",
			},
			{
				Destination: "/dev",
				Type:        "tmpfs",
				Source:      "tmpfs",
				Options: []string{
					"nosuid",
					"strictatime",
					"mode=755",
					"size=65536k",
				},
			},
			{
				Destination: "/dev/pts",
				Type:        "devpts",
				Source:      "devpts",
				Options: []string{
					"nosuid",
					"noexec",
					"newinstance",
					"ptmxmode=0666",
					"mode=0620",
					"gid=0",
				},
			},
			{
				Destination: "/dev/shm",
				Type:        "tmpfs",
				Source:      "shm",
				Options: []string{
					"nosuid",
					"noexec",
					"nodev",
					"mode=1777",
					"size=65536k",
				},
			},
			{
				Destination: "/dev/mqueue",
				Type:        "mqueue",
				Source:      "mqueue",
				Options: []string{
					"nosuid",
					"noexec",
					"nodev",
				},
			},
			{
				Destination: "/sys",
				Type:        "none",
				Source:      "/sys",
				Options: []string{
					"rbind",
					"nosuid",
					"noexec",
					"nodev",
					"ro",
				},
			},
			{
				Destination: "/sys/fs/cgroup",
				Type:        "cgroup",
				Source:      "cgroup",
				Options: []string{
					"nosuid",
					"noexec",
					"nodev",
					"relatime",
					"ro",
				},
			}},
		Linux: &rspec.Linux{
			UIDMappings: []rspec.LinuxIDMapping{
				{ContainerID: 0, HostID: 0, Size: 1},
			},
			GIDMappings: []rspec.LinuxIDMapping{
				{ContainerID: 0, HostID: 0, Size: 1},
			},
			Resources: &rspec.LinuxResources{
				Devices: []rspec.LinuxDeviceCgroup{
					{Allow: false,
						Access: "rwm",
					}},
			},
			Namespaces: []rspec.LinuxNamespace{
				ns("pid"),
				//			ns("network"),
				ns("ipc"),
				ns("uts"),
				ns("mount"),
				ns("cgroup"),
				ns("user"),
			},
			MaskedPaths: []string{
				"/proc/acpi",
				"/proc/asound",
				"/proc/kcore",
				"/proc/keys",
				"/proc/latency_stats",
				"/proc/timer_list",
				"/proc/timer_stats",
				"/proc/sched_debug",
				"/sys/firmware",
				"/proc/scsi",
			},
			ReadonlyPaths: []string{
				"/proc/bus",
				"/proc/fs",
				"/proc/irq",
				"/proc/sys",
				"/proc/sysrq-trigger",
			},
		}}

	return spec
}

func ns(name string) rspec.LinuxNamespace {
	return rspec.LinuxNamespace{
		Type: rspec.LinuxNamespaceType(name),
	}
}

func mainErr() error {
	dir, err := os.MkdirTemp("", "")
	if err != nil {
		return err
	}

	rootfs := filepath.Join(dir, "rootfs")
	if err := os.Mkdir(rootfs, 0755); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	bindDirs := []string{cwd}
	for _, a := range os.Args[1:] {
		abs, err := filepath.Abs(a)
		if err != nil {
			return err
		}
		bindDirs = append(bindDirs, abs)
	}

	d := defaultSpec(rootfs, cwd)

	data, err := json.Marshal(d)
	if err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0644); err != nil {
		return err
	}
	id := "claude-" + cwd
	id = strings.ReplaceAll(id, "/", "_")
	var b strings.Builder
	fmt.Fprintf(&b, "set -e\n")
	fmt.Fprintf(&b, "mount --rbind / %q\n", rootfs)
	fmt.Fprintf(&b, "mount --make-rslave %q\n", rootfs)
	homeInRoot := filepath.Join(rootfs, home)
	fmt.Fprintf(&b, "mkdir -p %q\n", homeInRoot)
	fmt.Fprintf(&b, "mount -t tmpfs -o nosuid,nodev,mode=755 tmpfs %q\n", homeInRoot)
	tmpInRoot := filepath.Join(rootfs, "tmp")
	fmt.Fprintf(&b, "mkdir -p %q\n", tmpInRoot)
	fmt.Fprintf(&b, "mount -t tmpfs -o nosuid,nodev,mode=1777 tmpfs %q\n", tmpInRoot)
	for _, d := range bindDirs {
		dest := filepath.Join(rootfs, d)
		fmt.Fprintf(&b, "mkdir -p %q\n", dest)
		fmt.Fprintf(&b, "mount --rbind %q %q\n", d, dest)
	}
	fmt.Fprintf(&b, "exec runc --rootless=true run --bundle %q %q\n", dir, id)
	cmd := exec.Command("unshare", "--user", "--map-root-user", "--mount", "sh", "-c", b.String())
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	log.Printf("running %v", cmd.Args)
	return cmd.Run()
}

func main() {
	err := mainErr()
	if err != nil {
		log.Fatal(err)
	}

}
