package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

const (
	prSetNoNewPrivs      = 38
	prCapBsetDrop        = 24
	prCapAmbient         = 47
	prCapAmbientClearAll = 4
)

func dropAllCaps() error {
	if _, _, e := syscall.Syscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); e != 0 {
		return fmt.Errorf("PR_SET_NO_NEW_PRIVS: %w", e)
	}
	if _, _, e := syscall.Syscall6(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientClearAll, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("PR_CAP_AMBIENT_CLEAR_ALL: %w", e)
	}
	for i := uintptr(0); i < 64; i++ {
		// EINVAL means cap doesn't exist; stop iterating.
		if _, _, e := syscall.Syscall(syscall.SYS_PRCTL, prCapBsetDrop, i, 0); e == syscall.EINVAL {
			break
		}
	}
	type header struct {
		version uint32
		pid     int32
	}
	type data struct {
		effective, permitted, inheritable uint32
	}
	hdr := header{version: 0x20080522} // _LINUX_CAPABILITY_VERSION_3
	var d [2]data
	if _, _, e := syscall.Syscall(syscall.SYS_CAPSET,
		uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&d[0])), 0); e != 0 {
		return fmt.Errorf("capset: %w", e)
	}
	return nil
}

const (
	childEnv = "_RUNCLAUDE_CHILD"
	initEnv  = "_RUNCLAUDE_INIT"
)

type Config struct {
	Rootfs          string   `json:"rootfs"`
	Home            string   `json:"home"`
	CacheDir        string   `json:"cacheDir"`
	Cwd             string   `json:"cwd"`
	Binds           []string `json:"binds"`
	BashArgs        []string `json:"bashArgs"`
	RestrictNet     bool     `json:"restrictNet"`
	ExposeLocalhost bool     `json:"exposeLocalhost"`
}

func loadConfig(envName string) (*Config, error) {
	var c Config
	if err := json.Unmarshal([]byte(os.Getenv(envName)), &c); err != nil {
		return nil, fmt.Errorf("decode %s: %w", envName, err)
	}
	return &c, nil
}

func encodeConfig(c *Config) string {
	data, err := json.Marshal(c)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func claudeBinds(home string) ([]string, error) {
	binds := []string{
		filepath.Join(home, ".claude"),
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".config", "claude"),
	}
	claude, err := exec.LookPath("claude")
	if err == nil {
		binds = append(binds, filepath.Dir(claude))
		if target, err := filepath.EvalSymlinks(claude); err == nil {
			binds = append(binds, filepath.Dir(target))
		}
	}
	var out []string
	for _, b := range binds {
		if _, err := os.Stat(b); err == nil {
			out = append(out, b)
		}
	}
	return out, nil
}

func mainErr() error {
	dir, err := os.MkdirTemp("", "runclaude-")
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
	if !filepath.IsAbs(home) || strings.Trim(home, "/") == "" {
		return fmt.Errorf("$HOME must be absolute and not %q, got %q", "/", home)
	}

	claudeMode := flag.Bool("claude", false, "bind files needed for `claude` and run it as the shell command")
	restrictNet := flag.Bool("restrict-net", true, "run in a new network namespace with pasta providing user-mode networking")
	exposeLocalhost := flag.Bool("expose-localhost", true, "expose host listening ports inside the netns (requires --restrict-net)")
	flag.Parse()

	sum := sha256.Sum256([]byte(cwd))
	cacheBase, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	cacheDir := filepath.Join(cacheBase, "runclaude", hex.EncodeToString(sum[:])[:16])
	for _, sub := range []string{"home", "tmp", "run"} {
		if err := os.MkdirAll(filepath.Join(cacheDir, sub), 0700); err != nil {
			return err
		}
	}

	cfg := &Config{
		Rootfs:          rootfs,
		Home:            home,
		CacheDir:        cacheDir,
		Cwd:             cwd,
		Binds:           []string{cwd},
		RestrictNet:     *restrictNet,
		ExposeLocalhost: *exposeLocalhost,
	}
	for _, a := range flag.Args() {
		abs, err := filepath.Abs(a)
		if err != nil {
			return err
		}
		cfg.Binds = append(cfg.Binds, abs)
	}
	if *claudeMode {
		extra, err := claudeBinds(home)
		if err != nil {
			return err
		}
		cfg.Binds = append(cfg.Binds, extra...)
		cfg.BashArgs = []string{"-c", "claude --dangerously-skip-permissions"}
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command("unshare",
		"--user", "--map-current-user", "--map-auto", "--keep-caps", "--mount",
		"--", self,
	)
	cmd.Env = append(os.Environ(), childEnv+"="+encodeConfig(cfg))
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func setupDev(rootfs string) error {
	dev := filepath.Join(rootfs, "dev")
	if err := os.MkdirAll(dev, 0755); err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", dev, "tmpfs",
		syscall.MS_NOSUID|syscall.MS_STRICTATIME, "mode=755"); err != nil {
		return fmt.Errorf("tmpfs %s: %w", dev, err)
	}

	for _, name := range []string{"null", "zero", "full", "random", "urandom", "tty"} {
		src := "/dev/" + name
		dst := filepath.Join(dev, name)
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0666)
		if err != nil {
			return err
		}
		f.Close()
		if err := syscall.Mount(src, dst, "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind %s: %w", src, err)
		}
	}

	pts := filepath.Join(dev, "pts")
	if err := os.Mkdir(pts, 0755); err != nil {
		return err
	}
	if err := syscall.Mount("devpts", pts, "devpts",
		syscall.MS_NOSUID|syscall.MS_NOEXEC,
		"newinstance,ptmxmode=0666,mode=0620"); err != nil {
		return fmt.Errorf("devpts: %w", err)
	}

	shm := filepath.Join(dev, "shm")
	if err := os.Mkdir(shm, 0755); err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", shm, "tmpfs",
		syscall.MS_NOSUID|syscall.MS_NOEXEC|syscall.MS_NODEV, "mode=1777"); err != nil {
		return fmt.Errorf("tmpfs /dev/shm: %w", err)
	}

	for _, l := range [][2]string{
		{"pts/ptmx", "ptmx"},
		{"/proc/self/fd", "fd"},
		{"/proc/self/fd/0", "stdin"},
		{"/proc/self/fd/1", "stdout"},
		{"/proc/self/fd/2", "stderr"},
	} {
		if err := os.Symlink(l[0], filepath.Join(dev, l[1])); err != nil {
			return err
		}
	}
	return nil
}

