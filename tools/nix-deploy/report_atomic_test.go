package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestReportConcurrentPublication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	d := &deployer{}
	prior := newReport()
	prior.record("system", "previous", "ok")
	if err := d.writeJSON(path, []string{"previous"}, prior); err != nil {
		t.Fatal(err)
	}

	// Different sizes expose truncation and mixed writes. Exercise both the
	// ordinary report writer and the signal/early-failure report writer.
	host := strings.Repeat("fixture-host", 2048)
	current := newReport()
	current.record("system", host, "failed")
	start, done := make(chan struct{}), make(chan struct{})
	errors := make(chan error, 1)
	recordError := func(err error) {
		select {
		case errors <- err:
		default:
		}
	}
	check := func() error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var report runReport
		if err := json.Unmarshal(data, &report); err != nil {
			return fmt.Errorf("incomplete report (%d bytes): %w", len(data), err)
		}
		if report.Version != version || report.Hosts == nil {
			return fmt.Errorf("report lost its version or hosts")
		}
		switch len(report.Hosts) {
		case 0:
			if report.Failed {
				return nil
			}
		case 1:
			h := report.Hosts[0]
			if h.Host == "previous" && h.System.Status == "ok" && !report.Failed {
				return nil
			}
			if h.Host == host && h.System.Status == "failed" && report.Failed {
				return nil
			}
		}
		return fmt.Errorf("report is not a complete prior, activation-failure or interrupted snapshot")
	}
	var readers, writers sync.WaitGroup
	for i := 0; i < 2; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			for {
				if err := check(); err != nil {
					recordError(err)
					return
				}
				select {
				case <-done:
					return
				default:
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		writers.Add(1)
		go func(failed bool) {
			defer writers.Done()
			<-start
			for j := 0; j < 32; j++ {
				if failed {
					writeFailedJSON(path)
				} else if err := d.writeJSON(path, []string{host}, current); err != nil {
					recordError(err)
					return
				}
			}
		}(i%2 == 0)
	}
	close(start)
	writers.Wait()
	close(done)
	readers.Wait()
	select {
	case err := <-errors:
		t.Fatal(err)
	default:
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("report directory contains temporary files: %v, %v", entries, err)
	}
}

// reportACLFixture uses real ACLs, including the Linux mask/group distinction.
func reportACLFixture(t *testing.T, path string, inherited bool) []byte {
	t.Helper()
	var set, get *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		for _, tool := range []string{"setfacl", "getfacl"} {
			if _, err := exec.LookPath(tool); err != nil {
				t.Skipf("ACL fixture requires %s: %v", tool, err)
			}
		}
		collector := 65534
		if os.Getuid() == collector {
			collector--
		}
		acl := fmt.Sprintf("u::rw-,u:%d:r--,g::---,m::r--,o::---", collector)
		if inherited {
			set = exec.Command("setfacl", "--default", "--set", acl, "--", path)
		} else {
			set = exec.Command("setfacl", "--set", acl, "--", path)
		}
		get = exec.Command("getfacl", "--omit-header", "--numeric", "--", path)
	case "darwin":
		acl := "group:everyone allow read"
		if inherited {
			acl += ",file_inherit,directory_inherit"
		}
		set = exec.Command("/bin/chmod", "+a", acl, path)
		get = exec.Command("/bin/ls", "-lde", path)
	default:
		t.Skip("ACL fixtures cover Linux and Darwin")
	}
	set.Env = append(os.Environ(), "LC_ALL=C")
	if output, err := set.CombinedOutput(); err != nil {
		if strings.Contains(string(output), "Operation not supported") {
			t.Skipf("filesystem does not support ACL fixtures: %s", output)
		}
		t.Fatalf("set fixture ACL: %v: %s", err, output)
	}
	output, err := get.CombinedOutput()
	if err != nil {
		t.Fatalf("read fixture ACL: %v: %s", err, output)
	}
	return output
}

func TestReportPublicationRejectsAccessACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, []byte("prior report"), 0o600); err != nil {
		t.Fatal(err)
	}
	acl := reportACLFixture(t, path, false)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" {
		if before.Mode().Perm() != 0o640 || !strings.Contains(string(acl), "group::---") || !strings.Contains(string(acl), "mask::r--") {
			t.Fatalf("fixture must expose the mask as group mode bits: %o, %s", before.Mode().Perm(), acl)
		}
	}
	if err := (&deployer{}).writeJSON(path, []string{"fixture"}, newReport()); err == nil || !strings.Contains(err.Error(), "ACL") {
		t.Fatalf("ACL-bearing destination publication = %v", err)
	}
	// The early-failure/signal writer must leave the ACL-bearing report too.
	writeFailedJSON(path)
	if data, err := os.ReadFile(path); err != nil || string(data) != "prior report" {
		t.Fatalf("ACL refusal changed the prior report: %q, %v", data, err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
		t.Fatalf("ACL refusal replaced the prior inode or mode: %v, %v", after, err)
	}
	var get *exec.Cmd
	if runtime.GOOS == "linux" {
		get = exec.Command("getfacl", "--omit-header", "--numeric", "--", path)
	} else {
		get = exec.Command("/bin/ls", "-lde", path)
	}
	if output, err := get.CombinedOutput(); err != nil || string(output) != string(acl) {
		t.Fatalf("ACL refusal changed permissions: %v, %s", err, output)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("ACL refusal left temporary files: %v, %v", entries, err)
	}
}

func TestReportPublicationRejectsInheritedACL(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%v", existing), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "report.json")
			// Create the prior report before adding inheritance to the directory.
			if existing {
				if err := os.WriteFile(path, []byte("prior report"), 0o640); err != nil {
					t.Fatal(err)
				}
			}
			reportACLFixture(t, dir, true)
			if err := writeReportFile(path, []byte("replacement")); err == nil || !strings.Contains(err.Error(), "ACL") {
				t.Fatalf("inherited ACL publication = %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if existing {
				if data, err := os.ReadFile(path); err != nil || string(data) != "prior report" || len(entries) != 1 {
					t.Fatalf("inherited ACL refusal changed the prior report or left temporary files: %q, %v, %v", data, err, entries)
				}
			} else if len(entries) != 0 {
				t.Fatalf("inherited ACL refusal published a report or left temporary files: %v", entries)
			}
		})
	}
}

func TestReportPublicationACLInspectionFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, []byte("prior report"), 0o640); err != nil {
		t.Fatal(err)
	}
	// A missing ACL inspector must fail closed, rather than assume plain mode.
	priorInspector := reportACLInspector
	reportACLInspector = filepath.Join(t.TempDir(), "missing-ls")
	t.Cleanup(func() { reportACLInspector = priorInspector })
	if err := writeReportFile(path, []byte("replacement")); err == nil || !strings.Contains(err.Error(), "inspect report ACL") {
		t.Fatalf("ACL inspection failure = %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "prior report" {
		t.Fatalf("ACL inspection failure changed the prior report: %q, %v", data, err)
	}
}

func TestReportPublicationPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	writeFailedJSON(path)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("new report permissions: %v, %v", info, err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := (&deployer{}).writeJSON(path, []string{"fixture"}, newReport()); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("replacement report permissions: %v, %v", info, err)
	}
}

