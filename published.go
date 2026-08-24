// This file makes network calls. Everything else in this package that reports
// on an installation — DetectInstall and its classification — is deliberately
// offline and cheap enough for a launch path. Keep the two apart: nothing here
// may be called from detection.

package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// npmRegistryURL is the public npm registry. The per-package dist-tags
// endpoint under it answers with a small JSON object of tag → version, which
// is what a channel lookup needs and a fraction of the full package document.
const npmRegistryURL = "https://registry.npmjs.org"

// npmLatestTag is the dist-tag a node-managed codex install tracks. It is the
// tag `npm install -g @openai/codex` resolves, which is what codex's own
// updater runs. The registry also publishes `alpha`, `beta` and per-platform
// tags; an install from one of those is a different release stream, which is
// what the prerelease rule in versionVerdict is about.
const npmLatestTag = "latest"

// releasesChannelLatestURL is codex's own release-channel metadata for the
// standalone installer, answering with `{"tag_name": "rust-<version>", ...}`.
const releasesChannelLatestURL = "https://releases.openai.com/codex/channels/latest"

// standaloneChannel is the release channel a standalone install tracks. The
// installer resolves `latest` unless CODEX_RELEASE pins an exact version, and
// a pinned install is not following a channel at all.
const standaloneChannel = "latest"

// githubLatestReleaseURL is the GitHub releases API for the codex repository,
// answering with the same `tag_name` for the same release.
const githubLatestReleaseURL = "https://api.github.com/repos/openai/codex/releases/latest"

// releaseTagPrefix is what codex prefixes its release tags with: the tag for
// 0.149.1 is `rust-v0.149.1`.
const releaseTagPrefix = "rust-v"

// homebrewCaskAPIURL serves one JSON document per cask, with the version
// Homebrew would install under .version.
const homebrewCaskAPIURL = "https://formulae.brew.sh/api/cask"

// defaultHomebrewCask is the cask OpenAI publishes codex under, used when a
// Homebrew install was detected but the Caskroom path carried no token.
const defaultHomebrewCask = "codex"

// defaultPublishedTimeout bounds a published-version lookup when the caller's
// context has no deadline. This is a background-tick call; it should give up
// long before a consumer notices.
const defaultPublishedTimeout = 10 * time.Second

// maxPublishedResponse caps how much of a lookup response is read. The largest
// of these endpoints (the GitHub release document) was ~270 KB when this was
// written; anything past the cap is not the answer to this question.
const maxPublishedResponse = 4 << 20

// PublishedSource records which service answered a published-version lookup,
// so a caller can weigh the number and tell the user where it came from.
type PublishedSource string

const (
	// PublishedSourceNPMRegistry is the npm registry's dist-tags for
	// CLIPackageName. It answers for node-managed installs (npm, pnpm, bun).
	PublishedSourceNPMRegistry PublishedSource = "npm-registry"

	// PublishedSourceReleaseChannel is codex's own release-channel metadata at
	// releases.openai.com, which answers for standalone installs.
	PublishedSourceReleaseChannel PublishedSource = "release-channel"

	// PublishedSourceGitHubReleases is the GitHub releases API for
	// openai/codex. It answers for standalone installs when the release
	// channel could not be reached — the standalone installer treats the two
	// as mirrors of one release, so this is the same stream and not a
	// substitute from a neighbouring one.
	PublishedSourceGitHubReleases PublishedSource = "github-releases"

	// PublishedSourceHomebrewCask is the Homebrew formulae API. Homebrew
	// installs track their cask, which can lag the other sources by hours to
	// days — so the cask, not the npm tag, is the honest comparison for them.
	PublishedSourceHomebrewCask PublishedSource = "homebrew-cask"

	// PublishedSourceNone means no trustworthy source exists for this install.
	PublishedSourceNone PublishedSource = "none"
)

// VersionStatus is the verdict on an installed version against a published
// one. It has three states, not two, because "I have a number but no verdict"
// is a real and common answer and a bool cannot express it.
//
// The zero value is VersionStatusUnknown, so a half-built result cannot read
// as good news.
type VersionStatus string

const (
	// VersionStatusUnknown means no comparison was possible. Published.Version
	// may still be set: "what is published?" is a fair question even when "am
	// I behind?" has no honest answer. Published.StatusReason says why.
	VersionStatusUnknown VersionStatus = ""

	// VersionStatusCurrent means the installed version is not older than the
	// published one. An install ahead of what is published — a locally built
	// binary, a release pulled before the channel caught up — is current, not
	// behind, which matches codex's own wording ("current version is not
	// older").
	VersionStatusCurrent VersionStatus = "current"

	// VersionStatusBehind means the installed version is older than the
	// published one on the same release stream.
	VersionStatusBehind VersionStatus = "behind"
)

