package firewall

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/safing/portmaster/detection/dga"
	"github.com/safing/portmaster/netenv"
	"golang.org/x/net/publicsuffix"

	"github.com/safing/portbase/log"
	"github.com/safing/portmaster/network"
	"github.com/safing/portmaster/network/netutils"
	"github.com/safing/portmaster/network/packet"
	"github.com/safing/portmaster/network/state"
	"github.com/safing/portmaster/process"
	"github.com/safing/portmaster/profile"
	"github.com/safing/portmaster/profile/endpoints"

	"github.com/agext/levenshtein"
)

// Call order:
//
// DNS Query:
// 1. DecideOnConnection
//    is called when a DNS query is made, may set verdict to Undeterminable to permit a DNS reply.
//    is called with a nil packet.
// 2. DecideOnResolvedDNS
//    is called to (possibly) filter out A/AAAA records that the filter would deny later.
//
// Network Connection:
// 3. DecideOnConnection
//    is called with the first packet of a network connection.

const noReasonOptionKey = ""

type deciderFn func(context.Context, *network.Connection, *profile.LayeredProfile, packet.Packet) bool

var defaultDeciders = []deciderFn{
	checkPortmasterConnection,
	checkSelfCommunication,
	checkConnectionType,
	checkConnectionScope,
	checkEndpointLists,
	checkResolverScope,
	checkConnectivityDomain,
	checkBypassPrevention,
	checkFilterLists,
	dropInbound,
	checkDomainHeuristics,
	checkAutoPermitRelated,
}

var dnsFromSystemResolverDeciders = []deciderFn{
	checkConnectivityDomain,
	checkBypassPrevention,
}

// DecideOnConnection makes a decision about a connection.
// When called, the connection and profile is already locked.
func DecideOnConnection(ctx context.Context, conn *network.Connection, pkt packet.Packet) {
	// Check if we have a process and profile.
	layeredProfile := conn.Process().Profile()
	if layeredProfile == nil {
		conn.Deny("unknown process or profile", noReasonOptionKey)
		return
	}

	// Check if the layered profile needs updating.
	if layeredProfile.NeedsUpdate() {
		// Update revision counter in connection.
		conn.ProfileRevisionCounter = layeredProfile.Update()
		conn.SaveWhenFinished()

		// Reset verdict for connection.
		log.Tracer(ctx).Infof("filter: re-evaluating verdict on %s", conn)
		conn.Verdict = network.VerdictUndecided

		// Reset entity if it exists.
		if conn.Entity != nil {
			conn.Entity.ResetLists()
		}
	} else {
		// Check if the revision counter of the connection needs updating.
		revCnt := layeredProfile.RevisionCnt()
		if conn.ProfileRevisionCounter != revCnt {
			conn.ProfileRevisionCounter = revCnt
			conn.SaveWhenFinished()
		}
	}

	// DNS request from the system resolver require a special decision process,
	// because the original requesting process is not known. Here, we only check
	// global-only and the most important per-app aspects. The resulting
	// connection is then blocked when the original requesting process is known.
	if conn.Type == network.DNSRequest && conn.Process().IsSystemResolver() {
		// Run all deciders and return if they came to a conclusion.
		done, _ := runDeciders(ctx, dnsFromSystemResolverDeciders, conn, layeredProfile, pkt)
		if !done {
			conn.Accept("allowing system resolver dns request", noReasonOptionKey)
		}
		return
	}

	// Run all deciders and return if they came to a conclusion.
	done, defaultAction := runDeciders(ctx, defaultDeciders, conn, layeredProfile, pkt)
	if done {
		return
	}

	// Deciders did not conclude, use default action.
	switch defaultAction {
	case profile.DefaultActionPermit:
		conn.Accept("allowed by default action", profile.CfgOptionDefaultActionKey)
	case profile.DefaultActionAsk:
		prompt(ctx, conn, pkt)
	default:
		conn.Deny("blocked by default action", profile.CfgOptionDefaultActionKey)
	}
}

func runDeciders(ctx context.Context, selectedDeciders []deciderFn, conn *network.Connection, layeredProfile *profile.LayeredProfile, pkt packet.Packet) (done bool, defaultAction uint8) {
	// Read-lock all the profiles.
	layeredProfile.LockForUsage()
	defer layeredProfile.UnlockForUsage()

	// Go though all deciders, return if one sets an action.
	for _, decider := range selectedDeciders {
		if decider(ctx, conn, layeredProfile, pkt) {
			return true, profile.DefaultActionNotSet
		}
	}

	// Return the default action.
	return false, layeredProfile.DefaultAction()
}

