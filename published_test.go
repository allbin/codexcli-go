package codexcli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The published-version fixtures are live captures taken on 2026-08-24, all
// describing codex 0.149.1:
//
//	published_npm_dist_tags.json     registry.npmjs.org/-/package/@openai%2fcodex/dist-tags (verbatim)
//	published_releases_channel.json  releases.openai.com/codex/channels/latest (assets truncated to two)
//	published_github_release.json    api.github.com/repos/openai/codex/releases/latest (trimmed to the read keys)
//	published_brew_cask.json         formulae.brew.sh/api/cask/codex.json (trimmed to the read keys)
//
// The truncations only drop entries from lists this package never reads; the
// shape each parser walks is the shape the service answered with.
const publishedFixtureVersion = "0.149.1"

func publishedFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// stubHTTP serves each URL from the given body, and fails the test on a
// request to anything else — so a lookup that reaches for a second source, or
// for one it should never consult, is a failure rather than a live call.
func stubHTTP(t *testing.T, bodies map[string][]byte) (*http.Client, *[]string) {
	t.Helper()
	var seen []string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		seen = append(seen, req.URL.String())
		if ua := req.Header.Get("User-Agent"); !strings.HasPrefix(ua, "codexcli-go/") {
			// The GitHub API rejects requests without one.
			t.Errorf("User-Agent = %q, want a codexcli-go identifier", ua)
		}
		body, ok := bodies[req.URL.String()]
		if !ok {
			t.Errorf("unexpected request to %s", req.URL)
			return &http.Response{StatusCode: 599, Status: "599 Unexpected", Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(string(body))),
			Request:    req,
		}, nil
	})}
	return client, &seen
}

func statusHTTP(t *testing.T, statuses map[string]int) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		code, ok := statuses[req.URL.String()]
		if !ok {
			t.Errorf("unexpected request to %s", req.URL)
			code = 599
		}
		return &http.Response{
			StatusCode: code,
			Status:     http.StatusText(code),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	})}
}

// noHTTP fails the test on any request at all: the "no trustworthy source"
// answer must be reached without touching the network.
func noHTTP(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Errorf("made a request to %s for an install with no trustworthy source", req.URL)
		return nil, errors.New("unexpected request")
	})}
}

// publishedEnv builds an installEnv over a synthetic layout, with no codex on
// the machine.
func publishedEnv(realPath, version string, files map[string]string) installEnv {
	return installEnv{
		lookPath: func(string) (string, error) { return "/home/u/.local/bin/codex", nil },
		evalSymlink: func(p string) (string, error) {
			if p == "/home/u/.local/bin/codex" {
				return realPath, nil
			}
			return "", os.ErrNotExist
		},
		readFile: func(name string) ([]byte, error) {
			if content, ok := files[name]; ok {
				return []byte(content), nil
			}
			return nil, os.ErrNotExist
		},
		runVersion: func(context.Context, string) (string, error) { return version, nil },
		codexHome:  fakeCodexHome,
	}
}

func TestLatestPublished_SourcePerInstallMethod(t *testing.T) {
	tests := []struct {
		name       string
		realPath   string
		files      map[string]string
		fixture    string
		wantURL    string
		wantSource PublishedSource
		wantMethod InstallMethod
		wantChan   string
	}{
		{
			name:       "npm global reads the registry dist-tags",
			realPath:   "/usr/local/lib/node_modules/@openai/codex/bin/codex.js",
			files:      map[string]string{"/usr/local/lib/node_modules/@openai/codex/package.json": cliPackageJSON},
			fixture:    "published_npm_dist_tags.json",
			wantURL:    npmDistTagsURL(),
			wantSource: PublishedSourceNPMRegistry,
			wantMethod: InstallNPMGlobal,
			wantChan:   "latest",
		},
		{
			name:       "standalone reads codex's own release channel",
			realPath:   standaloneReal,
			fixture:    "published_releases_channel.json",
			wantURL:    releasesChannelLatestURL,
			wantSource: PublishedSourceReleaseChannel,
			wantMethod: InstallNative,
			wantChan:   "latest",
		},
		{
			// The cask decides what a brew install tracks, so there is no
			// channel to report.
			name:       "homebrew reads its cask",
			realPath:   "/opt/homebrew/Caskroom/codex/0.148.0/codex",
			fixture:    "published_brew_cask.json",
			wantURL:    homebrewCaskAPIURL + "/codex.json",
			wantSource: PublishedSourceHomebrewCask,
			wantMethod: InstallPackageManager,
			wantChan:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, seen := stubHTTP(t, map[string][]byte{tt.wantURL: publishedFixture(t, tt.fixture)})
			pub, err := latestPublished(context.Background(), "codex",
				publishedEnv(tt.realPath, "0.148.0", tt.files),
				[]PublishedOption{WithPublishedHTTPClient(client)})
			if err != nil {
				t.Fatalf("latestPublished: %v", err)
			}

			if pub.Version != publishedFixtureVersion {
				t.Errorf("Version = %q, want %q", pub.Version, publishedFixtureVersion)
			}
			if pub.Installed != "0.148.0" {
				t.Errorf("Installed = %q, want 0.148.0", pub.Installed)
			}
			if pub.Status != VersionStatusBehind {
				t.Errorf("Status = %q, want %q (reason %q)", pub.Status, VersionStatusBehind, pub.StatusReason)
			}
			if pub.StatusReason != "" {
				t.Errorf("StatusReason = %q, want empty for a known verdict", pub.StatusReason)
			}
			if pub.Source != tt.wantSource {
				t.Errorf("Source = %q, want %q", pub.Source, tt.wantSource)
			}
			if pub.URL != tt.wantURL {
				t.Errorf("URL = %q, want %q", pub.URL, tt.wantURL)
			}
			if pub.Method != tt.wantMethod {
				t.Errorf("Method = %q, want %q", pub.Method, tt.wantMethod)
			}
			if pub.Channel != tt.wantChan {
				t.Errorf("Channel = %q, want %q", pub.Channel, tt.wantChan)
			}
			if len(*seen) != 1 {
				t.Errorf("made %d requests, want exactly one", len(*seen))
			}
		})
	}
}