// Known reports whether the status is a verdict at all. Gate anything
// reassuring on this: a false UpdateAvailable-style read of an unknown status
// is exactly the bug the three states exist to prevent.
func (s VersionStatus) Known() bool {
	return s == VersionStatusCurrent || s == VersionStatusBehind
}

// Published is the version published for an install's own release stream.
type Published struct {
	// Version is the published version (e.g. "0.149.1"). It is reported
	// whenever a source answered, including when Status is unknown.
	Version string

	// Installed is what the CLI reports for itself right now, carried along so
	// one call answers the whole question. "" when the version probe failed.
	Installed string

	// Status is the verdict: behind, current, or unknown. Check
	// Status.Known() before rendering anything reassuring.
	Status VersionStatus

	// StatusReason explains an unknown Status in one human-readable line, and
	// is "" when Status is known. It exists so a consumer can say why there is
	// no verdict instead of silently showing a blank.
	StatusReason string

	// Channel is the release stream consulted — "latest" for the npm and
	// standalone sources. It is "" for Homebrew, whose cask decides what it
	// tracks; the cask queried is in URL.
	Channel string

	// Source records which service answered.
	Source PublishedSource

	// URL is the endpoint that was queried, for a consumer that wants to show
	// or log where the number came from.
	URL string

	// Method is the detected install method the source was chosen from.
	Method InstallMethod
}

// ErrPublishedUnknown matches the error returned by [LatestPublished] when no
// trustworthy published-version source exists for *this* install. Use
// errors.Is to distinguish it: an honest "cannot determine" is a correct
// answer, and materially better than a number from the wrong stream.
//
// It means "no number is available for this install, and no substitute would
// be correct" — never "the lookup failed". A failed request comes back as an
// ordinary wrapped error, because that is transient and worth retrying on the
// next tick; this one is a stable property of the install and never is.
//
// Nothing in this package degrades to a neighbouring stream's number to avoid
// returning it. A version-manager install does not borrow npm's tag, an
// unrecognized Homebrew cask does not fall back to the npm registry, and an
// alpha install is not measured against `latest` — those streams disagree, and
// a wrong number there manufactures a "behind" that is not true.
var ErrPublishedUnknown = errors.New("codexcli: no trustworthy published-version source for this install")

// PublishedUnknownError reports why a published-version lookup was not even
// attempted, or why its answer cannot be trusted for this install.
type PublishedUnknownError struct {
	// Method is the detected install method.
	Method InstallMethod

	// Reason is a human-readable explanation, ready to show a user.
	Reason string
}

func (e *PublishedUnknownError) Error() string {
	return fmt.Sprintf("codexcli: cannot determine the published version for a %s install: %s", e.Method, e.Reason)
}

func (e *PublishedUnknownError) Is(target error) bool { return target == ErrPublishedUnknown }

// PublishedOption configures a single [LatestPublished] call.
type PublishedOption func(*publishedOptions)

type publishedOptions struct {
	client  *http.Client
	timeout time.Duration
}

// WithPublishedHTTPClient sets the HTTP client used for the lookup. Use it to
// route through a proxy, pin a transport, or stub the network in tests.
func WithPublishedHTTPClient(c *http.Client) PublishedOption {
	return func(o *publishedOptions) { o.client = c }
}

// WithPublishedTimeout bounds the lookup. It applies only when the caller's
// context has no deadline of its own.
func WithPublishedTimeout(d time.Duration) PublishedOption {
	return func(o *publishedOptions) { o.timeout = d }
}