// checkPortmasterConnection allows all connection that originate from
// portmaster itself.
func checkPortmasterConnection(ctx context.Context, conn *network.Connection, _ *profile.LayeredProfile, pkt packet.Packet) bool {
	// Grant own outgoing connections.
	if conn.Process().Pid == ownPID &&
		(pkt == nil || pkt.IsOutbound()) {
		log.Tracer(ctx).Infof("filter: granting own connection %s", conn)
		conn.Accept("connection by Portmaster", noReasonOptionKey)
		conn.Internal = true
		return true
	}

	return false
}

// checkSelfCommunication checks if the process is communicating with itself.
func checkSelfCommunication(ctx context.Context, conn *network.Connection, _ *profile.LayeredProfile, pkt packet.Packet) bool {
	// check if process is communicating with itself
	if pkt != nil {
		// TODO: evaluate the case where different IPs in the 127/8 net are used.
		pktInfo := pkt.Info()
		if conn.Process().Pid >= 0 && pktInfo.Src.Equal(pktInfo.Dst) {
			// get PID
			otherPid, _, err := state.Lookup(&packet.Info{
				Inbound:  !pktInfo.Inbound, // we want to know the process on the other end
				Version:  pktInfo.Version,
				Protocol: pktInfo.Protocol,
				Src:      pktInfo.Src,
				SrcPort:  pktInfo.SrcPort,
				Dst:      pktInfo.Dst,
				DstPort:  pktInfo.DstPort,
			}, true)
			if err != nil {
				log.Tracer(ctx).Warningf("filter: failed to find local peer process PID: %s", err)
			} else {
				// get primary process
				otherProcess, err := process.GetOrFindProcess(ctx, otherPid)
				if err != nil {
					log.Tracer(ctx).Warningf("filter: failed to find load local peer process with PID %d: %s", otherPid, err)
				} else if otherProcess.Path == conn.Process().Path {
					conn.Accept("process internal connection", noReasonOptionKey)
					conn.Internal = true
					return true
				}
			}
		}
	}

	return false
}

func checkEndpointLists(ctx context.Context, conn *network.Connection, p *profile.LayeredProfile, _ packet.Packet) bool {
	var result endpoints.EPResult
	var reason endpoints.Reason

	// check endpoints list
	var optionKey string
	if conn.Inbound {
		result, reason = p.MatchServiceEndpoint(ctx, conn.Entity)
		optionKey = profile.CfgOptionServiceEndpointsKey
	} else {
		result, reason = p.MatchEndpoint(ctx, conn.Entity)
		optionKey = profile.CfgOptionEndpointsKey
	}
	switch result {
	case endpoints.Denied:
		conn.DenyWithContext(reason.String(), optionKey, reason.Context())
		return true
	case endpoints.Permitted:
		conn.AcceptWithContext(reason.String(), optionKey, reason.Context())
		return true
	}

	return false
}

func checkConnectionType(ctx context.Context, conn *network.Connection, p *profile.LayeredProfile, _ packet.Packet) bool {
	switch {
	case conn.Type != network.IPConnection:

		// Decider only applies to IP connections.
		return false

	case conn.Inbound &&
		!conn.Entity.IPScope.IsLocalhost() &&
		p.BlockInbound():

		// BlockInbound does not apply to the Localhost scope.
		conn.Drop("inbound connections blocked", profile.CfgOptionBlockInboundKey)
		return true

	case conn.Entity.IPScope.IsGlobal() &&
		conn.Entity.Domain == "" &&
		p.BlockP2P():

		// BlockP2P only applies to the Global scope.
		conn.Block("direct connections (P2P) blocked", profile.CfgOptionBlockP2PKey)
		return true

	default:

		return false
	}
}

func checkConnectivityDomain(_ context.Context, conn *network.Connection, p *profile.LayeredProfile, _ packet.Packet) bool {
	switch {
	case conn.Entity.Domain == "":
		// Only applies if a domain is available.
		return false

	case netenv.GetOnlineStatus() > netenv.StatusPortal:
		// Special grant only applies if network status is Portal (or even more limited).
		return false

	case conn.Inbound:
		// Special grant only applies to outgoing connections.
		return false

	case p.BlockScopeInternet():
		// Special grant only applies if application is allowed to connect to the Internet.
		return false

	case netenv.IsConnectivityDomain(conn.Entity.Domain):
		// Special grant!
		conn.Accept("special grant for connectivity domain during network bootstrap", noReasonOptionKey)
		return true

	default:
		// Not a special grant domain
		return false
	}
}

