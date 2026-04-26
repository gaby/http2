package http2

import (
	"crypto/tls"
	"errors"
	"net"

	"github.com/valyala/fasthttp"
)

// ErrServerSupport indicates whether the server supports HTTP/2 or not.
var ErrServerSupport = errors.New("server doesn't support HTTP/2")

// ClientTransport wraps a Client to implement fasthttp.RoundTripper and
// provides access to the underlying HTTP/2 client for connection management.
type ClientTransport struct {
	client *Client
}

// RoundTrip implements fasthttp.RoundTripper by calling the wrapped client's Do method.
func (ca *ClientTransport) RoundTrip(hc *fasthttp.HostClient, req *fasthttp.Request, resp *fasthttp.Response) (retry bool, err error) {
	return ca.client.RoundTrip(hc, req, resp)
}

// Close gracefully closes all HTTP/2 connections managed by this transport.
func (ca *ClientTransport) Close() {
	ca.client.Close()
}

func configureDialer(d *Dialer) {
	if d.TLSConfig == nil {
		d.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			MaxVersion: tls.VersionTLS13,
		}
	}

	tlsConfig := d.TLSConfig

	emptyServerName := tlsConfig.ServerName == ""
	if emptyServerName {
		host, _, err := net.SplitHostPort(d.Addr)
		if err != nil {
			host = d.Addr
		}

		tlsConfig.ServerName = host
	}

	tlsConfig.NextProtos = append(tlsConfig.NextProtos, "h2")
}

// ConfigureClient configures the fasthttp.HostClient to run over HTTP/2.
func ConfigureClient(c *fasthttp.HostClient, opts ClientOpts) error {
	emptyServerName := c.TLSConfig != nil && c.TLSConfig.ServerName == ""

	d := &Dialer{
		Addr:      c.Addr,
		TLSConfig: c.TLSConfig,
		NetDial:   c.Dial,
		Timeout:   opts.DialTimeout,
		H2C:       opts.H2C,
	}

	cl := createClient(d, opts)
	cl.conns.Init()

	_, _, err := cl.createConn()
	if err != nil {
		if errors.Is(err, ErrServerSupport) && c.TLSConfig != nil { // remove added config settings
			for i := range c.TLSConfig.NextProtos {
				if c.TLSConfig.NextProtos[i] == "h2" {
					c.TLSConfig.NextProtos = append(c.TLSConfig.NextProtos[:i], c.TLSConfig.NextProtos[i+1:]...)
				}
			}

			if emptyServerName {
				c.TLSConfig.ServerName = ""
			}
		}

		return err
	}

	if !opts.H2C {
		c.IsTLS = true
		c.TLSConfig = d.TLSConfig
	}

	c.Transport = &ClientTransport{client: cl}

	return nil
}

// ConfigureServer configures the fasthttp server to handle
// HTTP/2 connections. The HTTP/2 connection can be only
// established if the fasthttp server is using TLS.
//
// Future implementations may support HTTP/2 through plain TCP.
//
// This package currently supports the following fasthttp.Server settings:
//   - Handler: Obviously, the handler is taken from the Server.
//   - ReadTimeout: Will cancel a stream if the client takes more than ReadTimeout
//     to send a request. This option NEVER closes the connection.
//   - IdleTimeout: Will close the connection if the client doesn't send a request
//     within the IdleTimeout. This option ignores any PING/PONG mechanism.
//     To disable the option you can set it to zero. No value is taken by default,
//     which means that by default ALL connections are open until either endpoint
//     closes the connection.
func ConfigureServer(s *fasthttp.Server, cnf ServerConfig) *Server {
	cnf.defaults()

	s2 := &Server{
		s:   s,
		cnf: cnf,
	}

	s.NextProto(H2TLSProto, s2.ServeConn)

	return s2
}

// ConfigureServerAndConfig configures the fasthttp server to handle HTTP/2 connections
// and your own tlsConfig file. If you are NOT using your own tls config, you may want to use ConfigureServer.
func ConfigureServerAndConfig(s *fasthttp.Server, tlsConfig *tls.Config, cnf ...ServerConfig) *Server {
	var cfg ServerConfig
	if len(cnf) > 0 {
		cfg = cnf[0]
	}
	cfg.defaults()

	s2 := &Server{
		s:   s,
		cnf: cfg,
	}

	s.NextProto(H2TLSProto, s2.ServeConn)
	tlsConfig.NextProtos = append(tlsConfig.NextProtos, H2TLSProto)

	return s2
}

// ConfigureServerH2C configures a fasthttp server to handle HTTP/2 prior-knowledge
// cleartext (h2c) connections. This enables HTTP/2 without TLS, which is useful
// for internal services, testing, and behind TLS-terminating load balancers.
//
// To use h2c, set up a plain (non-TLS) listener and pass connections through
// Server.ServeConn directly, or use this with fasthttp's connection handler.
//
// Example:
//
//	s := &fasthttp.Server{Handler: handler}
//	h2s := http2.ConfigureServerH2C(s, http2.ServerConfig{})
//	// For h2c, use ListenAndServe (not ListenAndServeTLS)
//	s.ListenAndServe(":8080")
func ConfigureServerH2C(s *fasthttp.Server, cnf ServerConfig) *Server {
	cnf.defaults()

	s2 := &Server{
		s:   s,
		cnf: cnf,
	}

	// Register for h2c protocol negotiation
	s.NextProto(H2Clean, s2.ServeConn)

	return s2
}

// ConfigureClientH2C configures the fasthttp.HostClient for cleartext HTTP/2 (h2c).
// This is a convenience wrapper that sets H2C=true in the ClientOpts.
func ConfigureClientH2C(c *fasthttp.HostClient, opts ClientOpts) error {
	opts.H2C = true
	return ConfigureClient(c, opts)
}

// GetClientTransport returns the HTTP/2 ClientTransport from a configured
// fasthttp.HostClient, or nil if the client was not configured for HTTP/2.
// This is useful for accessing the Close() method for cleanup.
func GetClientTransport(c *fasthttp.HostClient) *ClientTransport {
	if ct, ok := c.Transport.(*ClientTransport); ok {
		return ct
	}
	return nil
}

var ErrNotAvailableStreams = errors.New("ran out of available streams")
