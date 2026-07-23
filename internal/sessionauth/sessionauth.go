// Package sessionauth defines relay-issued authorization metadata for a
// controller session. Metadata travels only over the authenticated agent
// control channel; controller traffic cannot declare or modify it.
package sessionauth

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Capabilities uint8

const (
	Exec Capabilities = 1 << iota
	Read
	Write
	Logs
	Console
)

const (
	GrantCapabilities = Exec | Read | Write
	FullCapabilities  = GrantCapabilities | Logs | Console
)

var capabilityNames = []struct {
	name string
	cap  Capabilities
}{
	{"exec", Exec},
	{"read", Read},
	{"write", Write},
	{"logs", Logs},
	{"console", Console},
}

func (c Capabilities) Has(required Capabilities) bool { return c&required == required }

func (c Capabilities) Valid() bool { return c != 0 && c&^FullCapabilities == 0 }

func (c Capabilities) String() string {
	var names []string
	for _, item := range capabilityNames {
		if c.Has(item.cap) {
			names = append(names, item.name)
		}
	}
	return strings.Join(names, ",")
}

func Parse(value string) (Capabilities, error) {
	if strings.TrimSpace(value) == "" {
		return 0, fmt.Errorf("empty capabilities")
	}
	var caps Capabilities
	for _, raw := range strings.Split(value, ",") {
		name := strings.TrimSpace(raw)
		var found Capabilities
		for _, item := range capabilityNames {
			if name == item.name {
				found = item.cap
				break
			}
		}
		if found == 0 {
			return 0, fmt.Errorf("unknown capability %q", name)
		}
		if caps.Has(found) {
			return 0, fmt.Errorf("duplicate capability %q", name)
		}
		caps |= found
	}
	return caps, nil
}

// ParseGrant parses capabilities stored in acl.perms. Administrative console
// and device logs are owner-only and therefore invalid in an ACL grant.
func ParseGrant(value string) (Capabilities, error) {
	caps, err := Parse(value)
	if err != nil {
		return 0, err
	}
	if caps&^GrantCapabilities != 0 {
		return 0, fmt.Errorf("ACL grant contains owner-only capabilities")
	}
	return caps, nil
}

func (c Capabilities) MarshalJSON() ([]byte, error) {
	if !c.Valid() {
		return nil, fmt.Errorf("invalid capabilities %#x", uint8(c))
	}
	return json.Marshal(c.String())
}

func (c *Capabilities) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("capabilities must be a string: %w", err)
	}
	parsed, err := Parse(value)
	if err != nil {
		return err
	}
	*c = parsed
	return nil
}

// Open is relay-authenticated metadata for one E2E controller session.
type Open struct {
	Op              string       `json:"op,omitempty"`
	Session         string       `json:"session"`
	URL             string       `json:"url,omitempty"`
	CallerNamespace string       `json:"caller_namespace"`
	OwnerNamespace  string       `json:"owner_namespace"`
	Device          string       `json:"device"`
	Capabilities    Capabilities `json:"capabilities"`
}

func (o Open) ValidFor(device string) bool {
	return o.Session != "" && o.CallerNamespace != "" && o.OwnerNamespace != "" &&
		o.Device == device && o.Capabilities.Valid()
}