func childMain() error {
	cfg, err := loadConfig(childEnv)
	if err != nil {
		return err
	}

	if err := syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("make-rprivate /: %w", err)
	}
	if err := syscall.Mount(cfg.Rootfs, cfg.Rootfs, "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind rootfs onto itself: %w", err)
	}

	homePrefix := "/" + strings.SplitN(strings.TrimPrefix(cfg.Home, "/"), "/", 2)[0]
	entries, err := os.ReadDir("/")
	if err != nil {
		return err
	}
	for _, e := range entries {
		src := "/" + e.Name()
		if src == homePrefix || src == "/proc" || src == "/tmp" || src == "/run" || src == "/dev" {
			continue
		}
		dest := filepath.Join(cfg.Rootfs, src)
		info, err := os.Lstat(src)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(src)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, dest); err != nil {
				return err
			}
			continue
		}
		if !info.IsDir() {
			continue
		}
		if err := os.Mkdir(dest, 0755); err != nil {
			return err
		}
		if err := syscall.Mount(src, dest, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("rbind %s -> %s: %w", src, dest, err)
		}
		if err := syscall.Mount("", dest, "", syscall.MS_SLAVE|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("make-rslave %s: %w", dest, err)
		}
		if err := syscall.Mount("", dest, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
			if err != syscall.EPERM {
				// Some pseudo-filesystems (sysfs, etc.) refuse a RO remount in a
				// nested user ns. Keep them writable rather than failing.
				log.Printf("warning: remount-ro %s: %v", dest, err)
			}
		}
	}

	if err := setupDev(cfg.Rootfs); err != nil {
		return err
	}

	type cacheMount struct {
		dest, sub string
		mode      os.FileMode
	}
	for _, m := range []cacheMount{
		{cfg.Home, "home", 0700},
		{"/tmp", "tmp", 01777},
		{"/run", "run", 0755},
	} {
		src := filepath.Join(cfg.CacheDir, m.sub)
		if err := os.Chmod(src, m.mode); err != nil {
			return err
		}
		dst := filepath.Join(cfg.Rootfs, m.dest)
		if err := os.MkdirAll(dst, 0755); err != nil {
			return err
		}
		if err := syscall.Mount(src, dst, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("bind %s -> %s: %w", src, dst, err)
		}
		if err := syscall.Mount("", dst, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_NOSUID|syscall.MS_NODEV, ""); err != nil {
			log.Printf("warning: remount-nosuid %s: %v", dst, err)
		}
	}
	runInRoot := filepath.Join(cfg.Rootfs, "run")
	runUserInRoot := filepath.Join(runInRoot, "user", fmt.Sprintf("%d", os.Getuid()))
	if err := os.MkdirAll(runUserInRoot, 0700); err != nil {
		return err
	}
	if cfg.RestrictNet {
		// Host's resolver (typically 127.0.0.53) isn't reachable from the
		// netns. Provide a stub-resolv.conf pointing at public resolvers
		// that pasta will proxy via the host stack.
		dest := filepath.Join(runInRoot, "systemd", "resolve")
		if err := os.MkdirAll(dest, 0755); err != nil {
			return err
		}
		stub := "nameserver 1.1.1.1\nnameserver 8.8.8.8\n"
		if err := os.WriteFile(filepath.Join(dest, "stub-resolv.conf"), []byte(stub), 0644); err != nil {
			return err
		}
	} else if _, err := os.Stat("/run/systemd/resolve"); err == nil {
		dest := filepath.Join(runInRoot, "systemd", "resolve")
		if err := os.MkdirAll(dest, 0755); err != nil {
			return err
		}
		if err := syscall.Mount("/run/systemd/resolve", dest, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("rbind /run/systemd/resolve: %w", err)
		}
		if err := syscall.Mount("", dest, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
			log.Printf("warning: remount-ro /run/systemd/resolve: %v", err)
		}
	}

	for _, d := range cfg.Binds {
		dest := filepath.Join(cfg.Rootfs, d)
		info, err := os.Stat(d)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := os.MkdirAll(dest, 0755); err != nil {
				return err
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				return err
			}
			f.Close()
		}
		if err := syscall.Mount(d, dest, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("rbind %s -> %s: %w", d, dest, err)
		}
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	var cmd *exec.Cmd
	if cfg.RestrictNet {
		hostToNs := "none"
		if cfg.ExposeLocalhost {
			hostToNs = "auto"
		}
		cmd = exec.Command("pasta",
			"--config-net", "--no-map-gw", "--ipv4-only", "--netns-only",
			"--quiet", "--log-file=/dev/null",
			"-a", "100.64.0.2", "-n", "24", "-g", "100.64.0.1",
			"-t", hostToNs, "-u", hostToNs, "-T", "none", "-U", "none",
			"--runas", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
			"--", self, "--clone-init",
		)
	} else {
		cmd = exec.Command(self, "--clone-init")
	}
	cmd.Env = append(os.Environ(), initEnv+"="+encodeConfig(cfg))
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}

