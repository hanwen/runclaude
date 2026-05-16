package main

import (
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	rspec "github.com/opencontainers/runtime-spec/specs-go"
)

func defaultSpec(rootPath string) *rspec.Spec {
	caps := []string{
		"CAP_AUDIT_WRITE",
		"CAP_KILL",
		"CAP_NET_BIND_SERVICE",
	}
	return &rspec.Spec{
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
			Cwd: "/",
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
					"gid=5",
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
				Type:        "sysfs",
				Source:      "sysfs",
				Options: []string{
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
}

func ns(name string) rspec.LinuxNamespace {
	return rspec.LinuxNamespace{
		Type: rspec.LinuxNamespaceType(name),
	}
}

func mainErr() error {
	d := defaultSpec("/")

	dir, err := os.MkdirTemp("", "")
	if err != nil {
		return err
	}

	data, err := json.Marshal(d)
	if err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0644); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	id := "claude-" + cwd
	id = strings.Replace(id, "/", "_", -1)
	cmd := exec.Command("runc", "--rootless=true", "run", "--bundle", dir, id)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	log.Printf("running %v", cmd.Args)
	return cmd.Run()
}

func main() {
	err := mainErr()
	if err != nil {
		log.Fatal(err)
	}

}
