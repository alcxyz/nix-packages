package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	testOrigin = "https://deck.example.com"
	testRP     = "deck.example.com"
)

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func encodedPublicKey(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

func newTestVerifier(t *testing.T, key *ecdsa.PrivateKey) *Verifier {
	t.Helper()
	verifier, err := newVerifier(encodedPublicKey(t, key), testRP, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

// authenticator is a software passkey: what a browser and its authenticator
// produce for an assertion, with each input open to the tests.
type authenticator struct {
	key         *ecdsa.PrivateKey
	rpID        string
	origin      string
	clientType  string
	flags       byte
	crossOrigin bool
}

func owner(key *ecdsa.PrivateKey) authenticator {
	return authenticator{
		key:        key,
		rpID:       testRP,
		origin:     testOrigin,
		clientType: "webauthn.get",
		flags:      flagUserPresent | flagUserVerified,
	}
}

func (a authenticator) sign(t *testing.T, requestBytes []byte) Assertion {
	t.Helper()
	clientData, err := json.Marshal(map[string]any{
		"type":        a.clientType,
		"challenge":   base64.RawURLEncoding.EncodeToString(challengeFor(requestBytes)),
		"origin":      a.origin,
		"crossOrigin": a.crossOrigin,
	})
	if err != nil {
		t.Fatal(err)
	}
	rpHash := sha256.Sum256([]byte(a.rpID))
	authenticatorData := append(rpHash[:], a.flags, 0, 0, 0, 1)
	clientHash := sha256.Sum256(clientData)
	signed := sha256.Sum256(append(append([]byte{}, authenticatorData...), clientHash[:]...))
	signature, err := ecdsa.SignASN1(rand.Reader, a.key, signed[:])
	if err != nil {
		t.Fatal(err)
	}
	return Assertion{
		ClientDataJSON:    base64.RawURLEncoding.EncodeToString(clientData),
		AuthenticatorData: base64.RawURLEncoding.EncodeToString(authenticatorData),
		Signature:         base64.RawURLEncoding.EncodeToString(signature),
	}
}

func testRequest(t *testing.T) ([]byte, Request) {
	t.Helper()
	now := time.Now()
	request, err := newRequest(now, now.Add(time.Minute), testRP, "credential", "Title", "Reason", "Impact", "op; SHA-256 0123", "Requested by T3 thread abc", "")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return data, request
}

func TestVerifyAcceptsOnlyTheOwnerOnThisRequest(t *testing.T) {
	ownerKey := newKey(t)
	verifier := newTestVerifier(t, ownerKey)
	requestBytes, _ := testRequest(t)
	if err := verifier.verify(requestBytes, owner(ownerKey).sign(t, requestBytes)); err != nil {
		t.Fatalf("a valid assertion was refused: %v", err)
	}

	otherBytes, _ := testRequest(t)
	other := owner(newKey(t))
	cases := []struct {
		name      string
		assertion Assertion
	}{
		{"another key", other.sign(t, requestBytes)},
		{"the owner's signature on another request", owner(ownerKey).sign(t, otherBytes)},
		{"another origin", func() Assertion {
			a := owner(ownerKey)
			a.origin = "https://evil.example.com"
			return a.sign(t, requestBytes)
		}()},
		{"another relying party", func() Assertion { a := owner(ownerKey); a.rpID = "id.example.com"; return a.sign(t, requestBytes) }()},
		{"a registration, not an assertion", func() Assertion {
			a := owner(ownerKey)
			a.clientType = "webauthn.create"
			return a.sign(t, requestBytes)
		}()},
		{"no user verification", func() Assertion { a := owner(ownerKey); a.flags = flagUserPresent; return a.sign(t, requestBytes) }()},
		{"no user presence", func() Assertion { a := owner(ownerKey); a.flags = flagUserVerified; return a.sign(t, requestBytes) }()},
		{"cross-origin", func() Assertion { a := owner(ownerKey); a.crossOrigin = true; return a.sign(t, requestBytes) }()},
		{"no signature", func() Assertion { a := owner(ownerKey).sign(t, requestBytes); a.Signature = ""; return a }()},
		{"empty", Assertion{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := verifier.verify(requestBytes, c.assertion); err == nil {
				t.Fatal("the assertion was accepted")
			}
		})
	}
}

// A changed byte anywhere in the request, such as text shown to the person,
// makes the owner's signature on the original fail.
func TestVerifyBindsEveryByteOfTheRequest(t *testing.T) {
	ownerKey := newKey(t)
	verifier := newTestVerifier(t, ownerKey)
	requestBytes, _ := testRequest(t)
	assertion := owner(ownerKey).sign(t, requestBytes)
	for i := range requestBytes {
		changed := append([]byte{}, requestBytes...)
		changed[i] ^= 0x01
		if err := verifier.verify(changed, assertion); err == nil {
			t.Fatalf("byte %d changed and the assertion was still accepted", i)
		}
	}
}

func TestNewVerifierRefusesOtherKeys(t *testing.T) {
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newVerifier(encodedPublicKey(t, p384), testRP, testOrigin); err == nil {
		t.Fatal("a P-384 key was accepted")
	}
	if _, err := newVerifier("not base64!", testRP, testOrigin); err == nil {
		t.Fatal("a malformed key was accepted")
	}
	if _, err := newVerifier(encodedPublicKey(t, newKey(t)), "", testOrigin); err == nil {
		t.Fatal("an empty relying party was accepted")
	}
}

// answer waits for the request file, then writes the response the given
// function makes from its exact bytes.
func answer(t *testing.T, dir string, respond func(id string, requestBytes []byte) Response) {
	t.Helper()
	go func() {
		for range 200 {
			matches, _ := filepath.Glob(filepath.Join(dir, "*.request.json"))
			if len(matches) == 1 {
				data, err := os.ReadFile(matches[0])
				if err != nil {
					return
				}
				id := filepath.Base(matches[0])[:32]
				response, _ := json.Marshal(respond(id, data))
				path := filepath.Join(dir, id+".response.json")
				os.WriteFile(path+".tmp", response, 0o600)
				os.Rename(path+".tmp", path)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

func waitArgs(t *testing.T, dir string, key *ecdsa.PrivateKey, timeout string) []string {
	t.Helper()
	return []string{
		"wait", "--spool", dir, "--origin", testOrigin, "--rp-id", testRP,
		"--public-key", encodedPublicKey(t, key), "--credential-id", "credential",
		"--timeout", timeout, "--title", "Title", "--reason", "Reason", "--impact", "Impact",
		"--operation", "op; SHA-256 0123", "--requester", "No T3 thread",
	}
}

func spoolFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestWaitApproves(t *testing.T) {
	dir := t.TempDir()
	key := newKey(t)
	answer(t, dir, func(_ string, requestBytes []byte) Response {
		assertion := owner(key).sign(t, requestBytes)
		return Response{Decision: "approve", Assertion: &assertion}
	})
	if code := run(waitArgs(t, dir, key, "10s")); code != exitApproved {
		t.Fatalf("exit %d", code)
	}
	if files := spoolFiles(t, dir); len(files) != 0 {
		t.Fatalf("left in the spool: %v", files)
	}
}

func TestWaitDenies(t *testing.T) {
	dir := t.TempDir()
	answer(t, dir, func(string, []byte) Response { return Response{Decision: "deny"} })
	if code := run(waitArgs(t, dir, newKey(t), "10s")); code != exitDenied {
		t.Fatalf("exit %d", code)
	}
}

func TestWaitExpiresWithoutAnAnswer(t *testing.T) {
	dir := t.TempDir()
	if code := run(waitArgs(t, dir, newKey(t), "1s")); code != exitExpired {
		t.Fatalf("exit %d", code)
	}
	if files := spoolFiles(t, dir); len(files) != 0 {
		t.Fatalf("left in the spool: %v", files)
	}
}

// Anything that can write to the spool can only offer answers: another passkey's
// approval or a replay of the owner's earlier signature changes nothing, and the
// owner's real approval still works afterwards.
func TestWaitIgnoresForgeriesThenAcceptsTheOwner(t *testing.T) {
	dir := t.TempDir()
	ownerKey := newKey(t)
	earlierBytes, _ := testRequest(t)
	replay := owner(ownerKey).sign(t, earlierBytes)
	other := owner(newKey(t))

	spool, err := openSpool(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	requestBytes, request := testRequest(t)
	if err := spool.publish(requestBytes, request.ID); err != nil {
		t.Fatal(err)
	}
	verifier := newTestVerifier(t, ownerKey)

	write := func(data []byte) {
		path := spool.path(request.ID, ".response.json")
		if err := writeAtomic(dir, path, data); err != nil {
			t.Fatal(err)
		}
	}
	marshal := func(response Response) []byte {
		data, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	forged := other.sign(t, requestBytes)
	bad := [][]byte{
		marshal(Response{Decision: "approve", Assertion: &forged}),
		marshal(Response{Decision: "approve", Assertion: &replay}),
		marshal(Response{Decision: "approve"}),
		marshal(Response{Decision: "maybe"}),
		[]byte("not json"),
	}
	for _, data := range bad {
		write(data)
		response, err := spool.readResponse(request.ID)
		if err == nil && response != nil {
			if _, answered := evaluate(response, verifier, requestBytes); answered {
				t.Fatalf("answered by %s", data)
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- waitForAnswer(ctx, time.Now().Add(5*time.Second), spool, verifier, request.ID, requestBytes)
	}()
	write(marshal(Response{Decision: "approve", Assertion: &forged}))
	time.Sleep(2 * pollInterval)
	real := owner(ownerKey).sign(t, requestBytes)
	write(marshal(Response{Decision: "approve", Assertion: &real}))
	if code := <-done; code != exitApproved {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(spool.path(request.ID, ".rejected")); err != nil {
		t.Fatalf("the forged answer was not marked rejected: %v", err)
	}
}

// Editing the spool's copy of the text the person sees does not change what
// the helper checks against, so a signature made over the edited text fails.
func TestWaitChecksItsOwnCopyOfTheRequest(t *testing.T) {
	dir := t.TempDir()
	key := newKey(t)
	answer(t, dir, func(id string, requestBytes []byte) Response {
		var request Request
		json.Unmarshal(requestBytes, &request)
		request.Impact = "Nothing important"
		edited, _ := json.Marshal(request)
		os.WriteFile(filepath.Join(dir, id+".request.json"), edited, 0o600)
		assertion := owner(key).sign(t, edited)
		return Response{Decision: "approve", Assertion: &assertion}
	})
	if code := run(waitArgs(t, dir, key, "2s")); code != exitExpired {
		t.Fatalf("exit %d", code)
	}
}

func TestSpoolSweepRemovesOnlyDeadRequests(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	live := Request{ID: "0123456789abcdef0123456789abcdef", ExpiresAt: now.Add(time.Minute).Unix()}
	dead := Request{ID: "fedcba9876543210fedcba9876543210", ExpiresAt: now.Add(-time.Minute).Unix()}
	for _, request := range []Request{live, dead} {
		data, _ := json.Marshal(request)
		if err := os.WriteFile(filepath.Join(dir, request.ID+".request.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, dead.ID+".response.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unrelated"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openSpool(dir, now); err != nil {
		t.Fatal(err)
	}
	files := spoolFiles(t, dir)
	want := map[string]bool{live.ID + ".request.json": true, "unrelated": true}
	if len(files) != len(want) {
		t.Fatalf("files %v", files)
	}
	for _, name := range files {
		if !want[name] {
			t.Fatalf("files %v", files)
		}
	}
}

func TestWaitRefusesAnApprovalOnceTheWaitHasEnded(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name     string
		ctx      context.Context
		deadline time.Time
	}{
		// A live context with a past deadline: the moment before its timer runs.
		{"past the deadline", context.Background(), time.Now().Add(-time.Millisecond)},
		{"after a signal", cancelled, time.Now().Add(time.Minute)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			key := newKey(t)
			spool, err := openSpool(dir, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			requestBytes, request := testRequest(t)
			assertion := owner(key).sign(t, requestBytes)
			data, err := json.Marshal(Response{Decision: "approve", Assertion: &assertion})
			if err != nil {
				t.Fatal(err)
			}
			if err := writeAtomic(dir, spool.path(request.ID, ".response.json"), data); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(c.ctx, time.Second)
			defer cancel()
			if code := waitForAnswer(ctx, c.deadline, spool, newTestVerifier(t, key), request.ID, requestBytes); code != exitExpired {
				t.Fatalf("exit %d", code)
			}
		})
	}
}

// A good answer that arrives while a bad one is being read must survive.
func TestReadResponseKeepsAReplacementAnswer(t *testing.T) {
	dir := t.TempDir()
	spool := &Spool{dir: dir}
	id := "0123456789abcdef0123456789abcdef"
	if err := writeAtomic(dir, spool.path(id, ".response.json"), []byte("not json")); err != nil {
		t.Fatal(err)
	}
	if _, err := spool.readResponse(id); err == nil {
		t.Fatal("bad JSON was accepted")
	}
	if err := writeAtomic(dir, spool.path(id, ".response.json"), []byte(`{"decision":"deny"}`)); err != nil {
		t.Fatal(err)
	}
	response, err := spool.readResponse(id)
	if err != nil || response == nil || response.Decision != "deny" {
		t.Fatalf("response %v, err %v", response, err)
	}
	if _, err := os.Stat(spool.path(id, ".response.claim")); err == nil {
		t.Fatal("the claim file was left behind")
	}
}

func TestWaitIsNotHeldUpByAFIFO(t *testing.T) {
	dir := t.TempDir()
	key := newKey(t)
	go func() {
		for range 200 {
			matches, _ := filepath.Glob(filepath.Join(dir, "*.request.json"))
			if len(matches) == 1 {
				syscall.Mkfifo(filepath.Join(dir, filepath.Base(matches[0])[:32]+".response.json"), 0o600)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	done := make(chan int, 1)
	go func() { done <- run(waitArgs(t, dir, key, "2s")) }()
	select {
	case code := <-done:
		if code != exitExpired {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait outlasted its deadline")
	}
}

func TestShortTimeoutIsStillOpenWhenItStarts(t *testing.T) {
	now := time.Unix(100, 100_000_000)
	request, err := newRequest(now, now.Add(500*time.Millisecond), testRP, "c", "t", "r", "i", "o", "q", "")
	if err != nil {
		t.Fatal(err)
	}
	if request.ExpiresAt <= now.Unix() {
		t.Fatalf("expires %d, starts %d", request.ExpiresAt, now.Unix())
	}
}

func TestNewVerifierRefusesAnOriginThatCouldNeverMatch(t *testing.T) {
	key := encodedPublicKey(t, newKey(t))
	for _, origin := range []string{"", "deck.example.com", "https://deck.example.com/", "https://deck.example.com/consent"} {
		if _, err := newVerifier(key, testRP, origin); err == nil {
			t.Errorf("origin %q was accepted", origin)
		}
	}
	if _, err := newVerifier(key, "localhost", "http://localhost:3785"); err != nil {
		t.Errorf("a port was refused: %v", err)
	}
}

func TestWaitRefusesAnOversizedRequest(t *testing.T) {
	args := waitArgs(t, t.TempDir(), newKey(t), "1s")
	args = append(args, "--reason", strings.Repeat("r", maxRequestBytes))
	if code := run(args); code != exitUsage {
		t.Fatalf("exit %d", code)
	}
}

func TestRequesterURLIsSignedAndOnlyHTTPS(t *testing.T) {
	now := time.Now()
	request, err := newRequest(now, now.Add(time.Minute), testRP, "c", "t", "r", "i", "o", "q", "https://t3.example.com/e/thread")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"requesterUrl":"https://t3.example.com/e/thread"`) {
		t.Fatalf("not in the signed bytes: %s", data)
	}
	withoutURL := request
	withoutURL.RequesterURL = ""
	data, err = json.Marshal(withoutURL)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "requesterUrl") {
		t.Fatalf("an empty URL was published: %s", data)
	}
	args := append(waitArgs(t, t.TempDir(), newKey(t), "1s"), "--requester-url", "javascript:alert(1)")
	if code := run(args); code != exitUsage {
		t.Fatalf("exit %d", code)
	}
}

func TestUsage(t *testing.T) {
	if code := run([]string{"other"}); code != exitUsage {
		t.Fatalf("exit %d", code)
	}
	if code := run([]string{"wait", "--spool", t.TempDir()}); code != exitUsage {
		t.Fatalf("exit %d", code)
	}
}
