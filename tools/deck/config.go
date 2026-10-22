package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is deck's own configuration file.
type Config struct {
	Listen       string         `toml:"listen"`
	BasePath     string         `toml:"basePath"`
	ContentDir   string         `toml:"contentDir"`
	PublicOrigin string         `toml:"publicOrigin"`
	Auth         AuthConfig     `toml:"auth"`
	Threads      ThreadsConfig  `toml:"threads"`
	Commands     CommandsConfig `toml:"commands"`
}

type AuthConfig struct {
	Issuer   string `toml:"issuer"`
	ClientID string `toml:"clientID"`
	Group    string `toml:"group"`
}

type ThreadsConfig struct {
	WindowDays int `toml:"windowDays"`
	// QuietMinutes is a pointer so an explicit 0 (no quiet period) differs
	// from unset (10).
	QuietMinutes       *int             `toml:"quietMinutes"`
	RefreshConcurrency int              `toml:"refreshConcurrency"`
	CallsPerHour       int              `toml:"callsPerHour"`
	SummaryRole        string           `toml:"summaryRole"`
	SummaryRoles       []string         `toml:"summaryRoles"`
	Instances          []InstanceConfig `toml:"instance"`
}

// InstanceConfig names one T3 server: its state database and web app.
type InstanceConfig struct {
	Name          string `toml:"name"`
	Label         string `toml:"label"`
	DB            string `toml:"db"`
	WebURL        string `toml:"webURL"`
	EnvironmentID string `toml:"environmentID"`
}

// CommandsConfig holds the T3 adapter commands; each defaults to its name on
// PATH.
type CommandsConfig struct {
	Inventory string `toml:"inventory"`
	Status    string `toml:"status"`
	Messages  string `toml:"messages"`
	Group     string `toml:"group"`
	Ack       string `toml:"ack"`
}

var instanceNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// basePathRE admits literal path segments only: the base path is joined
// into ServeMux patterns, where braces would make wildcards.
var basePathRE = regexp.MustCompile(`^/([A-Za-z0-9_~-][A-Za-z0-9._~-]*/)*$`)

func defaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "deck.toml"
	}
	return filepath.Join(dir, "deck", "config.toml")
}

