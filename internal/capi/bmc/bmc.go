// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package bmc

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	bmclib "github.com/bmc-toolbox/bmclib/v2"
	"github.com/bmc-toolbox/bmclib/v2/bmc"
	bmclibErrs "github.com/bmc-toolbox/bmclib/v2/errors"
	"github.com/bmc-toolbox/bmclib/v2/providers/intelamt"
	"github.com/bmc-toolbox/bmclib/v2/providers/redfish"
	"github.com/jacobweinstock/iamt"
)

// Credentials identifies and authenticates to a machine's BMC.
//
// Address accepts "host", "host:port", or "scheme://host[:port]". A port
// applies to the Redfish and Intel AMT providers; a scheme applies to Intel
// AMT (its default is http on 16992, while TLS-enabled AMT listens on 16993).
type Credentials struct {
	Address  string
	Username string
	Password string
}

// Client is the generic out-of-band control surface a bootstrapper needs.
// Unsupported operations return an error wrapping ErrUnsupported.
type Client interface {
	PowerState(ctx context.Context) (PowerState, error)
	PowerOn(ctx context.Context) error
	PowerOff(ctx context.Context) error
	PowerCycle(ctx context.Context) error
	SetBootDevice(ctx context.Context, dev BootDevice, persistent, efi bool) error
	// InsertMedia attaches an ISO. armed reports that the BMC already
	// selected the media as the next boot (Intel AMT's one-click recovery
	// does), in which case SetBootDevice must not be called: it would replace
	// the armed boot.
	InsertMedia(ctx context.Context, isoURL string) (armed bool, err error)
	EjectMedia(ctx context.Context) error
	// SetHTTPBootURI sets the UEFI HTTP boot URI; armed has the same meaning
	// as for InsertMedia. An empty uri clears it.
	SetHTTPBootURI(ctx context.Context, uri string) (armed bool, err error)
	PostCode(ctx context.Context) (string, error)
}

// bmcSession is the slice of *bmclib.Client the implementation uses, so tests
// can substitute a fake.
type bmcSession interface {
	Open(ctx context.Context) error
	Close(ctx context.Context) error
	GetPowerState(ctx context.Context) (string, error)
	SetPowerState(ctx context.Context, state string) (bool, error)
	SetBootDevice(ctx context.Context, bootDevice string, setPersistent, efiBoot bool) (bool, error)
	SetVirtualMedia(ctx context.Context, kind, mediaURL string) (bool, error)
	SetHTTPBootURI(ctx context.Context, uri string) (bool, error)
	PostCode(ctx context.Context) (string, int, error)
	GetMetadata() bmc.Metadata
}

const (
	defaultBMCTimeout  = 90 * time.Second
	perProviderTimeout = 30 * time.Second
	virtualMediaKindCD = "CD"
)

// Lib implements Client with a fresh bmclib session per operation.
type Lib struct {
	creds      Credentials
	logger     *log.Logger
	timeout    time.Duration
	newSession func() bmcSession

	mu sync.Mutex
	// provider is the bmclib provider that opened successfully; later
	// sessions try only that provider instead of every driver.
	provider string
}

// openSession is an open bmclib session plus what was learned at open time.
// bmclib overwrites the client metadata after every call, so facts from Open
// (which provider actually connected) must be captured before any operation.
type openSession struct {
	bmcSession
	// amt is true when the Intel AMT provider serves this session.
	amt bool
}

// parseAddress splits "host", "host:port", or "scheme://host[:port]".
func parseAddress(address string) (host string, port int, scheme string, err error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", 0, "", errors.New("bmc address is empty")
	}
	if strings.Contains(address, "://") {
		u, err := url.Parse(address)
		if err != nil {
			return "", 0, "", fmt.Errorf("parsing bmc address %q: %w", address, err)
		}
		scheme = strings.ToLower(u.Scheme)
		host = u.Hostname()
		if p := u.Port(); p != "" {
			port, err = strconv.Atoi(p)
			if err != nil {
				return "", 0, "", fmt.Errorf("parsing bmc port in %q: %w", address, err)
			}
		}
		if host == "" {
			return "", 0, "", fmt.Errorf("bmc address %q has no host", address)
		}
		return host, port, scheme, nil
	}
	if h, p, err := net.SplitHostPort(address); err == nil {
		port, err = strconv.Atoi(p)
		if err != nil {
			return "", 0, "", fmt.Errorf("parsing bmc port in %q: %w", address, err)
		}
		return h, port, "", nil
	}
	return address, 0, "", nil
}

