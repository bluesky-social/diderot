package sessionfixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const syntheticManifestSHA256 = "4ed5db4ae5b3b640f38a0e272c499d4532bcab25d8a9b8cd0baec9442b88fd80"

func TestLoadSyntheticBundle(t *testing.T) {
	root := filepath.Join("testdata", "synthetic-v1")
	bundle, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Manifest.Sessions) != 4 || len(bundle.Documents) != 4 {
		t.Fatalf("sessions/documents = %d/%d, want 4/4", len(bundle.Manifest.Sessions), len(bundle.Documents))
	}
	owners := map[string]bool{}
	harnesses := map[string]bool{}
	var incomplete, ownerOnly int
	for _, session := range bundle.Manifest.Sessions {
		owners[session.Owner] = true
		harnesses[session.Harness] = true
		if !session.Complete {
			incomplete++
		}
		if session.Privacy == PrivacyOwnerOnly {
			ownerOnly++
		}
	}
	if len(owners) != 2 || !harnesses["codex"] || !harnesses["claude-code"] || !harnesses["pi"] || incomplete != 1 || ownerOnly != 2 {
		t.Fatalf("fixture coverage owners=%v harnesses=%v incomplete=%d owner_only=%d", owners, harnesses, incomplete, ownerOnly)
	}
	manifest, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(manifest)
	if got := hex.EncodeToString(sum[:]); got != syntheticManifestSHA256 {
		t.Fatalf("synthetic manifest sha256 = %s, want %s; run go generate and review fixture changes", got, syntheticManifestSHA256)
	}
}

func TestValidateSessionAllowsEmptyOriginApp(t *testing.T) {
	bundle, err := Load(filepath.Join("testdata", "synthetic-v1"))
	if err != nil {
		t.Fatal(err)
	}
	session := bundle.Manifest.Sessions[0]
	session.OriginApp = ""
	if err := validateSession(session, map[string]struct{}{session.Owner: {}}); err != nil {
		t.Fatalf("validate empty origin_app: %v", err)
	}
}

func TestLoadRejectsUnsafeOrInconsistentBundle(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string, map[string]any)
	}{
		{
			name: "unknown manifest field",
			mutate: func(_ *testing.T, _ string, manifest map[string]any) {
				manifest["surprise"] = true
			},
		},
		{
			name: "path traversal",
			mutate: func(_ *testing.T, _ string, manifest map[string]any) {
				firstSession(manifest)["document"].(map[string]any)["path"] = "documents/../manifest.json.gz"
			},
		},
		{
			name: "duplicate UUID",
			mutate: func(_ *testing.T, _ string, manifest map[string]any) {
				sessions := manifest["sessions"].([]any)
				copy := mapsClone(sessions[0].(map[string]any))
				copy["document"] = mapsClone(copy["document"].(map[string]any))
				copy["document"].(map[string]any)["path"] = "documents/duplicate.ndjson.gz"
				manifest["sessions"] = append(sessions, copy)
			},
		},
		{
			name: "stored hash mismatch",
			mutate: func(_ *testing.T, _ string, manifest map[string]any) {
				firstSession(manifest)["document"].(map[string]any)["stored_sha256"] = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
			},
		},
		{
			name: "document envelope mismatch",
			mutate: func(_ *testing.T, _ string, manifest map[string]any) {
				firstSession(manifest)["client"] = "different-client"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := copySynthetic(t)
			manifest := readManifestMap(t, root)
			test.mutate(t, root, manifest)
			writeManifestMap(t, root, manifest)
			if _, err := Load(root); err == nil {
				t.Fatal("Load succeeded, want validation error")
			}
		})
	}
}

func TestLoadRejectsUnreferencedFile(t *testing.T) {
	root := copySynthetic(t)
	if err := os.WriteFile(filepath.Join(root, "documents", "orphan.ndjson.gz"), []byte("not a fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil {
		t.Fatal("Load succeeded with an unreferenced file")
	}
}

func TestValidateDocumentMatchesIngestWhitespaceSemantics(t *testing.T) {
	bundle, err := Load(filepath.Join("testdata", "synthetic-v1"))
	if err != nil {
		t.Fatal(err)
	}
	session := bundle.Manifest.Sessions[0]
	lines := bytes.Split(bytes.TrimSuffix(bundle.Documents[session.SessionID], []byte{'\n'}), []byte{'\n'})
	var modified bytes.Buffer
	modified.WriteString("\n  ")
	for index, line := range lines {
		modified.Write(line)
		modified.WriteString("  \n")
		if index == 0 {
			modified.WriteByte('\n')
		}
	}
	if err := validateDocument(session, modified.Bytes()); err != nil {
		t.Fatalf("validate whitespace-compatible document: %v", err)
	}
}

func TestLoadRejectsSymlinkedObject(t *testing.T) {
	root := copySynthetic(t)
	manifest := readManifestMap(t, root)
	path := firstSession(manifest)["document"].(map[string]any)["path"].(string)
	objectPath := filepath.Join(root, filepath.FromSlash(path))
	contents, err := os.ReadFile(objectPath)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target.gz")
	if err := os.WriteFile(target, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(objectPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, objectPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil {
		t.Fatal("Load succeeded for symlinked object")
	}
}

func copySynthetic(t *testing.T) string {
	t.Helper()
	source := filepath.Join("testdata", "synthetic-v1")
	destination := t.TempDir()
	if err := filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil || relative == "." {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	return destination
}

func readManifestMap(t *testing.T, root string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func writeManifestMap(t *testing.T, root string, manifest map[string]any) {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func firstSession(manifest map[string]any) map[string]any {
	return manifest["sessions"].([]any)[0].(map[string]any)
}

func mapsClone(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
