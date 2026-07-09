package zeroconf

import (
	"net"
	"os"

	"github.com/grandcat/zeroconf"
)

// BuiltinRegistrar implements ServiceRegistrar using the grandcat/zeroconf library,
// which provides a pure-Go mDNS responder.
type BuiltinRegistrar struct {
	server *zeroconf.Server
	ifaces []net.Interface

	serviceType string
	domain      string
	port        int
	txt         []string
}

// NewBuiltinRegistrar creates a new built-in mDNS service registrar.
// If ifaces is empty, the service will be advertised on all interfaces.
func NewBuiltinRegistrar(ifaces []net.Interface) *BuiltinRegistrar {
	return &BuiltinRegistrar{ifaces: ifaces}
}

// Register publishes the service using the built-in mDNS responder.
func (b *BuiltinRegistrar) Register(name, serviceType, domain string, port int, txt []string) error {
	b.serviceType = serviceType
	b.domain = domain
	b.port = port
	b.txt = txt

	// PULSAR_ZEROCONF_HOST (Pulsar node apps): advertise the SRV target under
	// this host name instead of os.Hostname(). On macOS the kernel hostname
	// can drift from the Bonjour LocalHostName (rename, DHCP), leaving the
	// SRV record pointing at a *.local name phones fail to resolve — the
	// speaker shows nowhere. RegisterProxy with nil IPs keeps grandcat's
	// per-interface A/AAAA answering, so only the name changes.
	if host := os.Getenv("PULSAR_ZEROCONF_HOST"); host != "" {
		var err error
		b.server, err = zeroconf.RegisterProxy(name, serviceType, domain, port, host, nil, txt, b.ifaces)
		return err
	}

	var err error
	b.server, err = zeroconf.Register(name, serviceType, domain, port, txt, b.ifaces)
	return err
}

func (b *BuiltinRegistrar) UpdateName(name string) error {
	// Zeroconf library does not support dynamic updates, so we need to restart the server with the new name.
	b.Shutdown()
	return b.Register(name, b.serviceType, b.domain, b.port, b.txt)
}

// Shutdown stops the mDNS responder.
func (b *BuiltinRegistrar) Shutdown() {
	if b.server != nil {
		b.server.Shutdown()
	}
}
