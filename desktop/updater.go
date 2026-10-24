package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The app's own version. appversion.txt says "dev" in the repository; the
// release workflow writes the tag's version into it before bundling, so a
// released app knows which release it is without per-platform linker flags.
//
//go:embed appversion.txt
var appVersionFile string

func appVersion() string {
	v := strings.TrimPrefix(strings.TrimSpace(appVersionFile), "v")
	if v == "" {
		return "dev"
	}
	return v
}

// releasesAPI is where releases are listed; tests point it elsewhere.
var releasesAPI = "https://api.github.com/repos/Luqueee/mole/releases?per_page=30"

// downloadPrefix is the only place an update is downloaded from.
var downloadPrefix = "https://github.com/Luqueee/mole/releases/download/"

const releasePagePrefix = "https://github.com/Luqueee/mole/releases/tag/"

// AppUpdate says whether a newer desktop release exists.
type AppUpdate struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	Available bool   `json:"available"`
	Page      string `json:"page"`
	Asset     string `json:"asset"`
	Size      int64  `json:"size"`
	// Installable is false when this platform has no asset, or the release
	// carries no checksum to verify it against: the page is then all there is.
	Installable bool   `json:"installable"`
	Error       string `json:"error"`
	CheckedMs   int64  `json:"checkedMs"`
}

type releaseAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

