package k8snode

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Usage is the command-line help.
const Usage = `Usage: k8s-node-power reboot [options] <host>
       k8s-node-power off [options] <host>
       k8s-node-power on [options] <host>

Kubernetes-aware node power helpers.

Commands:
  reboot                   Move database primaries, cordon, drain, reboot,
                           wait for Ready, and uncordon.
  off                      Move database primaries, cordon, drain, power off,
                           and leave cordoned.
  on                       Wait for host/node to return, uncordon, settle.

Options:
  --node <name>            Kubernetes node name. Defaults to <host>.
  --ssh-target <target>    SSH target. Defaults to root@<host>.
  --skip-drain             Cordon only, then reboot. Use for intentional disruption.
  --force-drain            Pass --force to kubectl drain for unmanaged pods.
  --bypass-pdb             Use pod deletion instead of eviction; ignores PDBs.
  --leave-cordoned         Keep the node cordoned after it returns.
  --resume-maintenance     Reuse an already cordoned, drained, and detached
                           node for another power cycle. Fails closed if the
                           maintenance-state gates do not pass.
  --check-only             Validate preflight or resumed-maintenance gates and
                           report planned database switchovers without changes.
  --needed                 reboot only: first read whether the host needs a
                           reboot (its booted kernel, initrd, kernel modules or
                           kernel parameters differ from the current or
                           next-boot system's) and, when it does not and the
                           node is not cordoned, skip it without changing
                           anything. When that cannot be told, the reboot goes
                           ahead. Not with --resume-maintenance.
  --drain-timeout <dur>    kubectl drain duration. Default: 10m.
  --ready-timeout <dur>    kubectl wait duration. Default: 10m.
  --fence-timeout <dur>    Agent recovery/heartbeat wait duration. Default: 10m.
  --settle-timeout <dur>   Workload/Longhorn health wait duration. Default: 90m.
  --ssh-timeout <seconds>  SSH return timeout. Default: 900.
  --require-longhorn-backup-target
                           Fail if the Longhorn backup target is unavailable.
  --min-free-pod-slots N   Require N aggregate Pod slots after evacuation on
                           the remaining stable nodes.
                           Default: 20.
  -h, --help               Show this help.
  --version                Print the version.

Durations are a whole number of seconds, optionally followed by s, m or h.

Agent-enabled targets require healthy, verified fence states on every server
before maintenance, with no recent unfence, disabled annotation, or out-of-service
taint. The target agent must publish stopped before reboot/off. Before uncordon,
its healthy agent must return in the original mode (on accepts either mode)
and publish a heartbeat recorded by the API server after the agent was stopped
or, for on, after a new maintenance baseline established once the node is Ready.
Fence failures after cordon leave the node cordoned. Targets without an agent
annotation skip these gates. --check-only also checks fence preflight.

Requires kubectl access to the target cluster and SSH sudo rights on the host.
K8S_NODE_SSH_USER (default root) is the SSH user for the other nodes' network
audits, and K8S_NODE_NETWORK_AUDIT_SCRIPT names the audit helper.
`

// ErrHelp asks for the help text on standard output; ErrUsage for the help
// text on standard error and exit status 2.
var (
	ErrHelp  = errors.New("help requested")
	ErrUsage = errors.New("no host given")
)

// Options are a validated command line.
type Options struct {
	// Action is reboot, off or on.
	Action    string
	Host      string
	Node      string
	SSHTarget string

	SkipDrain         bool
	ForceDrain        bool
	BypassPDB         bool
	LeaveCordoned     bool
	ResumeMaintenance bool
	CheckOnly         bool
	// Needed skips a reboot the host does not need.
	Needed bool

	// Durations keep the operator's spelling, except that ParseArgs adds "s"
	// to a bare number of seconds: kubectl rejects --timeout=30, and every
	// duration also reaches kubectl or a message as given.
	DrainTimeout  string
	ReadyTimeout  string
	FenceTimeout  string
	SettleTimeout string

	SSHTimeoutSeconds           int
	RequireLonghornBackupTarget bool
	MinFreePodSlots             int

	// NetworkAuditScript is the local path of the audit helper run on every
	// node over SSH.
	NetworkAuditScript string
	// NodeSSHUser logs in to the other nodes for their network audits.
	NodeSSHUser string
}

var (
	durationPattern      = regexp.MustCompile(`^([0-9]+)([smh]?)$`)
	fenceDurationPattern = regexp.MustCompile(`^([1-9][0-9]*|0)[smh]?$`)
	positiveInteger      = regexp.MustCompile(`^[1-9][0-9]*$`)
	safeName             = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	digitsOnly           = regexp.MustCompile(`^[0-9]+$`)
)

