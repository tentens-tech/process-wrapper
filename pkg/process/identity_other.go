//go:build !linux

package process

import (
	"reflect"
	"syscall"
)

// processIdentity holds the SysProcAttr fields that decide which user a child
// runs as.
type processIdentity struct {
	Credential *syscall.Credential
}

func identityOf(a *syscall.SysProcAttr) processIdentity {
	if a == nil || a.Credential == nil {
		return processIdentity{}
	}
	c := *a.Credential
	if len(c.Groups) > 0 {
		c.Groups = append([]uint32(nil), c.Groups...)
	} else {
		c.Groups = nil
	}
	return processIdentity{Credential: &c}
}

// dropsPrivileges reports whether the identity runs the child as someone other
// than the supervisor.
func (id processIdentity) dropsPrivileges() bool {
	return id.Credential != nil
}

func (id processIdentity) isSet() bool {
	return id.Credential != nil
}

func (id processIdentity) equal(other processIdentity) bool {
	return reflect.DeepEqual(id, other)
}

func (id processIdentity) applyTo(a *syscall.SysProcAttr) {
	a.Credential = id.Credential
}
