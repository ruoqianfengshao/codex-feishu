package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRepo   = "ruoqianfengshao/codex-feishu"
	defaultAPIURL = "https://api.github.com"
)

type Options struct {
	Repo           string
	CurrentVersion string
	TargetVersion  string
	TargetPath     string
	APIBaseURL     string
	HTTPClient     *http.Client
	GOOS           string
	GOARCH         string
	CheckOnly      bool
}

type Result struct {
	CurrentVersion string
	LatestVersion  string
	ReleaseURL     string
	AssetName      string
	TargetPath     string
	Updated        bool
	AlreadyLatest  bool
}

type releaseInfo struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func Check(ctx context.Context, opts Options) (Result, error) {
	opts = normalizeOptions(opts)
	rel, err := fetchRelease(ctx, opts)
	if err != nil {
		return Result{}, err
	}
	assetName, _, err := releaseAsset(rel, opts)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		CurrentVersion: cleanVersion(opts.CurrentVersion),
		LatestVersion:  cleanVersion(rel.TagName),
		ReleaseURL:     rel.HTMLURL,
		AssetName:      assetName,
		TargetPath:     opts.TargetPath,
	}
	if opts.TargetVersion == "" && compareVersions(result.LatestVersion, result.CurrentVersion) <= 0 {
		result.AlreadyLatest = true
	}
	return result, nil
}

func Update(ctx context.Context, opts Options) (Result, error) {
	opts = normalizeOptions(opts)
	result, err := Check(ctx, opts)
	if err != nil {
		return Result{}, err
	}
	if result.AlreadyLatest || opts.CheckOnly {
		return result, nil
	}
	rel, err := fetchRelease(ctx, opts)
	if err != nil {
		return Result{}, err
	}
	assetName, assetURL, err := releaseAsset(rel, opts)
	if err != nil {
		return Result{}, err
	}
	checksumURL, err := releaseAssetURL(rel, "SHA256SUMS")
	if err != nil {
		return Result{}, err
	}
	targetPath := opts.TargetPath
	if strings.TrimSpace(targetPath) == "" {
		targetPath, err = os.Executable()
		if err != nil {
			return Result{}, err
		}
	}
	targetPath = filepath.Clean(targetPath)
	if strings.Contains(targetPath, string(filepath.Separator)+"go-build") {
		return Result{}, fmt.Errorf("refusing to update temporary Go build binary: %s", targetPath)
	}
	tmpDir, err := os.MkdirTemp(filepath.Dir(targetPath), ".ctr-go-update-*")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(tmpDir)

	archivePath := filepath.Join(tmpDir, assetName)
	if err := downloadFile(ctx, opts.HTTPClient, assetURL, archivePath); err != nil {
		return Result{}, err
	}
	sumsPath := filepath.Join(tmpDir, "SHA256SUMS")
	if err := downloadFile(ctx, opts.HTTPClient, checksumURL, sumsPath); err != nil {
		return Result{}, err
	}
	if err := verifyChecksum(archivePath, sumsPath, assetName); err != nil {
		return Result{}, err
	}
	binaryPath, err := extractBinary(archivePath, tmpDir, opts.GOOS)
	if err != nil {
		return Result{}, err
	}
	if err := replaceBinary(binaryPath, targetPath); err != nil {
		return Result{}, err
	}
	result.AssetName = assetName
	result.TargetPath = targetPath
	result.Updated = true
	return result, nil
}

func normalizeOptions(opts Options) Options {
	if strings.TrimSpace(opts.Repo) == "" {
		opts.Repo = defaultRepo
	}
	if strings.TrimSpace(opts.APIBaseURL) == "" {
		opts.APIBaseURL = defaultAPIURL
	}
	opts.APIBaseURL = strings.TrimRight(opts.APIBaseURL, "/")
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 5 * time.Minute}
	}
	if strings.TrimSpace(opts.GOOS) == "" {
		opts.GOOS = runtime.GOOS
	}
	if strings.TrimSpace(opts.GOARCH) == "" {
		opts.GOARCH = runtime.GOARCH
	}
	opts.CurrentVersion = cleanVersion(opts.CurrentVersion)
	opts.TargetVersion = cleanVersion(opts.TargetVersion)
	return opts
}

