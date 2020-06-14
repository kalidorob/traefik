package healthcheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/containous/traefik/v2/pkg/config/runtime"
	"github.com/containous/traefik/v2/pkg/log"
	"github.com/containous/traefik/v2/pkg/safe"
	"github.com/go-kit/kit/metrics"
	"github.com/vulcand/oxy/roundrobin"
	grpcLib "google.golang.org/grpc"
	grpcHealth "google.golang.org/grpc/health/grpc_health_v1"
)

const (
	serverUp   = "UP"
	serverDown = "DOWN"
)

var singleton *HealthCheck
var once sync.Once

// Balancer is the set of operations required to manage the list of servers in a load-balancer.
type Balancer interface {
	Servers() []*url.URL
	RemoveServer(u *url.URL) error
	UpsertServer(u *url.URL, options ...roundrobin.ServerOption) error
}

// BalancerHandler includes functionality for load-balancing management.
type BalancerHandler interface {
	ServeHTTP(w http.ResponseWriter, req *http.Request)
	Balancer
}

// metricsRegistry is a local interface in the health check package,
// exposing only the required metrics necessary for the health check package.
// This makes it easier for the tests.
type metricsRegistry interface {
	BackendServerUpGauge() metrics.Gauge
}

// Options are the public health check options.
type Options struct {
	Headers         map[string]string
	Hostname        string
	Scheme          string
	Path            string
	Port            int
	FollowRedirects bool
	Transport       http.RoundTripper
	Interval        time.Duration
	Timeout         time.Duration
	LB              Balancer
	Service         string
}

func (opt Options) String() string {
	return fmt.Sprintf("[Hostname: %s Headers: %v Path: %s Port: %d Interval: %s Timeout: %s FollowRedirects: %v Service: %s]", opt.Hostname, opt.Headers, opt.Path, opt.Port, opt.Interval, opt.Timeout, opt.FollowRedirects, opt.Service)
}

type backendURL struct {
	url    *url.URL
	weight int
}

// BackendConfig HealthCheck configuration for a backend.
type BackendConfig struct {
	Options
	name         string
	disabledURLs []backendURL
}

func (b *BackendConfig) newHostPort(serverURL *url.URL) (string, error) {
	return serverURL.Host, nil
}

func (b *BackendConfig) newRequest(serverURL *url.URL) (*http.Request, error) {
	u, err := serverURL.Parse(b.Path)
	if err != nil {
		return nil, err
	}

	if len(b.Scheme) > 0 {
		u.Scheme = b.Scheme
	}

	if b.Port != 0 {
		u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(b.Port))
	}

	return http.NewRequest(http.MethodGet, u.String(), http.NoBody)
}

// this function adds additional http headers and hostname to http.request.
func (b *BackendConfig) addHeadersAndHost(req *http.Request) *http.Request {
	if b.Options.Hostname != "" {
		req.Host = b.Options.Hostname
	}

	for k, v := range b.Options.Headers {
		req.Header.Set(k, v)
	}
	return req
}

// HealthCheck struct.
type HealthCheck struct {
	Backends map[string]*BackendConfig
	metrics  metricsRegistry
	cancel   context.CancelFunc
}

// SetBackendsConfiguration set backends configuration.
func (hc *HealthCheck) SetBackendsConfiguration(parentCtx context.Context, backends map[string]*BackendConfig) {
	hc.Backends = backends
	if hc.cancel != nil {
		hc.cancel()
	}
	ctx, cancel := context.WithCancel(parentCtx)
	hc.cancel = cancel

	for _, backend := range backends {
		currentBackend := backend
		safe.Go(func() {
			hc.execute(ctx, currentBackend)
		})
	}
}

func (hc *HealthCheck) execute(ctx context.Context, backend *BackendConfig) {
	logger := log.FromContext(ctx)
	logger.Debugf("Initial health check for backend: %q", backend.name)

	hc.checkBackend(ctx, backend)
	ticker := time.NewTicker(backend.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Debugf("Stopping current health check goroutines of backend: %s", backend.name)
			return
		case <-ticker.C:
			logger.Debugf("Refreshing health check for backend: %s", backend.name)
			hc.checkBackend(ctx, backend)
		}
	}
}

func (hc *HealthCheck) checkBackend(ctx context.Context, backend *BackendConfig) {
	logger := log.FromContext(ctx)

	enabledURLs := backend.LB.Servers()
	var newDisabledURLs []backendURL
	for _, disabledURL := range backend.disabledURLs {
		if err := hc.checkHealth(disabledURL.url, backend); err == nil {
			logger.Warnf("Health check up: Returning to server list. Backend: %q URL: %q Weight: %d",
				backend.name, disabledURL.url.String(), disabledURL.weight)
			if err = backend.LB.UpsertServer(disabledURL.url, roundrobin.Weight(disabledURL.weight)); err != nil {
				logger.Error(err)
			}
		} else {
			logger.Warnf("Health check still failing. Backend: %q URL: %q Reason: %s", backend.name, disabledURL.url.String(), err)
			newDisabledURLs = append(newDisabledURLs, disabledURL)
		}
	}
	backend.disabledURLs = newDisabledURLs

	for _, enableURL := range enabledURLs {
		if err := hc.checkHealth(enableURL, backend); err != nil {
			weight := 1
			rr, ok := backend.LB.(*roundrobin.RoundRobin)
			if ok {
				var gotWeight bool
				weight, gotWeight = rr.ServerWeight(enableURL)
				if !gotWeight {
					weight = 1
				}
			}
			logger.Warnf("Health check failed, removing from server list. Backend: %q URL: %q Weight: %d Reason: %s", backend.name, enableURL.String(), weight, err)
			if err := backend.LB.RemoveServer(enableURL); err != nil {
				logger.Error(err)
			}
			backend.disabledURLs = append(backend.disabledURLs, backendURL{enableURL, weight})
		}
	}
}

