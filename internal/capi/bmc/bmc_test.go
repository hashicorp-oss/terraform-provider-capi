// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package bmc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bmc-toolbox/bmclib/v2/bmc"
)

// fakeSession mimics bmclib's metadata behaviour: Open records the provider
// that connected, and every later operation replaces the metadata with that
// operation's own (which no longer lists the open connections).
type fakeSession struct {
	provider       string   // provider name reported by Open; "gofish" when empty
	openConns      []string // providers reported open; defaults to [name()]
	failed         map[string]string
	opened, closed bool
	power          string
	powerErr       error
	setPowerCalls  []string
	bootCalls      []string
	mediaCalls     []string
	httpCalls      []string
	setErr         error
	metadata       bmc.Metadata
}

func (f *fakeSession) name() string {
	if f.provider == "" {
		return "gofish"
	}
	return f.provider
}

func (f *fakeSession) Open(context.Context) error {
	f.opened = true
	conns := f.openConns
	if conns == nil {
		conns = []string{f.name()}
	}
	f.metadata = bmc.Metadata{SuccessfulOpenConns: conns, ProvidersAttempted: conns}
	return nil
}

func (f *fakeSession) Close(context.Context) error { f.closed = true; return nil }

// op replaces the metadata the way bmclib does after each call.
func (f *fakeSession) op() (bool, error) {
	f.metadata = bmc.Metadata{
		SuccessfulProvider:   f.name(),
		ProvidersAttempted:   []string{f.name()},
		FailedProviderDetail: f.failed,
	}
	return f.setErr == nil, f.setErr
}

func (f *fakeSession) GetPowerState(context.Context) (string, error) {
	_, _ = f.op()
	return f.power, f.powerErr
}

func (f *fakeSession) SetPowerState(_ context.Context, state string) (bool, error) {
	f.setPowerCalls = append(f.setPowerCalls, state)
	return f.op()
}

func (f *fakeSession) SetBootDevice(
	_ context.Context,
	dev string,
	persistent, efi bool,
) (bool, error) {
	f.bootCalls = append(f.bootCalls, dev)
	return f.op()
}

func (f *fakeSession) SetVirtualMedia(_ context.Context, kind, url string) (bool, error) {
	f.mediaCalls = append(f.mediaCalls, kind+":"+url)
	return f.op()
}

func (f *fakeSession) SetHTTPBootURI(_ context.Context, uri string) (bool, error) {
	f.httpCalls = append(f.httpCalls, uri)
	return f.op()
}

func (f *fakeSession) PostCode(context.Context) (string, int, error) {
	_, _ = f.op()
	return "ok", 0, nil
}
func (f *fakeSession) GetMetadata() bmc.Metadata { return f.metadata }

func newTestBMC(s *fakeSession) *Lib {
	b := New(Credentials{Address: "10.0.0.1", Username: "u", Password: "p"}, nil)
	b.newSession = func() bmcSession { return s }
	return b
}

func TestParseAddress(t *testing.T) {
	for _, tc := range []struct {
		in     string
		host   string
		port   int
		scheme string
		bad    bool
	}{
		{in: "10.0.0.1", host: "10.0.0.1"},
		{in: "bmc.example.com:443", host: "bmc.example.com", port: 443},
		{in: "https://10.0.0.160:16993", host: "10.0.0.160", port: 16993, scheme: "https"},
		{in: "http://10.0.0.160", host: "10.0.0.160", scheme: "http"},
		{in: "", bad: true},
		{in: "https://", bad: true},
	} {
		host, port, scheme, err := parseAddress(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("parseAddress(%q) expected error", tc.in)
			}
			continue
		}
		if err != nil || host != tc.host || port != tc.port || scheme != tc.scheme {
			t.Errorf(
				"parseAddress(%q) = %q,%d,%q,%v; want %q,%d,%q",
				tc.in,
				host,
				port,
				scheme,
				err,
				tc.host,
				tc.port,
				tc.scheme,
			)
		}
	}
}

func TestBMCLib_PowerStateNormalizesCase(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want PowerState
	}{{"On", PowerOn}, {"on", PowerOn}, {"Off", PowerOff}, {"off", PowerOff}, {"", PowerUnknown}, {"Paused", PowerUnknown}} {
		s := &fakeSession{power: tc.raw}
		got, err := newTestBMC(s).PowerState(context.Background())
		if err != nil {
			t.Fatalf("PowerState(%q) error = %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("PowerState(%q) = %q, want %q", tc.raw, got, tc.want)
		}
		if !s.opened || !s.closed {
			t.Fatalf(
				"session should be opened and closed, got opened=%v closed=%v",
				s.opened,
				s.closed,
			)
		}
	}
}

