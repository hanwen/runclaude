package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	rspec "github.com/opencontainers/runtime-spec/specs-go"
)

const childEnv = "_RUNCLAUDE_CHILD"

func defaultSpec(rootPath, cwd string, uid, gid uint32) *rspec.Spec {
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
				UID: uid,
				GID: gid,
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
				{ContainerID: 0, HostID: 0, Size: uid},
				{ContainerID: uid, HostID: uid, Size: 1},
			},
			GIDMappings: []rspec.LinuxIDMapping{
				{ContainerID: 0, HostID: 0, Size: gid},
				{ContainerID: gid, HostID: gid, Size: 1},
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

	uid := uint32(os.Getuid())
	gid := uint32(os.Getgid())
	d := defaultSpec(rootfs, cwd, uid, gid)

	data, err := json.Marshal(d)
	if err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0644); err != nil {
		return err
	}
	id := "claude-" + cwd
	id = strings.ReplaceAll(id, "/", "_")

	self, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{
		"--user", "--map-current-user", "--map-auto", "--keep-caps", "--mount",
		"--", self, "--bundle", dir, "--rootfs", rootfs, "--home", home, "--id", id,
	}
	for _, d := range bindDirs {
		args = append(args, "--bind", d)
	}
	cmd := exec.Command("unshare", args...)
	cmd.Env = append(os.Environ(), childEnv+"=1")
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	log.Printf("running %v", cmd.Args)
	return cmd.Run()
}

func childMain() error {
	var bundle, rootfs, home, id string
	var binds []string
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--bundle":
			i++
			bundle = args[i]
		case "--rootfs":
			i++
			rootfs = args[i]
		case "--home":
			i++
			home = args[i]
		case "--id":
			i++
			id = args[i]
		case "--bind":
			i++
			binds = append(binds, args[i])
		}
	}

	if data, err := os.ReadFile("/proc/self/uid_map"); err == nil {
		log.Printf("child uid_map: %s", data)
	}
	if data, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "Cap") || strings.HasPrefix(line, "Uid") {
				log.Printf("child %s", line)
			}
		}
	}
	if err := syscall.Mount("/", rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("rbind / -> %s: %w", rootfs, err)
	}
	if err := syscall.Mount("", rootfs, "", syscall.MS_SLAVE|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("make-rslave %s: %w", rootfs, err)
	}
	homeInRoot := filepath.Join(rootfs, home)
	if err := os.MkdirAll(homeInRoot, 0755); err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", homeInRoot, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=755"); err != nil {
		return fmt.Errorf("tmpfs %s: %w", homeInRoot, err)
	}
	tmpInRoot := filepath.Join(rootfs, "tmp")
	if err := os.MkdirAll(tmpInRoot, 0755); err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", tmpInRoot, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=1777"); err != nil {
		return fmt.Errorf("tmpfs %s: %w", tmpInRoot, err)
	}
	for _, d := range binds {
		dest := filepath.Join(rootfs, d)
		if err := os.MkdirAll(dest, 0755); err != nil {
			return err
		}
		if err := syscall.Mount(d, dest, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("rbind %s -> %s: %w", d, dest, err)
		}
	}

	runc, err := exec.LookPath("runc")
	if err != nil {
		return err
	}
	return syscall.Exec(runc, []string{"runc", "--rootless=true", "run", "--bundle", bundle, id}, os.Environ())
}

func main() {
	var err error
	if os.Getenv(childEnv) != "" {
		err = childMain()
	} else {
		err = mainErr()
	}
	if err != nil {
		log.Fatal(err)
	}
}
