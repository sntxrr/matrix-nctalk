// matrix-nctalk - A Matrix–Nextcloud Talk puppeting bridge.
// Copyright (C) 2026 Don O'Neill
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package connector

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// serverResolveTimeout bounds the lookup done while checking where a server
// address points, so a slow resolver cannot hold a login open.
const serverResolveTimeout = 5 * time.Second

// resolver is the name lookup used when checking a server address. Tests
// substitute one; production uses the system resolver.
type hostResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// checkServerAddress refuses a Nextcloud address that points somewhere the
// bridge's own network can reach but the user should not be able to aim it at.
//
// The login flow makes the bridge fetch a URL of the user's choosing, which
// without this is a way for anyone permitted to use the bridge to probe the
// network it runs in — link-local metadata services above all. Naming a host in
// allowed_servers is what marks an internal one as deliberate, which is why a
// bridge pointed at a Nextcloud on the same private network keeps working
// without new config.
//
// This is not proof against DNS rebinding: the name is resolved here and again
// when the request is made, and a hostile resolver can answer differently.
// Closing that needs a dialer that checks the address it actually connects to.
// What this does stop is a user typing an internal address or a name that
// plainly resolves to one.
func (nc *NCTalkConnector) checkServerAddress(ctx context.Context, host string) error {
	// An explicitly listed host is the operator saying they meant it.
	if nc.Config.ServerExplicitlyAllowed(host) {
		return nil
	}

	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	hostname = strings.Trim(hostname, "[]")

	if ip := net.ParseIP(hostname); ip != nil {
		if internalIP(ip) {
			return internalAddressError(host, ip)
		}
		return nil
	}

	// "localhost" usually resolves to a loopback address, but it does not have
	// to, and the name is the clearest possible statement of intent.
	if strings.EqualFold(hostname, "localhost") || strings.HasSuffix(strings.ToLower(hostname), ".localhost") {
		return internalAddressError(host, net.IPv4(127, 0, 0, 1))
	}

	lookupCtx, cancel := context.WithTimeout(ctx, serverResolveTimeout)
	defer cancel()
	addrs, err := nc.resolver().LookupIPAddr(lookupCtx, hostname)
	if err != nil {
		// Not reachable is the caller's problem to report; this check only
		// refuses addresses it can see are internal.
		return nil
	}
	for _, addr := range addrs {
		if internalIP(addr.IP) {
			return internalAddressError(host, addr.IP)
		}
	}
	return nil
}

func (nc *NCTalkConnector) resolver() hostResolver {
	if nc.dnsResolver != nil {
		return nc.dnsResolver
	}
	return net.DefaultResolver
}

// internalIP reports whether an address belongs to the network the bridge runs
// in rather than the public internet.
func internalIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

func internalAddressError(host string, ip net.IP) error {
	return fmt.Errorf(
		"%s is on a private or loopback address (%s), which this bridge will not connect to;\n"+
			"if that is really where your Nextcloud is, add it to network.allowed_servers in the bridge config",
		host, ip)
}
