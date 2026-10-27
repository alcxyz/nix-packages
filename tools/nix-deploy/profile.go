package main

import (
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Build profiles bound how hard the shared build loads this machine. Light
// leaves the Nix settings alone, so one large build still gets every core.
// Heavy caps parallel builds and the cores each may use, for runs that
// compile many derivations, such as the first run after a flake update: with
// the host defaults, several large from-source builds overlap until the
// machine runs out of memory.
const (
	profileAuto  = "auto"
	profileLight = "light"
	profileHeavy = "heavy"
)

// pendingBuilds returns the derivations Nix would build to realise the given
// installables, as listed by `nix build --dry-run`, and the paths it would
// fetch from a substituter, which are not builds; derivations a remote
// builder would take are. A timeout above zero bounds the listing.
func pendingBuilds(installables []string, timeout time.Duration) ([]string, []string, error) {
	// The listing is parsed, so it must be plain text whatever the caller's
	// log format.
	argv := append([]string{"nix", "build", "--dry-run", "--log-format", "raw"}, installables...)
	output, status := capture(timeout, argv...)
	if status != 0 {
		return nil, nil, fmt.Errorf("nix build --dry-run failed: %s", oneLine(lastLine(output)))
	}
	return parsePending(output)
}

// parsePendingBuilds reads the derivations listed under Nix's "will be built"
// heading ("this derivation will be built:" or "these N derivations will be
// built:"); the paths to fetch are listed under a "will be fetched" heading.
// A list under any other heading, or a "will be built" heading with no list
// read, is an error rather than nothing to build, so a change in Nix's
// output cannot pass as a light run. Under systemd (IN_SYSTEMD) Nix starts
// each line with a log level such as "<3>", which is dropped.
func parsePendingBuilds(output string) ([]string, error) {
	drvs, _, err := parsePending(output)
	return drvs, err
}

// parsePending reads the derivations to build and, apart, the paths to fetch.
func parsePending(output string) (drvs, fetches []string, err error) {
	heading := ""
	announced := false
	for _, line := range strings.Split(output, "\n") {
		line = logLevel.ReplaceAllString(line, "")
		if !strings.HasPrefix(line, " ") {
			heading = strings.TrimSpace(line)
			if strings.Contains(heading, "will be built") {
				announced = true
			}
			continue
		}
		path := strings.TrimSpace(line)
		if !strings.HasPrefix(path, "/") {
			continue
		}
		switch {
		case strings.HasSuffix(heading, "will be built:"):
			drvs = append(drvs, path)
		case strings.Contains(heading, "will be fetched"):
			fetches = append(fetches, path)
		default:
			return nil, nil, fmt.Errorf("unexpected listing under %q", heading)
		}
	}
	if announced && len(drvs) == 0 {
		return nil, nil, fmt.Errorf("derivations to build announced but none listed")
	}
	return drvs, fetches, nil
}

// logLevel is the prefix Nix's raw log format adds to each line under
// systemd.
var logLevel = regexp.MustCompile(`^<[0-7]>`)

// chooseProfile picks the build profile for the changed targets, prints it
// with the reason, and returns it. An explicit profile is used as given;
// otherwise the run is heavy when it would build at least heavyBuilds
// derivations. When the count fails, the run is heavy, since the safe side
// of a wrong guess is a slower build.
func (d *deployer) chooseProfile(targets []target, evaluated map[string]evaluation, r *report) string {
	profile := d.settings.buildProfile
	if profile != profileAuto {
		fmt.Printf("deploy: %s build profile (%s)%s\n", profile, d.settings.profileSource, d.profileDetails(profile))
		r.profile = profile
		return profile
	}
	var installables []string
	for _, t := range targets {
		installables = append(installables, evaluated[t.key()].Drv+"^out")
	}
	drvs, _, err := pendingBuilds(installables, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "deploy: cannot count pending builds (%v); heavy build profile%s\n", err, d.profileDetails(profileHeavy))
		r.profile = profileHeavy
		return profileHeavy
	}
	count := len(drvs)
	r.pendingBuilds = &count
	profile = profileLight
	if count >= d.settings.heavyBuilds {
		profile = profileHeavy
	}
	fmt.Printf("deploy: %d derivations to build (heavy from %d); %s build profile%s\n",
		count, d.settings.heavyBuilds, profile, d.profileDetails(profile))
	r.profile = profile
	return profile
}

// previewProfile prints the build profile a dry run's changed targets would
// build under; counting builds nothing.
func (d *deployer) previewProfile(changed []target, evaluated map[string]evaluation, r *report) {
	if len(changed) == 0 {
		return
	}
	d.ui.section("build profile")
	d.chooseProfile(changed, evaluated, r)
}

func (d *deployer) profileDetails(profile string) string {
	if profile == profileHeavy {
		jobs, cores := d.heavyLimits()
		return fmt.Sprintf(": max-jobs %s, cores %s", limitText(jobs), limitText(cores))
	}
	return ": host defaults"
}

func limitText(value int) string {
	if value < 0 {
		return "unchanged (host setting unknown)"
	}
	return strconv.Itoa(value)
}

// buildOptions are the Nix options a build under the profile passes. Nix
// sends both settings to the daemon with every connection, so they bound
// this run's builds only. A limit whose host setting is unknown is left to
// the host.
func (d *deployer) buildOptions(profile string) []string {
	if profile != profileHeavy {
		return nil
	}
	jobs, cores := d.heavyLimits()
	var options []string
	if jobs >= 0 {
		options = append(options, "--option", "max-jobs", strconv.Itoa(jobs))
	}
	if cores >= 0 {
		options = append(options, "--option", "cores", strconv.Itoa(cores))
	}
	return options
}

// heavyLimits returns the heavy max-jobs and cores, each capped at this
// machine's own setting, so heavy never loads a host more than its defaults
// would; -1 leaves a setting that cannot be read to the host.
func (d *deployer) heavyLimits() (jobs, cores int) {
	if d.hostLimits == nil {
		d.hostLimits = &hostLimits{jobs: nixSetting("max-jobs"), cores: nixSetting("cores")}
	}
	return capLimits(d.settings.heavyMaxJobs, d.settings.heavyCores, *d.hostLimits, runtime.NumCPU())
}

// hostLimits are this machine's Nix max-jobs and cores; -1 is unknown.
type hostLimits struct {
	jobs  int
	cores int
}

// capLimits lowers the heavy values to the host's own, or returns -1 for a
// host setting that is unknown. Nix resolves max-jobs = auto to the CPU
// count, and keeps 0 (remote builders only); 0 cores means every CPU.
func capLimits(jobs, cores int, host hostLimits, cpus int) (int, int) {
	jobs = min(jobs, host.jobs)
	if host.cores == 0 {
		cores = min(cores, cpus)
	} else {
		cores = min(cores, host.cores)
	}
	return jobs, cores
}

// nixSetting reads one of this machine's Nix settings as a number, or -1.
// `nix config show NAME` needs Nix 2.20; older releases list every setting
// with `nix show-config`.
func nixSetting(name string) int {
	if output, status := capture(0, "nix", "config", "show", name); status == 0 {
		if value, err := strconv.Atoi(strings.TrimSpace(lastLine(output))); err == nil && value >= 0 {
			return value
		}
	}
	output, status := capture(0, "nix", "show-config")
	if status != 0 {
		return -1
	}
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, " = ")
		if !ok || strings.TrimSpace(key) != name {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && n >= 0 {
			return n
		}
	}
	return -1
}