// LatestPublished reports the version published for the install's own release
// stream, using the default client's binary.
//
// # This one touches the network, but only just
//
// One HTTP request (two if a fallback is needed), plus the offline detection
// [DetectInstall] already does. It is the cheap way to answer "am I behind?":
// [Doctor] answers it too, but spends a CLI spawn, DNS and a WebSocket to the
// model provider — ~1.2s against codex 0.148 — on its way there. Use Doctor
// when you want codex's own account of everything; use this for a background
// tick. Neither runs as part of detection, and detection never runs either.
//
// # The streams do not agree
//
// This belongs in this library rather than in a consumer because only this
// library knows which stream a given install tracks, and they publish
// different numbers. The source is resolved from the detected install:
//
//   - npm-global (npm, pnpm or bun) reads the npm registry's dist-tags for
//     CLIPackageName — over plain HTTP, never by shelling out to `npm view`,
//     which assumes an npm on PATH that a server generally does not have.
//   - native reads codex's own release-channel metadata at
//     releases.openai.com, the endpoint the standalone installer resolves
//     `latest` through, falling back to the GitHub releases API the installer
//     also accepts for the same release.
//   - Homebrew reads the cask it was installed from, because a brew install
//     tracks its cask rather than a channel, and the cask can lag.
//   - Everything else — a version manager, another OS package manager, an
//     install nothing could classify — has no trustworthy source and returns
//     [ErrPublishedUnknown]. Borrowing npm's number for those would be a wrong
//     answer dressed as a right one.
//
// # The verdict is three states
//
// [Published.Status] is behind, current, or unknown, and unknown is the zero
// value. It is unknown when either version could not be read or parsed, and
// when the installed version is a prerelease: npm publishes `alpha` and `beta`
// dist-tags alongside `latest`, so an install from one of those tracks a
// stream this lookup did not consult, and semver would happily call it
// "behind" a release it is not following. [Published.Version] is still
// reported in every one of those cases.
//
// A failed lookup is never fatal. The error is returned and the caller keeps
// whatever answer it had — a missed tick is not news.
func LatestPublished(ctx context.Context, opts ...PublishedOption) (*Published, error) {
	return defaultInstallClient.LatestPublished(ctx, opts...)
}

// LatestPublished reports the version published for this client's install. See
// the package-level [LatestPublished] for the full contract.
//
// The client's WithCodexHome default applies to the detection this runs.
func (c *Client) LatestPublished(ctx context.Context, opts ...PublishedOption) (*Published, error) {
	resolved := resolveOptions(c.defaults, nil)
	pub, err := latestPublished(ctx, c.binaryPath(), osInstallEnv(resolved.codexHome), opts)
	if err != nil {
		c.log().Debug("latest published", "err", err)
		return nil, err
	}
	c.log().Debug("latest published",
		"version", pub.Version, "installed", pub.Installed,
		"status", pub.Status, "statusReason", pub.StatusReason,
		"channel", pub.Channel, "source", pub.Source,
		"url", pub.URL, "method", pub.Method)
	return pub, nil
}

func latestPublished(ctx context.Context, binary string, env installEnv, opts []PublishedOption) (*Published, error) {
	var o publishedOptions
	for _, opt := range opts {
		opt(&o)
	}

	info, err := detectInstall(ctx, binary, env)
	if err != nil {
		return nil, err
	}

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		timeout := o.timeout
		if timeout <= 0 {
			timeout = defaultPublishedTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	client := o.client
	if client == nil {
		client = http.DefaultClient
	}

	pub := &Published{Installed: info.Version, Method: info.Method}

	switch info.Method {
	case InstallNPMGlobal:
		pub.Source = PublishedSourceNPMRegistry
		pub.Channel = npmLatestTag
		pub.URL = npmDistTagsURL()
		pub.Version, err = fetchNPMDistTag(ctx, client, pub.URL, npmLatestTag)

	case InstallNative:
		pub.Channel = standaloneChannel
		pub.Version, pub.Source, pub.URL, err = fetchStandaloneRelease(ctx, client)

	case InstallPackageManager:
		cask, ok := homebrewCask(info)
		if !ok {
			return nil, &PublishedUnknownError{Method: info.Method, Reason: unknownPackageManagerReason(info)}
		}
		pub.Source = PublishedSourceHomebrewCask
		pub.URL = homebrewCaskAPIURL + "/" + cask + ".json"
		pub.Version, err = fetchHomebrewCask(ctx, client, pub.URL)
		if isNotFound(err) {
			// A cask Homebrew does not publish is a stable fact about this
			// install, not a request worth retrying.
			return nil, &PublishedUnknownError{
				Method: info.Method,
				Reason: fmt.Sprintf("Homebrew publishes no cask named %q", cask),
			}
		}

	default:
		pub.Source = PublishedSourceNone
		return nil, &PublishedUnknownError{
			Method: info.Method,
			Reason: "nothing records which release stream this install tracks",
		}
	}
	if err != nil {
		return nil, err
	}

	pub.Status, pub.StatusReason = versionVerdict(pub.Installed, pub.Version, pub.Channel)
	return pub, nil
}

// npmDistTagsURL builds the registry's dist-tags endpoint for the CLI package.
// The scoped package name is escaped rather than concatenated: the "@" and "/"
// are path-significant.
func npmDistTagsURL() string {
	return npmRegistryURL + "/-/package/" + url.PathEscape(CLIPackageName) + "/dist-tags"
}

// homebrewCask reports the cask a Homebrew install was installed from, and
// whether it is one this package will query.
//
// The token comes from a Caskroom path segment, so it is validated before it
// reaches a URL. An empty token falls back to the published cask name, which
// is what a Caskroom layout with nothing after it means.
func homebrewCask(info *InstallInfo) (string, bool) {
	if info.PackageManager != "homebrew" {
		return "", false
	}
	if info.PackageName == "" {
		return defaultHomebrewCask, true
	}
	if !validCaskToken(info.PackageName) {
		return "", false
	}
	return info.PackageName, true
}

// validCaskToken reports whether s is shaped like a Homebrew cask token. The
// character set is deliberately narrow: this string is interpolated into a URL
// path, and it came off the filesystem.
func validCaskToken(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case i > 0 && (r == '-' || r == '_' || r == '.' || r == '+' || r == '@'):
		default:
			return false
		}
	}
	return true
}