// GetHealthCheck returns the health check which is guaranteed to be a singleton.
func GetHealthCheck() *HealthCheck {
	once.Do(func() {
		singleton = newHealthCheck()
	})
	return singleton
}

func newHealthCheck() *HealthCheck {
	return &HealthCheck{
		Backends: make(map[string]*BackendConfig),
	}
}

// NewBackendConfig Instantiate a new BackendConfig.
func NewBackendConfig(options Options, backendName string) *BackendConfig {
	return &BackendConfig{
		Options: options,
		name:    backendName,
	}
}

// checkHealth returns a nil error in case it was successful and otherwise
// a non-nil error with a meaningful description why the health check failed.
func (hc *HealthCheck) checkHealth(serverURL *url.URL, backend *BackendConfig) error {
	u, err := serverURL.Parse("/")
	if err != nil {
		return err
	}

	if backend.Options.Service != "" || (u.Scheme == "h2c" && backend.Path == "/grpc.health.v1.Health/Check") {
		return checkHealthGRPC(serverURL, backend)
	}

	return checkHealthHTTP(serverURL, backend)
}

// checkHealthGRPC returns a nil error in case it was successful and otherwise
// a non-nil error with a meaningful description why the health check failed.
func checkHealthGRPC(serverURL *url.URL, backend *BackendConfig) error {
	addr, err := backend.newHostPort(serverURL)
	if err != nil {
		return fmt.Errorf("failed to create GRPC address: %w", err)
	}

	opts := []grpcLib.DialOption{grpcLib.WithInsecure()}

	conn, err := grpcLib.Dial(addr, opts...)
	if err != nil {
		return err
	}
	// Make sure we don't leak any fds.
	defer conn.Close()

	client := grpcHealth.NewHealthClient(conn)

	req := &grpcHealth.HealthCheckRequest{Service: ""}

	if backend.Options.Service != "" {
		req.Service = backend.Options.Service
	}

	x, err := client.Check(context.Background(), req)
	if err != nil {
		return err
	}

	if x.Status != grpcHealth.HealthCheckResponse_SERVING {
		return errors.New("not serving")
	}

	return nil
}

// checkHealthHTTP returns a nil error in case it was successful and otherwise
// a non-nil error with a meaningful description why the health check failed.
func checkHealthHTTP(serverURL *url.URL, backend *BackendConfig) error {
	req, err := backend.newRequest(serverURL)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	req = backend.addHeadersAndHost(req)

	client := http.Client{
		Timeout:   backend.Options.Timeout,
		Transport: backend.Options.Transport,
	}

	if !backend.FollowRedirects {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("received error status code: %v", resp.StatusCode)
	}

	return nil
}

// NewLBStatusUpdater returns a new LbStatusUpdater.
func NewLBStatusUpdater(bh BalancerHandler, info *runtime.ServiceInfo) *LbStatusUpdater {
	return &LbStatusUpdater{
		BalancerHandler: bh,
		serviceInfo:     info,
	}
}

// LbStatusUpdater wraps a BalancerHandler and a ServiceInfo,
// so it can keep track of the status of a server in the ServiceInfo.
type LbStatusUpdater struct {
	BalancerHandler
	serviceInfo *runtime.ServiceInfo // can be nil
}

// RemoveServer removes the given server from the BalancerHandler,
// and updates the status of the server to "DOWN".
func (lb *LbStatusUpdater) RemoveServer(u *url.URL) error {
	err := lb.BalancerHandler.RemoveServer(u)
	if err == nil && lb.serviceInfo != nil {
		lb.serviceInfo.UpdateServerStatus(u.String(), serverDown)
	}
	return err
}

// UpsertServer adds the given server to the BalancerHandler,
// and updates the status of the server to "UP".
func (lb *LbStatusUpdater) UpsertServer(u *url.URL, options ...roundrobin.ServerOption) error {
	err := lb.BalancerHandler.UpsertServer(u, options...)
	if err == nil && lb.serviceInfo != nil {
		lb.serviceInfo.UpdateServerStatus(u.String(), serverUp)
	}
	return err
}

// Balancers is a list of Balancers(s) that implements the Balancer interface.
type Balancers []Balancer

// Servers returns the servers url from all the BalancerHandler.
func (b Balancers) Servers() []*url.URL {
	var servers []*url.URL
	for _, lb := range b {
		servers = append(servers, lb.Servers()...)
	}

	return servers
}

// RemoveServer removes the given server from all the BalancerHandler,
// and updates the status of the server to "DOWN".
func (b Balancers) RemoveServer(u *url.URL) error {
	for _, lb := range b {
		if err := lb.RemoveServer(u); err != nil {
			return err
		}
	}
	return nil
}

// UpsertServer adds the given server to all the BalancerHandler,
// and updates the status of the server to "UP".
func (b Balancers) UpsertServer(u *url.URL, options ...roundrobin.ServerOption) error {
	for _, lb := range b {
		if err := lb.UpsertServer(u, options...); err != nil {
			return err
		}
	}
	return nil
}