type release struct {
	Tag        string         `json:"tag_name"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []releaseAsset `json:"assets"`
}

// assetName is the bundle that installs the app on this system.
func assetName() string {
	switch runtime.GOOS {
	case "darwin":
		return "mole.dmg"
	case "windows":
		return "mole-amd64-installer.exe"
	case "linux":
		return "mole.deb"
	}
	return ""
}

// versionParts reads "0.5.0" (or "v0.5.0") as numbers; ok is false for anything else.
func versionParts(v string) (p [3]int, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	fields := strings.Split(v, ".")
	if len(fields) == 0 || len(fields) > 3 || v == "" {
		return p, false
	}
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return p, false
		}
		p[i] = n
	}
	return p, true
}

// newerVersion reports whether a is a later version than b.
func newerVersion(a, b string) bool {
	x, ok1 := versionParts(a)
	y, ok2 := versionParts(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

var updates struct {
	mu   sync.Mutex
	last AppUpdate
	at   time.Time
	rel  *release
}

// CheckAppUpdate asks GitHub for the newest desktop release. The answer is
// kept for an hour unless force is set. A failed check is reported in Error,
// never as an error: being offline is normal.
func (m *MoleService) CheckAppUpdate(force bool) AppUpdate {
	updates.mu.Lock()
	defer updates.mu.Unlock()
	if !force && !updates.at.IsZero() && time.Since(updates.at) < time.Hour {
		return updates.last
	}
	cur := appVersion()
	res := AppUpdate{Current: cur, CheckedMs: time.Now().UnixMilli()}
	rel, err := latestDesktopRelease()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	updates.rel = rel
	res.Latest = strings.TrimPrefix(rel.Tag, "desktop-v")
	res.Page = releasePagePrefix + rel.Tag
	res.Available = cur != "dev" && newerVersion(res.Latest, cur)
	if a := pickAsset(rel); a != nil {
		res.Asset, res.Size = a.Name, a.Size
		res.Installable = strings.HasPrefix(a.Digest, "sha256:") && strings.HasPrefix(a.URL, downloadPrefix)
	}
	updates.last, updates.at = res, time.Now()
	return res
}

func pickAsset(r *release) *releaseAsset {
	want := assetName()
	for i := range r.Assets {
		if r.Assets[i].Name == want {
			return &r.Assets[i]
		}
	}
	return nil
}

func latestDesktopRelease() (*release, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesAPI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "mole-desktop/"+appVersion())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not check for updates: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("could not check for updates: GitHub answered %s", resp.Status)
	}
	var rels []release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&rels); err != nil {
		return nil, fmt.Errorf("could not read the release list: %w", err)
	}
	var best *release
	for i := range rels {
		r := &rels[i]
		if r.Draft || r.Prerelease || !strings.HasPrefix(r.Tag, "desktop-v") {
			continue
		}
		if _, ok := versionParts(strings.TrimPrefix(r.Tag, "desktop-v")); !ok {
			continue
		}
		if best == nil || newerVersion(strings.TrimPrefix(r.Tag, "desktop-v"), strings.TrimPrefix(best.Tag, "desktop-v")) {
			best = r
		}
	}
	if best == nil {
		return nil, errors.New("no desktop release found")
	}
	return best, nil
}

// UpdateProgress is the state of the download started by DownloadAppUpdate.
type UpdateProgress struct {
	// Phase is "idle", "downloading", "verifying", "opened" or "failed".
	Phase string `json:"phase"`
	Bytes int64  `json:"bytes"`
	Total int64  `json:"total"`
	Path  string `json:"path"`
	Error string `json:"error"`
}

var dl struct {
	mu sync.Mutex
	p  UpdateProgress
}

func setProgress(f func(*UpdateProgress)) {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	f(&dl.p)
}

// GetUpdateProgress reports the download in progress.
func (m *MoleService) GetUpdateProgress() UpdateProgress {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	if dl.p.Phase == "" {
		return UpdateProgress{Phase: "idle"}
	}
	return dl.p
}

// DownloadAppUpdate downloads the newest release's installer in the
// background, checks it against the digest GitHub lists for it, and opens it
// (the disk image on macOS, the installer on Windows, the package on Linux).
// The app does not replace itself: the system's own installer does.
func (m *MoleService) DownloadAppUpdate() error {
	dl.mu.Lock()
	if dl.p.Phase == "downloading" || dl.p.Phase == "verifying" {
		dl.mu.Unlock()
		return errors.New("an update is already downloading")
	}
	dl.mu.Unlock()

	updates.mu.Lock()
	rel := updates.rel
	updates.mu.Unlock()
	if rel == nil {
		return errors.New("check for updates first")
	}
	a := pickAsset(rel)
	if a == nil {
		return fmt.Errorf("release %s has no %s for this system", rel.Tag, assetName())
	}
	if !strings.HasPrefix(a.URL, downloadPrefix) {
		return fmt.Errorf("refusing to download from %s", a.URL)
	}
	want, ok := strings.CutPrefix(a.Digest, "sha256:")
	if !ok || len(want) != 64 {
		return errors.New("the release lists no checksum for its installer; open the release page instead")
	}
	setProgress(func(p *UpdateProgress) { *p = UpdateProgress{Phase: "downloading", Total: a.Size} })
	go func() {
		path, err := fetchInstaller(a, strings.ToLower(want))
		if err == nil {
			err = openInstaller(path)
		}
		setProgress(func(p *UpdateProgress) {
			if err != nil {
				p.Phase, p.Error = "failed", err.Error()
				return
			}
			p.Phase, p.Path = "opened", path
		})
	}()
	return nil
}

func updatesDir(version string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "mole", "updates", version)
	return dir, os.MkdirAll(dir, 0o755)
}

// fetchInstaller downloads a into the cache and returns its path once the
// bytes match the digest. A file that does not match is deleted.
func fetchInstaller(a *releaseAsset, wantSum string) (string, error) {
	tag := strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(strings.TrimSuffix(strings.TrimPrefix(a.URL, downloadPrefix), "/"+a.Name))
	dir, err := updatesDir(tag)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(dir, a.Name)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "mole-desktop/"+appVersion())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}
	tmp, err := os.CreateTemp(dir, ".part-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	var n int64
	buf := make([]byte, 64<<10)
	for {
		k, rerr := resp.Body.Read(buf)
		if k > 0 {
			if _, werr := tmp.Write(buf[:k]); werr != nil {
				tmp.Close()
				return "", werr
			}
			h.Write(buf[:k])
			n += int64(k)
			setProgress(func(p *UpdateProgress) { p.Bytes = n })
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			tmp.Close()
			return "", fmt.Errorf("download interrupted: %w", rerr)
		}
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	setProgress(func(p *UpdateProgress) { p.Phase = "verifying" })
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSum {
		return "", fmt.Errorf("the download does not match its published checksum (got %s…, want %s…): not opening it", got[:12], wantSum[:12])
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	_ = os.Remove(dest)
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return "", err
	}
	return dest, nil
}

// openInstaller hands the verified file to the system.
func openInstaller(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not open %s: %w", path, err)
	}
	go cmd.Wait()
	return nil
}

// OpenReleasePage opens a release page in the browser.
func (m *MoleService) OpenReleasePage(url string) error {
	if !strings.HasPrefix(url, releasePagePrefix) || strings.ContainsAny(url, " \n\r\"'") {
		return fmt.Errorf("not a mole release page: %s", url)
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// GetAppVersion is the running app's version ("dev" for a build from source).
func (m *MoleService) GetAppVersion() string { return appVersion() }