func TestLatestPublished_StandaloneFallsBackToGitHub(t *testing.T) {
	// The two sources are mirrors of one release, so falling back is the same
	// stream — not a substitute from a neighbouring one.
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case releasesChannelLatestURL:
			return &http.Response{StatusCode: 503, Status: "503 Service Unavailable", Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		case githubLatestReleaseURL:
			return &http.Response{
				StatusCode: 200, Status: "200 OK",
				Body:    io.NopCloser(strings.NewReader(string(publishedFixture(t, "published_github_release.json")))),
				Request: req,
			}, nil
		}
		t.Errorf("unexpected request to %s", req.URL)
		return nil, errors.New("unexpected request")
	})}

	pub, err := latestPublished(context.Background(), "codex",
		publishedEnv(standaloneReal, "0.149.1", nil),
		[]PublishedOption{WithPublishedHTTPClient(client)})
	if err != nil {
		t.Fatalf("latestPublished: %v", err)
	}
	if pub.Source != PublishedSourceGitHubReleases {
		t.Errorf("Source = %q, want %q", pub.Source, PublishedSourceGitHubReleases)
	}
	if pub.URL != githubLatestReleaseURL {
		t.Errorf("URL = %q, want the source that actually answered", pub.URL)
	}
	if pub.Version != publishedFixtureVersion {
		t.Errorf("Version = %q, want %q", pub.Version, publishedFixtureVersion)
	}
	if pub.Status != VersionStatusCurrent {
		t.Errorf("Status = %q, want %q", pub.Status, VersionStatusCurrent)
	}
}

func TestLatestPublished_TransientFailureIsNotTheSentinel(t *testing.T) {
	// A 503 is worth retrying on the next tick; ErrPublishedUnknown never is,
	// so the two must not be confusable.
	client := statusHTTP(t, map[string]int{
		releasesChannelLatestURL: 503,
		githubLatestReleaseURL:   500,
	})
	_, err := latestPublished(context.Background(), "codex",
		publishedEnv(standaloneReal, "0.148.0", nil),
		[]PublishedOption{WithPublishedHTTPClient(client)})
	if err == nil {
		t.Fatal("err = nil, want the failed lookup reported")
	}
	if errors.Is(err, ErrPublishedUnknown) {
		t.Errorf("err = %v, want a transient failure rather than the sentinel", err)
	}
	// Both attempts are reported: a caller diagnosing this needs to see that
	// neither mirror answered.
	for _, want := range []string{"releases.openai.com", "api.github.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %s", err, want)
		}
	}
}

