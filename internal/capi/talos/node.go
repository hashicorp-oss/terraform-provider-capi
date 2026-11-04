// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package talos

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cosi-project/runtime/pkg/safe"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/network"
	runtimeres "github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Node is the Talos API surface the reconciler needs.
type Node interface {
	// Probe classifies what answers on the Talos API port. tc may be nil
	// before secrets exist; a configured node is then reported Foreign.
	Probe(ctx context.Context, tc *clientconfig.Config) (TalosState, error)
	// ApplyConfiguration applies a machine config to a node in maintenance
	// mode with reboot mode.
	ApplyConfiguration(ctx context.Context, cfg []byte) error
	// Bootstrap bootstraps etcd on a configured node.
	Bootstrap(ctx context.Context, tc *clientconfig.Config) error
	// EtcdState reports whether etcd has members on a configured node.
	EtcdState(ctx context.Context, tc *clientconfig.Config) (EtcdState, error)
	// Kubeconfig fetches the admin kubeconfig from a configured node.
	Kubeconfig(ctx context.Context, tc *clientconfig.Config) ([]byte, error)
	// Reset wipes the node's system disk without leaving etcd and halts it.
	Reset(ctx context.Context, tc *clientconfig.Config) error
	// ResetToMaintenance wipes the STATE and EPHEMERAL partitions and reboots the
	// node, so it returns without a machine config — in maintenance mode — ready
	// to be re-provisioned. Requires Talos API access (an "ours" node); this is
	// the graceful alternative to the one-shot reset (wipe) boot image.
	ResetToMaintenance(ctx context.Context, tc *clientconfig.Config) error
	// Stage reports the Talos machine stage (running, booting, installing, ...)
	// for observability — a freshly-installed control plane sits in "booting"
	// until etcd is bootstrapped, so it is logged, not used to gate progress.
	// Requires API access; returns "" on error.
	Stage(ctx context.Context, tc *clientconfig.Config) (string, error)
	// PinNetwork writes the node's identity (hostname, address, route,
	// resolvers) to META as metal platform network configuration, so it
	// survives a machine-config apply that no longer carries it.
	PinNetwork(ctx context.Context, tc *clientconfig.Config, pin NetworkPin) error
}

// MachineryNode implements Node with the Talos machinery client.
type MachineryNode struct {
	// endpoint is "ip" or "ip:port"; the client appends port 50000 when missing.
	endpoint    string
	dialTimeout time.Duration
	// opTimeout bounds a single API call (dial + RPC). Without it, calls like
	// EtcdMemberList block indefinitely while the node is up but its etcd is
	// still "waiting to join the cluster" (pre-bootstrap), which would freeze the
	// reconciler's observe loop.
	opTimeout time.Duration
}

// NewMachineryNode returns a Node for the Talos API at ip (port 50000).
func NewMachineryNode(ip string) *MachineryNode {
	return &MachineryNode{endpoint: ip, dialTimeout: 10 * time.Second, opTimeout: 20 * time.Second}
}

// opCtx bounds a single Talos API call so a node that accepts connections but
// whose service is not yet serving cannot block the caller forever.
func (n *MachineryNode) opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if n.opTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, n.opTimeout)
}

// insecureClient dials without verifying the server certificate and records
// the server certificate's common name into cn.
func (n *MachineryNode) insecureClient(ctx context.Context, cn *string) (*client.Client, error) {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: true, // maintenance mode has no CA; identity is checked via CN
		MinVersion:         tls.VersionTLS12,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) > 0 {
				*cn = cs.PeerCertificates[0].Subject.CommonName
			}
			return nil
		},
	}
	return client.New(ctx, client.WithEndpoints(n.endpoint), client.WithTLSConfig(tlsCfg))
}

// authClient dials with this run's talosconfig, overriding endpoints to the node.
func (n *MachineryNode) authClient(ctx context.Context, tc *clientconfig.Config) (*client.Client, error) {
	return client.New(ctx, client.WithConfig(tc), client.WithEndpoints(n.endpoint))
}