func loadConfig(path string) (Config, error) {
	var cfg Config
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return Config{}, fmt.Errorf("config %s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	if err := cfg.normalize(); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// normalize fills defaults and rejects an unusable configuration.
func (c *Config) normalize() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:3775"
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen %q: %w", c.Listen, err)
	}
	if c.BasePath == "" {
		c.BasePath = "/threads/"
	}
	if !strings.HasPrefix(c.BasePath, "/") {
		c.BasePath = "/" + c.BasePath
	}
	if !strings.HasSuffix(c.BasePath, "/") {
		c.BasePath += "/"
	}
	if !basePathRE.MatchString(c.BasePath) {
		return fmt.Errorf("basePath %q may hold only letters, digits and . _ ~ - in its segments", c.BasePath)
	}
	if c.BasePath == "/healthz/" {
		return errors.New("basePath must not be /healthz/")
	}
	if c.ContentDir == "" {
		dir := os.Getenv("XDG_DATA_HOME")
		if !filepath.IsAbs(dir) {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("contentDir default: %w", err)
			}
			dir = filepath.Join(home, ".local", "share")
		}
		c.ContentDir = filepath.Join(dir, "deck", "pages")
	}
	if !filepath.IsAbs(c.ContentDir) {
		return errors.New("contentDir must be an absolute path")
	}

	if c.PublicOrigin == "" {
		return errors.New("publicOrigin is required (for example https://deck.example.com)")
	}
	origin, ok := normalizeOrigin(c.PublicOrigin)
	if !ok {
		return fmt.Errorf("publicOrigin %q must be a scheme and host only, such as https://deck.example.com", c.PublicOrigin)
	}
	c.PublicOrigin = origin

	if c.Threads.WindowDays == 0 {
		c.Threads.WindowDays = 3
	}
	if c.Threads.QuietMinutes == nil {
		quiet := 10
		c.Threads.QuietMinutes = &quiet
	}
	if c.Threads.RefreshConcurrency == 0 {
		c.Threads.RefreshConcurrency = 2
	}
	if c.Threads.CallsPerHour == 0 {
		c.Threads.CallsPerHour = 60
	}
	if c.Threads.SummaryRole == "" {
		c.Threads.SummaryRole = summaryRole
	}
	if c.Threads.SummaryRoles == nil {
		c.Threads.SummaryRoles = []string{"light", "standard", "deep"}
	}
	roles := []string{}
	for _, role := range append(c.Threads.SummaryRoles, c.Threads.SummaryRole) {
		if role == "" || strings.TrimSpace(role) != role {
			return errors.New("threads.summaryRoles must contain nonempty role names without surrounding whitespace")
		}
		if !oneOf(role, roles) {
			roles = append(roles, role)
		}
	}
	c.Threads.SummaryRoles = roles
	if c.Threads.WindowDays < 1 {
		return errors.New("threads.windowDays must be at least 1")
	}
	if *c.Threads.QuietMinutes < 0 {
		return errors.New("threads.quietMinutes must not be negative")
	}
	if c.Threads.RefreshConcurrency < 1 {
		return errors.New("threads.refreshConcurrency must be at least 1")
	}
	if c.Threads.CallsPerHour < 1 {
		return errors.New("threads.callsPerHour must be at least 1")
	}

	if c.Commands.Inventory == "" {
		c.Commands.Inventory = "t3-thread-inventory"
	}
	if c.Commands.Status == "" {
		c.Commands.Status = "t3-thread-status"
	}
	if c.Commands.Messages == "" {
		c.Commands.Messages = "t3-thread-messages"
	}
	if c.Commands.Group == "" {
		c.Commands.Group = "t3-thread-overview-group"
	}
	if c.Commands.Ack == "" {
		c.Commands.Ack = "t3-thread-overview-ack"
	}

	if len(c.Threads.Instances) == 0 {
		return errors.New("at least one [[threads.instance]] is required")
	}
	seen := map[string]bool{}
	for i := range c.Threads.Instances {
		in := &c.Threads.Instances[i]
		if !instanceNameRE.MatchString(in.Name) {
			return fmt.Errorf("threads.instance %d: name %q must match %s", i+1, in.Name, instanceNameRE)
		}
		if seen[in.Name] {
			return fmt.Errorf("threads.instance %q is defined twice", in.Name)
		}
		seen[in.Name] = true
		if in.Label == "" {
			in.Label = in.Name
		}
		if in.DB == "" {
			return fmt.Errorf("threads.instance %q: db is required", in.Name)
		}
		u, err := url.Parse(in.WebURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("threads.instance %q: webURL %q must be an http or https URL", in.Name, in.WebURL)
		}
		in.WebURL = strings.TrimRight(in.WebURL, "/")
	}
	return nil
}

// validateAuth requires a complete [auth] table unless auth is disabled.
func (c *Config) validateAuth(devNoAuth bool) error {
	if devNoAuth {
		host, _, _ := net.SplitHostPort(c.Listen)
		if !isLoopbackHost(host) {
			return fmt.Errorf("-dev-no-auth needs a loopback listen address, not %q", c.Listen)
		}
		return nil
	}
	if c.Auth.Issuer == "" || c.Auth.ClientID == "" || c.Auth.Group == "" {
		return errors.New("auth.issuer, auth.clientID and auth.group are required")
	}
	u, err := url.Parse(c.Auth.Issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("auth.issuer %q must be an https URL", c.Auth.Issuer)
	}
	return nil
}

// normalizeOrigin reduces an http or https URL with no path, query or
// credentials to its origin as browsers send it: scheme and host in lower
// case, without the scheme's default port.
func normalizeOrigin(s string) (string, bool) {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host, true
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Config) instance(name string) (InstanceConfig, bool) {
	for _, in := range c.Threads.Instances {
		if in.Name == name {
			return in, true
		}
	}
	return InstanceConfig{}, false
}
