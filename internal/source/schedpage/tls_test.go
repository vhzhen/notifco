package schedpage

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

// The embedded certificate must be the intended public CA, a CA at all, and not
// expired. When it expires the client fails closed (notes show as unavailable),
// and this test is the reminder to replace it.
func TestEmbeddedIntermediateIsValid(t *testing.T) {
	block, _ := pem.Decode(globalSignIntermediate)
	if block == nil {
		t.Fatal("embedded certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if got := cert.Subject.CommonName; got != "GlobalSign GCC R46 OV TLS CA 2025" {
		t.Errorf("unexpected certificate %q", got)
	}
	if !cert.IsCA {
		t.Error("embedded certificate is not a CA")
	}
	if time.Now().After(cert.NotAfter) {
		t.Errorf("embedded certificate expired %s: replace it", cert.NotAfter)
	}
}

func TestTLSConfigKeepsVerificationOn(t *testing.T) {
	c := tlsConfig()
	if c.InsecureSkipVerify {
		t.Fatal("certificate verification must never be disabled")
	}
	if c.RootCAs == nil || c.MinVersion < 0x0303 {
		t.Errorf("unexpected TLS config: roots=%v min=%x", c.RootCAs != nil, c.MinVersion)
	}
}
