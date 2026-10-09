package broker

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func certificate(t *testing.T, ca bool) []byte {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: ca, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestSandboxCAKeepsSystemTrustAndRemovesDisabledOverride(t *testing.T) {
	dir := t.TempDir()
	system, custom := certificate(t, true), certificate(t, true)
	for name, data := range map[string][]byte{"system.pem": system, "custom.pem": custom} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tools := filepath.Join(dir, "tools")
	if err := os.Mkdir(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := installCA(filepath.Join(dir, "custom.pem"), filepath.Join(dir, "system.pem"), tools); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(tools, filepath.Base(toolsCAFile))
	data, err := os.ReadFile(installed)
	if err != nil || !bytes.Contains(data, system) || !bytes.Contains(data, custom) {
		t.Fatalf("combined trust bundle lost a certificate: %v", err)
	}
	if err := installCA("", "", tools); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(installed); !os.IsNotExist(err) {
		t.Fatalf("disabled custom trust left its bundle: %v", err)
	}
}

func TestSandboxCARefusesKeysMalformedAndUnboundedInputs(t *testing.T) {
	dir := t.TempDir()
	valid := certificate(t, true)
	for name, data := range map[string][]byte{
		"empty": nil, "text": []byte("not PEM"), "partial": []byte("-----BEGIN CERTIFICATE-----\nmissing"),
		"leaf": certificate(t, false), "oversize": bytes.Repeat([]byte("x"), maxCABundle+1),
		"key-after-cert":     append(append([]byte{}, valid...), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("fixture")})...),
		"garbage-after-cert": append(append([]byte{}, valid...), []byte("trailing garbage")...),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readCABundle(path, true); err == nil {
				t.Fatal("accepted an invalid CA input")
			}
		})
	}
	path := filepath.Join(dir, "valid")
	if err := os.WriteFile(path, valid, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, dir, filepath.Join(dir, "missing")} {
		if _, err := readCABundle(path, true); err == nil {
			t.Errorf("accepted nonregular or missing CA input %q", path)
		}
	}
	ancestor := filepath.Join(t.TempDir(), "linked-parent")
	if err := os.Symlink(dir, ancestor); err != nil {
		t.Fatal(err)
	}
	if _, err := readCABundle(filepath.Join(ancestor, "valid"), true); err == nil {
		t.Fatal("accepted a symlink ancestor")
	}
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCABundle(fifo, true); err == nil {
		t.Fatal("accepted a FIFO")
	}
}
