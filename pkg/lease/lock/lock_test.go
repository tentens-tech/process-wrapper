package lock

import "testing"

func TestTLSInfoDefaults(t *testing.T) {
	i := TLSInfo("", "", "")
	if i.TrustedCAFile != DefaultCaCertPath || i.CertFile != DefaultClientCertPath || i.KeyFile != DefaultClientKeyPath {
		t.Errorf("unexpected defaults %+v", i)
	}
	// every file falls back to its default independently
	i = TLSInfo("/ca.crt", "", "")
	if i.TrustedCAFile != "/ca.crt" || i.CertFile != DefaultClientCertPath || i.KeyFile != DefaultClientKeyPath {
		t.Errorf("unexpected files %+v", i)
	}
	i = TLSInfo("", "/tls.crt", "/tls.key")
	if i.TrustedCAFile != DefaultCaCertPath || i.CertFile != "/tls.crt" || i.KeyFile != "/tls.key" {
		t.Errorf("unexpected files %+v", i)
	}
}