func checkConnectionScope(_ context.Context, conn *network.Connection, p *profile.LayeredProfile, _ packet.Packet) bool {
	// If we are handling a DNS request, check if we can immediately block it.
	if conn.Type == network.DNSRequest {
		// DNS is expected to resolve to LAN or Internet addresses.
		// Localhost queries are immediately responded to by the nameserver.
		if p.BlockScopeInternet() && p.BlockScopeLAN() {
			conn.Block("Internet and LAN access blocked", profile.CfgOptionBlockScopeInternetKey)
			return true
		}

		return false
	}

	// Check if the network scope is permitted.
	switch conn.Entity.IPScope {
	case netutils.Global, netutils.GlobalMulticast:
		if p.BlockScopeInternet() {
			conn.Deny("Internet access blocked", profile.CfgOptionBlockScopeInternetKey) // Block Outbound / Drop Inbound
			return true
		}
	case netutils.SiteLocal, netutils.LinkLocal, netutils.LocalMulticast:
		if p.BlockScopeLAN() {
			conn.Block("LAN access blocked", profile.CfgOptionBlockScopeLANKey) // Block Outbound / Drop Inbound
			return true
		}
	case netutils.HostLocal:
		if p.BlockScopeLocal() {
			conn.Block("Localhost access blocked", profile.CfgOptionBlockScopeLocalKey) // Block Outbound / Drop Inbound
			return true
		}
	default: // netutils.Unknown and netutils.Invalid
		conn.Deny("invalid IP", noReasonOptionKey) // Block Outbound / Drop Inbound
		return true
	}

	return false
}

func checkBypassPrevention(_ context.Context, conn *network.Connection, p *profile.LayeredProfile, _ packet.Packet) bool {
	if p.PreventBypassing() {
		// check for bypass protection
		result, reason, reasonCtx := PreventBypassing(conn)
		switch result {
		case endpoints.Denied:
			conn.BlockWithContext("bypass prevention: "+reason, profile.CfgOptionPreventBypassingKey, reasonCtx)
			return true
		case endpoints.Permitted:
			conn.AcceptWithContext("bypass prevention: "+reason, profile.CfgOptionPreventBypassingKey, reasonCtx)
			return true
		case endpoints.NoMatch:
		}
	}
	return false
}

func checkFilterLists(ctx context.Context, conn *network.Connection, p *profile.LayeredProfile, pkt packet.Packet) bool {
	// apply privacy filter lists
	result, reason := p.MatchFilterLists(ctx, conn.Entity)
	switch result {
	case endpoints.Denied:
		conn.DenyWithContext(reason.String(), profile.CfgOptionFilterListsKey, reason.Context())
		return true
	case endpoints.NoMatch:
		// nothing to do
	default:
		log.Tracer(ctx).Debugf("filter: filter lists returned unsupported verdict: %s", result)
	}
	return false
}

func checkResolverScope(_ context.Context, conn *network.Connection, p *profile.LayeredProfile, _ packet.Packet) bool {
	// If the IP address was resolved, check the scope of the resolver.
	switch {
	case conn.Type != network.IPConnection:
		// Only applies to IP connections.
	case !p.RemoveOutOfScopeDNS():
		// Out of scope checking is not active.
	case conn.Resolver == nil:
		// IP address of connection was not resolved.
	case conn.Resolver.IPScope.IsGlobal() &&
		(conn.Entity.IPScope.IsLAN() || conn.Entity.IPScope.IsLocalhost()):
		// Block global resolvers from returning LAN/Localhost IPs.
		conn.Block("DNS server horizon violation: global DNS server returned local IP address", profile.CfgOptionRemoveOutOfScopeDNSKey)
		return true
	case conn.Resolver.IPScope.IsLAN() &&
		conn.Entity.IPScope.IsLocalhost():
		// Block LAN resolvers from returning Localhost IPs.
		conn.Block("DNS server horizon violation: LAN DNS server returned localhost IP address", profile.CfgOptionRemoveOutOfScopeDNSKey)
		return true
	}

	return false
}

