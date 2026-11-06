package process

import (
	"os/exec"
	"syscall"
)

// sandbox holds the execution settings of a supervised command. Exec probes and
// command hooks start binaries that the supervised process may be able to modify,
// so they must run with these settings rather than with the supervisor's.
type sandbox struct {
	dir         string
	sysProcAttr *syscall.SysProcAttr
}

func (c *Command) sandbox() sandbox {
	return sandbox{dir: c.Dir, sysProcAttr: c.SysProcAttr}
}

// apply configures cmd to run in the supervised command's directory with its
// process attributes. own is the child's configured attributes; it is used only
// when the supervised command runs with the supervisor's privileges, so a child
// never gets more than the supervised process has.
func (s sandbox) apply(cmd *exec.Cmd, own *syscall.SysProcAttr) {
	cmd.Dir = s.dir
	attr := s.sysProcAttr
	if attr == nil {
		attr = own
	}
	cmd.SysProcAttr = childSysProcAttr(attr)
}

func childSysProcAttr(attr *syscall.SysProcAttr) *syscall.SysProcAttr {
	if attr == nil {
		return nil
	}
	child := *attr
	if attr.Credential != nil {
		credential := *attr.Credential
		credential.Groups = append([]uint32(nil), attr.Credential.Groups...)
		child.Credential = &credential
	}
	// The controlling terminal and the pidfd output belong to the supervised process.
	child.Setctty = false
	child.Foreground = false
	child.Ctty = 0
	child.PidFD = nil
	return &child
}
