package h2spec

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log"
	"math/big"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/dgrr/http2"
	"github.com/stretchr/testify/require"
	"github.com/summerwind/h2spec/config"
	"github.com/summerwind/h2spec/generic"
	"github.com/summerwind/h2spec/hpack"
	h2spec "github.com/summerwind/h2spec/http2"
	"github.com/summerwind/h2spec/spec"
	"github.com/valyala/fasthttp"
)

// TestH2Spec runs the full h2spec conformance suite (generic, http2 and hpack)
// against a server built on this library. The whole tree must pass: this is the
// RFC parity gate. Individual case selection is intentionally avoided so new
// h2spec cases are picked up automatically.
func TestH2Spec(t *testing.T) {
	port := launchLocalServer(t)

	// Silence h2spec's own stdout reporting.
	oldout := os.Stdout
	os.Stdout = nil
	t.Cleanup(func() {
		os.Stdout = oldout
	})

	specs := []struct {
		name string
		tg   *spec.TestGroup
	}{
		{name: "generic", tg: generic.Spec()},
		{name: "http2", tg: h2spec.Spec()},
		{name: "hpack", tg: hpack.Spec()},
	}

	for _, s := range specs {
		s := s
		t.Run(s.name, func(t *testing.T) {
			conf := &config.Config{
				Host:         "127.0.0.1",
				Port:         port,
				Path:         "/",
				Timeout:      time.Second,
				MaxHeaderLen: 4000,
				TLS:          true,
				Insecure:     true,
			}

			s.tg.Test(conf)

			require.Zerof(t, s.tg.FailedCount,
				"%s: %d failed, %d passed, %d skipped",
				s.name, s.tg.FailedCount, s.tg.PassedCount, s.tg.SkippedCount)
		})
	}
}

func launchLocalServer(t *testing.T) int {
	t.Helper()

	certPEM, keyPEM, err := KeyPair("test.default", time.Time{})
	if err != nil {
		log.Fatalf("Unable to generate certificate: %v", err)
	}

	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.Response.AppendBodyString("Test	 HTTP2")
		},
	}
	http2.ConfigureServer(server, http2.ServerConfig{})

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	go func() {
		log.Println(server.ServeTLSEmbed(ln, certPEM, keyPEM))
	}()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	portInt, err := strconv.Atoi(port)
	require.NoError(t, err)

	return portInt
}

// DefaultDomain domain for the default certificate.
const DefaultDomain = "TEST DEFAULT CERT"

// KeyPair generates cert and key files.
func KeyPair(domain string, expiration time.Time) ([]byte, []byte, error) {
	rsaPrivKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaPrivKey)})

	certPEM, err := PemCert(rsaPrivKey, domain, expiration)
	if err != nil {
		return nil, nil, err
	}
	return certPEM, keyPEM, nil
}

// PemCert generates PEM cert file.
func PemCert(privKey *rsa.PrivateKey, domain string, expiration time.Time) ([]byte, error) {
	derBytes, err := derCert(privKey, expiration, domain)
	if err != nil {
		return nil, err
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes}), nil
}

func derCert(privKey *rsa.PrivateKey, expiration time.Time, domain string) ([]byte, error) {
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, err
	}

	if expiration.IsZero() {
		expiration = time.Now().Add(365 * (24 * time.Hour))
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: DefaultDomain,
		},
		NotBefore: time.Now(),
		NotAfter:  expiration,

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageKeyAgreement | x509.KeyUsageDataEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{domain},
	}

	return x509.CreateCertificate(rand.Reader, &template, &template, &privKey.PublicKey, privKey)
}

// func printTest(tg *spec.TestGroup) {
// 	for _, group := range tg.Groups {
// 		printTest(group)
// 	}
// 	for i := range tg.Tests {
// 		fmt.Printf("{desc: \"%s/%d\"},\n", tg.ID(), i+1)
// 	}
// }