func fetchRelease(ctx context.Context, opts Options) (releaseInfo, error) {
	path := "latest"
	if opts.TargetVersion != "" {
		path = "tags/v" + opts.TargetVersion
	}
	url := fmt.Sprintf("%s/repos/%s/releases/%s", opts.APIBaseURL, opts.Repo, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return releaseInfo{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "codex-feishu-updater")
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return releaseInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return releaseInfo{}, fmt.Errorf("github release request failed: %s %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var rel releaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return releaseInfo{}, err
	}
	if cleanVersion(rel.TagName) == "" {
		return releaseInfo{}, errors.New("github release has no tag_name")
	}
	return rel, nil
}

func releaseAsset(rel releaseInfo, opts Options) (string, string, error) {
	name, err := assetName(cleanVersion(rel.TagName), opts.GOOS, opts.GOARCH)
	if err != nil {
		return "", "", err
	}
	url, err := releaseAssetURL(rel, name)
	return name, url, err
}

func releaseAssetURL(rel releaseInfo, name string) (string, error) {
	for _, asset := range rel.Assets {
		if asset.Name == name && strings.TrimSpace(asset.BrowserDownloadURL) != "" {
			return asset.BrowserDownloadURL, nil
		}
	}
	return "", fmt.Errorf("release asset %q not found", name)
}

func assetName(version, goos, goarch string) (string, error) {
	if version == "" {
		return "", errors.New("release version is empty")
	}
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	switch goarch {
	case "amd64", "arm64":
	default:
		return "", fmt.Errorf("unsupported architecture: %s/%s", goos, goarch)
	}
	return fmt.Sprintf("ctr-go_v%s_%s_%s%s", version, goos, goarch, ext), nil
}

func downloadFile(ctx context.Context, client *http.Client, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "codex-feishu-updater")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download failed for %s: %s", url, resp.Status)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(file, resp.Body)
	return err
}

func verifyChecksum(archivePath, sumsPath, assetName string) error {
	expected, err := checksumForAsset(sumsPath, assetName)
	if err != nil {
		return err
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("checksum mismatch for %s", assetName)
	}
	return nil
}

func checksumForAsset(sumsPath, assetName string) (string, error) {
	data, err := os.ReadFile(sumsPath)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if filepath.Base(name) == assetName {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("checksum for %s not found", assetName)
}

func extractBinary(archivePath, tmpDir, goos string) (string, error) {
	if strings.HasSuffix(archivePath, ".zip") {
		return extractZipBinary(archivePath, tmpDir, goos)
	}
	return extractTarGzBinary(archivePath, tmpDir, goos)
}

func extractTarGzBinary(archivePath, tmpDir, goos string) (string, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if header.FileInfo().IsDir() || filepath.Base(header.Name) != binaryName(goos) {
			continue
		}
		path := filepath.Join(tmpDir, "ctr-go-extracted")
		if err := writeExtracted(path, reader, header.FileInfo().Mode()); err != nil {
			return "", err
		}
		return path, nil
	}
	return "", errors.New("ctr-go binary not found in archive")
}

func extractZipBinary(archivePath, tmpDir, goos string) (string, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	for _, file := range reader.File {
		if file.FileInfo().IsDir() || filepath.Base(file.Name) != binaryName(goos) {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return "", err
		}
		defer rc.Close()
		path := filepath.Join(tmpDir, "ctr-go-extracted")
		if err := writeExtracted(path, rc, file.FileInfo().Mode()); err != nil {
			return "", err
		}
		return path, nil
	}
	return "", errors.New("ctr-go binary not found in archive")
}

func writeExtracted(path string, reader io.Reader, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := io.Copy(file, reader); err != nil {
		return err
	}
	if mode&0o111 == 0 {
		mode |= 0o755
	}
	return file.Chmod(mode)
}

func replaceBinary(src, target string) error {
	if runtime.GOOS == "windows" {
		return errors.New("self-update is not supported on Windows while ctr-go is running")
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	tmp := target + ".new"
	if err := copyBinary(src, tmp, info.Mode()); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func copyBinary(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode|0o111)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Chmod(mode | 0o111); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func binaryName(goos string) string {
	if goos == "windows" {
		return "ctr-go.exe"
	}
	return "ctr-go"
}

func cleanVersion(value string) string {
	return strings.TrimPrefix(strings.TrimSpace(value), "v")
}

func compareVersions(a, b string) int {
	aa := versionParts(a)
	bb := versionParts(b)
	for i := 0; i < len(aa) || i < len(bb); i++ {
		var av, bv int
		if i < len(aa) {
			av = aa[i]
		}
		if i < len(bb) {
			bv = bb[i]
		}
		if av > bv {
			return 1
		}
		if av < bv {
			return -1
		}
	}
	return 0
}

func versionParts(value string) []int {
	value = cleanVersion(value)
	out := make([]int, 0, 3)
	for _, part := range strings.Split(value, ".") {
		var digits strings.Builder
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				break
			}
			digits.WriteRune(ch)
		}
		if digits.Len() == 0 {
			out = append(out, 0)
			continue
		}
		parsed, err := strconv.Atoi(digits.String())
		if err != nil {
			parsed = 0
		}
		out = append(out, parsed)
	}
	return out
}