func checkDomainHeuristics(ctx context.Context, conn *network.Connection, p *profile.LayeredProfile, _ packet.Packet) bool {
	if !p.DomainHeuristics() {
		return false
	}

	if conn.Entity.Domain == "" {
		return false
	}

	trimmedDomain := strings.TrimRight(conn.Entity.Domain, ".")
	etld1, err := publicsuffix.EffectiveTLDPlusOne(trimmedDomain)
	if err != nil {
		// we don't apply any checks here and let the request through
		// because a malformed domain-name will likely be dropped by
		// checks better suited for that.
		log.Tracer(ctx).Warningf("filter: failed to get eTLD+1: %s", err)
		return false
	}

	domainToCheck := strings.Split(etld1, ".")[0]
	score := dga.LmsScore(domainToCheck)
	if score < 5 {
		log.Tracer(ctx).Debugf(
			"filter: possible data tunnel by %s in eTLD+1 %s: %s has an lms score of %.2f",
			conn.Process(),
			etld1,
			domainToCheck,
			score,
		)
		conn.Block("possible DGA domain commonly used by malware", profile.CfgOptionDomainHeuristicsKey)
		return true
	}
	log.Tracer(ctx).Tracef("filter: LMS score of eTLD+1 %s is %.2f", etld1, score)

	// 100 is a somewhat arbitrary threshold to ensure we don't mess
	// around with CDN domain names to early. They use short second-level
	// domains that would trigger LMS checks but are to small to actually
	// exfiltrate data.
	if len(conn.Entity.Domain) > len(etld1)+100 {
		domainToCheck = trimmedDomain[0:len(etld1)]
		score := dga.LmsScoreOfDomain(domainToCheck)
		if score < 10 {
			log.Tracer(ctx).Debugf(
				"filter: possible data tunnel by %s in subdomain of %s: %s has an lms score of %.2f",
				conn.Process(),
				conn.Entity.Domain,
				domainToCheck,
				score,
			)
			conn.Block("possible data tunnel for covert communication and protection bypassing", profile.CfgOptionDomainHeuristicsKey)
			return true
		}
		log.Tracer(ctx).Tracef("filter: LMS score of entire domain is %.2f", score)
	}

	return false
}

func dropInbound(_ context.Context, conn *network.Connection, _ *profile.LayeredProfile, _ packet.Packet) bool {
	// implicit default=block for inbound
	if conn.Inbound {
		conn.Drop("incoming connection blocked by default", profile.CfgOptionServiceEndpointsKey)
		return true
	}
	return false
}

func checkAutoPermitRelated(_ context.Context, conn *network.Connection, p *profile.LayeredProfile, _ packet.Packet) bool {
	// Auto permit is disabled for default action permit.
	if p.DefaultAction() == profile.DefaultActionPermit {
		return false
	}

	// Check if auto permit is disabled.
	if p.DisableAutoPermit() {
		return false
	}

	// Check for relation to auto permit.
	related, reason := checkRelation(conn)
	if related {
		conn.Accept(reason, profile.CfgOptionDisableAutoPermitKey)
		return true
	}

	return false
}

// checkRelation tries to find a relation between a process and a communication. This is for better out of the box experience and is _not_ meant to thwart intentional malware.
func checkRelation(conn *network.Connection) (related bool, reason string) {
	if conn.Entity.Domain != "" {
		return false, ""
	}
	// don't check for unknown processes
	if conn.Process().Pid < 0 {
		return false, ""
	}

	pathElements := strings.Split(conn.Process().Path, string(filepath.Separator))
	// only look at the last two path segments
	if len(pathElements) > 2 {
		pathElements = pathElements[len(pathElements)-2:]
	}
	domainElements := strings.Split(conn.Entity.Domain, ".")

	var domainElement string
	var processElement string

matchLoop:
	for _, domainElement = range domainElements {
		for _, pathElement := range pathElements {
			if levenshtein.Match(domainElement, pathElement, nil) > 0.5 {
				related = true
				processElement = pathElement
				break matchLoop
			}
		}
		if levenshtein.Match(domainElement, conn.Process().Name, nil) > 0.5 {
			related = true
			processElement = conn.Process().Name
			break matchLoop
		}
		if levenshtein.Match(domainElement, conn.Process().ExecName, nil) > 0.5 {
			related = true
			processElement = conn.Process().ExecName
			break matchLoop
		}
	}

	if related {
		reason = fmt.Sprintf("auto allowed: domain is related to process: %s is related to %s", domainElement, processElement)
	}
	return related, reason
}
