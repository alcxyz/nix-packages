package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Catppuccin colors for phase labels and headings.
const (
	colorText   = "205;214;244"
	colorGreen  = "166;227;161"
	colorBlue   = "116;199;236"
	colorMauve  = "203;166;247"
	colorRed    = "243;139;168"
	colorArrows = "180;190;254"
)

type ui struct {
	color      bool
	hostColors map[string]string
}

func newUI(hostColors map[string]string) *ui {
	color := false
	if info, err := os.Stdout.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		color = os.Getenv("NO_COLOR") == ""
	}
	return &ui{color: color, hostColors: hostColors}
}

// paint colors text on a terminal; a nil ui paints nothing.
func (u *ui) paint(rgb, text string) string {
	if u == nil || !u.color {
		return text
	}
	return "\033[38;2;" + rgb + "m" + text + "\033[0m"
}

func (u *ui) phase(phase string) string {
	switch phase {
	case "check":
		return u.paint(colorGreen, "[check]")
	case "system":
		return u.paint(colorBlue, "[system]")
	case "hm":
		return u.paint(colorMauve, "[hm]")
	default:
		return u.paint(colorText, "["+phase+"]")
	}
}

// host labels a line about a host, in the host's color: [host].
func (u *ui) host(host string) string {
	return u.hostName("["+host+"]", host)
}

// hostName paints text, usually the host's name, in the host's color.
func (u *ui) hostName(text, host string) string {
	if u == nil {
		return text
	}
	rgb, ok := u.hostColors[host]
	if !ok {
		rgb = colorText
	}
	return u.paint(rgb, text)
}

// hostColumn is a host's name in a table column of width characters; it is
// padded before it is painted, so the escape codes do not count.
func (u *ui) hostColumn(host string, width int) string {
	return u.hostName(fmt.Sprintf("%-*s", width, host), host)
}

func (u *ui) section(title string) {
	fmt.Printf("\n%s %s\n", u.paint(colorArrows, "==>"), u.paint(colorText, title))
}

// warnPartial reports hosts whose system switched while Home Manager did not.
// Their userspace runs from an older package set, which can fail to load the
// new system's graphics drivers at the next login.
func (u *ui) warnPartial(hosts []string) {
	for _, host := range hosts {
		fmt.Fprintf(os.Stderr, "%s %s system updated, Home Manager NOT updated. Fix the error above, then run: deploy --hm %s\n",
			u.paint(colorRed, "deploy: WARNING"), u.host(host), host)
	}
	fmt.Fprintln(os.Stderr, "deploy: until then, Home Manager programs lag the system and may fail to load its graphics drivers.")
}

func formatGiB(kib int64) string {
	return fmt.Sprintf("%d.%01d GiB", kib/1048576, ((kib%1048576)*10)/1048576)
}

// formatWait writes a duration in whole hours, minutes and seconds,
// leaving out zero parts: 1h, 1h30m, 45s.
func formatWait(d time.Duration) string {
	d = d.Round(time.Second)
	if d <= 0 {
		return "0s"
	}
	var text string
	if h := d / time.Hour; h > 0 {
		text += fmt.Sprintf("%dh", h)
	}
	if m := d % time.Hour / time.Minute; m > 0 {
		text += fmt.Sprintf("%dm", m)
	}
	if sec := d % time.Minute / time.Second; sec > 0 {
		text += fmt.Sprintf("%ds", sec)
	}
	return text
}

// unitCount names a single unit, or counts several.
func unitCount(units []string) string {
	if len(units) == 1 {
		return units[0]
	}
	return fmt.Sprintf("%d units", len(units))
}

func joinHosts(hosts []string) string {
	return strings.Join(hosts, " ")
}