// ParseArgs reads the options that follow the action.
func ParseArgs(action string, args []string) (Options, error) {
	opts := Options{
		Action:             action,
		DrainTimeout:       "10m",
		ReadyTimeout:       "10m",
		FenceTimeout:       "10m",
		SettleTimeout:      "90m",
		SSHTimeoutSeconds:  900,
		MinFreePodSlots:    20,
		NetworkAuditScript: os.Getenv("K8S_NODE_NETWORK_AUDIT_SCRIPT"),
		NodeSSHUser:        os.Getenv("K8S_NODE_SSH_USER"),
	}
	if opts.NodeSSHUser == "" {
		opts.NodeSSHUser = "root"
	}
	switch action {
	case "reboot", "off", "on":
	default:
		return opts, fmt.Errorf("unsupported action: %s", action)
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		// value returns the option's argument.
		value := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", arg)
			}
			i++
			return args[i], nil
		}
		var err error
		switch arg {
		case "--node":
			opts.Node, err = value()
		case "--ssh-target":
			opts.SSHTarget, err = value()
		case "--skip-drain":
			opts.SkipDrain = true
		case "--force-drain":
			opts.ForceDrain = true
		case "--bypass-pdb":
			opts.BypassPDB = true
		case "--leave-cordoned":
			opts.LeaveCordoned = true
		case "--resume-maintenance":
			opts.ResumeMaintenance = true
		case "--check-only":
			opts.CheckOnly = true
		case "--needed":
			opts.Needed = true
		case "--drain-timeout":
			opts.DrainTimeout, err = value()
		case "--ready-timeout":
			opts.ReadyTimeout, err = value()
		case "--fence-timeout":
			opts.FenceTimeout, err = value()
			// The Bash tool rejected leading zeros, which its arithmetic
			// would read as octal; keep the same accepted spellings.
			if err == nil && !fenceDurationPattern.MatchString(opts.FenceTimeout) {
				err = fmt.Errorf("unsupported fence duration: %s", opts.FenceTimeout)
			}
		case "--settle-timeout":
			opts.SettleTimeout, err = value()
		case "--ssh-timeout":
			var text string
			text, err = value()
			if err == nil {
				opts.SSHTimeoutSeconds, err = strconv.Atoi(text)
				if err != nil || opts.SSHTimeoutSeconds < 0 {
					err = fmt.Errorf("--ssh-timeout must be a whole number of seconds")
				}
			}
		case "--require-longhorn-backup-target":
			opts.RequireLonghornBackupTarget = true
		case "--min-free-pod-slots":
			var text string
			text, err = value()
			if err == nil {
				if !positiveInteger.MatchString(text) {
					err = fmt.Errorf("--min-free-pod-slots must be a positive integer")
				} else {
					opts.MinFreePodSlots, _ = strconv.Atoi(text)
				}
			}
		case "-h", "--help":
			return opts, ErrHelp
		default:
			// A host cannot start with a dash: ssh would read it as an
			// option.
			if strings.HasPrefix(arg, "-") {
				return opts, fmt.Errorf("unknown option: %s", arg)
			}
			if opts.Host != "" {
				return opts, fmt.Errorf("only one host can be supplied")
			}
			opts.Host = arg
		}
		if err != nil {
			return opts, err
		}
	}

	if opts.Host == "" {
		return opts, ErrUsage
	}
	if opts.Node == "" {
		opts.Node = opts.Host
	}
	if opts.SSHTarget == "" {
		opts.SSHTarget = "root@" + opts.Host
	}
	if opts.Action == "off" {
		opts.LeaveCordoned = true
	}

	// Every duration is also waited on locally; reject one that cannot be
	// before any cluster state changes. kubectl rejects a bare number of
	// seconds other than 0, so it is passed on as Ns.
	for _, text := range []*string{&opts.DrainTimeout, &opts.ReadyTimeout, &opts.FenceTimeout, &opts.SettleTimeout} {
		if _, err := parseDuration(*text); err != nil {
			return opts, err
		}
		if digitsOnly.MatchString(*text) {
			*text += "s"
		}
	}
	if !safeName.MatchString(opts.NodeSSHUser) {
		return opts, fmt.Errorf("unsafe Kubernetes node SSH user: %s", opts.NodeSSHUser)
	}
	if opts.ResumeMaintenance && opts.Action == "on" {
		return opts, fmt.Errorf("--resume-maintenance is for reboot/off cycles; use k8s-node-power on to finalize maintenance")
	}
	if opts.Needed && opts.Action != "reboot" {
		return opts, fmt.Errorf("--needed applies only to reboot")
	}
	// A resumed maintenance is a deliberate power cycle of a cordoned node.
	if opts.Needed && opts.ResumeMaintenance {
		return opts, fmt.Errorf("--needed and --resume-maintenance cannot be combined")
	}
	if opts.ResumeMaintenance && opts.SkipDrain {
		return opts, fmt.Errorf("--resume-maintenance and --skip-drain cannot be combined")
	}
	return opts, nil
}

// parseDuration reads a whole number of seconds, minutes (m) or hours (h).
func parseDuration(text string) (time.Duration, error) {
	match := durationPattern.FindStringSubmatch(text)
	if match == nil {
		return 0, fmt.Errorf("unsupported duration: %s", text)
	}
	n, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil || n > 1<<31 {
		return 0, fmt.Errorf("unsupported duration: %s", text)
	}
	unit := time.Second
	switch match[2] {
	case "m":
		unit = time.Minute
	case "h":
		unit = time.Hour
	}
	return time.Duration(n) * unit, nil
}

// duration reads a duration that ParseArgs already validated.
func duration(text string) time.Duration {
	d, _ := parseDuration(text)
	return d
}
