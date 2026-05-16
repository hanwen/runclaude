package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

const (
	childEnv = "_RUNCLAUDE_CHILD"
	initEnv  = "_RUNCLAUDE_INIT"
)

type Config struct {
	Rootfs string   `json:"rootfs"`
	Home   string   `json:"home"`
	Cwd    string   `json:"cwd"`
	Binds  []string `json:"binds"`
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

	cfg := &Config{
		Rootfs: rootfs,
		Home:   home,
		Cwd:    cwd,
		Binds:  []string{cwd},
	}
	for _, a := range os.Args[1:] {
		abs, err := filepath.Abs(a)
		if err != nil {
			return err
		}
		cfg.Binds = append(cfg.Binds, abs)
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

	if err := syscall.Mount("/", cfg.Rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("rbind / -> %s: %w", cfg.Rootfs, err)
	}
	if err := syscall.Mount("", cfg.Rootfs, "", syscall.MS_SLAVE|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("make-rslave %s: %w", cfg.Rootfs, err)
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
		if err := os.MkdirAll(dest, 0755); err != nil {
			return err
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
	return syscall.Exec("/bin/bash", []string{"bash"}, os.Environ())
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
