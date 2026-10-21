package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmptyManifestSnapshotRejectedBeforeCreatingPaths(t *testing.T) {
	binary := candidateBinary(t)
	for _, input := range []string{"", " \n\t"} {
		for _, mode := range []string{"", "--status", "--directories"} {
			f := newParityFixture(t)
			fixtureWrite(t, f.manifest, input, 0600)
			result := runParity(t, f, binary, []string{f.root, f.manifest, mode})
			if result.status != 2 || result.stdout != "" || result.stderr != "workspace-sync: invalid manifest\n" {
				t.Fatalf("empty snapshot, mode %q: %#v", mode, result)
			}
			if _, err := os.Lstat(f.root); !os.IsNotExist(err) {
				t.Fatalf("empty snapshot created root: %v", err)
			}
		}
	}
}

func TestEveryManifestDocumentValidatedBeforeCreatingPaths(t *testing.T) {
	binary := candidateBinary(t)
	valid := `{"directories":["valid"],"repositories":[],"links":[]}`
	for _, invalid := range []string{
		`{"directories":["../outside"],"repositories":[],"links":[]}`,
		`{"directories":[],"repositories":[{"path":"../outside","url":"local"}],"links":[]}`,
		`{"directories":[],"repositories":[],"links":[{"path":"../outside","target":"valid","profiles":[]}]}`,
		`null`, `{"directories":["earlier"],"repositories":[],"links":[]} {`,
	} {
		for _, mode := range []string{"", "--status", "--directories"} {
			f := newParityFixture(t)
			fixtureWrite(t, f.manifest, invalid+"\n"+valid, 0600)
			result := runParity(t, f, binary, []string{f.root, f.manifest, mode})
			if result.status != 2 || result.stdout != "" || !strings.Contains(result.stderr, "invalid manifest") {
				t.Fatalf("invalid document, mode %q: %#v", mode, result)
			}
			for _, path := range []string{f.root, filepath.Join(f.base, "outside")} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("created a path from an invalid stream: %s: %v", path, err)
				}
			}
		}
	}
}

func TestValidManifestStreamUsesAllDocuments(t *testing.T) {
	binary := candidateBinary(t)
	manifest := `{"directories":["first"],"repositories":[],"links":[]} ` + "\n" +
		`{"directories":["second"],"repositories":[],"links":[]}`
	for _, operand := range []string{"file", "-", "/dev/stdin"} {
		t.Run(operand, func(t *testing.T) {
			f := newParityFixture(t)
			path := operand
			var options []parityCase
			if operand == "file" {
				path = f.manifest
				fixtureWrite(t, path, manifest, 0600)
			} else {
				options = []parityCase{{stdin: manifest}}
			}
			result := runParity(t, f, binary, []string{f.root, path}, options...)
			if result.status != 0 {
				t.Fatalf("valid stream: %#v", result)
			}
			for _, name := range []string{"first", "second"} {
				if !directory(filepath.Join(f.root, name)) {
					t.Fatalf("missing directory from snapshot: %s", name)
				}
			}
		})
	}
}

func TestExtractionUsesValidatedSnapshot(t *testing.T) {
	binary := candidateBinary(t)
	f := newParityFixture(t)
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env, "SNAPSHOT_JQ="+jq, "SNAPSHOT_MANIFEST="+f.manifest)
	fixtureWrite(t, f.manifest, `{"directories":["original"],"repositories":[],"links":[]}`, 0600)
	fixtureShellScript(t, filepath.Join(f.bin, "jq"), `
"$SNAPSHOT_JQ" "$@" || exit $?
printf '%s' '{"directories":["replacement"],"repositories":[],"links":[]}' > "$SNAPSHOT_MANIFEST"
`)
	result := runParity(t, f, binary, []string{f.root, f.manifest})
	if result.status != 0 || !directory(filepath.Join(f.root, "original")) {
		t.Fatalf("original snapshot not consumed: %#v", result)
	}
	if _, err := os.Lstat(filepath.Join(f.root, "replacement")); !os.IsNotExist(err) {
		t.Fatalf("consumed replacement manifest: %v", err)
	}
}
