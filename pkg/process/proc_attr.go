package process

import "syscall"

// groupProcAttr returns a copy of attr that starts the process in its own
// process group, so the process can be stopped together with its
// descendants (shell background jobs, forked workers). Process group
// settings configured in sysProcAttr (setsid, setpgid, foreground) are kept.
func groupProcAttr(attr *syscall.SysProcAttr) *syscall.SysProcAttr {
	a := &syscall.SysProcAttr{}
	if attr != nil {
		copied := *attr
		a = &copied
	}
	if !a.Setsid && !a.Setpgid && !a.Foreground {
		a.Setpgid = true
		a.Pgid = 0
	}
	return a
}

// childProcAttr returns process attributes for a hook or exec probe of a
// supervised command. The child never runs with more privileges than the
// supervised command: when the command drops privileges with
// sysProcAttr.credential, the child gets the same credential, otherwise a
// command that can rewrite its probe or hook script would get it executed
// as the wrapper user (often root).
func childProcAttr(own, supervised *syscall.SysProcAttr) *syscall.SysProcAttr {
	a := groupProcAttr(own)
	if supervised != nil && supervised.Credential != nil {
		cred := *supervised.Credential
		cred.Groups = append([]uint32(nil), supervised.Credential.Groups...)
		a.Credential = &cred
	}
	return a
}

// SignalGroup sends sig to the process group led by pid, so its descendants
// get it too. If pid doesn't lead a process group, only pid is signalled.
func SignalGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		// kill(0) and kill(-1) would signal the wrapper itself or everything
		return syscall.ESRCH
	}
	if err := syscall.Kill(-pid, sig); err == nil {
		return nil
	}
	return syscall.Kill(pid, sig)
}
