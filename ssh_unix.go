//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// setProcessGroup makes ssh the leader of its own process group. Without
// this, exec.CommandContext only kills the ssh leader on ctx cancel, and
// any ProxyCommand / ControlMaster / ProxyJump child it spawned can be
// orphaned and keep the local forward open.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup signals -PID (the whole process group) so children die
// with the leader. Falls back to killing just the leader if pgid lookup
// fails (e.g. process already exited).
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return
	}
	_ = cmd.Process.Kill()
}
