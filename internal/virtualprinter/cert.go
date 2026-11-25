package virtualprinter

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func caInfo(data []byte) (object, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("Invalid certificate")
	}
	cert, e := x509.ParseCertificate(block.Bytes)
	if e != nil {
		return nil, e
	}
	sum := sha256.Sum256(cert.Raw)
	digits := make([]string, len(sum))
	for i, b := range sum {
		digits[i] = fmt.Sprintf("%02X", b)
	}
	return object{"pem": string(data), "fingerprint_sha256": strings.Join(digits, ":"), "not_valid_after": cert.NotAfter.UTC().Format(time.RFC3339)}, nil
}
func serialNumber() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
}
func (s *Service) authority() ([]byte, *rsa.PrivateKey, error) {
	s.caMu.Lock()
	defer s.caMu.Unlock()
	path := filepath.Join(s.dir, "authority.pem")
	b, e := os.ReadFile(path)
	if e == nil {
		cert, rest := pem.Decode(b)
		pk, _ := pem.Decode(rest)
		if cert == nil || pk == nil {
			return nil, nil, errors.New("invalid authority file")
		}
		key, e := x509.ParsePKCS1PrivateKey(pk.Bytes)
		return pem.EncodeToMemory(cert), key, e
	}
	if !os.IsNotExist(e) {
		return nil, nil, e
	}
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		return nil, nil, e
	}
	sn, e := serialNumber()
	if e != nil {
		return nil, nil, e
	}
	ca := &x509.Certificate{SerialNumber: sn, Subject: pkix.Name{CommonName: "Bambastic Virtual Printer CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, e := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if e != nil {
		return nil, nil, e
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	priv := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	tmp, e := os.CreateTemp(s.dir, "ca-*.tmp")
	if e != nil {
		return nil, nil, e
	}
	defer os.Remove(tmp.Name())
	_, e = tmp.Write(append(append([]byte{}, cert...), priv...))
	ce := tmp.Close()
	if e == nil {
		e = ce
	}
	if e == nil {
		e = os.Rename(tmp.Name(), path)
	}
	return cert, key, e
}
func (s *Service) tlsConfig(c cfg) (*tls.Config, error) {
	if c.Tailscale {
		if config, err := s.tailscaleCert(); err == nil {
			return config, nil
		}
	}
	pub, parentKey, e := s.authority()
	if e != nil {
		return nil, e
	}
	block, _ := pem.Decode(pub)
	ca, e := x509.ParseCertificate(block.Bytes)
	if e != nil {
		return nil, e
	}
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		return nil, e
	}
	sn, e := serialNumber()
	if e != nil {
		return nil, e
	}
	leaf := &x509.Certificate{SerialNumber: sn, Subject: pkix.Name{CommonName: c.Serial}, DNSNames: []string{c.Serial}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(1, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	for _, addr := range []string{s.listeningIP(c), c.Bind} {
		if ip := net.ParseIP(addr); ip != nil && !ip.IsUnspecified() {
			leaf.IPAddresses = append(leaf.IPAddresses, ip)
		}
	}
	der, e := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, parentKey)
	if e != nil {
		return nil, e
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der, block.Bytes}, PrivateKey: key}}}, nil
}
