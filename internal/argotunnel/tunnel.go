package argotunnel

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/cloudflare/cloudflare-ingress-controller/internal/cloudflare"
	"github.com/cloudflare/cloudflared/origin"
	"github.com/cloudflare/cloudflared/tunnelrpc/pogs"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	repairDelay      = 20 * time.Millisecond
	repairJitter     = 1.0
	tunnelServerName = "cftunnel.com"
)

type tunnelRoute struct {
	name      string
	namespace string
	options   tunnelOptions
	links     tunnelRouteLinkMap
}

type tunnelRule struct {
	service resource
	secret  resource
	host    string
	port    int32
}

type tunnelRouteLinkMap map[tunnelRule]tunnelLink

type tunnelLink interface {
	host() string
	routeRule() tunnelRule
	originURL() string
	originCert() []byte
	options() tunnelOptions
	equal(other tunnelLink) bool
	start() error
	stop() error
}

type syncTunnelLink struct {
	mu     sync.RWMutex
	rule   tunnelRule
	cert   []byte
	opts   tunnelOptions
	config *origin.TunnelConfig
	errCh  chan error
	quitCh chan struct{}
	stopCh chan struct{}
	log    *logrus.Logger
}

func (l *syncTunnelLink) host() string {
	return l.rule.host
}

func (l *syncTunnelLink) originURL() string {
	return l.config.OriginUrl
}

func (l *syncTunnelLink) routeRule() tunnelRule {
	return l.rule
}

func (l *syncTunnelLink) originCert() []byte {
	return l.cert
}

func (l *syncTunnelLink) options() tunnelOptions {
	return l.opts
}

func (l *syncTunnelLink) equal(other tunnelLink) bool {
	if l.rule.host != other.host() {
		return false
	}
	if l.config.OriginUrl != other.originURL() {
		return false
	}
	if l.opts != other.options() {
		return false
	}
	if !bytes.Equal(l.cert, other.originCert()) {
		return false
	}
	return true
}

func (l *syncTunnelLink) start() (err error) {
	if l.stopCh != nil {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.stopCh != nil {
		return nil
	}

	l.log.Infof("link start host: %s, origin: %s", l.host(), l.originURL())
	l.stopCh = make(chan struct{})
	l.quitCh = make(chan struct{})
	go repairFunc(l)()
	go launchFunc(l)()
	return
}

func (l *syncTunnelLink) stop() (err error) {
	if l.stopCh == nil {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.stopCh == nil {
		return nil
	}

	l.log.Infof("link stop host: %s, origin: %s", l.host(), l.originURL())
	close(l.quitCh)
	close(l.stopCh)
	l.quitCh = nil
	l.stopCh = nil
	return
}

func newTunnelLink(rule tunnelRule, cert []byte, options tunnelOptions) tunnelLink {
	return &syncTunnelLink{
		rule:   rule,
		cert:   cert,
		opts:   options,
		config: newLinkTunnelConfig(rule, cert, options),
		errCh:  make(chan error),
		log:    logrus.StandardLogger(),
	}
}

func newLinkTunnelConfig(rule tunnelRule, cert []byte, options tunnelOptions) *origin.TunnelConfig {
	httpTransport := newLinkHTTPTransport()
	return &origin.TunnelConfig{
		EdgeAddrs:  []string{}, // load default values later, see github.com/cloudflare/cloudflared/blob/master/origin/discovery.go#
		OriginUrl:  getOriginURL(rule),
		Hostname:   rule.host,
		OriginCert: cert,
		TlsConfig: &tls.Config{
			RootCAs:    cloudflare.GetCloudflareRootCA(),
			ServerName: tunnelServerName,
		},
		ClientTlsConfig:   httpTransport.TLSClientConfig, // *tls.Config
		Retries:           options.retries,
		HeartbeatInterval: options.heartbeatInterval,
		MaxHeartbeats:     options.heartbeatCount,
		ClientID:          rand.String(32),
		BuildInfo:         origin.GetBuildInfo(),
		ReportedVersion:   versionConfig.version,
		LBPool:            options.lbPool,
		Tags:              []pogs.Tag{},
		HAConnections:     options.haConnections,
		HTTPTransport:     httpTransport,
		Metrics:           metricsConfig.metrics,
		MetricsUpdateFreq: metricsConfig.updateFrequency,
		// todo: alter logger creation to allow easy disable for tests
		ProtocolLogger:     logrus.New(), // new to avoid sharing log-level (very noisy)
		Logger:             logrus.StandardLogger(),
		IsAutoupdated:      false,
		GracePeriod:        options.gracePeriod,
		RunFromTerminal:    false, // bool
		NoChunkedEncoding:  options.noChunkedEncoding,
		CompressionQuality: options.compressionQuality,
	}
}

func newLinkHTTPTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   time.Second * 30,
			KeepAlive: time.Second * 30,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       time.Second * 90,
		TLSHandshakeTimeout:   time.Second * 10,
		ExpectContinueTimeout: time.Second * 1,
		TLSClientConfig:       &tls.Config{},
	}
}

func getOriginURL(rule tunnelRule) (url string) {
	url = fmt.Sprintf("%s.%s:%d", rule.service.name, rule.service.namespace, rule.port)
	return
}

func launchFunc(l *syncTunnelLink) func() {
	cfg := l.config
	errCh := l.errCh
	stopCh := l.stopCh
	return func() {
		// panic-recover - trigger tunnel repair machanism
		// The call to origin.StartTunnelDaemon has been observed to panic.
		// Process the panic into an error on errCh to trigger tunnel repair.
		defer func() {
			if r := recover(); r != nil {
				e := fmt.Errorf("origin daemon run time panic: %v", r)
				errCh <- e
			}
		}()
		errCh <- origin.StartTunnelDaemon(cfg, stopCh, make(chan struct{}))
	}
}

func repairFunc(l *syncTunnelLink) func() {
	ll := l
	errCh := l.errCh
	quitCh := l.quitCh
	return func() {
		log := logrus.StandardLogger()
		for {
			select {
			case <-quitCh:
				return
			case err, open := <-errCh:
				if !open {
					return
				}
				if err != nil {
					func() {
						log.WithFields(logrus.Fields{
							"origin":   ll.config.OriginUrl,
							"hostname": ll.rule.host,
						}).Errorf("link exited with error (%s) '%v', repairing ...", reflect.TypeOf(err), err)

						// linear back-off on runtime error
						delay := wait.Jitter(repairDelay, repairJitter)
						log.WithFields(logrus.Fields{
							"origin":   ll.config.OriginUrl,
							"hostname": ll.rule.host,
						}).Infof("link repair starts in %v", delay)

						select {
						case <-quitCh:
							log.WithFields(logrus.Fields{
								"origin":   ll.config.OriginUrl,
								"hostname": ll.rule.host,
							}).Infof("link repair canceled, stop detected.")
							return
						case <-time.After(delay):
						}

						ll.mu.Lock()
						defer ll.mu.Unlock()

						if ll.stopCh == nil {
							log.WithFields(logrus.Fields{
								"origin":   ll.config.OriginUrl,
								"hostname": ll.rule.host,
							}).Infof("link repair canceled, stop detected.")
							return
						}

						close(ll.stopCh)
						ll.config.ClientID = rand.String(32)
						ll.stopCh = make(chan struct{})
						go launchFunc(ll)()
					}()
				}
			}
		}
	}
}