func unknownPackageManagerReason(info *InstallInfo) string {
	if info.PackageManager == "homebrew" {
		return fmt.Sprintf("cask %q is not a name this package will query", info.PackageName)
	}
	return fmt.Sprintf("%s publishes no version feed this package can trust", info.PackageManager)
}

// fetchNPMDistTag reads one dist-tag from the registry. A tag the registry
// does not publish is reported as such rather than falling back to another
// tag.
func fetchNPMDistTag(ctx context.Context, client *http.Client, endpoint, tag string) (string, error) {
	body, err := fetchPublished(ctx, client, endpoint)
	if err != nil {
		return "", err
	}
	var tags map[string]string
	if err := json.Unmarshal(body, &tags); err != nil {
		return "", fmt.Errorf("codexcli: decode npm dist-tags: %w", err)
	}
	version := tags[tag]
	if version == "" {
		return "", fmt.Errorf("codexcli: npm publishes no %q dist-tag for %s", tag, CLIPackageName)
	}
	return version, nil
}

// fetchStandaloneRelease reads the version the standalone installer would
// install, from the same metadata the installer itself resolves.
//
// codex's install.sh accepts two interchangeable sources for one release:
// releases.openai.com's channel document and the GitHub releases API, each
// answering with the same `tag_name`. This asks the former first — the GitHub
// API rate-limits unauthenticated callers per IP, which a background tick on a
// shared host will find — and falls back to the latter, which is the
// installer's own default. Both errors are reported when neither answers.
func fetchStandaloneRelease(ctx context.Context, client *http.Client) (version string, source PublishedSource, endpoint string, err error) {
	version, chanErr := fetchReleaseTag(ctx, client, releasesChannelLatestURL)
	if chanErr == nil {
		return version, PublishedSourceReleaseChannel, releasesChannelLatestURL, nil
	}
	version, ghErr := fetchReleaseTag(ctx, client, githubLatestReleaseURL)
	if ghErr == nil {
		return version, PublishedSourceGitHubReleases, githubLatestReleaseURL, nil
	}
	return "", PublishedSourceReleaseChannel, releasesChannelLatestURL, errors.Join(chanErr, ghErr)
}

// fetchReleaseTag reads a release document's `tag_name` and strips codex's
// `rust-v` prefix off it.
func fetchReleaseTag(ctx context.Context, client *http.Client, endpoint string) (string, error) {
	body, err := fetchPublished(ctx, client, endpoint)
	if err != nil {
		return "", err
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &release); err != nil {
		return "", fmt.Errorf("codexcli: decode release metadata from %s: %w", endpoint, err)
	}
	version := releaseVersionFromTag(release.TagName)
	if version == "" {
		return "", fmt.Errorf("codexcli: %s answered with tag %q, which is not a codex release tag", endpoint, release.TagName)
	}
	return version, nil
}

// releaseVersionFromTag extracts the version from a codex release tag
// ("rust-v0.149.1" → "0.149.1"), returning "" for anything else rather than
// passing an unrecognized string off as a version.
func releaseVersionFromTag(tag string) string {
	rest, ok := strings.CutPrefix(strings.TrimSpace(tag), releaseTagPrefix)
	if !ok {
		return ""
	}
	if _, ok := parseVersion(rest); !ok {
		return ""
	}
	return rest
}

