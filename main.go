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
)

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

	homePrefix := "/" + strings.SplitN(strings.TrimPrefix(cfg.Home, "/"), "/", 2)[0]
	entries, err := os.ReadDir("/")
	if err != nil {
		return err
	}
	for _, e := range entries {
		src := "/" + e.Name()
		if src == homePrefix || src == "/proc" || src == "/tmp" {
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

	if err := syscall.Chroot(cfg.Rootfs); err != nil {
		return fmt.Errorf("chroot %s: %w", cfg.Rootfs, err)
	}
	if err := os.Chdir(cfg.Cwd); err != nil {
		return fmt.Errorf("chdir %s: %w", cfg.Cwd, err)
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