// Probe implements Node.
func (n *MachineryNode) Probe(ctx context.Context, tc *clientconfig.Config) (TalosState, error) {
	ctx, cancel := context.WithTimeout(ctx, n.dialTimeout)
	defer cancel()

	var serverCN string
	c, err := n.insecureClient(ctx, &serverCN)
	if err != nil {
		return TalosUnreachable, nil
	}
	_, verr := c.Version(ctx)
	_ = c.Close()

	// No certificate seen means no TLS handshake completed: nothing is listening.
	if serverCN == "" {
		return TalosUnreachable, nil
	}
	if verr == nil && serverCN == constants.MaintenanceServiceCommonName {
		return TalosMaintenance, nil
	}
	if serverCN == constants.MaintenanceServiceCommonName {
		// Maintenance service answered TLS but the RPC failed; try again later.
		return TalosUnreachable, nil
	}

	// A configured node presented a certificate. Is it ours?
	if tc == nil {
		return TalosForeign, nil
	}
	ac, err := n.authClient(ctx, tc)
	if err != nil {
		return TalosForeign, nil
	}
	defer func() { _ = ac.Close() }()
	if _, err := ac.Version(ctx); err != nil {
		return TalosForeign, nil
	}
	return TalosOurs, nil
}

// ApplyConfiguration implements Node.
func (n *MachineryNode) ApplyConfiguration(ctx context.Context, cfg []byte) error {
	var cn string
	c, err := n.insecureClient(ctx, &cn)
	if err != nil {
		return fmt.Errorf("dialing maintenance service: %w", err)
	}
	defer func() { _ = c.Close() }()

	_, err = c.ApplyConfiguration(ctx, &machineapi.ApplyConfigurationRequest{
		Data: cfg,
		Mode: machineapi.ApplyConfigurationRequest_REBOOT,
	})
	if err != nil {
		// A REBOOT-mode apply tears down the maintenance apid as the node
		// leaves maintenance for the configured system, so the RPC frequently
		// fails to return even though the config was accepted: the connection
		// drops (Unavailable/EOF/"transport is closing") or the now-configured
		// apid demands a client certificate ("certificate required"). Treat
		// those as applied — the caller confirms by waiting for the node to
		// reboot and come back — while still surfacing a genuine rejection
		// (e.g. an invalid config, which returns InvalidArgument).
		if isMaintenanceTeardownErr(ctx, err) {
			return nil
		}
		return fmt.Errorf("applying configuration: %w", err)
	}
	return nil
}

