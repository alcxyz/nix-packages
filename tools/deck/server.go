package main

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

//go:embed web/templates/*.html web/static/*
var webFS embed.FS

var templateFuncs = template.FuncMap{
	"iso": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	},
	"clock": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return t.Local().Format("15:04:05")
	},
	"ago": func(now, t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return idleText(int(now.Sub(t).Minutes())) + " ago"
	},
	"ownerLabel": func(owner string) string {
		switch owner {
		case "user":
			return "you"
		case "agent":
			return "agent"
		}
		return owner
	},
}

func parseTemplates() (*template.Template, error) {
	return template.New("page").Funcs(templateFuncs).ParseFS(webFS, "web/templates/*.html")
}

// Server serves the page, its fragment, its static files and the actions.
type Server struct {
	deck *Deck
	tmpl *template.Template
	log  *slog.Logger
}

func newHandler(d *Deck, auth *Authenticator, log *slog.Logger) (http.Handler, error) {
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	s := &Server{deck: d, tmpl: tmpl, log: log}
	static, err := fs.Sub(webFS, "web/static")
	if err != nil {
		return nil, err
	}
	base := d.cfg.BasePath

	app := http.NewServeMux()
	app.HandleFunc("GET "+base+"{$}", s.page)
	app.HandleFunc("GET "+base+"fragment", s.fragment)
	app.Handle("GET "+base+"static/", http.StripPrefix(base+"static/", http.FileServerFS(static)))
	app.HandleFunc("POST "+base+"actions/refresh", s.refresh)
	app.HandleFunc("POST "+base+"actions/refresh-all", s.refreshAll)
	app.HandleFunc("POST "+base+"actions/ack", s.ack)
	app.HandleFunc("POST "+base+"actions/forget", s.forget)
	if base != "/" {
		app.Handle("GET "+base[:len(base)-1], http.RedirectHandler(base, http.StatusMovedPermanently))
	}

	// Reserve the dashboard namespace, including its method errors. Content
	// dispatch avoids adding wildcard patterns that conflict with custom bases.
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validContentRequestPath(r) {
			http.NotFound(w, r)
			return
		}
		_, pattern := app.Handler(r)
		reserved := base != "/" && (r.URL.Path == strings.TrimSuffix(base, "/") || strings.HasPrefix(r.URL.Path, base))
		if pattern != "" || reserved || (base == "/" && strings.HasPrefix(r.URL.Path, "/actions/")) {
			app.ServeHTTP(w, r)
			return
		}
		s.content(w, r)
	})
	protected := requireAuth(auth, log, sameOrigin(d.cfg.PublicOrigin, dispatch))
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte("ok\n"))
			return
		}
		protected.ServeHTTP(w, r)
	})
	return securityHeaders(root), nil
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		// same-origin, not no-referrer: under no-referrer browsers send
		// "Origin: null" on plain form posts, which the Origin check refuses,
		// and the page's forms must work without its script.
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) render(w http.ResponseWriter, name string, v PageView) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, v); err != nil {
		s.log.Error("render failed", "template", name, "err", err.Error())
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	s.deck.requestFacts()
	s.render(w, "page.html", s.deck.view())
}

func (s *Server) fragment(w http.ResponseWriter, r *http.Request) {
	s.deck.requestFacts()
	s.render(w, "content", s.deck.view())
}

// done answers an action: a script gets 204, a plain form a redirect back.
func (s *Server) done(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Deck-Fetch") == "1" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, s.deck.cfg.BasePath, http.StatusSeeOther)
}

// formThread reads and checks the instance and thread of an action.
func (s *Server) formThread(w http.ResponseWriter, r *http.Request) (InstanceConfig, string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return InstanceConfig{}, "", false
	}
	in, ok := s.deck.cfg.instance(r.PostForm.Get("instance"))
	id := r.PostForm.Get("thread")
	if !ok || !validThreadID(id) {
		http.Error(w, "unknown instance or thread", http.StatusBadRequest)
		return InstanceConfig{}, "", false
	}
	return in, id, true
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	in, id, ok := s.formThread(w, r)
	if !ok {
		return
	}
	role := r.PostForm.Get("role")
	if role == "" {
		role = s.deck.cfg.Threads.SummaryRole
	}
	if len(s.deck.models[role]) == 0 {
		http.Error(w, "unknown or unusable summary role", http.StatusBadRequest)
		return
	}
	if err := s.deck.requestSummary(in.Name, id, role); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.log.Info("summary requested", "instance", in.Name, "thread", id, "role", role)
	s.done(w, r)
}

