package process

import (
	"fmt"
	"net"
	"sort"
)

// DNSProbe fails when the address list of URL changes since the process
// start, so the process is restarted after failureThreshold and connects to
// the new addresses (e.g. a client that resolves a broker only once)
type DNSProbe struct {
	URL         string `yaml:"url"`
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
		p.addressList, err = getAddressListByURL(p.URL)
		return err == nil
	}

	err = p.checkAddressListForChange()
	return err == nil
}

func (p *DNSProbe) checkAddressListForChange() error {
	currentAddressList, err := getAddressListByURL(p.URL)
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

func getAddressListByURL(url string) (addressList []net.IP, err error) {
	addressList, err = net.LookupIP(url)
	if err != nil {
		return nil, err
	}

	return addressList, nil
}