// cloneInitMain runs after pasta (or directly) inside the netns, then clones
// into a fresh PID/IPC/UTS/cgroup namespace to become the container's init.
func cloneInitMain() error {
	cfg, err := loadConfig(initEnv)
	if err != nil {
		return err
	}
	if cfg.RestrictNet {
		blackhole := []string{
			"10.0.0.0/8",
			"172.16.0.0/12",
			"192.168.0.0/16",
			"169.254.169.254/32",
		}
		for _, r := range blackhole {
			out, err := exec.Command("ip", "-4", "route", "add", "blackhole", r).CombinedOutput()
			if err != nil {
				return fmt.Errorf("ip route add blackhole %s: %w: %s", r, err, out)
			}
		}
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), initEnv+"="+encodeConfig(cfg))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS | syscall.CLONE_NEWCGROUP,
	}
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}

func initMain() error {
	cfg, err := loadConfig(initEnv)
	if err != nil {
		return err
	}

	if err := syscall.Sethostname([]byte("runclaude")); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}

	procPath := filepath.Join(cfg.Rootfs, "proc")
	if err := os.MkdirAll(procPath, 0755); err != nil {
		return err
	}
	if err := syscall.Mount("proc", procPath, "proc",
		syscall.MS_NOSUID|syscall.MS_NOEXEC|syscall.MS_NODEV, ""); err != nil {
		return fmt.Errorf("mount proc: %w", err)
	}

	if err := os.Chdir(cfg.Rootfs); err != nil {
		return fmt.Errorf("chdir rootfs: %w", err)
	}
	if err := syscall.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := syscall.Unmount(".", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}
	if err := os.Chdir(cfg.Cwd); err != nil {
		return fmt.Errorf("chdir %s: %w", cfg.Cwd, err)
	}

	if err := dropAllCaps(); err != nil {
		return err
	}

	os.Unsetenv(childEnv)
	os.Unsetenv(initEnv)
	argv := append([]string{"bash"}, cfg.BashArgs...)
	return syscall.Exec("/bin/bash", argv, os.Environ())
}

func main() {
	cloneInit := len(os.Args) > 1 && os.Args[1] == "--clone-init"
	var err error
	switch {
	case os.Getenv(initEnv) != "" && cloneInit:
		err = cloneInitMain()
	case os.Getenv(initEnv) != "":
		err = initMain()
	case os.Getenv(childEnv) != "":
		err = childMain()
	default:
		err = mainErr()
	}
	if err != nil {
		log.Fatal(err)
	}
}
