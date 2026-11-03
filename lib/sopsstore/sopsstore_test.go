package sopsstore

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// dummyKey is not a real key; it only has lines long enough to be looked for.
const dummyKey = "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
	"b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n" +
	"QyNTUxOQAAACBkdW1teWR1bW15ZHVtbXlkdW1teWR1bW15ZHVtbXlkdW1teQAAAJhkdW1t\n" +
	"-----END OPENSSH PRIVATE KEY-----\n"

var secretFile = File{Path: "k8s/app/deploy-key.sops.yaml", Key: "deploy-key", Secret: Secret{Name: "app-deploy-key", Namespace: "platform"}}

const encrypted = "ENC[AES256_GCM,data:Zm9v,iv:aXY=,tag:dGFn,type:str]"

// sopsOutput is what sops encrypt writes for secretFile under an
// encrypted_regex of ^(data|stringData)$.
const sopsOutput = "apiVersion: v1\nkind: Secret\nmetadata:\n    name: app-deploy-key\n    namespace: platform\n" +
	"type: Opaque\nstringData:\n    deploy-key: " + encrypted + "\n" +
	"sops:\n    age:\n        - recipient: age1dummy\n          enc: |\n            -----BEGIN AGE ENCRYPTED FILE-----\n" +
	"    lastmodified: \"2026-01-01T00:00:00Z\"\n    encrypted_regex: ^(data|stringData)$\n    version: 3.9.0\n"

func TestSecretDocument(t *testing.T) {
	want := "apiVersion: v1\nkind: Secret\nmetadata:\n    name: \"app-deploy-key\"\n    namespace: \"platform\"\n" +
		"type: Opaque\nstringData:\n    \"deploy-key\": |\n" +
		"        -----BEGIN OPENSSH PRIVATE KEY-----\n"
	if got := secretFile.Document(dummyKey); !strings.HasPrefix(got, want) || !strings.HasSuffix(got, "\n        -----END OPENSSH PRIVATE KEY-----\n") {
		t.Errorf("document =\n%s", got)
	}
	flat := File{Path: "a.yaml", Key: "site_key"}
	if got := flat.Document(dummyKey); !strings.HasPrefix(got, "\"site_key\": |\n    -----BEGIN") {
		t.Errorf("flat document =\n%s", got)
	}
}

func TestCheckSecret(t *testing.T) {
	if err := secretFile.checkEncrypted([]byte(sopsOutput), dummyKey); err != nil {
		t.Errorf("sops output refused: %v", err)
	}
	quoted := strings.Replace(sopsOutput, "name: app-deploy-key", `name: "app-deploy-key"`, 1)
	if err := secretFile.checkEncrypted([]byte(quoted), dummyKey); err != nil {
		t.Errorf("quoted name refused: %v", err)
	}
	plainData := strings.Replace(sopsOutput, "deploy-key: "+encrypted, "deploy-key: |\n        "+strings.Join(strings.Split(strings.TrimSpace(dummyKey), "\n"), "\n        "), 1)
	for name, out := range map[string]string{
		"stringData plaintext": plainData,
		"metadata encrypted":   strings.Replace(sopsOutput, "name: app-deploy-key", "name: "+encrypted, 1),
		"kind encrypted":       strings.Replace(sopsOutput, "kind: Secret", "kind: "+encrypted, 1),
		"wrong namespace":      strings.Replace(sopsOutput, "namespace: platform", "namespace: other", 1),
		"value elsewhere":      strings.Replace(sopsOutput, "deploy-key: ", "other-key: ", 1),
		"extra data key":       strings.Replace(sopsOutput, "stringData:\n", "stringData:\n    extra: "+encrypted+"\n", 1),
		"repeated key":         strings.Replace(sopsOutput, "stringData:\n", "stringData:\n    deploy-key: "+encrypted+"\n", 1),
		"extra top level":      sopsOutput + "data:\n    x: " + encrypted + "\n",
		"no sops metadata":     strings.SplitN(sopsOutput, "sops:", 2)[0],
		"flat output":          "deploy-key: " + encrypted + "\nsops:\n    version: 3.9.0\n",
	} {
		if err := secretFile.checkEncrypted([]byte(out), dummyKey); err == nil {
			t.Errorf("%s: accepted\n%s", name, out)
		}
	}
}

func TestSecretNames(t *testing.T) {
	for _, name := range []string{"app", "app-deploy-key", "a.b-c", "0"} {
		if ValidSecretName(name) != nil {
			t.Errorf("name %q refused", name)
		}
	}
	for _, name := range []string{"", "App", "-a", "a-", "a..b", "a_b", strings.Repeat("a", 254)} {
		if ValidSecretName(name) == nil {
			t.Errorf("name %q accepted", name)
		}
	}
	for _, ns := range []string{"platform", "a-1"} {
		if ValidNamespace(ns) != nil {
			t.Errorf("namespace %q refused", ns)
		}
	}
	for _, ns := range []string{"", "a.b", "A", "-a", strings.Repeat("a", 64)} {
		if ValidNamespace(ns) == nil {
			t.Errorf("namespace %q accepted", ns)
		}
	}
	for _, key := range []string{"deploy-key", "id_ed25519", ".ssh-key", "a.b", "a..b"} {
		if ValidSecretKey(key) != nil {
			t.Errorf("key %q refused", key)
		}
	}
	for _, key := range []string{"", ".", "..", "..a", "a/b", "a b", "a:b", strings.Repeat("a", 254)} {
		if ValidSecretKey(key) == nil {
			t.Errorf("key %q accepted", key)
		}
	}
}

// With sops and age-keygen installed, the checks hold against real sops,
// with a throwaway age recipient.
func TestRealSops(t *testing.T) {
	for _, program := range []string{"sops", "age-keygen"} {
		if _, err := exec.LookPath(program); err != nil {
			t.Skipf("%s is not installed", program)
		}
	}
	dir := t.TempDir()
	identity := filepath.Join(t.TempDir(), "age.txt")
	if err := exec.Command("age-keygen", "-o", identity).Run(); err != nil {
		t.Fatal(err)
	}
	recipient, err := exec.Command("age-keygen", "-y", identity).Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "k8s", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })

	for _, tt := range []struct {
		name, rule string
		ok         bool
	}{
		{"data and stringData", `encrypted_regex: "^(data|stringData)$"`, true},
		{"stringData left plaintext", `encrypted_regex: "^(other)$"`, false},
		{"everything encrypted", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			config := "creation_rules:\n  - path_regex: ^k8s/.*\\.sops\\.yaml$\n    age: " + strings.TrimSpace(string(recipient)) + "\n"
			if tt.rule != "" {
				config += "    " + tt.rule + "\n"
			}
			if err := os.WriteFile(".sops.yaml", []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			os.Remove(secretFile.Path)
			ctx := context.Background()
			err := secretFile.Check(ctx)
			if tt.ok != (err == nil) {
				t.Fatalf("Check = %v", err)
			}
			if !tt.ok {
				t.Logf("refused: %v", err)
				return
			}
			if err := secretFile.Save(ctx, dummyKey); err != nil {
				t.Fatal(err)
			}
			written, err := os.ReadFile(secretFile.Path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(written), "\n    name: app-deploy-key\n") || strings.Contains(string(written), "QyNTUxOQAAACBk") {
				t.Errorf("written:\n%s", written)
			}
		})
	}
}
