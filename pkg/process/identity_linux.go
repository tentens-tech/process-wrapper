package process

import (
	"reflect"
	"syscall"
)

// processIdentity holds the SysProcAttr fields that decide which user a child
// runs as and which privileges it keeps after exec.
type processIdentity struct {
	Credential                 *syscall.Credential
	AmbientCaps                []uintptr
	CloneNewUser               bool
	UnshareNewUser             bool
	UidMappings                []syscall.SysProcIDMap
	GidMappings                []syscall.SysProcIDMap
	GidMappingsEnableSetgroups bool
}

func identityOf(a *syscall.SysProcAttr) processIdentity {
	if a == nil {
		return processIdentity{}
	}
	id := processIdentity{
		CloneNewUser:               a.Cloneflags&syscall.CLONE_NEWUSER != 0,
		UnshareNewUser:             a.Unshareflags&syscall.CLONE_NEWUSER != 0,
		GidMappingsEnableSetgroups: a.GidMappingsEnableSetgroups,
	}
	if a.Credential != nil {
		c := *a.Credential
		if len(c.Groups) > 0 {
			c.Groups = append([]uint32(nil), c.Groups...)
		} else {
			c.Groups = nil
		}
		id.Credential = &c
	}
	if len(a.AmbientCaps) > 0 {
		id.AmbientCaps = append([]uintptr(nil), a.AmbientCaps...)
	}
	if len(a.UidMappings) > 0 {
		id.UidMappings = append([]syscall.SysProcIDMap(nil), a.UidMappings...)
	}
	if len(a.GidMappings) > 0 {
		id.GidMappings = append([]syscall.SysProcIDMap(nil), a.GidMappings...)
	}
	return id
}

// dropsPrivileges reports whether the identity runs the child as someone other
// than the supervisor.
func (id processIdentity) dropsPrivileges() bool {
	return id.Credential != nil || id.CloneNewUser || id.UnshareNewUser
}

func (id processIdentity) isSet() bool {
	return !reflect.DeepEqual(id, processIdentity{})
}

func (id processIdentity) equal(other processIdentity) bool {
	return reflect.DeepEqual(id, other)
}

func (id processIdentity) applyTo(a *syscall.SysProcAttr) {
	a.Credential = id.Credential
	a.AmbientCaps = id.AmbientCaps
	a.Cloneflags &^= syscall.CLONE_NEWUSER
	if id.CloneNewUser {
		a.Cloneflags |= syscall.CLONE_NEWUSER
	}
	a.Unshareflags &^= syscall.CLONE_NEWUSER
	if id.UnshareNewUser {
		a.Unshareflags |= syscall.CLONE_NEWUSER
	}
	a.UidMappings = id.UidMappings
	a.GidMappings = id.GidMappings
	a.GidMappingsEnableSetgroups = id.GidMappingsEnableSetgroups
}
