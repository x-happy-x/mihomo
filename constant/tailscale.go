package constant

import "context"

// TailscalePeer is a view-friendly snapshot of one node in a tailnet.
//
// It deliberately carries only what a dashboard needs to list devices and pick
// an exit node, rather than mirroring the whole upstream peer status.
type TailscalePeer struct {
	ID       string   `json:"id"`
	HostName string   `json:"hostName"`
	DNSName  string   `json:"dnsName"`
	OS       string   `json:"os,omitempty"`
	IPs      []string `json:"ips"`
	Tags     []string `json:"tags,omitempty"`
	Routes   []string `json:"routes,omitempty"`
	Relay    string   `json:"relay,omitempty"`

	// Online reports whether the node is connected to the control plane.
	Online bool `json:"online"`
	// Self marks the node this outbound runs as.
	Self bool `json:"self"`

	// ExitNode reports whether this peer is the exit node currently in use.
	ExitNode bool `json:"exitNode"`
	// ExitNodeOption reports whether this peer may be selected as an exit node,
	// meaning it both offers and is approved for exit node duty.
	ExitNodeOption bool `json:"exitNodeOption"`

	// LastSeen is RFC3339, empty while the node is online or never seen.
	LastSeen string `json:"lastSeen,omitempty"`

	RxBytes int64 `json:"rxBytes"`
	TxBytes int64 `json:"txBytes"`
}

// TailscaleStatus is a snapshot of a tailnet as seen by one outbound.
type TailscaleStatus struct {
	// BackendState is an ipn.State string, e.g. "Running" or "NeedsLogin".
	BackendState string `json:"backendState"`

	Self *TailscalePeer `json:"self,omitempty"`

	// ExitNode is what this outbound is configured to use, empty when none.
	// It is the value accepted by SetExitNode, not necessarily one in effect.
	ExitNode string `json:"exitNode"`
	// ExitNodeActive reports whether traffic is actually leaving through an
	// exit node right now.
	ExitNodeActive bool `json:"exitNodeActive"`

	Peers []TailscalePeer `json:"peers"`
}

// TailscaleAdapter is implemented by outbounds backed by a tailnet. It lets the
// API list that tailnet and change the exit node without a config reload.
//
// Declaring it here keeps the API layer free of the build tags that gate the
// Tailscale outbound itself: without those tags nothing implements it and the
// API simply reports that the proxy is not a Tailscale one.
type TailscaleAdapter interface {
	// TailscaleStatus returns the current tailnet snapshot, starting the
	// outbound first when it has not been dialed yet.
	TailscaleStatus(ctx context.Context) (*TailscaleStatus, error)

	// SetExitNode switches the exit node. The node may be an IP, a hostname or
	// a MagicDNS name; an empty string clears the exit node. The change applies
	// immediately and lasts until the outbound is recreated by a config reload.
	SetExitNode(ctx context.Context, node string) error
}
