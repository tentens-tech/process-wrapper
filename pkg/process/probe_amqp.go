package process

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	log "github.com/sirupsen/logrus"
)

const (
	AMQPProbeRabbitMqUserEnvName = "AMQP_PROBE_RABBITMQ_USER"
	AMQPProbeRabbitMqPassEnvName = "AMQP_PROBE_RABBITMQ_PASS"         // #nosec G101
	AMQPProbeDsnTemplateEnvName  = "AMQP_PROBE_RABBITMQ_DSN_TEMPLATE" // "amqp://%v:%v@localhost:5672"
	AMQPProbeQueueName           = "amqp-probe-queue"                 // Queue for probe, will be created if not exist
	AMQPConnectionHeartbit       = 30 * time.Second
	// Defaulttimeout is the default timeout of every amqp operation (dial,
	// create channel, declare queue, get message)
	Defaulttimeout = 5 * time.Second
)

// AMQPProbe keeps a connection and a channel open and checks the broker with
// basic.get on a durable probe queue, every operation has a timeout.
type AMQPProbe struct {
	Name  string            `yaml:"name"`
	Value string            `yaml:"value"`
	Envs  map[string]string `yaml:"envs"`
	// OperationsTimeout bounds dial, channel, queue declare and get, default 5s
	OperationsTimeout Duration `yaml:"operationsTimeout"`
	// TLSInsecureSkipVerify disables broker certificate verification for amqps
	TLSInsecureSkipVerify bool `yaml:"tlsInsecureSkipVerify"`
	// ReuseConn is deprecated: the connection is always reused
	ReuseConn bool `yaml:"reuseConn"`

	conn *amqpProbeConn
}

// amqpProbeConn is the runtime state of a probe, it isn't copied with the
// probe config (replicas get their own connection)
type amqpProbeConn struct {
	mu         sync.Mutex
	dsn        string
	connection *amqp.Connection
	channel    *amqp.Channel
	netConn    net.Conn
}

func (p *AMQPProbe) String() string {
	dsn := fmt.Sprintf(p.Envs[AMQPProbeDsnTemplateEnvName], p.Envs[AMQPProbeRabbitMqUserEnvName], "xxx")
	return dsn
}

func (p *AMQPProbe) timeout() time.Duration {
	if p.OperationsTimeout.Duration > 0 {
		return p.OperationsTimeout.Duration
	}
	return Defaulttimeout
}

func (p *AMQPProbe) Run(name string) bool {
	logger := log.WithFields(log.Fields{"name": name})
	if p.conn == nil {
		p.conn = &amqpProbeConn{}
	}
	c := p.conn
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.dsn == "" {
		dsn, err := p.DSN()
		if err != nil {
			logger.Errorf("Failed to create DSN: %v", err)
			return false
		}
		c.dsn = dsn
	}

	if c.connection == nil {
		if err := p.connect(c); err != nil {
			logger.Errorf("Failed to open amqp probe connection: %v", err)
			c.reset()
			return false
		}
		logger.Infof("Created new amqp probe connection")
	}

	if c.connection.IsClosed() || c.channel.IsClosed() {
		logger.Warnf("Connection is closed, reset amqp probe connection")
		c.reset()
		return false
	}
	logger.Debugf("Reuse existing connection")

	if err := c.withDeadline(p.timeout(), func() error {
		_, _, err := c.channel.Get(AMQPProbeQueueName, false)
		return err
	}); err != nil {
		logger.Warnf("Error getting queue, reset amqp probe connection, %v", err)
		c.reset()
		return false
	}

	logger.Debugf("Amqp probe passed")
	return true
}

// connect opens the connection and the channel and declares the probe queue
func (p *AMQPProbe) connect(c *amqpProbeConn) error {
	timeout := p.timeout()
	config := amqp.Config{
		Heartbeat: AMQPConnectionHeartbit,
		// #nosec G402 -- verification is disabled only on explicit request
		TLSClientConfig: &tls.Config{InsecureSkipVerify: p.TLSInsecureSkipVerify, MinVersion: tls.VersionTLS12},
		Dial: func(network, addr string) (net.Conn, error) {
			netConn, err := net.DialTimeout(network, addr, timeout)
			if err != nil {
				return nil, err
			}
			// amqp091 has no operation timeouts: they are set as deadlines on
			// the socket. The deadline covers the TLS and AMQP handshake and is
			// cleared once the connection is up
			if err := netConn.SetDeadline(time.Now().Add(timeout)); err != nil {
				_ = netConn.Close()
				return nil, err
			}
			c.netConn = netConn
			return netConn, nil
		},
	}
	var err error
	if c.connection, err = amqp.DialConfig(c.dsn, config); err != nil {
		return err
	}
	if c.netConn != nil {
		_ = c.netConn.SetDeadline(time.Time{})
	}
	if err = c.withDeadline(timeout, func() error {
		var err error
		c.channel, err = c.connection.Channel()
		return err
	}); err != nil {
		return fmt.Errorf("create channel: %w", err)
	}
	if err = c.withDeadline(timeout, func() error {
		_, err := c.channel.QueueDeclare(AMQPProbeQueueName, true, false, false, false, nil)
		return err
	}); err != nil {
		return fmt.Errorf("declare queue: %w", err)
	}
	return nil
}

// withDeadline runs op with a read and write deadline on the connection socket
func (c *amqpProbeConn) withDeadline(timeout time.Duration, op func() error) error {
	if c.netConn == nil {
		return errors.New("no network connection")
	}
	if err := c.netConn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set deadline: %w", err)
	}
	err := op()
	_ = c.netConn.SetDeadline(time.Time{})
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("operation timed out: %w", err)
	}
	return err
}

func (c *amqpProbeConn) reset() {
	if c.channel != nil {
		_ = c.channel.Close()
		c.channel = nil
	}
	if c.connection != nil {
		_ = c.connection.Close()
		c.connection = nil
	}
	if c.netConn != nil {
		_ = c.netConn.Close()
		c.netConn = nil
	}
}

func (p *AMQPProbe) DSN() (string, error) {
	dsnTemplate, ok := p.Envs[AMQPProbeDsnTemplateEnvName]
	if !ok {
		return "", fmt.Errorf("failed to get %v env", AMQPProbeDsnTemplateEnvName)
	}
	user, ok := p.Envs[AMQPProbeRabbitMqUserEnvName]
	if !ok {
		return "", fmt.Errorf("failed to get %v env", AMQPProbeRabbitMqUserEnvName)
	}
	pass, ok := p.Envs[AMQPProbeRabbitMqPassEnvName]
	if !ok {
		return "", fmt.Errorf("failed to get %v env", AMQPProbeRabbitMqPassEnvName)
	}
	return fmt.Sprintf(dsnTemplate, user, pass), nil
}
