package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// consentBase is the approvals page's path.
const consentBase = "/consent/"

// Bounds on what the relay accepts; credential-approval publishes at most 16 KiB,
// and a real assertion is under 4 KiB.
const (
	maxConsentRequestBytes = 16 << 10
	maxConsentAnswerBytes  = 32 << 10
)

var consentIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// consentRequest holds the fields deck reads from a request. deck only relays:
// the page hashes the request's exact bytes and credential-approval verifies
// the signature, so nothing here decides whether an approval counts.
type consentRequest struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
	Title     string `json:"title"`
	Requester string `json:"requester"`
}

func (s *Server) consentRoutes(app *http.ServeMux) {
	app.Handle("GET "+strings.TrimSuffix(consentBase, "/"), http.RedirectHandler(consentBase, http.StatusMovedPermanently))
	app.HandleFunc("GET "+consentBase+"{$}", s.consentList)
	app.HandleFunc("GET "+consentBase+"enroll", s.consentEnroll)
	app.HandleFunc("GET "+consentBase+"{id}", s.consentPage)
	app.HandleFunc("GET "+consentBase+"{id}/request", s.consentRequestBytes)
	app.HandleFunc("GET "+consentBase+"{id}/status", s.consentStatus)
	app.HandleFunc("POST "+consentBase+"{id}/approve", s.consentApprove)
	app.HandleFunc("POST "+consentBase+"{id}/deny", s.consentDeny)
}

func (s *Server) consentPath(id, suffix string) string {
	return filepath.Join(s.deck.cfg.Consent.SpoolDir, id+suffix)
}

// openConsentRequest returns a live request's exact bytes, or false when the
// ID is malformed or the request has ended.
func (s *Server) openConsentRequest(id string) ([]byte, *consentRequest, bool) {
	if !consentIDRE.MatchString(id) {
		return nil, nil, false
	}
	data, err := readBounded(s.consentPath(id, ".request.json"), maxConsentRequestBytes)
	if err != nil {
		return nil, nil, false
	}
	var request consentRequest
	if err := json.Unmarshal(data, &request); err != nil || request.ID != id || request.ExpiresAt <= time.Now().Unix() {
		return nil, nil, false
	}
	return data, &request, true
}

// readBounded reads a regular file up to the limit. A non-blocking open keeps a
// FIFO in the spool from holding up the request.
func readBounded(path string, limit int64) ([]byte, error) {
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

// The consent pages take their assets from this process's dashboard path, so a
// pages-only process serving /static/ from another build cannot supply them.
type consentListView struct {
	BasePath string
	Requests []consentRequest
}

func (s *Server) consentList(w http.ResponseWriter, r *http.Request) {
	view := consentListView{BasePath: s.deck.cfg.BasePath}
	entries, err := os.ReadDir(s.deck.cfg.Consent.SpoolDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.Error("consent: cannot read the spool", "err", err.Error())
	}
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".request.json")
		if !ok {
			continue
		}
		if _, request, ok := s.openConsentRequest(id); ok {
			view.Requests = append(view.Requests, *request)
		}
	}
	sort.Slice(view.Requests, func(i, j int) bool { return view.Requests[i].CreatedAt > view.Requests[j].CreatedAt })
	s.render(w, "consent-list.html", view)
}

func (s *Server) consentEnroll(w http.ResponseWriter, r *http.Request) {
	s.render(w, "consent-enroll.html", struct{ BasePath string }{BasePath: s.deck.cfg.BasePath})
}

// consentPage serves the answer page's shell. Its script fills in the text from
// the same bytes it hashes for the passkey, so what is shown is what is signed.
func (s *Server) consentPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !consentIDRE.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	s.render(w, "consent.html", struct{ BasePath, ID string }{BasePath: s.deck.cfg.BasePath, ID: id})
}

func (s *Server) consentRequestBytes(w http.ResponseWriter, r *http.Request) {
	data, _, ok := s.openConsentRequest(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// consentStatus lets the page follow a request: pending, rejected when the last
// approval did not verify, or gone once it has ended by any path.
func (s *Server) consentStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	state := "gone"
	if _, _, ok := s.openConsentRequest(id); ok {
		state = "pending"
		if _, err := os.Stat(s.consentPath(id, ".response.json")); err != nil {
			if _, err := os.Stat(s.consentPath(id, ".rejected")); err == nil {
				state = "rejected"
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"state": state})
}

func (s *Server) consentApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, _, ok := s.openConsentRequest(id); !ok {
		http.NotFound(w, r)
		return
	}
	var assertion struct {
		ClientDataJSON    string `json:"clientDataJSON"`
		AuthenticatorData string `json:"authenticatorData"`
		Signature         string `json:"signature"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxConsentAnswerBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&assertion); err != nil {
		http.Error(w, "malformed approval", http.StatusBadRequest)
		return
	}
	for _, field := range []string{assertion.ClientDataJSON, assertion.AuthenticatorData, assertion.Signature} {
		if _, err := base64.RawURLEncoding.DecodeString(field); err != nil || field == "" {
			http.Error(w, "malformed approval", http.StatusBadRequest)
			return
		}
	}
	response, err := json.Marshal(map[string]any{"decision": "approve", "assertion": assertion})
	if err != nil {
		http.Error(w, "cannot relay the approval", http.StatusInternalServerError)
		return
	}
	s.writeConsentResponse(w, id, response)
}

func (s *Server) consentDeny(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, _, ok := s.openConsentRequest(id); !ok {
		http.NotFound(w, r)
		return
	}
	s.writeConsentResponse(w, id, []byte(`{"decision":"deny"}`))
}

// writeConsentResponse hands the answer to credential-approval. The 202 means
// only that it was passed on; the page follows the outcome through the status.
func (s *Server) writeConsentResponse(w http.ResponseWriter, id string, response []byte) {
	os.Remove(s.consentPath(id, ".rejected"))
	dir := s.deck.cfg.Consent.SpoolDir
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err == nil {
		defer os.Remove(tmp.Name())
		_, err = tmp.Write(response)
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
	}
	if err == nil {
		err = os.Rename(tmp.Name(), s.consentPath(id, ".response.json"))
	}
	if err != nil {
		s.log.Error("consent: cannot write the answer", "err", err.Error())
		http.Error(w, "cannot relay the answer", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
