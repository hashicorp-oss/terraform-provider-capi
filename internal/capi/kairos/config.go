// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

// Package kairos implements capi.Bootstrapper for a disposable bootstrap
// node: a machine booted through its BMC from a user-built Kairos live ISO
// that runs k3s entirely from RAM. It hosts the management cluster only until
// CAPI pivots to the workload cluster; then it is powered off and the media
// ejected. Nothing is ever written to its disk.
package kairos

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/ssh"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi/bmc"
)

// Defaults applied by Config.withDefaults.
const (
	DefaultSSHUser     = "kairos"
	DefaultSSHPort     = 22
	defaultClusterName = "capi-bootstrap"
	apiPort            = "6443"
)

// Sentinel errors returned by the bootstrapper, always wrapped in
// capi.BootstrapError by the public methods.
var (
	ErrBootTimeout       = errors.New("live node did not answer over SSH within the boot timeout")
	ErrAttemptsExhausted = errors.New("boot attempts exhausted")
	ErrKubeconfigTimeout = errors.New("k3s did not publish its kubeconfig within the timeout")
	ErrKubernetesTimeout = errors.New("kubernetes API did not become ready within the timeout")
)

// Machine identifies the disposable bootstrap machine.
type Machine struct {
	// Hostname names the machine in logs and errors.
	Hostname string
	// Address is where the live node answers: SSH and, once k3s is up, the
	// Kubernetes API. The ISO's cloud-config gives the node this address.
	Address string
	// BMC boots and powers the machine.
	BMC bmc.Credentials
}

// SSHConfig is how the live node is reached.
type SSHConfig struct {
	// User is the login the ISO's cloud-config authorizes; default "kairos".
	User string
	// Port is the SSH port; default 22.
	Port int
	// PrivateKey is the PEM-encoded key whose public half is in the
	// cloud-config's ssh_authorized_keys.
	PrivateKey string
	// HostKey, when set, is the node's host public key in authorized_keys
	// format, and any other host key is refused. Empty accepts any host key:
	// a live image that runs from RAM generates a new one at every boot, so
	// there is nothing to pin unless the ISO embeds one.
	HostKey string
}

// Config is the complete bootstrapper configuration.
type Config struct {
	Machine Machine
	// Boot controls how the ISO is booted: virtual media through the BMC, or
	// manual, where the machine is booted by hand (a USB stick and a power
	// button) and only waited for. Method http (a UKI over UEFI HTTP boot)
	// is rejected.
	Boot bmc.BootConfig
	// ISO is the user-built Kairos live image (AuroraBoot's
	// kairos.iso.custom.iso): an http(s) URL the BMC can fetch, or with Intel
	// AMT storage redirection also a local path.
	ISO string
	SSH SSHConfig
	// Addons are installed on the live cluster before it manages anything.
	// k3s brings its own CNI.
	Addons capi.AddonSet
}

func (c Config) withDefaults() Config {
	if c.Boot.Method == "" {
		c.Boot.Method = bmc.BootMethodAuto
	}
	if c.Boot.Timeout == 0 {
		c.Boot.Timeout = bmc.DefaultBootTimeout
	}
	if c.Boot.Attempts <= 0 {
		c.Boot.Attempts = bmc.DefaultBootAttempts
	}
	if c.SSH.User == "" {
		c.SSH.User = DefaultSSHUser
	}
	if c.SSH.Port == 0 {
		c.SSH.Port = DefaultSSHPort
	}
	return c
}

// Validate reports every missing or invalid field at once.
func (c Config) Validate() error {
	var errs []error
	if c.Machine.Address == "" {
		errs = append(errs, errors.New("machine address is required"))
	}
	if c.Machine.BMC.Address == "" && c.Boot.Method != bmc.BootMethodManual {
		errs = append(errs, errors.New("bmc address is required unless the boot method is manual"))
	}
	if c.ISO == "" {
		errs = append(errs, errors.New("iso is required"))
	}
	if c.SSH.PrivateKey == "" {
		errs = append(errs, errors.New("ssh private key is required"))
	} else if _, err := ssh.ParsePrivateKey([]byte(c.SSH.PrivateKey)); err != nil {
		errs = append(errs, fmt.Errorf("ssh private key: %w", err))
	}
	if c.SSH.HostKey != "" {
		if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(c.SSH.HostKey)); err != nil {
			errs = append(errs, fmt.Errorf("ssh host key: %w", err))
		}
	}
	switch c.Boot.Method {
	case bmc.BootMethodAuto, bmc.BootMethodVirtualMedia, bmc.BootMethodManual:
	case bmc.BootMethodHTTP:
		errs = append(errs, errors.New(`boot method "http" boots a UKI over UEFI HTTP boot, not a live ISO; use "virtual_media"`))
	default:
		errs = append(errs, fmt.Errorf("unknown boot method %q", c.Boot.Method))
	}
	return errors.Join(errs...)
}