func TestReportPublicationOwnership(t *testing.T) {
	for _, nonDefault := range []bool{false, true} {
		t.Run(fmt.Sprintf("non-default-group=%v", nonDefault), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "report.json")
			if err := os.WriteFile(path, []byte("prior report"), 0o640); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			owner := *info.Sys().(*syscall.Stat_t)
			if nonDefault {
				groups, err := os.Getgroups()
				if err != nil {
					t.Fatal(err)
				}
				if os.Geteuid() == 0 {
					groups = append(groups, int(owner.Gid)+1)
				}
				changed := false
				for _, group := range groups {
					if group != int(owner.Gid) && os.Chown(path, -1, group) == nil {
						owner.Gid = uint32(group)
						changed = true
						break
					}
				}
				if !changed {
					t.Skip("no permitted non-default group for the fixture")
				}
			}
			if err := (&deployer{}).writeJSON(path, []string{"fixture"}, newReport()); err != nil {
				t.Fatal(err)
			}
			writeFailedJSON(path)
			info, err = os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			got := info.Sys().(*syscall.Stat_t)
			if got.Uid != owner.Uid || got.Gid != owner.Gid || info.Mode().Perm() != 0o640 {
				t.Fatalf("ownership/mode = %d:%d %o, want %d:%d 640", got.Uid, got.Gid, info.Mode().Perm(), owner.Uid, owner.Gid)
			}
			t.Logf("preserved uid=%d gid=%d mode=0640", got.Uid, got.Gid)
		})
	}
}

func TestReportPublicationOwnershipFailure(t *testing.T) {
	const childEnv = "NIXFLEET_REPORT_OWNERSHIP_TEST"
	if path := os.Getenv(childEnv); path != "" {
		// Drop privileges only in this isolated test process.
		if err := syscall.Setgroups(nil); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Setgid(65534); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Setuid(65534); err != nil {
			t.Fatal(err)
		}
		err := writeReportFile(path, []byte("replacement"))
		if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "preserve ownership") {
			t.Fatalf("ownership failure = %v", err)
		}
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("requires root to prepare a report owned by another user")
	}
	// A direct child of /tmp is traversable after the child drops privileges,
	// unlike testing's private parent directories (also on Darwin).
	dir, err := os.MkdirTemp("/tmp", "nixfleet-report-owner-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "report.json")
	if err := os.WriteFile(path, []byte("prior report"), 0o640); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	owner := *info.Sys().(*syscall.Stat_t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestReportPublicationOwnershipFailure$")
	cmd.Env = append(os.Environ(), childEnv+"="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unprivileged report writer: %v\n%s", err, output)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "prior report" {
		t.Fatalf("ownership failure changed the prior report: %q, %v", data, err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	got := after.Sys().(*syscall.Stat_t)
	if got.Uid != owner.Uid || got.Gid != owner.Gid || after.Mode() != info.Mode() {
		t.Fatal("ownership failure changed the prior report's ownership or permissions")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ownership failure left temporary files: %v, %v", entries, err)
	}
}

func TestReportPublicationFailureKeepsDestination(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	if err := os.WriteFile(path, []byte("prior report"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := (&deployer{}).writeJSON(link, nil, newReport()); err == nil {
		t.Fatal("report writer accepted a symbolic link")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "prior report" {
		t.Fatalf("failed publication changed the prior report: %q, %v", data, err)
	}
	if _, err := os.Readlink(link); err != nil {
		t.Fatalf("failed publication replaced the link: %v", err)
	}
	if err := (&deployer{}).writeJSON(dir, nil, newReport()); err == nil {
		t.Fatal("report writer accepted a directory")
	}
	if err := (&deployer{}).writeJSON(filepath.Join(dir, "missing", "report.json"), nil, newReport()); err == nil {
		t.Fatal("report writer accepted a missing parent directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("failed publication left temporary files: %v, %v", entries, err)
	}
}

func TestInterruptRemovesReportTemps(t *testing.T) {
	temp, err := os.CreateTemp(t.TempDir(), ".nixfleet-report-*")
	if err != nil {
		t.Fatal(err)
	}
	temp.Close()
	reportTemps.Store(temp.Name(), struct{}{})
	defer reportTemps.Delete(temp.Name())
	removeReportTemps()
	if _, err := os.Stat(temp.Name()); !os.IsNotExist(err) {
		t.Fatalf("temporary report %s survived the interrupt cleanup: %v", temp.Name(), err)
	}
}
