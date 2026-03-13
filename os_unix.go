//go:build !windows

package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

func stopDaemonOS() error {
	pid, err := readDaemonPid()
	if err != nil {
		return err
	}
	if pid == 0 {
		return fmt.Errorf("daemon pid not found")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	return nil
}

func daemonRunningOS() (bool, int) {
	pid, err := readDaemonPid()
	if err != nil || pid == 0 {
		return false, 0
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false, 0
	}
	return true, pid
}

func detectDiskFreeGBOS(path string) int {
	if strings.TrimSpace(path) == "" {
		path = "."
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0
	}
	freeBytes := stat.Bavail * uint64(stat.Bsize)
	if freeBytes == 0 {
		return 0
	}
	return int(freeBytes / 1024 / 1024 / 1024)
}

func daemonSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}
