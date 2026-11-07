package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

const metadataLimit = 16 * 1024

var contentSlugRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
var contentAssetRE = regexp.MustCompile(`^[a-z0-9]+([_-][a-z0-9]+)*\.(html|css|js|png|jpg|jpeg|gif|webp|avif|svg|ico|json)$`)

type ContentPage struct {
	Title   string `json:"title"`
	Date    string `json:"date"`
	Summary string `json:"summary"`
	// Pinned keeps a page that stays current, such as a live report, ahead of
	// the dated ones in its collection.
	Pinned bool `json:"pinned"`
	// Tile is set when the bundle ships tile.js, the module the overview and
	// the collection index call for its live tile (ADR-0013).
	Tile bool   `json:"-"`
	URL  string `json:"-"`
}

type CollectionView struct {
	Name  string
	URL   string
	Pages []ContentPage
}

type LibraryView struct {
	BasePath string
	// BacklogPath is the backlog view's path, empty when it is off.
	BacklogPath string
	// ConsentPath is the approvals page's path, empty when it is off.
	ConsentPath string
	Collections []CollectionView
	// Needs counts what waits on the owner in the in-process views; empty
	// in the pages-only mode.
	Needs []NeedView
	// Tiles are the pages that ship tile.js, across collections, and
	// TileCollections the collections that hold them, which the overview
	// links beside the tiles rather than listing in the library.
	Tiles           []ContentPage
	TileCollections []CollectionView
	// Library is the collections without tiles, for the overview's library.
	Library []CollectionView
}

// NeedView is one count in the overview's "Needs you" row, linking to the
// view filtered to it.
type NeedView struct {
	Count int
	Label string
	URL   string
	// Tone colours a count above zero: attn, danger, ok or "" for neutral.
	Tone string
	// Known is false while the view has not read its facts yet.
	Known bool
}

// MonthGroup is a calendar month of a collection's pages, for its index.
type MonthGroup struct {
	Label string
	Pages []ContentPage
}

// recentPages is how many of a collection's pages the root index lists, pinned
// pages first.
const recentPages = 3

// parsedDate parses the validated metadata date; readContentPage rejects others.
func (p ContentPage) parsedDate() time.Time {
	t, _ := time.Parse(time.DateOnly, p.Date)
	return t
}

// DisplayDate formats the page date for reading, for example 23 Oct 2026.
func (p ContentPage) DisplayDate() string { return p.parsedDate().Format("2 Jan 2006") }

// Day and Month split the date for the collection index's date column.
func (p ContentPage) Day() string   { return p.parsedDate().Format("2") }
func (p ContentPage) Month() string { return p.parsedDate().Format("Jan") }

// Title turns the collection's directory name into a heading.
func (c CollectionView) Title() string {
	words := strings.Split(c.Name, "-")
	if len(words[0]) > 0 {
		words[0] = strings.ToUpper(words[0][:1]) + words[0][1:]
	}
	return strings.Join(words, " ")
}

// Recent returns the first pages; Pages is already sorted pinned first, then
// newest first.
func (c CollectionView) Recent() []ContentPage {
	if len(c.Pages) > recentPages {
		return c.Pages[:recentPages]
	}
	return c.Pages
}

// Tiles returns the pages that ship tile.js, in index order.
func (c CollectionView) Tiles() []ContentPage {
	var tiles []ContentPage
	for _, page := range c.Pages {
		if page.Tile {
			tiles = append(tiles, page)
		}
	}
	return tiles
}

// Others returns the pages without a tile, for the dated list below the
// tiles, grouped like Months.
func (c CollectionView) Others() []MonthGroup {
	var rest CollectionView
	for _, page := range c.Pages {
		if !page.Tile {
			rest.Pages = append(rest.Pages, page)
		}
	}
	return rest.Months()
}

// HasTiles reports whether any page in the collection has a tile; the
// overview links such a collection beside its tiles instead of in the
// library.
func (c CollectionView) HasTiles() bool {
	for _, page := range c.Pages {
		if page.Tile {
			return true
		}
	}
	return false
}

// HasPinned reports whether the collection pins any page; they sort first.
func (c CollectionView) HasPinned() bool {
	return len(c.Pages) > 0 && c.Pages[0].Pinned
}