// New returns a BMC backed by bmclib with provider autodetection.
func New(creds Credentials, logger *log.Logger) *Lib {
	if logger == nil {
		logger = log.New(log.Writer(), "[bmc] ", log.LstdFlags)
	}
	b := &Lib{creds: creds, logger: logger, timeout: defaultBMCTimeout}

	host, port, scheme, err := parseAddress(creds.Address)
	if err != nil {
		// Let Open fail with the real message; keep the raw address.
		host = creds.Address
	}
	opts := []bmclib.Option{bmclib.WithPerProviderTimeout(perProviderTimeout)}
	if port > 0 {
		opts = append(opts, bmclib.WithIntelAMTPort(uint32(port)), bmclib.WithRedfishPort(strconv.Itoa(port)))
	}
	if scheme != "" {
		opts = append(opts, bmclib.WithIntelAMTHostScheme(scheme))
	}

	b.newSession = func() bmcSession {
		c := bmclib.NewClient(host, creds.Username, creds.Password, opts...)
		if p := b.preferredProvider(); p != "" {
			c.Registry.Drivers = c.Registry.For(p)
		}
		return c
	}
	return b
}

func (b *Lib) preferredProvider() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.provider
}

func (b *Lib) rememberProvider(conns []string) {
	p := preferProvider(conns)
	if p == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.provider != p {
		b.provider = p
		b.logger.Printf("bmc %s: using provider %s for subsequent sessions", b.creds.Address, p)
	}
}

// preferProvider chooses the single provider all later sessions should use,
// from the set that opened. Pinning one control surface is what keeps virtual
// media and the boot override on the same device: a BMC that speaks both IPMI
// and Redfish otherwise has bmclib attach the CD via Redfish but set the boot
// device via IPMI (each capability picks its own driver), and the IPMI boot
// flag does not select the Redfish virtual CD, so the node ignores the media
// and drops to the boot menu. Intel AMT wins when present (it self-arms the
// boot and can stream storage redirection); then Redfish, which serves power,
// media, boot device and HTTP boot from one surface; otherwise the first
// provider that opened.
func preferProvider(conns []string) string {
	for _, want := range []string{intelamt.ProviderName, redfish.ProviderName} {
		if slices.Contains(conns, want) {
			return want
		}
	}
	if len(conns) > 0 {
		return conns[0]
	}
	return ""
}

// withSession opens a session, runs fn, and closes the session. Errors are
// annotated with bmclib's provider metadata.
func (b *Lib) withSession(ctx context.Context, op string, fn func(context.Context, *openSession) error) error {
	s := b.newSession()
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	if err := s.Open(ctx); err != nil {
		return wrapError(op, err, s.GetMetadata())
	}
	opened := s.GetMetadata().SuccessfulOpenConns
	b.rememberProvider(opened)
	sess := &openSession{bmcSession: s, amt: slices.Contains(opened, intelamt.ProviderName)}
	defer func() {
		if cerr := s.Close(ctx); cerr != nil {
			b.logger.Printf("bmc %s: close: %v", op, cerr)
		}
	}()

	if err := fn(ctx, sess); err != nil {
		return wrapError(op, err, s.GetMetadata())
	}
	b.logger.Printf("bmc %s: %s ok via %s", b.creds.Address, op, s.GetMetadata().SuccessfulProvider)
	return nil
}

// wrapError maps "no provider implements this" to ErrUnsupported and
// everything else to a *Error carrying provider metadata.
func wrapError(op string, err error, md bmc.Metadata) error {
	if errors.Is(err, bmclibErrs.ErrProviderImplementation) ||
		errors.Is(err, bmclibErrs.ErrNotImplemented) ||
		strings.Contains(err.Error(), "implementations found") {
		return fmt.Errorf("bmc %s: %w", op, ErrUnsupported)
	}
	return &Error{Op: op, Err: err, Attempted: md.ProvidersAttempted, Failed: md.FailedProviderDetail}
}

