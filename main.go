package main

import (
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
	prSetNoNewPrivs       = 38
	prCapBsetDrop         = 24
	prCapAmbient          = 47
	prCapAmbientClearAll  = 4
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
	Rootfs    string   `json:"rootfs"`
	Home      string   `json:"home"`
	Cwd       string   `json:"cwd"`
	Binds     []string `json:"binds"`
	BashArgs  []string `json:"bashArgs"`
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
	flag.Parse()

	cfg := &Config{
		Rootfs: rootfs,
		Home:   home,
		Cwd:    cwd,
		Binds:  []string{cwd},
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
		if src == homePrefix || src == "/proc" || src == "/tmp" || src == "/run" {
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
	}

	homeInRoot := filepath.Join(cfg.Rootfs, cfg.Home)
	if err := os.MkdirAll(homeInRoot, 0755); err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", homeInRoot, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=755"); err != nil {
		return fmt.Errorf("tmpfs %s: %w", homeInRoot, err)
	}

	tmpInRoot := filepath.Join(cfg.Rootfs, "tmp")
	if err := os.MkdirAll(tmpInRoot, 0755); err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", tmpInRoot, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=1777"); err != nil {
		return fmt.Errorf("tmpfs %s: %w", tmpInRoot, err)
	}

	runInRoot := filepath.Join(cfg.Rootfs, "run")
	if err := os.MkdirAll(runInRoot, 0755); err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", runInRoot, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=755"); err != nil {
		return fmt.Errorf("tmpfs %s: %w", runInRoot, err)
	}
	runUserInRoot := filepath.Join(runInRoot, "user", fmt.Sprintf("%d", os.Getuid()))
	if err := os.MkdirAll(runUserInRoot, 0700); err != nil {
		return err
	}
	if _, err := os.Stat("/run/systemd/resolve"); err == nil {
		dest := filepath.Join(runInRoot, "systemd", "resolve")
		if err := os.MkdirAll(dest, 0755); err != nil {
			return err
		}
		if err := syscall.Mount("/run/systemd/resolve", dest, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("rbind /run/systemd/resolve: %w", err)
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
	var err error
	switch {
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
