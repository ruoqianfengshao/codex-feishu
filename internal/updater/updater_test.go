package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckReportsAvailableRelease(t *testing.T) {
	archive := testArchive(t, "new binary")
	sum := sha256.Sum256(archive)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/releases/latest":
			_, _ = fmt.Fprintf(w, `{"tag_name":"v0.6.14","html_url":"%s/release","assets":[{"name":"ctr-go_v0.6.14_darwin_arm64.tar.gz","browser_download_url":"%s/archive"},{"name":"SHA256SUMS","browser_download_url":"%s/sums"}]}`, serverURL(r), serverURL(r), serverURL(r))
		default:
			http.NotFound(w, r)
		}
		_ = sum
	}))
	defer server.Close()

	result, err := Check(context.Background(), Options{
		Repo:           "owner/repo",
		APIBaseURL:     server.URL,
		CurrentVersion: "0.6.13",
		GOOS:           "darwin",
		GOARCH:         "arm64",
	})
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if result.AlreadyLatest {
		t.Fatal("AlreadyLatest = true, want false")
	}
	if result.LatestVersion != "0.6.14" || result.AssetName != "ctr-go_v0.6.14_darwin_arm64.tar.gz" {
		t.Fatalf("result = %#v", result)
	}
}

func TestUpdateDownloadsVerifiesAndReplacesBinary(t *testing.T) {
	archive := testArchive(t, "new binary")
	sum := sha256.Sum256(archive)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/releases/latest":
			_, _ = fmt.Fprintf(w, `{"tag_name":"v0.6.14","html_url":"%s/release","assets":[{"name":"ctr-go_v0.6.14_darwin_arm64.tar.gz","browser_download_url":"%s/archive"},{"name":"SHA256SUMS","browser_download_url":"%s/sums"}]}`, server.URL, server.URL, server.URL)
		case "/archive":
			_, _ = w.Write(archive)
		case "/sums":
			_, _ = fmt.Fprintf(w, "%x  ctr-go_v0.6.14_darwin_arm64.tar.gz\n", sum)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	target := filepath.Join(t.TempDir(), "ctr-go")
	if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
		t.Fatalf("write target failed: %v", err)
	}
	result, err := Update(context.Background(), Options{
		Repo:           "owner/repo",
		APIBaseURL:     server.URL,
		CurrentVersion: "0.6.13",
		TargetPath:     target,
		GOOS:           "darwin",
		GOARCH:         "arm64",
	})
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if !result.Updated {
		t.Fatal("Updated = false, want true")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target failed: %v", err)
	}
	if string(data) != "new binary" {
		t.Fatalf("target data = %q", data)
	}
}

func TestUpdateRejectsChecksumMismatch(t *testing.T) {
	archive := testArchive(t, "new binary")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/releases/latest":
			_, _ = fmt.Fprintf(w, `{"tag_name":"v0.6.14","assets":[{"name":"ctr-go_v0.6.14_darwin_arm64.tar.gz","browser_download_url":"%s/archive"},{"name":"SHA256SUMS","browser_download_url":"%s/sums"}]}`, server.URL, server.URL)
		case "/archive":
			_, _ = w.Write(archive)
		case "/sums":
			_, _ = fmt.Fprintln(w, strings.Repeat("0", 64)+"  ctr-go_v0.6.14_darwin_arm64.tar.gz")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	target := filepath.Join(t.TempDir(), "ctr-go")
	if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
		t.Fatalf("write target failed: %v", err)
	}
	_, err := Update(context.Background(), Options{
		Repo:           "owner/repo",
		APIBaseURL:     server.URL,
		CurrentVersion: "0.6.13",
		TargetPath:     target,
		GOOS:           "darwin",
		GOARCH:         "arm64",
	})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want checksum mismatch", err)
	}
}

func testArchive(t *testing.T, binary string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	data := []byte(binary)
	if err := tw.WriteHeader(&tar.Header{Name: "ctr-go", Mode: 0o755, Size: int64(len(data))}); err != nil {
		t.Fatalf("WriteHeader failed: %v", err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close failed: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close failed: %v", err)
	}
	return buf.Bytes()
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}
