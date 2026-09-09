package relay

import (
	"fmt"
	"sort"
	"strings"
)

// SharedPeer is a device another namespace has granted this caller access to.
// Its target is always namespace-qualified: a bare label is looked up in the
// caller's own namespace first, so only the qualified form is unambiguous.
type SharedPeer struct {
	Owner  string `json:"owner"`
	Device string `json:"device"`
	Label  string `json:"label"`
	Target string `json:"target"`
	Perms  string `json:"perms"`
	Online bool   `json:"online"`
}

// sharedPeers lists the devices shared with ns. Grants live in the admin
// database, so a relay running on static tokens has none and returns nil.
//
// Without this a grantee had no way to discover a device shared with them:
// /peers only ever reported their own namespace, so they could not learn the
// owner namespace that --target needs, and every guess came back 403.
func (r *Relay) sharedPeers(ns string) []SharedPeer {
	if r.admin == nil || ns == "" {
		return nil
	}
	rows, err := r.admin.ListDevices(ns)
	if err != nil {
		return nil
	}
	var out []SharedPeer
	for _, row := range rows {
		if shared, _ := row["shared"].(bool); !shared {
			continue
		}
		owner, _ := row["owner"].(string)
		// `name` is the canonical route: the UUID once upgraded, the old name until then.
		device, _ := row["name"].(string)
		if owner == "" || owner == ns || device == "" {
			continue
		}
		perms, _ := row["perms"].(string)
		out = append(out, SharedPeer{
			Owner:  owner,
			Device: device,
			Label:  sharedPeerLabel(row),
			Target: owner + "/" + device,
			Perms:  perms,
			Online: r.deviceLive(owner, device),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Owner != out[j].Owner {
			return out[i].Owner < out[j].Owner
		}
		return out[i].Device < out[j].Device
	})
	return out
}

// sharedPeerLabel prefers what the owner named the device over its route.
func sharedPeerLabel(row map[string]any) string {
	for _, key := range []string{"alias", "display_name", "legacy_name", "name"} {
		if v, _ := row[key].(string); v != "" {
			return v
		}
	}
	return ""
}

// resolveShared matches a bare target against the devices shared with callerNS.
// Owners and grantees name devices independently, so a label may repeat across
// namespaces; that is refused rather than resolved by grant order.
func (r *Relay) resolveShared(callerNS, target string) (SharedPeer, string, bool) {
	if target == "" {
		return SharedPeer{}, "", false
	}
	var matches []SharedPeer
	for _, peer := range r.sharedPeers(callerNS) {
		if peer.Device == target || strings.EqualFold(peer.Label, target) {
			matches = append(matches, peer)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], "", true
	case 0:
		return SharedPeer{}, noSuchDeviceReason(callerNS, target), false
	default:
		owners := make([]string, 0, len(matches))
		for _, m := range matches {
			owners = append(owners, m.Target)
		}
		return SharedPeer{}, fmt.Sprintf("device %q is shared with you by more than one namespace (%s); address it as owner/device",
			target, strings.Join(owners, ", ")), false
	}
}

// noSuchDeviceReason explains a 403 for a target in the caller's own namespace.
// It is only ever used there: saying whether a device exists in someone else's
// namespace would answer a question the caller has no grant to ask.
func noSuchDeviceReason(callerNS, target string) string {
	return fmt.Sprintf("no device %q in namespace %q; a device shared with you is addressed as owner/device — run `wanctl peers` to list both", target, callerNS)
}

// peersBody is the /peers and /h/peers response. `devices` and `aliases` keep
// their exact previous meaning - the caller's own online devices - so existing
// consumers are unaffected; `shared` is added only when there is something to add.
func peersBody(ns string, devices []string, aliases map[string]string, shared []SharedPeer) map[string]any {
	out := map[string]any{"namespace": ns, "devices": devices, "aliases": aliases}
	if len(shared) > 0 {
		out["shared"] = shared
	}
	return out
}

// dialRefusal keeps the historic bare "forbidden" when there is nothing the
// caller may be told, so a probe of someone else's namespace learns nothing.
func dialRefusal(reason string) string {
	if reason == "" {
		return "forbidden"
	}
	return reason
}
