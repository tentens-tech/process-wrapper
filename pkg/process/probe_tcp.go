package process

import (
	"fmt"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	TCPProbeTimoutSeconds = 2
)

type TCPProbe struct {
	Socket string `yaml:"socket"`
}

func (p TCPProbe) Run(name string) bool {
	conn, err := net.DialTimeout("tcp", p.Socket, time.Second*TCPProbeTimoutSeconds)
	if err != nil {
		log.WithFields(log.Fields{"name": name, "socket": p.Socket}).Errorf("TCP probe failure, %v", err)
		return false
	}
	if err = conn.Close(); err != nil {
		log.WithFields(log.Fields{"name": name, "socket": p.Socket}).Debugf("TCP probe close error, %v", err)
	}
	log.WithFields(log.Fields{"name": name, "socket": p.Socket}).Debugf("TCP probe success")
	return true
}

func (p TCPProbe) String() string {
	return fmt.Sprintf("Connect tcp://%v", p.Socket)
}
