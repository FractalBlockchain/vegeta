package vegeta

import (
	"context"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rs/dnscache"
	"golang.org/x/net/http2"
)

// Attacker is an attack executor which wraps an http.Client
type Attacker struct {
	dialer    *net.Dialer
	client    http.Client
	stopch    chan struct{}
	workers   uint64
	redirects int
}

const (
	// DefaultRedirects is the default number of times an Attacker follows
	// redirects.
	DefaultRedirects = 10
	// DefaultTimeout is the default amount of time an Attacker waits for a request
	// before it times out.
	DefaultTimeout = 30 * time.Second
	// DefaultConnections is the default amount of max open idle connections per
	// target host.
	DefaultConnections = 10000
	// DefaultWorkers is the default initial number of workers used to carry an attack.
	DefaultWorkers = 10
	// NoFollow is the value when redirects are not followed but marked successful
	NoFollow = -1
)

var (
	// DefaultLocalAddr is the default local IP address an Attacker uses.
	DefaultLocalAddr = net.IPAddr{IP: net.IPv4zero}
	// DefaultTLSConfig is the default tls.Config an Attacker uses.
	DefaultTLSConfig = &tls.Config{InsecureSkipVerify: true}
)

// Cached DNS resolver
var resolver = &dnscache.Resolver{}

func dialContext(ctx context.Context, network string, addr string) (conn net.Conn, err error) {
	separator := strings.LastIndex(addr, ":")
	ips, err := resolver.LookupHost(ctx, addr[:separator])
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		conn, err = net.Dial(network, ip+addr[separator:])
		if err == nil {
			break
		}
	}
	return
}

// NewAttacker returns a new Attacker with default options which are overridden
// by the optionally provided opts.
func NewAttacker(opts ...func(*Attacker)) *Attacker {
	a := &Attacker{stopch: make(chan struct{}), workers: DefaultWorkers}
	a.dialer = &net.Dialer{
		LocalAddr: &net.TCPAddr{IP: DefaultLocalAddr.IP, Zone: DefaultLocalAddr.Zone},
		KeepAlive: 30 * time.Second,
		Timeout:   DefaultTimeout,
	}
	a.client = http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			Dial:                  a.dialer.Dial,
			DialContext:           dialContext,
			ResponseHeaderTimeout: DefaultTimeout,
			TLSClientConfig:       DefaultTLSConfig,
			TLSHandshakeTimeout:   10 * time.Second,
			MaxIdleConnsPerHost:   DefaultConnections,
		},
	}

	for _, opt := range opts {
		opt(a)
	}

	return a
}

// Workers returns a functional option which sets the initial number of workers
// an Attacker uses to hit its targets. More workers may be spawned dynamically
// to sustain the requested rate in the face of slow responses and errors.
func Workers(n uint64) func(*Attacker) {
	return func(a *Attacker) { a.workers = n }
}

// Connections returns a functional option which sets the number of maximum idle
// open connections per target host.
func Connections(n int) func(*Attacker) {
	return func(a *Attacker) {
		tr := a.client.Transport.(*http.Transport)
		tr.MaxIdleConnsPerHost = n
	}
}

// Redirects returns a functional option which sets the maximum
// number of redirects an Attacker will follow.
func Redirects(n int) func(*Attacker) {
	return func(a *Attacker) {
		a.redirects = n
		a.client.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
			switch {
			case n == NoFollow:
				return http.ErrUseLastResponse
			case n < len(via):
				return fmt.Errorf("stopped after %d redirects", n)
			default:
				return nil
			}
		}
	}
}

// Proxy returns a functional option which sets the `Proxy` field on
// the http.Client's Transport
func Proxy(proxy func(*http.Request) (*url.URL, error)) func(*Attacker) {
	return func(a *Attacker) {
		tr := a.client.Transport.(*http.Transport)
		tr.Proxy = proxy
	}
}

// Timeout returns a functional option which sets the maximum amount of time
// an Attacker will wait for a request to be responded to.
func Timeout(d time.Duration) func(*Attacker) {
	return func(a *Attacker) {
		tr := a.client.Transport.(*http.Transport)
		tr.ResponseHeaderTimeout = d
		a.dialer.Timeout = d
		tr.Dial = a.dialer.Dial
	}
}