func (s *Server) refreshAll(w http.ResponseWriter, r *http.Request) {
	s.deck.requestRefreshAll()
	s.log.Info("facts and summaries requested for all instances")
	s.done(w, r)
}

// ack accepts a settled thread as done, at the settlement the page showed.
// When the thread has settled again since, the user has not seen that
// settlement, so the request is refused. The cached facts may be up to a
// minute old, so the settlement is read again before the ack.
func (s *Server) ack(w http.ResponseWriter, r *http.Request) {
	in, id, ok := s.formThread(w, r)
	if !ok {
		return
	}
	shown := r.PostForm.Get("settledAt")
	if !settledAtRE.MatchString(shown) {
		http.Error(w, "missing or invalid settledAt", http.StatusBadRequest)
		return
	}
	t, found := s.deck.thread(in.Name, id)
	if !found || !t.settled() {
		http.Error(w, "not a settled thread", http.StatusNotFound)
		return
	}
	if t.SettledAt != shown {
		http.Error(w, "the thread has settled again since the page was loaded; reload and check it", http.StatusConflict)
		return
	}
	ctx, cancel := s.actionContext()
	defer cancel()
	inv, err := s.deck.cmds.inventory(ctx, in, s.deck.cfg.Threads.WindowDays)
	if err != nil {
		s.log.Warn("settlement check failed", "instance", in.Name, "err", err.Error())
		http.Error(w, "could not check the thread's settlement: "+err.Error(), http.StatusBadGateway)
		return
	}
	current, found := findThread(inv, id)
	if !found || !current.settled() || current.SettledAt != shown {
		s.deck.requestRefresh(in.Name)
		http.Error(w, "the thread has settled again since the page was loaded; reload and check it", http.StatusConflict)
		return
	}
	args, err := ackArgs([]string{id}, []string{shown})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.runAck(ctx, w, r, in, args)
}

func (s *Server) forget(w http.ResponseWriter, r *http.Request) {
	in, id, ok := s.formThread(w, r)
	if !ok {
		return
	}
	if _, found := s.deck.thread(in.Name, id); !found {
		http.Error(w, "unknown thread", http.StatusNotFound)
		return
	}
	args, err := forgetArgs([]string{id})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := s.actionContext()
	defer cancel()
	s.runAck(ctx, w, r, in, args)
}

// actionTimeout bounds all the commands of one action together, below the
// server's write timeout, so the outcome can still be written.
var actionTimeout = 2 * time.Minute

// actionContext is detached from the request: a client that goes away
// neither interrupts the ack nor leaves a cancelled refresh as the page's
// error. It derives from the service context instead, so shutdown stops
// the action's commands rather than leaving them running.
func (s *Server) actionContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(s.deck.base, actionTimeout)
}

// runAck runs the ack command and refreshes the instance's facts, so the
// page shows the change, both on the action's context. Only the facts are
// refreshed; probes and summaries follow on the next tick.
func (s *Server) runAck(ctx context.Context, w http.ResponseWriter, r *http.Request, in InstanceConfig, args []string) {
	if err := s.deck.cmds.ack(ctx, args); err != nil {
		s.log.Warn("acknowledge failed", "instance", in.Name, "err", err.Error())
		http.Error(w, "acknowledge failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.log.Info("acknowledge recorded", "instance", in.Name, "args", len(args))
	// Wait a little for the instance's refresh lock; when another refresh
	// holds it longer, hand the refresh to the facts loop instead.
	err := s.deck.refreshFacts(ctx, in, 15*time.Second)
	switch {
	case errors.Is(err, errRefreshBusy):
		s.deck.requestRefresh(in.Name)
	case err != nil:
		s.deck.factsError(in.Name, err)
	default:
		s.deck.schedule()
	}
	s.done(w, r)
}

// thread looks a thread up in the current facts.
func (d *Deck) thread(instance, id string) (InventoryThread, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f, ok := d.facts[instance]
	if !ok {
		return InventoryThread{}, false
	}
	return findThread(f.inventory, id)
}

func findThread(inv Inventory, id string) (InventoryThread, bool) {
	for _, t := range inv.Threads {
		if t.ThreadID == id {
			return t, true
		}
	}
	return InventoryThread{}, false
}
