package process

import (
	"fmt"
	"syscall"
)

const DefaultChildPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// childSysProcAttr returns the attributes for a program the supervisor starts on
// behalf of a supervised command, such as a postStart command hook or an exec
// probe. Such a program may be replaced by the supervised command's user, so it
// must never run with more privileges than that command. When the supervised
// command drops privileges, the child gets the same identity; a child config
// that asks for a different identity is refused.
func childSysProcAttr(own, supervised *syscall.SysProcAttr) (*syscall.SysProcAttr, error) {
	sup := identityOf(supervised)
	if !sup.dropsPrivileges() {
		if own == nil {
			return nil, nil
		}
		attr := *own
		return &attr, nil
	}
	if ownID := identityOf(own); ownID.isSet() && !ownID.equal(sup) {
		return nil, fmt.Errorf("sysProcAttr identity differs from the supervised command, refusing to run with a different identity")
	}
	var attr syscall.SysProcAttr
	if own != nil {
		attr = *own
	}
	sup.applyTo(&attr)
	return &attr, nil
}

// childEnv builds the environment of a child from its own configuration only,
// so the supervisor environment and the supervised command envs are not passed on.
func childEnv(layers ...map[string]string) []string {
	merged := make(map[string]string)
	for _, l := range layers {
		for k, v := range l {
			merged[k] = v
		}
	}
	if _, ok := merged["PATH"]; !ok {
		merged["PATH"] = DefaultChildPath
	}
	env := make([]string, 0, len(merged))
	for k, v := range merged {
		env = append(env, fmt.Sprintf("%v=%v", k, v))
	}
	return env
}