// Months groups the sorted pages under Pinned, then by calendar month, newest
// first.
func (c CollectionView) Months() []MonthGroup {
	var groups []MonthGroup
	for _, page := range c.Pages {
		label := page.parsedDate().Format("January 2006")
		if page.Pinned {
			label = "Pinned"
		}
		if len(groups) == 0 || groups[len(groups)-1].Label != label {
			groups = append(groups, MonthGroup{Label: label})
		}
		groups[len(groups)-1].Pages = append(groups[len(groups)-1].Pages, page)
	}
	return groups
}

// openContentDirectory refuses symlink directory entries, and compares the
// opened directory with the checked entry in case an operator replaced it.
// All child access still uses Root, which enforces the filesystem boundary.
func openContentDirectory(root *os.Root, name string) (*os.Root, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", name)
	}
	dir, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := dir.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		dir.Close()
		return nil, fmt.Errorf("%s changed while opening", name)
	}
	return dir, nil
}

func openContentFile(root *os.Root, name string) (*os.File, os.FileInfo, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", name)
	}
	// Nonblocking prevents a concurrent replacement with a FIFO from hanging
	// the request before the descriptor's type and identity can be checked.
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		f.Close()
		return nil, nil, fmt.Errorf("%s changed while opening", name)
	}
	return f, opened, nil
}

func readContentPage(dir *os.Root) (ContentPage, error) {
	f, _, err := openContentFile(dir, "metadata.json")
	if err != nil {
		return ContentPage{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, metadataLimit+1))
	if err != nil {
		return ContentPage{}, err
	}
	if len(data) > metadataLimit {
		return ContentPage{}, errors.New("metadata exceeds 16 KiB")
	}
	var page ContentPage
	if err := json.Unmarshal(data, &page); err != nil {
		return ContentPage{}, fmt.Errorf("invalid metadata: %w", err)
	}
	if strings.TrimSpace(page.Title) == "" {
		return ContentPage{}, errors.New("metadata title is required")
	}
	if date, err := time.Parse(time.DateOnly, page.Date); err != nil || date.Format(time.DateOnly) != page.Date {
		return ContentPage{}, errors.New("metadata date must be YYYY-MM-DD")
	}
	index, _, err := openContentFile(dir, "index.html")
	if err != nil {
		return ContentPage{}, fmt.Errorf("page index: %w", err)
	}
	index.Close()
	if info, err := dir.Lstat("tile.js"); err == nil && info.Mode().IsRegular() {
		page.Tile = true
	}
	return page, nil
}

func (s *Server) collection(root *os.Root, name string) (CollectionView, error) {
	view := CollectionView{Name: name, URL: "/" + name + "/"}
	dir, err := openContentDirectory(root, name)
	if err != nil {
		return view, err
	}
	defer dir.Close()
	entries, err := osReadContentDir(dir)
	if err != nil {
		return view, err
	}
	for _, entry := range entries {
		if !contentSlugRE.MatchString(entry.Name()) || !entry.IsDir() {
			continue
		}
		pageDir, err := openContentDirectory(dir, entry.Name())
		if err != nil {
			s.log.Warn("content page skipped", "collection", name, "page", entry.Name(), "err", err)
			continue
		}
		page, err := readContentPage(pageDir)
		pageDir.Close()
		if err != nil {
			s.log.Warn("content page skipped", "collection", name, "page", entry.Name(), "err", err)
			continue
		}
		page.URL = view.URL + entry.Name() + "/"
		view.Pages = append(view.Pages, page)
	}
	sort.Slice(view.Pages, func(i, j int) bool {
		if view.Pages[i].Pinned != view.Pages[j].Pinned {
			return view.Pages[i].Pinned
		}
		if view.Pages[i].Date != view.Pages[j].Date {
			return view.Pages[i].Date > view.Pages[j].Date
		}
		return view.Pages[i].URL < view.Pages[j].URL
	})
	return view, nil
}