// fetchHomebrewCask reads the version Homebrew would install for a cask.
func fetchHomebrewCask(ctx context.Context, client *http.Client, endpoint string) (string, error) {
	body, err := fetchPublished(ctx, client, endpoint)
	if err != nil {
		return "", err
	}
	var cask struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &cask); err != nil {
		return "", fmt.Errorf("codexcli: decode homebrew cask: %w", err)
	}
	if cask.Version == "" {
		return "", fmt.Errorf("codexcli: homebrew cask %s reports no version", endpoint)
	}
	return cask.Version, nil
}

// httpStatusError reports a non-200 answer, keeping the code so a stable "no
// such thing" (404) can be told apart from a transient failure.
type httpStatusError struct {
	URL        string
	Status     string
	StatusCode int
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("codexcli: fetch %s: unexpected status %s", e.URL, e.Status)
}

func isNotFound(err error) bool {
	var se *httpStatusError
	return errors.As(err, &se) && se.StatusCode == http.StatusNotFound
}

func fetchPublished(ctx context.Context, client *http.Client, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("codexcli: build request for %s: %w", endpoint, err)
	}
	// The GitHub API rejects requests without a User-Agent, and the other
	// endpoints are happier being able to attribute traffic.
	req.Header.Set("User-Agent", "codexcli-go/"+SDKVersion)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codexcli: fetch %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &httpStatusError{URL: endpoint, Status: resp.Status, StatusCode: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPublishedResponse+1))
	if err != nil {
		return nil, fmt.Errorf("codexcli: read %s: %w", endpoint, err)
	}
	if len(body) > maxPublishedResponse {
		return nil, fmt.Errorf("codexcli: %s answered with more than %d bytes, which is not a version document", endpoint, maxPublishedResponse)
	}
	return body, nil
}

// versionVerdict compares an installed version against a published one and
// says why when it will not.
//
// The prerelease rule is the codex-specific one: `0.149.0-alpha.4.3` is
// published under the `alpha` dist-tag, and comparing it against `latest`
// would report "behind" for an install that is following a different stream
// perfectly well.
func versionVerdict(installed, published, channel string) (VersionStatus, string) {
	if installed == "" {
		return VersionStatusUnknown, "the installed version could not be read"
	}
	iv, ok := parseVersion(installed)
	if !ok {
		return VersionStatusUnknown, fmt.Sprintf("the installed version %q is not one this package can compare", installed)
	}
	pv, ok := parseVersion(published)
	if !ok {
		return VersionStatusUnknown, fmt.Sprintf("the published version %q is not one this package can compare", published)
	}
	if iv.prerelease != "" {
		return VersionStatusUnknown, fmt.Sprintf("the installed version %q is a prerelease, which tracks a different release stream than %s", installed, verdictStreamName(channel))
	}
	if pv.prerelease != "" {
		return VersionStatusUnknown, fmt.Sprintf("the published version %q is a prerelease, which is not the stream this install follows", published)
	}
	if compareVersion(iv, pv) < 0 {
		return VersionStatusBehind, ""
	}
	return VersionStatusCurrent, ""
}

func verdictStreamName(channel string) string {
	if channel == "" {
		return "the one that was queried"
	}
	return strconv.Quote(channel)
}

// parsedVersion is a semver-shaped version, split far enough to order two of
// them. Build metadata is discarded; it does not affect precedence.
type parsedVersion struct {
	major, minor, patch int
	prerelease          string
}

// parseVersion parses "0.149.1", "v0.149.1" or "0.149.0-alpha.4.3". It is
// deliberately strict about the numeric core: an unparseable version is not
// evidence of anything, least of all of being behind.
func parseVersion(s string) (parsedVersion, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return parsedVersion{}, false
	}
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}

	var v parsedVersion
	if i := strings.IndexByte(s, '-'); i >= 0 {
		v.prerelease = s[i+1:]
		s = s[:i]
		if v.prerelease == "" {
			return parsedVersion{}, false
		}
	}

	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return parsedVersion{}, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return parsedVersion{}, false
		}
		nums[i] = n
	}
	v.major, v.minor, v.patch = nums[0], nums[1], nums[2]
	return v, true
}

// compareVersion orders two release versions. Prereleases never reach it —
// versionVerdict refuses them before comparing — so only the numeric core is
// considered.
func compareVersion(a, b parsedVersion) int {
	for _, pair := range [][2]int{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if pair[0] != pair[1] {
			if pair[0] < pair[1] {
				return -1
			}
			return 1
		}
	}
	return 0
}
