package runtime

import (
	"context"
	"errors"
	"time"
)

// NetworkPolicy is a sandbox's outbound rules, in the syntax egress.ParseRules
// accepts. With no rules a sandbox reaches the public internet. The ranges of
// the platform (private, loopback, link-local, metadata) are refused whatever
// the rules say.
type NetworkPolicy struct {
	AllowOut []string
	DenyOut  []string
}

// NetworkDenial is one outbound connection the sandbox's policy refused.
type NetworkDenial struct {
	Time        time.Time
	Destination string // address and port the sandbox tried to reach
	Host        string // the name it asked for, when the protocol carried one
	Reason      string
}

// ErrNoNetwork is returned by a NetworkDriver for a sandbox that has no network,
// such as one that runs on a runtime the driver cannot give one to.
var ErrNoNetwork = errors.New("the sandbox has no network")

// NetworkDriver is the optional Driver interface behind the "network"
// capability: sandboxes have a network interface whose outbound traffic obeys a
// NetworkPolicy given in CreateRequest.
type NetworkDriver interface {
	// UpdateNetwork replaces the policy of a running sandbox. It applies to
	// connections made afterwards. Rules the driver cannot parse are an
	// ErrInvalidRequest and leave the policy in force unchanged.
	UpdateNetwork(ctx context.Context, id SandboxID, policy NetworkPolicy) error
	// NetworkDenials returns the most recent refused connections, oldest first,
	// at most limit of them.
	NetworkDenials(ctx context.Context, id SandboxID, limit int) ([]NetworkDenial, error)
}