// PowerState reads the BMC-reported power state, normalized to on/off/unknown.
func (b *Lib) PowerState(ctx context.Context) (PowerState, error) {
	state := PowerUnknown
	err := b.withSession(ctx, "power-state", func(ctx context.Context, s *openSession) error {
		raw, err := s.GetPowerState(ctx)
		if err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "on":
			state = PowerOn
		case "off":
			state = PowerOff
		}
		return nil
	})
	return state, err
}

func (b *Lib) setPower(ctx context.Context, op, state string) error {
	return b.withSession(ctx, op, func(ctx context.Context, s *openSession) error {
		_, err := s.SetPowerState(ctx, state)
		return err
	})
}

// PowerOn powers the machine on.
func (b *Lib) PowerOn(ctx context.Context) error { return b.setPower(ctx, "power-on", "on") }

// PowerOff hard-powers the machine off.
func (b *Lib) PowerOff(ctx context.Context) error { return b.setPower(ctx, "power-off", "off") }

// PowerCycle hard-resets the machine.
func (b *Lib) PowerCycle(ctx context.Context) error {
	return b.setPower(ctx, "power-cycle", "cycle")
}

// SetBootDevice sets the next-boot (or persistent) boot device.
func (b *Lib) SetBootDevice(ctx context.Context, dev BootDevice, persistent, efi bool) error {
	return b.withSession(ctx, "set-boot-device", func(ctx context.Context, s *openSession) error {
		_, err := s.SetBootDevice(ctx, string(dev), persistent, efi)
		return err
	})
}

// InsertMedia attaches an ISO as virtual CD media. Intel AMT has no CD
// emulation: its "virtual media" is one-click recovery, which boots an EFI
// image over HTTPS, so an ISO is reported unsupported and the caller
// falls through to the UKI via SetHTTPBootURI.
func (b *Lib) InsertMedia(ctx context.Context, isoURL string) (bool, error) {
	err := b.withSession(ctx, "insert-media", func(ctx context.Context, s *openSession) error {
		if s.amt {
			return fmt.Errorf("intel amt boots EFI images over https, not ISOs: %w", ErrUnsupported)
		}
		_, err := s.SetVirtualMedia(ctx, virtualMediaKindCD, isoURL)
		return err
	})
	return false, err
}

// EjectMedia detaches virtual CD media (on Intel AMT: clears the armed boot).
func (b *Lib) EjectMedia(ctx context.Context) error {
	return b.withSession(ctx, "eject-media", func(ctx context.Context, s *openSession) error {
		_, err := s.SetVirtualMedia(ctx, virtualMediaKindCD, "")
		return err
	})
}

// SetHTTPBootURI sets the UEFI HTTP boot URI. An empty uri clears it. On
// Intel AMT a non-empty uri arms a one-shot boot of the image, and clearing
// is the same as ejecting.
func (b *Lib) SetHTTPBootURI(ctx context.Context, uri string) (bool, error) {
	var armed bool
	err := b.withSession(ctx, "set-http-boot-uri", func(ctx context.Context, s *openSession) error {
		if uri == "" && s.amt {
			_, err := s.SetVirtualMedia(ctx, virtualMediaKindCD, "")
			return err
		}
		_, err := s.SetHTTPBootURI(ctx, uri)
		if err != nil {
			return err
		}
		armed = uri != "" && s.amt
		return nil
	})
	return armed, err
}

// PostCode returns the BIOS POST code as a diagnostic string.
func (b *Lib) PostCode(ctx context.Context) (string, error) {
	var out string
	err := b.withSession(ctx, "post-code", func(ctx context.Context, s *openSession) error {
		status, code, err := s.PostCode(ctx)
		if err != nil {
			return err
		}
		out = fmt.Sprintf("%s (0x%02x)", status, code)
		return nil
	})
	return out, err
}

// RedirectHandle is a running storage-redirection session. Close tears it down.
type RedirectHandle interface {
	Close() error
}