// isMaintenanceTeardownErr reports whether err is the expected fallout of a
// REBOOT-mode ApplyConfiguration succeeding: the maintenance service goes away
// mid-call as the node transitions, so the RPC never gets a clean response. A
// caller-cancelled context is not counted (that is a real abort), and neither
// are RPC-level rejections such as InvalidArgument.
func isMaintenanceTeardownErr(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	if status.Code(err) == codes.Unavailable {
		return true
	}
	msg := err.Error()
	for _, s := range []string{
		"certificate required",
		"transport is closing",
		"error reading server preface",
		"connection refused",
		"connection reset",
		"EOF",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// Bootstrap implements Node.
func (n *MachineryNode) Bootstrap(ctx context.Context, tc *clientconfig.Config) error {
	ctx, cancel := n.opCtx(ctx)
	defer cancel()

	c, err := n.authClient(ctx, tc)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return c.Bootstrap(ctx, &machineapi.BootstrapRequest{})
}

// EtcdState implements Node. Any error listing members is reported as not
// bootstrapped, because etcd is not serving.
func (n *MachineryNode) EtcdState(ctx context.Context, tc *clientconfig.Config) (EtcdState, error) {
	ctx, cancel := n.opCtx(ctx)
	defer cancel()

	c, err := n.authClient(ctx, tc)
	if err != nil {
		return EtcdUnknown, err
	}
	defer func() { _ = c.Close() }()

	resp, err := c.EtcdMemberList(ctx, &machineapi.EtcdMemberListRequest{})
	if err != nil {
		return EtcdNotBootstrapped, nil
	}
	for _, msg := range resp.GetMessages() {
		if len(msg.GetMembers()) > 0 {
			return EtcdBootstrapped, nil
		}
	}
	return EtcdNotBootstrapped, nil
}

// Kubeconfig implements Node.
func (n *MachineryNode) Kubeconfig(ctx context.Context, tc *clientconfig.Config) ([]byte, error) {
	ctx, cancel := n.opCtx(ctx)
	defer cancel()

	c, err := n.authClient(ctx, tc)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	return c.Kubeconfig(ctx)
}

// Reset implements Node.
func (n *MachineryNode) Reset(ctx context.Context, tc *clientconfig.Config) error {
	ctx, cancel := n.opCtx(ctx)
	defer cancel()

	c, err := n.authClient(ctx, tc)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return c.ResetGeneric(ctx, &machineapi.ResetRequest{Graceful: false, Reboot: false})
}

// Stage implements Node.
func (n *MachineryNode) Stage(ctx context.Context, tc *clientconfig.Config) (string, error) {
	ctx, cancel := n.opCtx(ctx)
	defer cancel()

	c, err := n.authClient(ctx, tc)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()

	ms, err := safe.StateGet[*runtimeres.MachineStatus](ctx, c.COSI, runtimeres.NewMachineStatus().Metadata())
	if err != nil {
		return "", err
	}
	return ms.TypedSpec().Stage.String(), nil
}

// ResetToMaintenance implements Node.
func (n *MachineryNode) ResetToMaintenance(ctx context.Context, tc *clientconfig.Config) error {
	ctx, cancel := n.opCtx(ctx)
	defer cancel()

	c, err := n.authClient(ctx, tc)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	// Wiping STATE removes the stored machine config, so the node reboots without
	// one and comes up in maintenance mode; wiping EPHEMERAL clears its data.
	return c.ResetGeneric(ctx, &machineapi.ResetRequest{
		Graceful: false,
		Reboot:   true,
		SystemPartitionsToWipe: []*machineapi.ResetPartitionSpec{
			{Label: constants.StatePartitionLabel, Wipe: true},
			{Label: constants.EphemeralPartitionLabel, Wipe: true},
		},
	})
}

// PinNetwork implements Node. The link is the one already carrying the pinned
// address, else the one with the pinned MAC, read from the node's network
// status resources.
func (n *MachineryNode) PinNetwork(ctx context.Context, tc *clientconfig.Config, pin NetworkPin) error {
	ctx, cancel := n.opCtx(ctx)
	defer cancel()

	c, err := n.authClient(ctx, tc)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	addrLinks := map[string]string{}
	addrs, err := safe.StateListAll[*network.AddressStatus](ctx, c.COSI)
	if err != nil {
		return fmt.Errorf("listing address statuses: %w", err)
	}
	for a := range addrs.All() {
		addrLinks[a.TypedSpec().Address.Addr().String()] = a.TypedSpec().LinkName
	}
	macLinks := map[string]string{}
	links, err := safe.StateListAll[*network.LinkStatus](ctx, c.COSI)
	if err != nil {
		return fmt.Errorf("listing link statuses: %w", err)
	}
	for l := range links.All() {
		if hw := l.TypedSpec().HardwareAddr.String(); hw != "" {
			macLinks[strings.ToLower(hw)] = l.Metadata().ID()
		}
	}
	linkName, err := pickLinkName(pin, addrLinks, macLinks)
	if err != nil {
		return err
	}
	doc, err := platformNetworkDocument(pin, linkName)
	if err != nil {
		return err
	}
	return c.MetaWrite(ctx, metaNetworkKey, doc)
}