func TestBMCLib_PowerCommands(t *testing.T) {
	s := &fakeSession{}
	b := newTestBMC(s)
	ctx := context.Background()
	if err := b.PowerOn(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.PowerOff(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.PowerCycle(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.setPowerCalls, ","); got != "on,off,cycle" {
		t.Fatalf("power calls = %q", got)
	}
}

func TestBMCLib_MediaAndBootRedfishStyle(t *testing.T) {
	s := &fakeSession{provider: "gofish"}
	b := newTestBMC(s)
	ctx := context.Background()
	armed, err := b.InsertMedia(ctx, "https://example.com/talos.iso")
	if err != nil || armed {
		t.Fatalf("InsertMedia() = armed %v, err %v; want not armed", armed, err)
	}
	if err := b.EjectMedia(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.SetBootDevice(ctx, BootDeviceCDROM, false, true); err != nil {
		t.Fatal(err)
	}
	armed, err = b.SetHTTPBootURI(ctx, "https://example.com/uki.efi")
	if err != nil || armed {
		t.Fatalf("SetHTTPBootURI() = armed %v, err %v; want not armed", armed, err)
	}
	if _, err := b.SetHTTPBootURI(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.mediaCalls, ","); got != "CD:https://example.com/talos.iso,CD:" {
		t.Fatalf("media calls = %q", got)
	}
	if got := strings.Join(s.bootCalls, ","); got != "cdrom" {
		t.Fatalf("boot calls = %q", got)
	}
	if got := strings.Join(s.httpCalls, ","); got != "https://example.com/uki.efi," {
		t.Fatalf("http calls = %q", got)
	}
}

func TestBMCLib_AMTArmsBootAndClearsViaEject(t *testing.T) {
	s := &fakeSession{provider: "IntelAMT"}
	b := newTestBMC(s)
	ctx := context.Background()
	// AMT has no CD emulation: an ISO is unsupported so auto mode moves on to the UKI.
	if _, err := b.InsertMedia(
		ctx,
		"https://example.com/talos.iso",
	); !errors.Is(
		err,
		ErrUnsupported,
	) {
		t.Fatalf("InsertMedia() error = %v, want ErrUnsupported on AMT", err)
	}
	armed, err := b.SetHTTPBootURI(ctx, "https://example.com/uki.efi")
	if err != nil || !armed {
		t.Fatalf(
			"SetHTTPBootURI() = armed %v, err %v; want armed (metadata after the call no longer lists open conns)",
			armed,
			err,
		)
	}
	if _, err := b.SetHTTPBootURI(ctx, ""); err != nil {
		t.Fatal(err)
	}
	// Clearing on AMT must go through the media eject path, never an empty URI.
	if got := strings.Join(s.httpCalls, ","); got != "https://example.com/uki.efi" {
		t.Fatalf("http calls = %q", got)
	}
	if got := strings.Join(s.mediaCalls, ","); got != "CD:" {
		t.Fatalf("media calls = %q", got)
	}
	if b.preferredProvider() != "IntelAMT" {
		t.Fatalf("provider = %q, want IntelAMT remembered after first open", b.preferredProvider())
	}
}

func TestBMCLib_UnsupportedMapsToErrUnsupported(t *testing.T) {
	s := &fakeSession{
		setErr: errors.New("1 error occurred: no VirtualMediaSetter implementations found"),
	}
	_, err := newTestBMC(s).InsertMedia(context.Background(), "https://example.com/talos.iso")
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("InsertMedia() error = %v, want ErrUnsupported", err)
	}
}

func TestBMCLib_ErrorCarriesMetadata(t *testing.T) {
	s := &fakeSession{
		provider: "gofish",
		setErr:   errors.New("boom"),
		failed:   map[string]string{"gofish": "401", "ipmitool": "timeout"},
	}
	err := newTestBMC(s).PowerOn(context.Background())
	var bmcErr *Error
	if !errors.As(err, &bmcErr) {
		t.Fatalf("PowerOn() error = %T, want *Error", err)
	}
	msg := err.Error()
	for _, want := range []string{"bmc power-on: boom", "gofish: 401", "ipmitool: timeout"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
}

func TestBMCLib_PinsSingleProviderFromMany(t *testing.T) {
	// A BMC that speaks both IPMI and Redfish opens several providers. Without
	// pinning, bmclib attaches virtual media via Redfish but sets the boot
	// device via IPMI, and the IPMI boot flag does not select the Redfish CD.
	// The BMC must settle on one provider, preferring Redfish.
	s := &fakeSession{provider: "ipmi", openConns: []string{"ipmi", "lenovo", "gofish", "ipmitool"}}
	b := newTestBMC(s)
	if _, err := b.PowerState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := b.preferredProvider(); got != "gofish" {
		t.Fatalf("preferred provider = %q, want gofish", got)
	}
}

func TestBMCLib_PrefersAMTWhenPresent(t *testing.T) {
	s := &fakeSession{provider: "IntelAMT", openConns: []string{"gofish", "IntelAMT"}}
	b := newTestBMC(s)
	if _, err := b.PowerState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := b.preferredProvider(); got != "IntelAMT" {
		t.Fatalf("preferred provider = %q, want IntelAMT", got)
	}
}

func TestBMCLib_PinsFirstWhenNoPreferred(t *testing.T) {
	s := &fakeSession{provider: "ipmi", openConns: []string{"ipmi", "ipmitool"}}
	b := newTestBMC(s)
	if _, err := b.PowerState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := b.preferredProvider(); got != "ipmi" {
		t.Fatalf("preferred provider = %q, want ipmi", got)
	}
}