// ISORedirector is implemented by BMCs that can stream an ISO to the host as a
// bootable CD over a persistent session (Intel AMT IDE-R/USB-R), so the host
// boots with no dependency on its own firmware network stack. The image is
// streamed on demand from its URL using HTTP range requests — nothing is
// written to local disk. RedirectISO returns an error wrapping ErrUnsupported
// when the BMC or host is not AMT.
type ISORedirector interface {
	// RedirectSupported reports whether this BMC will use storage redirection,
	// i.e. the connected provider is Intel AMT. It is cheap: it reflects the
	// provider learned from an earlier operation this run.
	RedirectSupported(ctx context.Context) bool
	// RedirectISO streams the ISO at isoURL to the host over redirection.
	RedirectISO(ctx context.Context, isoURL string) (RedirectHandle, error)
}

// redirectHandle owns the resources of one redirection session. file is set
// only when serving a local image file (the streamed-URL model keeps none).
type redirectHandle struct {
	sess *iamt.RedirectSession
	cli  *iamt.Client
	file *os.File
}

func (h *redirectHandle) Close() error {
	var errs []error
	if h.sess != nil {
		errs = append(errs, h.sess.Close())
	}
	if h.cli != nil {
		errs = append(errs, h.cli.Close(context.Background()))
	}
	if h.file != nil {
		errs = append(errs, h.file.Close())
	}
	return errors.Join(errs...)
}

// isHTTPURL reports whether ref is an http(s) URL rather than a local path.
func isHTTPURL(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
}

// RedirectSupported implements ISORedirector.
func (b *Lib) RedirectSupported(context.Context) bool {
	return b.preferredProvider() == intelamt.ProviderName
}

// RedirectISO implements ISORedirector for Intel AMT hosts. It opens an AMT
// session, enables redirection, and serves the ISO at isoRef to the host as a
// bootable CD, arming a one-shot boot from it on the next host reset. isoRef may
// be either an http(s) URL — streamed on demand with range requests, nothing
// written to local disk — or a local file path, served from that file. The
// returned handle must be closed to end the session.
//
// It is meaningful only for Intel AMT. When this BMC has already opened a
// non-AMT provider, it returns ErrUnsupported without contacting the device.
func (b *Lib) RedirectISO(ctx context.Context, isoRef string) (RedirectHandle, error) {
	if p := b.preferredProvider(); p != "" && p != intelamt.ProviderName {
		return nil, fmt.Errorf("storage redirection: provider %q: %w", p, ErrUnsupported)
	}

	host, port, scheme, err := parseAddress(b.creds.Address)
	if err != nil {
		return nil, err
	}
	if scheme == "" {
		scheme = "http"
	}
	if port == 0 {
		if scheme == "https" {
			port = 16993
		} else {
			port = 16992
		}
	}

	cli := iamt.NewClient(host, b.creds.Username, b.creds.Password,
		iamt.WithScheme(scheme), iamt.WithPort(uint32(port)))
	if err := cli.Open(ctx); err != nil {
		return nil, fmt.Errorf("storage redirection: opening AMT session: %w", err)
	}

	// Streamed-URL model: fetch on demand, no local disk.
	if isHTTPURL(isoRef) {
		sess, err := cli.MountURL(ctx, isoRef)
		if err != nil {
			_ = cli.Close(ctx)
			return nil, fmt.Errorf("storage redirection: %w", err)
		}
		b.logger.Printf("bmc %s: storage redirection streaming %s", b.creds.Address, isoRef)
		return &redirectHandle{sess: sess, cli: cli}, nil
	}

	// Local-file model: serve a pre-downloaded image from disk.
	f, err := os.Open(isoRef) //nolint:gosec // operator-provided image path
	if err != nil {
		_ = cli.Close(ctx)
		return nil, fmt.Errorf("storage redirection: opening image: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		_ = cli.Close(ctx)
		return nil, fmt.Errorf("storage redirection: stat image: %w", err)
	}
	sess, err := cli.MountISO(ctx, f, st.Size())
	if err != nil {
		_ = f.Close()
		_ = cli.Close(ctx)
		return nil, fmt.Errorf("storage redirection: %w", err)
	}
	b.logger.Printf("bmc %s: storage redirection serving %s (%d bytes)", b.creds.Address, isoRef, st.Size())
	return &redirectHandle{sess: sess, cli: cli, file: f}, nil
}
