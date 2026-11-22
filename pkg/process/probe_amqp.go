package process

import (
	"fmt"
	amqp "github.com/rabbitmq/amqp091-go"
	log "github.com/sirupsen/logrus"
)

const (
	AMQPProbeRabbitMqUserEnvName = "AMQP_PROBE_RABBITMQ_USER"
	AMQPProbeRabbitMqPassEnvName = "AMQP_PROBE_RABBITMQ_PASS"         // #nosec G101
	AMQPProbeDsnTemplateEnvName  = "AMQP_PROBE_RABBITMQ_DSN_TEMPLATE" // "amqp://%v:%v@localhost:5672"
)

type AMQPProbe struct {
	Name       string            `yaml:"name"`
	Value      string            `yaml:"value"`
	Envs       map[string]string `yaml:"envs"`
	ReuseConn  bool              `yaml:"reuseConn"`
	connection *amqp.Connection
}

func (p *AMQPProbe) String() string {
	dsn := fmt.Sprintf(p.Envs[AMQPProbeDsnTemplateEnvName], p.Envs[AMQPProbeRabbitMqUserEnvName], "xxx")
	return dsn
}

func (p *AMQPProbe) Run(name string) bool {
	var err error
	logger := log.WithFields(log.Fields{"name": name})
	if p.connection != nil {
		closed := p.connection.IsClosed()
		if closed {
			p.connection = nil
			logger.Warnf("Reset amqp probe connection")
			return false
		}
		logger.Debugf("Reuse amqp probe connection")
		return true
	}

	dsn, ok := p.DSN(logger)
	if !ok {
		return false
	}

	p.connection, err = amqp.Dial(dsn)
	if err != nil {
		logger.Errorf("Failed to create amqp connection, %v", err)
		return false
	}

	if !p.ReuseConn {
		logger.Debugf("Do not reuse amqp connection")
		err = p.connection.Close()
		if err != nil {
			logger.Warnf("Amqp connection close error, %v", err)
			return false
		}
		p.connection = nil
	}
	logger.Debugf("Amqp return true")
	return true
}

func (p *AMQPProbe) DSN(logger *log.Entry) (string, bool) {
	var (
		ok                           bool
		dsn, dsnTemplate, user, pass string
	)

	dsnTemplate, ok = p.Envs[AMQPProbeDsnTemplateEnvName]
	if !ok {
		logger.Errorf("Failed to get %v env", AMQPProbeDsnTemplateEnvName)
		return "", false
	}

	user, ok = p.Envs[AMQPProbeRabbitMqUserEnvName]
	if !ok {
		logger.Errorf("Failed to get %v env", AMQPProbeRabbitMqUserEnvName)
		return "", false
	}

	pass, ok = p.Envs[AMQPProbeRabbitMqPassEnvName]
	if !ok {
		logger.Errorf("Failed to get %v env", AMQPProbeRabbitMqPassEnvName)
		return "", false
	}
	dsn = fmt.Sprintf(dsnTemplate, user, pass)
	return dsn, true
}
