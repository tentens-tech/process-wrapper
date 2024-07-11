package lock

import (
	"context"
	"crypto/tls"
	"fmt"
	log "github.com/sirupsen/logrus"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
	"net/http"
	"strings"
	"time"
)

const (
	DefaultEtcdEndpoint   = "localhost:2379"
	DefaultLeaseValue     = "lock-value"
	DefaultCaCertPath     = "/cert/tls/ca.crt"
	DefaultClientCertPath = "/cert/tls/tls.crt"
	DefaultClientKeyPath  = "/cert/tls/tls.key"
)

type Lock struct {
	Cli *clientv3.Client
}

func NewLock(etcdEndpoints, etcdCaCert, etcdClientCert, etcdClientKey string, sharedLockEtcdTLS bool) *Lock {
	lock := &Lock{Cli: func() *clientv3.Client {
		var (
			err       error
			tlsConfig *tls.Config
		)

		tlsConfig = func() *tls.Config {
			if !sharedLockEtcdTLS {
				return nil
			}
			// Configure TLS
			tlsInfo := transport.TLSInfo{
				TrustedCAFile: func() string {
					if etcdCaCert == "" {
						return DefaultCaCertPath
					}
					return etcdCaCert
				}(),
				CertFile: func() string {
					if etcdCaCert == "" {
						return DefaultClientCertPath
					}
					return etcdClientCert
				}(),
				KeyFile: func() string {
					if etcdCaCert == "" {
						return DefaultClientKeyPath
					}
					return etcdClientKey
				}(),
			}
			tlsConfig, err = tlsInfo.ClientConfig()
			if err != nil {
				log.Fatalf("Failed to create TLS configuration for etcd endpoints: %v", err)
			}
			return tlsConfig
		}()

		cli, err := clientv3.New(clientv3.Config{
			Endpoints: func() []string {
				if strings.Contains(etcdEndpoints, ",") {
					log.Printf("Use multiple etcd endpoints")
					return strings.Split(etcdEndpoints, ",")
				}
				if etcdEndpoints != "" {
					log.Warnf("Use one etcd server, %v", etcdEndpoints)
					return []string{etcdEndpoints}
				}
				log.Warnf("Use default etcd endpoint, %v", DefaultEtcdEndpoint)
				return []string{DefaultEtcdEndpoint}
			}(),
			DialTimeout: 5 * time.Second,
			TLS:         tlsConfig,
		})
		if err != nil {
			log.Fatalf("Failed to connect to etcd, %v", err)
		}
		return cli
	}()}
	return lock
}

func (lock *Lock) GetLease(key string, writer http.ResponseWriter, data []byte, leaseTTL int64) (clientv3.LeaseID, error) {
	var err error
	var value string

	if data == nil {
		value = DefaultLeaseValue
	} else {
		value = string(data)
	}

	// Try to get the key
	ctx := context.Background()
	getCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	resp, err := lock.Cli.Get(getCtx, key)
	cancel()
	if err != nil {
		return 0, fmt.Errorf("failed to get key from etcd: %v", err)
	}
	if len(resp.Kvs) != 0 {
		log.Debugf("Lock %v, already exists", key)
		writer.WriteHeader(http.StatusAccepted)
		// Todo: wait program
		return 0, nil
	}

	// If the key does not exist, create it with a new lease
	// Create a new lease

	var leaseResp *clientv3.LeaseGrantResponse
	leaseResp, err = lock.Cli.Grant(ctx, leaseTTL)
	if err != nil {
		writer.WriteHeader(http.StatusInternalServerError)
		return 0, fmt.Errorf("failed to create lease: %v", err)
	}

	var TxnResp *clientv3.TxnResponse
	TxnResp, err = lock.Cli.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
		Then(clientv3.OpPut(key, value, clientv3.WithLease(leaseResp.ID))).
		Commit()
	if err != nil {
		return 0, err
	}

	if !TxnResp.Succeeded {
		log.Warnf("Lease race")
		writer.WriteHeader(http.StatusAccepted)
		return 0, nil
	}

	log.Printf("%v key created with a new lease %v", key, leaseResp.ID)

	// Renew a lease
	err = lock.KeepLeaseOnce(lock.Cli, leaseResp.ID)
	if err != nil {
		writer.WriteHeader(http.StatusInternalServerError)
		return 0, fmt.Errorf("failed to prolong lease with leaseID: %v, %v", leaseResp.ID, err)
	}

	writer.WriteHeader(http.StatusCreated)

	return leaseResp.ID, nil
}

func (lock *Lock) KeepLeaseOnce(client *clientv3.Client, leaseID clientv3.LeaseID) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := client.KeepAliveOnce(ctx, leaseID)
	if err != nil {
		return err
	}
	log.Printf("KeepAlive lease: %v", leaseID)
	return nil
}