func TestLatestPublished_NoTrustworthySource(t *testing.T) {
	tests := []struct {
		name       string
		realPath   string
		wantMethod InstallMethod
		wantReason string
	}{
		{
			name:       "version manager",
			realPath:   "/home/u/.nvm/versions/node/v22.0.0/bin/codex",
			wantMethod: InstallVersionManager,
			wantReason: "release stream",
		},
		{
			name:       "unclassified binary",
			realPath:   "/usr/local/bin/codex",
			wantMethod: InstallUnknown,
			wantReason: "release stream",
		},
		{
			name:       "winget publishes no feed this package trusts",
			realPath:   "C:/Users/u/AppData/Local/Microsoft/WinGet/Packages/OpenAI.Codex/codex.exe",
			wantMethod: InstallPackageManager,
			wantReason: "winget",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pub, err := latestPublished(context.Background(), "codex",
				publishedEnv(tt.realPath, "0.148.0", nil),
				[]PublishedOption{WithPublishedHTTPClient(noHTTP(t))})
			if !errors.Is(err, ErrPublishedUnknown) {
				t.Fatalf("err = %v, want ErrPublishedUnknown", err)
			}
			if pub != nil {
				t.Errorf("pub = %+v, want nil", pub)
			}
			var unknown *PublishedUnknownError
			if !errors.As(err, &unknown) {
				t.Fatalf("err = %v, want a *PublishedUnknownError", err)
			}
			if unknown.Method != tt.wantMethod {
				t.Errorf("Method = %q, want %q", unknown.Method, tt.wantMethod)
			}
			if !strings.Contains(unknown.Reason, tt.wantReason) {
				t.Errorf("Reason = %q, want it to mention %q", unknown.Reason, tt.wantReason)
			}
		})
	}
}

func TestLatestPublished_UnpublishedCaskIsUnknownNotAFailure(t *testing.T) {
	// A cask Homebrew does not publish is a stable fact about the install, so
	// it must not read as a request worth retrying.
	client := statusHTTP(t, map[string]int{homebrewCaskAPIURL + "/codex-nightly.json": 404})
	_, err := latestPublished(context.Background(), "codex",
		publishedEnv("/opt/homebrew/Caskroom/codex-nightly/0.148.0/codex", "0.148.0", nil),
		[]PublishedOption{WithPublishedHTTPClient(client)})
	if !errors.Is(err, ErrPublishedUnknown) {
		t.Fatalf("err = %v, want ErrPublishedUnknown", err)
	}
	if !strings.Contains(err.Error(), "codex-nightly") {
		t.Errorf("err = %v, want it to name the cask", err)
	}
}