func osReadContentDir(root *os.Root) ([]os.DirEntry, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

// contentCollectionAvailable keeps the existing dashboard namespace, the
// shared asset path and the health endpoint out of the library even when
// matching directories exist.
func (s *Server) contentCollectionAvailable(name string) bool {
	base := s.deck.cfg.BasePath
	if !contentSlugRE.MatchString(name) || name == "healthz" || name == "static" {
		return false
	}
	if s.deck.cfg.Backlog != nil && strings.HasPrefix(s.deck.cfg.Backlog.BasePath, "/"+name+"/") {
		return false
	}
	if s.deck.cfg.Consent != nil && "/"+name+"/" == consentBase {
		return false
	}
	if base == "/" {
		return name != "fragment" && name != "actions"
	}
	return !strings.HasPrefix(base, "/"+name+"/")
}

func (s *Server) renderLibrary(w http.ResponseWriter, name string, view any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, view); err != nil {
		s.log.Error("render failed", "template", name, "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

// Validate before ServeMux can redirect a traversal or encoded separator to a
// dashboard route. Authentication and Origin checks already ran at this point.
func validContentRequestPath(r *http.Request) bool {
	clean := path.Clean(r.URL.Path)
	return (r.URL.Path == clean || r.URL.Path == clean+"/") &&
		!strings.ContainsAny(r.URL.Path, "\\\x00") && r.URL.RawPath == ""
}

func (s *Server) content(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	root, err := os.OpenRoot(s.deck.cfg.ContentDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Warn("content library unavailable", "err", err)
	}
	if root != nil {
		defer root.Close()
	}
	if r.URL.Path == "/" {
		view := LibraryView{BasePath: s.deck.cfg.BasePath}
		if s.deck.cfg.Backlog != nil {
			view.BacklogPath = s.deck.cfg.Backlog.BasePath
		}
		if s.deck.cfg.Consent != nil {
			view.ConsentPath = consentBase
		}
		if root != nil {
			entries, err := osReadContentDir(root)
			if err != nil {
				s.log.Warn("content library listing failed", "err", err)
			}
			for _, entry := range entries {
				if !entry.IsDir() || !s.contentCollectionAvailable(entry.Name()) {
					continue
				}
				collection, err := s.collection(root, entry.Name())
				if err != nil {
					s.log.Warn("content collection skipped", "collection", entry.Name(), "err", err)
					continue
				}
				// Only valid bundles appear in the indexes (ADR-0013), so a
				// directory without one, such as a checkout's docs/, is no
				// collection on the root index.
				if len(collection.Pages) == 0 {
					continue
				}
				view.Collections = append(view.Collections, collection)
			}
			sort.Slice(view.Collections, func(i, j int) bool { return view.Collections[i].Name < view.Collections[j].Name })
			for _, collection := range view.Collections {
				if tiles := collection.Tiles(); len(tiles) > 0 {
					view.Tiles = append(view.Tiles, tiles...)
					view.TileCollections = append(view.TileCollections, collection)
				} else {
					view.Library = append(view.Library, collection)
				}
			}
		}
		view.Needs = s.needs()
		s.renderLibrary(w, "library.html", view)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if root == nil || len(parts) > 3 || !s.contentCollectionAvailable(parts[0]) {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 {
		view, err := s.collection(root, parts[0])
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/") {
			contentRedirect(w, r)
			return
		}
		s.renderLibrary(w, "collection.html", struct {
			BasePath string
			CollectionView
		}{s.deck.cfg.BasePath, view})
		return
	}
	if !contentSlugRE.MatchString(parts[1]) {
		http.NotFound(w, r)
		return
	}
	collection, err := openContentDirectory(root, parts[0])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer collection.Close()
	dir, err := openContentDirectory(collection, parts[1])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer dir.Close()
	if _, err := readContentPage(dir); err != nil {
		s.log.Warn("content page refused", "collection", parts[0], "page", parts[1], "err", err)
		http.NotFound(w, r)
		return
	}
	name := "index.html"
	if len(parts) == 2 {
		if !strings.HasSuffix(r.URL.Path, "/") {
			contentRedirect(w, r)
			return
		}
	} else {
		name = parts[2]
		// metadata.json matches the asset pattern but stays private to the indexes.
		if !contentAssetRE.MatchString(name) || name == "metadata.json" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
	}
	f, info, err := openContentFile(dir, name)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("content asset refused", "collection", parts[0], "page", parts[1], "asset", name, "err", err)
		}
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	http.ServeContent(w, r, name, info.ModTime(), f)
}

func contentRedirect(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Path + "/"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}
