package main

import (
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

	binds := []string{cwd}
	for _, a := range os.Args[1:] {
		abs, err := filepath.Abs(a)
		if err != nil {
			return err
		}
		binds = append(binds, abs)
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{
		"--user", "--map-current-user", "--map-auto", "--keep-caps", "--mount",
		"--", self, "--rootfs", rootfs, "--home", home, "--cwd", cwd,
	}
	for _, d := range binds {
		args = append(args, "--bind", d)
	}
	cmd := exec.Command("unshare", args...)
	cmd.Env = append(os.Environ(), childEnv+"=1")
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func parseArgs() (rootfs, home, cwd string, binds []string) {
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--rootfs":
			i++
			rootfs = args[i]
		case "--home":
			i++
			home = args[i]
		case "--cwd":
			i++
			cwd = args[i]
		case "--bind":
			i++
			binds = append(binds, args[i])
		}
	}
	return
}

func childMain() error {
	rootfs, home, cwd, binds := parseArgs()

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

	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self,
		"--rootfs", rootfs, "--home", home, "--cwd", cwd,
	)
	cmd.Env = append(os.Environ(), initEnv+"=1")
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
	rootfs, _, cwd, _ := parseArgs()

	if err := syscall.Sethostname([]byte("runclaude")); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}

	procPath := filepath.Join(rootfs, "proc")
	if err := os.MkdirAll(procPath, 0755); err != nil {
		return err
	}
	if err := syscall.Mount("proc", procPath, "proc",
		syscall.MS_NOSUID|syscall.MS_NOEXEC|syscall.MS_NODEV, ""); err != nil {
		return fmt.Errorf("mount proc: %w", err)
	}

	if err := syscall.Chroot(rootfs); err != nil {
		return fmt.Errorf("chroot %s: %w", rootfs, err)
	}
	if err := os.Chdir(cwd); err != nil {
		return fmt.Errorf("chdir %s: %w", cwd, err)
	}

	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TERM=" + os.Getenv("TERM"),
		"HOME=" + os.Getenv("HOME"),
		"USER=" + os.Getenv("USER"),
	}
	return syscall.Exec("/bin/sh", []string{"sh"}, env)
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
