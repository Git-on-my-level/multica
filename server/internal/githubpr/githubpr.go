package githubpr

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type Reference struct {
	Owner  string
	Repo   string
	Number int32
	URL    string
}

var urlCandidateRE = regexp.MustCompile(`https://github\.com/[^\s<>()\[\]{}"'` + "`" + `]+`)
var ownerRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
var repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ParseCanonical parses the only PR URL shape Multica accepts for handoff:
// https://github.com/{owner}/{repo}/pull/{number}. Query strings, fragments,
// and one trailing slash are allowed; credentials, ports, and path suffixes
// such as /files are rejected.
func ParseCanonical(raw string) (Reference, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil {
		return Reference{}, false
	}
	escapedPath := u.EscapedPath()
	if !strings.HasPrefix(escapedPath, "/") || strings.Contains(escapedPath, "%") ||
		strings.Contains(escapedPath, "//") {
		return Reference{}, false
	}
	escapedPath = strings.TrimSuffix(escapedPath, "/")
	segs := strings.Split(strings.TrimPrefix(escapedPath, "/"), "/")
	if len(segs) != 4 || !strings.EqualFold(segs[2], "pull") {
		return Reference{}, false
	}
	n, err := strconv.ParseInt(segs[3], 10, 32)
	if err != nil || n <= 0 {
		return Reference{}, false
	}
	owner, repo := segs[0], segs[1]
	if !ownerRE.MatchString(owner) || !repoRE.MatchString(repo) ||
		strings.HasSuffix(strings.ToLower(repo), ".git") {
		return Reference{}, false
	}
	canonical := "https://github.com/" + owner + "/" + repo + "/pull/" + strconv.FormatInt(n, 10)
	return Reference{Owner: owner, Repo: repo, Number: int32(n), URL: canonical}, true
}

// ExtractCanonical returns distinct, unambiguous canonical PR URLs from agent
// output. The broad candidate scan deliberately includes suffixes so
// https://github.com/o/r/pull/1/files is rejected by ParseCanonical instead of
// being truncated into an apparently valid handoff.
func ExtractCanonical(text string) []Reference {
	seen := map[string]struct{}{}
	out := make([]Reference, 0)
	for _, raw := range urlCandidateRE.FindAllString(text, -1) {
		raw = strings.TrimRight(raw, ".,;:!?")
		ref, ok := ParseCanonical(raw)
		if !ok {
			continue
		}
		key := strings.ToLower(ref.URL)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ref)
	}
	return out
}

// RepositoryFromRemote normalizes GitHub HTTPS/SSH/scp clone URLs to the
// lower-case owner/repo key used for candidate ownership checks.
func RepositoryFromRemote(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	var path string
	lower := strings.ToLower(raw)
	if i := strings.Index(lower, "github.com:"); i > 0 && !strings.Contains(raw[:i], "://") {
		userHost := raw[:i+len("github.com")]
		at := strings.LastIndex(userHost, "@")
		host := userHost
		if at >= 0 {
			host = userHost[at+1:]
		}
		if !strings.EqualFold(host, "github.com") {
			return "", false
		}
		path = raw[i+len("github.com:"):]
	} else {
		u, err := url.Parse(raw)
		if err != nil || !strings.EqualFold(u.Hostname(), "github.com") ||
			(u.Scheme != "https" && u.Scheme != "ssh") ||
			u.RawQuery != "" || u.Fragment != "" {
			return "", false
		}
		if u.Scheme == "https" && u.User != nil {
			return "", false
		}
		path = strings.TrimPrefix(u.Path, "/")
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	segs := strings.Split(path, "/")
	if len(segs) != 2 || !ownerRE.MatchString(segs[0]) || !repoRE.MatchString(segs[1]) {
		return "", false
	}
	return strings.ToLower(segs[0] + "/" + segs[1]), true
}
