package files

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// isolateAWSConfig keeps the developer's ~/.aws files and environment out of
// the SDK's default chain, and supplies static credentials the way a MinIO or
// R2 deployment would.
func isolateAWSConfig(t *testing.T) {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv("AWS_CONFIG_FILE", missing)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", missing)
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_ENDPOINT_URL", "")
	t.Setenv("AWS_ENDPOINT_URL_S3", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "minio")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "minio-secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
}

func TestS3MirrorPathStyleEndpoint(t *testing.T) {
	isolateAWSConfig(t)
	var (
		mu       sync.Mutex
		stored   = map[string][]byte{}
		types    = map[string]string{}
		requests []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, r.Method+" "+r.Host+" "+r.URL.Path)
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=minio/") {
			http.Error(w, "unsigned", http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			stored[r.URL.Path] = body
			types[r.URL.Path] = r.Header.Get("Content-Type")
		case http.MethodGet:
			body, ok := stored[r.URL.Path]
			if !ok {
				http.Error(w, "missing", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", types[r.URL.Path])
			_, _ = w.Write(body)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	// A host name, not the server's IP: the SDK always path-addresses an IP
	// endpoint, which would hide a dropped PathStyle option.
	endpoint := strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	mirror, err := NewS3Mirror(context.Background(), S3MirrorConfig{
		Bucket:    "diderot-files",
		Region:    "us-east-1",
		Endpoint:  endpoint,
		PathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Mirror: %v", err)
	}
	if err := mirror.Put(context.Background(), "slack/ab/abcdef.png", "image/png", []byte("png-bytes")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	data, contentType, err := mirror.Get(context.Background(), "slack/ab/abcdef.png")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(data) != "png-bytes" || contentType != "image/png" {
		t.Fatalf("Get = %q, %q", data, contentType)
	}

	mu.Lock()
	defer mu.Unlock()
	host := strings.TrimPrefix(endpoint, "http://")
	want := []string{
		"PUT " + host + " /diderot-files/slack/ab/abcdef.png",
		"GET " + host + " /diderot-files/slack/ab/abcdef.png",
	}
	if strings.Join(requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(requests, "\n"), strings.Join(want, "\n"))
	}
}

func TestS3MirrorRegionResolution(t *testing.T) {
	isolateAWSConfig(t)
	if _, err := NewS3Mirror(context.Background(), S3MirrorConfig{Bucket: "b"}); err == nil ||
		!strings.Contains(err.Error(), "DIDEROT_IMAGES_REGION") {
		t.Fatalf("NewS3Mirror without a region: err = %v, want a missing-region error", err)
	}

	t.Setenv("AWS_REGION", "eu-west-1")
	mirror, err := NewS3Mirror(context.Background(), S3MirrorConfig{Bucket: "b"})
	if err != nil {
		t.Fatalf("NewS3Mirror with AWS_REGION: %v", err)
	}
	if got := mirror.client.Options().Region; got != "eu-west-1" {
		t.Fatalf("region = %q, want the SDK-resolved eu-west-1", got)
	}

	mirror, err = NewS3Mirror(context.Background(), S3MirrorConfig{Bucket: "b", Region: "auto"})
	if err != nil {
		t.Fatalf("NewS3Mirror with explicit region: %v", err)
	}
	if got := mirror.client.Options().Region; got != "auto" {
		t.Fatalf("region = %q, want the configured auto", got)
	}
}