// LocalAddr returns a functional option which sets the local address
// an Attacker will use with its requests.
func LocalAddr(addr net.IPAddr) func(*Attacker) {
	return func(a *Attacker) {
		tr := a.client.Transport.(*http.Transport)
		a.dialer.LocalAddr = &net.TCPAddr{IP: addr.IP, Zone: addr.Zone}
		tr.Dial = a.dialer.Dial
	}
}

// KeepAlive returns a functional option which toggles KeepAlive
// connections on the dialer and transport.
func KeepAlive(keepalive bool) func(*Attacker) {
	return func(a *Attacker) {
		tr := a.client.Transport.(*http.Transport)
		tr.DisableKeepAlives = !keepalive
		if !keepalive {
			a.dialer.KeepAlive = 0
			tr.Dial = a.dialer.Dial
		}
	}
}

// TLSConfig returns a functional option which sets the *tls.Config for a
// Attacker to use with its requests.
func TLSConfig(c *tls.Config) func(*Attacker) {
	return func(a *Attacker) {
		tr := a.client.Transport.(*http.Transport)
		tr.TLSClientConfig = c
	}
}

// HTTP2 returns a functional option which enables or disables HTTP/2 support
// on requests performed by an Attacker.
func HTTP2(enabled bool) func(*Attacker) {
	return func(a *Attacker) {
		if tr := a.client.Transport.(*http.Transport); enabled {
			http2.ConfigureTransport(tr)
		} else {
			tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		}
	}
}

// H2C returns a functional option which enables H2C support on requests
// performed by an Attacker
func H2C(enabled bool) func(*Attacker) {
	return func(a *Attacker) {
		if tr := a.client.Transport.(*http.Transport); enabled {
			a.client.Transport = &http2.Transport{
				AllowHTTP: true,
				DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
					return tr.Dial(network, addr)
				},
			}
		}
	}
}

// Attack reads its Targets from the passed Targeter and attacks them at
// the rate specified for the given duration. When the duration is zero the attack
// runs until Stop is called. Results are sent to the returned channel as soon
// as they arrive and will have their Attack field set to the given name.
func (a *Attacker) Attack(tr Targeter, rate uint64, du time.Duration, name string) <-chan *Result {
	var workers sync.WaitGroup
	results := make(chan *Result)
	ticks := make(chan uint64)
	for i := uint64(0); i < a.workers; i++ {
		workers.Add(1)
		go a.attack(tr, name, &workers, ticks, results)
	}

	go func() {
		defer close(results)
		defer workers.Wait()
		defer close(ticks)
		interval := 1e9 / rate
		hits := rate * uint64(du.Seconds())
		began, seq := time.Now(), uint64(0)
		for {
			now, next := time.Now(), began.Add(time.Duration(seq*interval))
			time.Sleep(next.Sub(now))
			select {
			case ticks <- seq:
				if seq++; seq == hits {
					return
				}
			case <-a.stopch:
				return
			default: // all workers are blocked. start one more and try again
				workers.Add(1)
				go a.attack(tr, name, &workers, ticks, results)
			}
		}
	}()

	return results
}

// Stop stops the current attack.
func (a *Attacker) Stop() {
	select {
	case <-a.stopch:
		return
	default:
		close(a.stopch)
	}
}

func (a *Attacker) attack(tr Targeter, name string, workers *sync.WaitGroup, ticks <-chan uint64, results chan<- *Result) {
	defer workers.Done()
	for seq := range ticks {
		results <- a.hit(tr, name, seq)
	}
}

func (a *Attacker) hit(tr Targeter, name string, seq uint64) *Result {
	var (
		res = Result{Attack: name, Seq: seq}
		tgt Target
		err error
	)

	defer func() {
		if err != nil {
			res.Error = err.Error()
		}
	}()

	if err = tr(&tgt); err != nil {
		a.Stop()
		return &res
	}

	req, err := tgt.Request()
	if err != nil {
		return &res
	}

	res.Timestamp = time.Now()
	r, err := a.client.Do(req)
	if err != nil {
		return &res
	}
	defer r.Body.Close()

	if res.Body, err = ioutil.ReadAll(r.Body); err != nil {
		return &res
	}
	res.Latency = time.Since(res.Timestamp)
	res.BytesIn = uint64(len(res.Body))

	if req.ContentLength != -1 {
		res.BytesOut = uint64(req.ContentLength)
	}

	if res.Code = uint16(r.StatusCode); res.Code < 200 || res.Code >= 400 {
		res.Error = r.Status
	}

	return &res
}
