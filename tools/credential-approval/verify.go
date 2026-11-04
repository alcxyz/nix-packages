package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
)

// Flags in a WebAuthn authenticator data block.
const (
	flagUserPresent  = 0x01
	flagUserVerified = 0x04
	// The RP ID hash, the flags byte and the four-byte signature counter.
	minAuthenticatorData = 32 + 1 + 4
)

// Assertion is what a browser returns from navigator.credentials.get, each
// field in unpadded base64url.
type Assertion struct {
	ClientDataJSON    string `json:"clientDataJSON"`
	AuthenticatorData string `json:"authenticatorData"`
	Signature         string `json:"signature"`
}

// Verifier checks assertions against the one pinned credential.
type Verifier struct {
	publicKey *ecdsa.PublicKey
	rpID      string
	origin    string
}

// newVerifier takes the credential's public key as base64 SubjectPublicKeyInfo
// DER, the form the browser's getPublicKey gives. Only P-256 (COSE ES256) is
// accepted.
func newVerifier(encodedKey, rpID, origin string) (*Verifier, error) {
	der, err := decodeBase64(encodedKey)
	if err != nil {
		return nil, fmt.Errorf("public key: %w", err)
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("public key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("public key: not an ECDSA P-256 key")
	}
	if rpID == "" {
		return nil, errors.New("the relying party ID is required")
	}
	// Browsers report an origin as scheme and host only, so anything else here
	// could never match and would refuse every real approval.
	originURL, err := url.Parse(origin)
	if err != nil || originURL.Scheme == "" || originURL.Host == "" || originURL.String() != originURL.Scheme+"://"+originURL.Host {
		return nil, fmt.Errorf("origin %q must be a scheme and host, with no path", origin)
	}
	return &Verifier{publicKey: key, rpID: rpID, origin: origin}, nil
}

// decodeBase64 accepts standard or URL-safe base64, padded or not, since the
// pinned key is pasted by hand.
func decodeBase64(s string) ([]byte, error) {
	if data, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return data, nil
	}
	if data, err := base64.URLEncoding.DecodeString(s); err == nil {
		return data, nil
	}
	if data, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return data, nil
	}
	return base64.StdEncoding.DecodeString(s)
}

// challengeFor is the WebAuthn challenge for a request: the hash of its exact
// bytes, so the signature covers everything the page shows.
func challengeFor(requestBytes []byte) []byte {
	sum := sha256.Sum256(requestBytes)
	return sum[:]
}

// verify reports why an assertion does not approve the request with these
// exact bytes, or nil if it does.
func (v *Verifier) verify(requestBytes []byte, assertion Assertion) error {
	clientData, err := base64.RawURLEncoding.DecodeString(assertion.ClientDataJSON)
	if err != nil {
		return fmt.Errorf("client data: %w", err)
	}
	authenticatorData, err := base64.RawURLEncoding.DecodeString(assertion.AuthenticatorData)
	if err != nil {
		return fmt.Errorf("authenticator data: %w", err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(assertion.Signature)
	if err != nil {
		return fmt.Errorf("signature: %w", err)
	}

	var client struct {
		Type        string `json:"type"`
		Challenge   string `json:"challenge"`
		Origin      string `json:"origin"`
		CrossOrigin bool   `json:"crossOrigin"`
	}
	if err := json.Unmarshal(clientData, &client); err != nil {
		return fmt.Errorf("client data: %w", err)
	}
	if client.Type != "webauthn.get" {
		return fmt.Errorf("client data type %q", client.Type)
	}
	if client.Origin != v.origin {
		return fmt.Errorf("origin %q", client.Origin)
	}
	if client.CrossOrigin {
		return errors.New("cross-origin assertion")
	}
	challenge, err := base64.RawURLEncoding.DecodeString(client.Challenge)
	if err != nil {
		return fmt.Errorf("challenge: %w", err)
	}
	if !bytes.Equal(challenge, challengeFor(requestBytes)) {
		return errors.New("the challenge is not this request's")
	}

	if len(authenticatorData) < minAuthenticatorData {
		return errors.New("authenticator data is too short")
	}
	rpHash := sha256.Sum256([]byte(v.rpID))
	if !bytes.Equal(authenticatorData[:32], rpHash[:]) {
		return errors.New("the relying party is not the pinned one")
	}
	flags := authenticatorData[32]
	if flags&flagUserPresent == 0 {
		return errors.New("the user was not present")
	}
	if flags&flagUserVerified == 0 {
		return errors.New("the user was not verified")
	}

	clientHash := sha256.Sum256(clientData)
	signed := sha256.Sum256(append(append([]byte{}, authenticatorData...), clientHash[:]...))
	if !ecdsa.VerifyASN1(v.publicKey, signed[:], signature) {
		return errors.New("the signature is not the pinned credential's")
	}
	return nil
}
