package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

// Bounds on what the spool may hold; a real assertion is under 4 KiB.
const (
	maxResponseBytes = 64 << 10
	maxRequestBytes  = 16 << 10
)

var requestFile = regexp.MustCompile(`^([0-9a-f]{32})\.`)

// Request is one question for the person. Its exact JSON bytes are what the
// passkey signs, and the helper checks against its own copy of them, so editing
// the spool file only makes the signature fail.
type Request struct {
	Version      int    `json:"version"`
	ID           string `json:"id"`
	Nonce        string `json:"nonce"`
	RPID         string `json:"rpId"`
	CredentialID string `json:"credentialId"`
	CreatedAt    int64  `json:"createdAt"`
	ExpiresAt    int64  `json:"expiresAt"`
	Title        string `json:"title"`
	Reason       string `json:"reason"`
	Impact       string `json:"impact"`
	Operation    string `json:"operation"`
	Requester    string `json:"requester"`
	// RequesterURL opens the requester, for a person to look at; empty when
	// there is none. It is signed like the rest, and proves no more.
	RequesterURL string `json:"requesterUrl,omitempty"`
}

func newRequest(now, deadline time.Time, rpID, credentialID, title, reason, impact, operation, requester, requesterURL string) (Request, error) {
	id := make([]byte, 16)
	nonce := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		return Request{}, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return Request{}, err
	}
	return Request{
		Version:      1,
		ID:           hex.EncodeToString(id),
		Nonce:        base64.RawURLEncoding.EncodeToString(nonce),
		RPID:         rpID,
		CredentialID: credentialID,
		CreatedAt:    now.Unix(),
		ExpiresAt:    expirySeconds(deadline),
		Title:        title,
		Reason:       reason,
		Impact:       impact,
		Operation:    operation,
		Requester:    requester,
		RequesterURL: requesterURL,
	}, nil
}

// expirySeconds rounds up, so a short timeout does not expire the moment it
// starts on the page, which reads whole seconds.
func expirySeconds(deadline time.Time) int64 {
	if deadline.Nanosecond() == 0 {
		return deadline.Unix()
	}
	return deadline.Unix() + 1
}

// readRegular reads a regular file up to the limit. A non-blocking open keeps
// a FIFO in the spool from holding up the wait past its deadline.
func readRegular(path string, limit int64) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("too long")
	}
	return data, nil
}

// Spool is the directory shared with the page that relays requests and answers.
// The page runs as the same user, since the directory is private to that user.
type Spool struct {
	dir string
}

func openSpool(dir string, now time.Time) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	spool := &Spool{dir: dir}
	spool.sweep(now)
	return spool, nil
}

func (s *Spool) path(id, suffix string) string {
	return filepath.Join(s.dir, id+suffix)
}

// sweep removes what an earlier run killed outright left behind: anything of a
// request that has expired or cannot be read. Requests still live stay.
func (s *Spool) sweep(now time.Time) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		match := requestFile.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		data, err := readRegular(s.path(match[1], ".request.json"), maxRequestBytes)
		var request Request
		if err == nil {
			err = json.Unmarshal(data, &request)
		}
		if err != nil || request.ExpiresAt <= now.Unix() {
			os.Remove(filepath.Join(s.dir, entry.Name()))
		}
	}
}

func (s *Spool) publish(requestBytes []byte, id string) error {
	return writeAtomic(s.dir, s.path(id, ".request.json"), requestBytes)
}

func writeAtomic(dir, path string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// remove deletes every file of the request.
func (s *Spool) remove(id string) {
	for _, suffix := range []string{".request.json", ".response.json", ".response.claim", ".rejected"} {
		os.Remove(s.path(id, suffix))
	}
}

// Response is the relay's copy of the person's answer. A deny needs no
// signature: forging one can only stop an operation.
type Response struct {
	Decision  string     `json:"decision"`
	Assertion *Assertion `json:"assertion"`
}

// readResponse returns the pending response, or nil when there is none. Renaming
// claims it first, so a good answer written after this one is left to the next
// read, and a bad answer cannot block a good one.
func (s *Spool) readResponse(id string) (*Response, error) {
	claim := s.path(id, ".response.claim")
	if err := os.Rename(s.path(id, ".response.json"), claim); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer os.Remove(claim)
	data, err := readRegular(claim, maxResponseBytes)
	if err != nil {
		return nil, err
	}
	var response Response
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// markRejected tells the page its last answer did not verify, without saying
// why; the reason goes to the helper's stderr.
func (s *Spool) markRejected(id string) {
	if err := writeAtomic(s.dir, s.path(id, ".rejected"), nil); err != nil {
		fmt.Fprintln(os.Stderr, "credential-approval: cannot mark the response rejected:", err)
	}
}

// answerURL is where the person answers this request. The origin has no path,
// as newVerifier checks.
func answerURL(origin, id string) string {
	return origin + "/consent/" + id
}
