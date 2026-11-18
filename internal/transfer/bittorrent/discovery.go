package bittorrent

import (
	"errors"
	"fmt"
	"strings"

	at "github.com/anacrolix/torrent"
)

// ErrInvalidDiscovery means the requested discovery profile is not one this
// adapter implements. It is a configuration error, never a reason to fall back
// to a more permissive profile.
var ErrInvalidDiscovery = errors.New("unsupported discovery profile")

// directListenHost is the only address a direct-mode client binds. It is
// applied to every network (tcp and, when uTP were enabled, udp).
const directListenHost = "127.0.0.1"

// Discovery selects which network branches a run is allowed to use.
//
// The zero value is DiscoveryPublic, so an unconfigured Config keeps the
// behaviour of earlier builds.
type Discovery string

const (
	// DiscoveryPublic is the historical behaviour: trackers, DHT, PEX and
	// explicitly configured peers may all contribute peer addresses. Webseeds
	// and WebTorrent stay disabled (they always have been here), and UPnP /
	// NAT-PMP port mapping is never requested.
	DiscoveryPublic Discovery = "public"

	// DiscoveryDirect uses only the peers the caller named. Trackers are not
	// announced to or scraped, no DHT is created, PEX is neither advertised nor
	// accepted, no webseed or metainfo source is added, and magnet-supplied
	// peer addresses are ignored. The listener binds loopback, so no unsolicited
	// connection from anywhere else can arrive, and uTP/IPv6 are off so the
	// client opens no UDP socket at all.
	//
	// Name resolution still happens for the peers the caller configured: those
	// addresses are the caller's instruction, and the system resolver is used
	// for them. Nothing else in a direct run resolves a name, because no
	// tracker, webseed, metainfo source or DHT bootstrap node exists to resolve.
	DiscoveryDirect Discovery = "direct"
)

// ParseDiscovery validates a profile name. The empty string and "public" both
// select the default; matching is case-insensitive and ignores surrounding
// space (the CLI reads this from a flag). Anything else is ErrInvalidDiscovery
// naming the valid values — there is no silent fallback.
func ParseDiscovery(raw string) (Discovery, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(DiscoveryPublic):
		return DiscoveryPublic, nil
	case string(DiscoveryDirect):
		return DiscoveryDirect, nil
	}
	return "", fmt.Errorf("%w: %q (want %q or %q)", ErrInvalidDiscovery, raw, DiscoveryPublic, DiscoveryDirect)
}

// String renders the profile, never empty.
func (d Discovery) String() string {
	if d == DiscoveryDirect {
		return string(DiscoveryDirect)
	}
	return string(DiscoveryPublic)
}

// IsDirect reports whether this profile is the explicit-peers-only one. Both
// the zero value and DiscoveryPublic are not direct.
func (d Discovery) IsDirect() bool { return d == DiscoveryDirect }

// applyClientDiscovery applies a profile to a library client config. It runs
// before the client is constructed, so a discoverer the profile forbids never
// exists — there is nothing to switch off later, and nothing that can fall back.
//
// Library paths this closes (pinned v1.62.0):
//   - NoDHT: client.go only builds anacrolix/dht servers when it is false, and
//     the DHT server is what shares the uTP UDP socket and calls the bootstrap
//     address lookup.
//   - DisableTrackers: client-tracker-announcer.go skips pending announce
//     updates entirely ("With trackers disabled there is nothing to announce"),
//     and torrent.go skips scraping.
//   - DisablePEX: the ut_pex extension is not advertised (client.go
//     makeBuiltinLtepProtocols / addBuiltinLtepProtocols) and incoming PEX
//     messages are dropped (pexconn.go).
//   - LocalServiceDiscovery is left nil: BEP 14 LPD is opt-in in this library
//     and this adapter never sets it, in either profile.
//   - DisableIPv6 + ListenHost loopback: the listener is `127.0.0.1:<port>` and
//     no `tcp6`/`udp6` socket is opened, so an inbound connection can only come
//     from the local host.
func applyClientDiscovery(cc *at.ClientConfig, d Discovery) {
	if !d.IsDirect() {
		return
	}
	cc.NoDHT = true
	cc.DisableTrackers = true
	cc.DisablePEX = true
	cc.DisableWebseeds = true
	cc.DisableWebtorrent = true
	cc.DisableUTP = true
	cc.DisableIPv6 = true
	cc.NoDefaultPortForwarding = true
	cc.LocalServiceDiscovery = nil
	cc.ListenHost = func(string) string { return directListenHost }
}

// applySpecDiscovery strips network hints a source carried. They are the same
// class of branch as the client switches above, but they travel with the
// metainfo or the magnet:
//
//   - trackers (magnet tr=, .torrent announce/announce-list),
//   - webseeds (magnet ws=, .torrent url-list),
//   - metainfo sources (magnet xs=/as=), which the library fetches over HTTP,
//   - DHT node hints (.torrent nodes),
//   - peer addresses (magnet x.pe), which MergeSpec turns into dials.
//
// Only the additive spec fields are touched: InfoBytes, the info hashes and the
// piece layers describe the torrent's identity and content, and a discovery
// policy has no business editing them.
func applySpecDiscovery(spec *at.TorrentSpec, d Discovery) {
	if spec == nil || !d.IsDirect() {
		return
	}
	spec.Trackers = nil
	spec.Webseeds = nil
	spec.Sources = nil
	spec.DhtNodes = nil
	spec.PeerAddrs = nil
}
