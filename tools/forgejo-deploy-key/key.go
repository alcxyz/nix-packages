package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"strings"
)

// deployKey is a new ed25519 key. Only its public half and fingerprint are
// ever printed; the private half goes to the agent and to sops on stdin.
type deployKey struct {
	private ed25519.PrivateKey
	public  ed25519.PublicKey
	comment string
}

func newDeployKey(comment string) (deployKey, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return deployKey{}, err
	}
	return deployKey{private: private, public: public, comment: comment}, nil
}

// publicWire is the public key in SSH wire format.
func (k deployKey) publicWire() []byte {
	var b []byte
	b = appendString(b, []byte("ssh-ed25519"))
	return appendString(b, k.public)
}

// authorizedKey is the public key line, as in authorized_keys.
func (k deployKey) authorizedKey() string {
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(k.publicWire()) + " " + k.comment
}

// fingerprint is the SHA256 fingerprint ssh-keygen -l and Forgejo show.
func (k deployKey) fingerprint() string {
	sum := sha256.Sum256(k.publicWire())
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// openSSHPrivateKey is the private key in OpenSSH's own unencrypted format
// (openssh-key-v1), which ssh, ssh-add and Git read.
func (k deployKey) openSSHPrivateKey() (string, error) {
	check := make([]byte, 4)
	if _, err := rand.Read(check); err != nil {
		return "", err
	}
	var private []byte
	private = append(private, check...)
	private = append(private, check...)
	private = appendString(private, []byte("ssh-ed25519"))
	private = appendString(private, k.public)
	private = appendString(private, k.private) // the seed, then the public key
	private = appendString(private, []byte(k.comment))
	for i := byte(1); len(private)%8 != 0; i++ {
		private = append(private, i)
	}

	var blob []byte
	blob = append(blob, "openssh-key-v1\x00"...)
	blob = appendString(blob, []byte("none")) // cipher
	blob = appendString(blob, []byte("none")) // KDF
	blob = appendString(blob, nil)            // KDF options
	blob = binary.BigEndian.AppendUint32(blob, 1)
	blob = appendString(blob, k.publicWire())
	blob = appendString(blob, private)

	encoded := base64.StdEncoding.EncodeToString(blob)
	var pem strings.Builder
	pem.WriteString("-----BEGIN OPENSSH PRIVATE KEY-----\n")
	for len(encoded) > 70 {
		pem.WriteString(encoded[:70])
		pem.WriteByte('\n')
		encoded = encoded[70:]
	}
	pem.WriteString(encoded)
	pem.WriteString("\n-----END OPENSSH PRIVATE KEY-----\n")
	return pem.String(), nil
}

func appendString(b, s []byte) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}