func TestHomebrewCask(t *testing.T) {
	tests := []struct {
		name     string
		info     *InstallInfo
		want     string
		wantOK   bool
		wantNote string
	}{
		{
			name:   "published cask",
			info:   &InstallInfo{PackageManager: "homebrew", PackageName: "codex"},
			want:   "codex",
			wantOK: true,
		},
		{
			name:   "empty token falls back to the published cask",
			info:   &InstallInfo{PackageManager: "homebrew"},
			want:   defaultHomebrewCask,
			wantOK: true,
		},
		{
			// The token comes off the filesystem and is interpolated into a
			// URL path.
			name:   "path traversal is refused",
			info:   &InstallInfo{PackageManager: "homebrew", PackageName: "../../etc/passwd"},
			wantOK: false,
		},
		{
			name:   "not homebrew",
			info:   &InstallInfo{PackageManager: "winget", PackageName: "OpenAI.Codex"},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := homebrewCask(tt.info)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("homebrewCask = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestVersionVerdict(t *testing.T) {
	tests := []struct {
		name       string
		installed  string
		published  string
		channel    string
		wantStatus VersionStatus
		wantReason string
	}{
		{
			name: "behind", installed: "0.148.0", published: "0.149.1", channel: "latest",
			wantStatus: VersionStatusBehind,
		},
		{
			name: "identical", installed: "0.149.1", published: "0.149.1", channel: "latest",
			wantStatus: VersionStatusCurrent,
		},
		{
			// A locally built binary, or a release pulled before the channel
			// caught up: not older, so not behind.
			name: "ahead", installed: "0.150.0", published: "0.149.1", channel: "latest",
			wantStatus: VersionStatusCurrent,
		},
		{
			name: "minor beats patch", installed: "0.149.9", published: "0.150.0", channel: "latest",
			wantStatus: VersionStatusBehind,
		},
		{
			// npm publishes alpha and beta dist-tags; an install from one of
			// them is not following `latest`, and semver would happily call it
			// behind a release it never tracked.
			name: "prerelease install gets no verdict", installed: "0.149.0-alpha.4.3", published: "0.149.1", channel: "latest",
			wantStatus: VersionStatusUnknown, wantReason: "prerelease",
		},
		{
			name: "unreadable install", installed: "", published: "0.149.1",
			wantStatus: VersionStatusUnknown, wantReason: "could not be read",
		},
		{
			name: "unparseable install", installed: "codex-cli", published: "0.149.1",
			wantStatus: VersionStatusUnknown, wantReason: "not one this package can compare",
		},
		{
			name: "unparseable published", installed: "0.148.0", published: "unknown",
			wantStatus: VersionStatusUnknown, wantReason: "not one this package can compare",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, reason := versionVerdict(tt.installed, tt.published, tt.channel)
			if status != tt.wantStatus {
				t.Errorf("status = %q, want %q (reason %q)", status, tt.wantStatus, reason)
			}
			if status.Known() != (tt.wantStatus != VersionStatusUnknown) {
				t.Errorf("Known() = %v for status %q", status.Known(), status)
			}
			if tt.wantReason == "" && reason != "" {
				t.Errorf("reason = %q, want empty for a known verdict", reason)
			}
			if tt.wantReason != "" && !strings.Contains(reason, tt.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", reason, tt.wantReason)
			}
		})
	}
}

func TestVersionStatusZeroValueIsUnknown(t *testing.T) {
	// A half-built result must not read as good news.
	var pub Published
	if pub.Status != VersionStatusUnknown || pub.Status.Known() {
		t.Errorf("zero Status = %q, Known() = %v", pub.Status, pub.Status.Known())
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in         string
		wantOK     bool
		wantPre    string
		wantMajor  int
		wantMinor  int
		wantPatch  int
		wantReason string
	}{
		{in: "0.149.1", wantOK: true, wantMinor: 149, wantPatch: 1},
		{in: "v0.149.1", wantOK: true, wantMinor: 149, wantPatch: 1},
		{in: "0.149.0-alpha.4.3", wantOK: true, wantMinor: 149, wantPre: "alpha.4.3"},
		{in: "0.149.1-linux-x64", wantOK: true, wantMinor: 149, wantPatch: 1, wantPre: "linux-x64"},
		{in: "1.2.3+build.5", wantOK: true, wantMajor: 1, wantMinor: 2, wantPatch: 3},
		{in: "0.149", wantOK: false},
		{in: "0.149.1.2", wantOK: false},
		{in: "codex-cli", wantOK: false},
		{in: "", wantOK: false},
		{in: "0.149.x", wantOK: false},
		{in: "0.149.1-", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := parseVersion(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("parseVersion(%q) ok = %v, want %v", tt.in, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.major != tt.wantMajor || got.minor != tt.wantMinor || got.patch != tt.wantPatch {
				t.Errorf("parseVersion(%q) = %d.%d.%d, want %d.%d.%d", tt.in,
					got.major, got.minor, got.patch, tt.wantMajor, tt.wantMinor, tt.wantPatch)
			}
			if got.prerelease != tt.wantPre {
				t.Errorf("prerelease = %q, want %q", got.prerelease, tt.wantPre)
			}
		})
	}
}

func TestReleaseVersionFromTag(t *testing.T) {
	tests := map[string]string{
		"rust-v0.149.1":       "0.149.1",
		" rust-v0.149.1 ":     "0.149.1",
		"rust-v0.149.0-alpha": "0.149.0-alpha",
		"v0.149.1":            "",
		"0.149.1":             "",
		"rust-vlatest":        "",
		"":                    "",
	}
	for tag, want := range tests {
		if got := releaseVersionFromTag(tag); got != want {
			t.Errorf("releaseVersionFromTag(%q) = %q, want %q", tag, got, want)
		}
	}
}

func TestFetchNPMDistTagMissingTag(t *testing.T) {
	// A tag the registry does not publish is reported, never swapped for
	// another tag's number.
	client, _ := stubHTTP(t, map[string][]byte{npmDistTagsURL(): publishedFixture(t, "published_npm_dist_tags.json")})
	if _, err := fetchNPMDistTag(context.Background(), client, npmDistTagsURL(), "nightly"); err == nil {
		t.Fatal("err = nil, want the missing dist-tag reported")
	}
}

func TestFetchNPMDistTagReadsTheLatestTag(t *testing.T) {
	client, _ := stubHTTP(t, map[string][]byte{npmDistTagsURL(): publishedFixture(t, "published_npm_dist_tags.json")})
	got, err := fetchNPMDistTag(context.Background(), client, npmDistTagsURL(), npmLatestTag)
	if err != nil {
		t.Fatal(err)
	}
	if got != publishedFixtureVersion {
		t.Errorf("version = %q, want %q", got, publishedFixtureVersion)
	}
}

func TestNPMDistTagsURLEscapesTheScopedName(t *testing.T) {
	// "@" and "/" are path-significant; concatenating the scoped name would
	// address a different package.
	got := npmDistTagsURL()
	if !strings.Contains(got, "%2Fcodex") {
		t.Errorf("npmDistTagsURL = %q, want the scope separator escaped", got)
	}
	if strings.Contains(got, "openai/codex") {
		t.Errorf("npmDistTagsURL = %q, want no bare path separator in the package name", got)
	}
}
