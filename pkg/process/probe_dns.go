package process

import (
	"context"
	"fmt"
	"net"
	"sort"
	"time"
)

// DefaultDNSProbeTimeout bounds a single lookup of the DNS probe
const DefaultDNSProbeTimeout = 5 * time.Second

// DNSProbe fails when the address list of URL changes since the process
// start, so the process is restarted after failureThreshold and connects to
// the new addresses (e.g. a client that resolves a broker only once)
type DNSProbe struct {
	URL string `yaml:"url"`
	// Timeout bounds a single lookup, default 5s
	Timeout     Duration `yaml:"timeout"`
	addressList []net.IP
}

// reset forgets the address list, the next run records the current one
func (p *DNSProbe) reset() {
	p.addressList = nil
}

func (p DNSProbe) String() string {
	return fmt.Sprintf("Check address list for the URL %v", p.URL)
}

func (p *DNSProbe) Run(_ string) bool {
	var err error

	if p.addressList == nil {
		p.addressList, err = getAddressListByURL(p.URL, p.timeout())
		return err == nil
	}

	err = p.checkAddressListForChange()
	return err == nil
}

func (p *DNSProbe) checkAddressListForChange() error {
	currentAddressList, err := getAddressListByURL(p.URL, p.timeout())
	if err != nil {
		return err
	}

	if !isAddressListsEqual(p.addressList, currentAddressList) {
		return fmt.Errorf("address list has changed for the URL: %v", p.URL)
	}

	p.addressList = currentAddressList

	return nil
}

func isAddressListsEqual(a, b []net.IP) bool {
	if len(a) != len(b) {
		return false
	}

	toSa := transformIPSliceToStringSlice(a)
	toSb := transformIPSliceToStringSlice(b)

	sort.Strings(toSa)
	sort.Strings(toSb)

	for i := range toSa {
		if toSa[i] != toSb[i] {
			return false
		}
	}

	return true
}

func transformIPSliceToStringSlice(ips []net.IP) []string {
	strs := make([]string, len(ips))
	for i, ip := range ips {
		strs[i] = ip.String()
	}

	return strs
}

func (p *DNSProbe) timeout() time.Duration {
	if p.Timeout.Duration > 0 {
		return p.Timeout.Duration
	}
	return DefaultDNSProbeTimeout
}

// lookupIP resolves a name, replaced in tests
var lookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

func getAddressListByURL(url string, timeout time.Duration) (addressList []net.IP, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	addressList, err = lookupIP(ctx, url)
	if err != nil {
		return nil, err
	}

	return addressList, nil
}
