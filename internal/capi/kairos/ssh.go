// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package kairos

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshDialTimeout bounds the TCP connect and the SSH handshake of one call.
const sshDialTimeout = 10 * time.Second

// SSH runs commands on the live node.
type SSH interface {
	// Run executes cmd and returns its standard output. A node that is not
	// reachable yet, or a command that exits non-zero, is an error.
	Run(ctx context.Context, cmd string) ([]byte, error)
}

// SSHFactory builds an SSH client for the node at address.
type SSHFactory func(cfg SSHConfig, address string) SSH

// sshRunner implements SSH with one connection per call, so a probe during
// boot never holds a half-open connection across the node's restarts.
type sshRunner struct {
	cfg     SSHConfig
	address string
}

// NewSSH returns the x/crypto/ssh implementation of SSH.
func NewSSH(cfg SSHConfig, address string) SSH {
	return &sshRunner{cfg: cfg, address: address}
}

// Run implements SSH.
func (s *sshRunner) Run(ctx context.Context, cmd string) ([]byte, error) {
	clientCfg, err := s.clientConfig()
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(s.address, strconv.Itoa(s.cfg.Port))
	dialer := net.Dialer{Timeout: sshDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	// The handshake and the command honour ctx through the connection.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	c, chans, reqs, err := ssh.NewClientConn(conn, addr, clientCfg)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	client := ssh.NewClient(c, chans, reqs)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	out, err := sess.Output(cmd)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cmd, err)
	}
	return out, nil
}

func (s *sshRunner) clientConfig() (*ssh.ClientConfig, error) {
	signer, err := ssh.ParsePrivateKey([]byte(s.cfg.PrivateKey))
	if err != nil {
		return nil, fmt.Errorf("parsing ssh private key: %w", err)
	}
	// A live image booted into RAM generates its host key at every boot, so
	// without an embedded key there is nothing to verify it against; the
	// node authenticates us, not the other way round (see SSHConfig.HostKey).
	hostKey := ssh.InsecureIgnoreHostKey()
	if s.cfg.HostKey != "" {
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s.cfg.HostKey))
		if err != nil {
			return nil, fmt.Errorf("parsing ssh host key: %w", err)
		}
		hostKey = ssh.FixedHostKey(pub)
	}
	return &ssh.ClientConfig{
		User:            s.cfg.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKey,
		Timeout:         sshDialTimeout,
	}, nil
}
