// Command refresh-stats rewrites the numeric badges in ../../index.html
// (stars, merged-PR counts, npm downloads, and the trailing-12-month
// activity block) from live GitHub and npm data. Run from this directory.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const htmlPath = "../../index.html"

const ghLogin = "IDisposable"

var httpClient = &http.Client{Timeout: 15 * time.Second}

func ghToken() string {
	return os.Getenv("GITHUB_TOKEN")
}

func doJSON(req *http.Request, out interface{}) error {
	if strings.Contains(req.URL.Host, "github.com") {
		req.Header.Set("Accept", "application/vnd.github+json")
		if tok := ghToken(); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	req.Header.Set("User-Agent", "idisposable-stats-refresh")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d: %s", req.URL, resp.StatusCode, string(body))
	}
	return json.Unmarshal(body, out)
}

type repoRef struct{ owner, repo string }

func (r repoRef) String() string { return r.owner + "/" + r.repo }

func repoStars(r repoRef) (int, error) {
	var out struct {
		StargazersCount int `json:"stargazers_count"`
	}
	req, _ := http.NewRequest("GET", fmt.Sprintf("https://api.github.com/repos/%s/%s", r.owner, r.repo), nil)
	err := doJSON(req, &out)
	return out.StargazersCount, err
}

func searchCount(query string) (int, error) {
	var out struct {
		TotalCount int `json:"total_count"`
	}
	req, _ := http.NewRequest("GET", "https://api.github.com/search/issues?q="+url.QueryEscape(query), nil)
	err := doJSON(req, &out)
	return out.TotalCount, err
}

func mergedPRs(r repoRef, author string) (int, error) {
	return searchCount(fmt.Sprintf("repo:%s/%s is:pr is:merged author:%s", r.owner, r.repo, author))
}

func userInfo(login string) (followers, publicRepos int, err error) {
	var out struct {
		Followers   int `json:"followers"`
		PublicRepos int `json:"public_repos"`
	}
	req, _ := http.NewRequest("GET", "https://api.github.com/users/"+login, nil)
	err = doJSON(req, &out)
	return out.Followers, out.PublicRepos, err
}

func npmDownloadsLastMonth(pkg string) (int, error) {
	var out struct {
		Downloads int `json:"downloads"`
	}
	req, _ := http.NewRequest("GET", "https://api.npmjs.org/downloads/point/last-month/"+url.PathEscape(pkg), nil)
	err := doJSON(req, &out)
	return out.Downloads, err
}

type contribRepo struct {
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
}

type contributions struct {
	TotalCommitContributions            int           `json:"totalCommitContributions"`
	TotalPullRequestReviewContributions int           `json:"totalPullRequestReviewContributions"`
	CommitByRepo                        []contribRepo `json:"commitContributionsByRepository"`
	PRByRepo                            []contribRepo `json:"pullRequestContributionsByRepository"`
	ReviewByRepo                        []contribRepo `json:"pullRequestReviewContributionsByRepository"`
}

// yearlyContributions reports the login's public contribution activity for
// the trailing twelve months, via GitHub's contributionsCollection GraphQL
// field (private-repo activity is excluded unless the caller authenticates
// as that user, so this naturally matches "public activity only").
func yearlyContributions(login string) (contributions, error) {
	now := time.Now().UTC()
	from := now.AddDate(-1, 0, 0)
	const q = `query($login:String!,$from:DateTime!,$to:DateTime!){
		user(login:$login){
			contributionsCollection(from:$from, to:$to){
				totalCommitContributions
				totalPullRequestReviewContributions
				commitContributionsByRepository(maxRepositories:100){repository{nameWithOwner}}
				pullRequestContributionsByRepository(maxRepositories:100){repository{nameWithOwner}}
				pullRequestReviewContributionsByRepository(maxRepositories:100){repository{nameWithOwner}}
			}
		}
	}`
	payload := map[string]interface{}{
		"query": q,
		"variables": map[string]string{
			"login": login,
			"from":  from.Format(time.RFC3339),
			"to":    now.Format(time.RFC3339),
		},
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", "https://api.github.com/graphql", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	var out struct {
		Data struct {
			User struct {
				ContributionsCollection contributions `json:"contributionsCollection"`
			} `json:"user"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := doJSON(req, &out); err != nil {
		return contributions{}, err
	}
	if len(out.Errors) > 0 {
		return contributions{}, fmt.Errorf("graphql: %s", out.Errors[0].Message)
	}
	return out.Data.User.ContributionsCollection, nil
}

func reposTouched(c contributions) int {
	seen := map[string]bool{}
	for _, list := range [][]contribRepo{c.CommitByRepo, c.PRByRepo, c.ReviewByRepo} {
		for _, r := range list {
			seen[r.Repository.NameWithOwner] = true
		}
	}
	return len(seen)
}

// formatK renders a count the way the page's badges already do: plain
// below 1000, one decimal place plus "k" or "M" above it (57000 -> "57k").
func formatK(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 1_000_000:
		return trimZero(float64(n)/1000) + "k"
	default:
		return trimZero(float64(n)/1_000_000) + "M"
	}
}

func trimZero(v float64) string {
	s := strconv.FormatFloat(v, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}

func formatComma(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var groups []string
	for len(s) > 3 {
		groups = append([]string{s[len(s)-3:]}, groups...)
		s = s[:len(s)-3]
	}
	groups = append([]string{s}, groups...)
	out := strings.Join(groups, ",")
	if neg {
		out = "-" + out
	}
	return out
}

// window returns the slice of html from anchor up to (excluding) the next
// "</div>", the smallest region that reliably contains one badge without
// bleeding into neighboring cards.
func window(html, anchor string) (start, end int, ok bool) {
	idx := strings.Index(html, anchor)
	if idx < 0 {
		return 0, 0, false
	}
	rel := strings.Index(html[idx:], "</div>")
	if rel < 0 {
		return 0, 0, false
	}
	return idx, idx + rel, true
}

func replaceInWindow(html, anchor string, re *regexp.Regexp, repl string) (string, error) {
	start, end, ok := window(html, anchor)
	if !ok {
		return html, fmt.Errorf("anchor not found: %q", anchor)
	}
	seg := html[start:end]
	if !re.MatchString(seg) {
		return html, fmt.Errorf("pattern %s not found near %q", re.String(), anchor)
	}
	return html[:start] + re.ReplaceAllString(seg, repl) + html[end:], nil
}

var (
	leadingStarRe  = regexp.MustCompile(`★\s*[\d,.]+[kM]?`)
	trailingStarRe = regexp.MustCompile(`[\d,.]+[kM]?★`)
	mergedRe       = regexp.MustCompile(`\d+ merged`)
	downloadsRe    = regexp.MustCompile(`↓\s*[\d,.]+[kM]?/mo`)
	heroStarRe     = regexp.MustCompile(`[\d,.]+[kM]?-star`)
	timesAMonthRe  = regexp.MustCompile(`[\d,]+ times a month`)
)

func setLeadingStar(html, anchor string, stars int) (string, error) {
	return replaceInWindow(html, anchor, leadingStarRe, "★ "+formatK(stars))
}

func setTrailingStar(html, anchor string, stars int) (string, error) {
	return replaceInWindow(html, anchor, trailingStarRe, formatK(stars)+"★")
}

func setMerged(html, anchor string, merged int) (string, error) {
	return replaceInWindow(html, anchor, mergedRe, fmt.Sprintf("%d merged", merged))
}

func setDownloads(html, anchor string, downloads int) (string, error) {
	return replaceInWindow(html, anchor, downloadsRe, "↓ "+formatK(downloads)+"/mo")
}

func setHeroStar(html string, stars int) (string, error) {
	if !heroStarRe.MatchString(html) {
		return html, fmt.Errorf("hero star phrase not found")
	}
	return heroStarRe.ReplaceAllString(html, formatK(stars)+"-star"), nil
}

func setTimesAMonth(html string, n int) (string, error) {
	if !timesAMonthRe.MatchString(html) {
		return html, fmt.Errorf(`"times a month" phrase not found`)
	}
	return timesAMonthRe.ReplaceAllString(html, formatComma(n)+" times a month"), nil
}

func setStatN(html, label string, value int) (string, error) {
	re := regexp.MustCompile(`<span class="n">\d+</span><span class="l">` + regexp.QuoteMeta(label) + `</span>`)
	if !re.MatchString(html) {
		return html, fmt.Errorf("stat label not found: %s", label)
	}
	repl := fmt.Sprintf(`<span class="n">%d</span><span class="l">%s</span>`, value, label)
	return re.ReplaceAllString(html, repl), nil
}

func href(r repoRef) string {
	return fmt.Sprintf(`href="https://github.com/%s/%s"`, r.owner, r.repo)
}

func main() {
	raw, err := os.ReadFile(htmlPath)
	if err != nil {
		log.Fatalf("read %s: %v", htmlPath, err)
	}
	html := string(raw)

	apply := func(name string, fn func(string) (string, error)) {
		next, err := fn(html)
		if err != nil {
			log.Printf("skip %s: %v", name, err)
			return
		}
		html = next
	}

	starRepos := []repoRef{
		{"IDisposable", "jellyfin-plugin-mindthegaps"},
		{"IDisposable", "jellyfin-plugin-subtitleocr"},
		{"IDisposable", "jellyfin-plugin-justwatch"},
		{"jellyfin", "jellyfin"},
		{"jetkvm", "kvm"},
		{"rs", "zerolog"},
		{"dotnet", "runtime"},
		{"IDisposable", "dom-to-image-more"},
		{"IDisposable", "cloudfinger"},
		{"IDisposable", "IFilterExtractor"},
		{"IDisposable", "Dynamic"},
		{"IDisposable", "claude-transcript-plugin"},
		{"microsoft", "coreutils"},
	}
	stars := map[repoRef]int{}
	for _, r := range starRepos {
		n, err := repoStars(r)
		if err != nil {
			log.Printf("stars %s: %v", r, err)
			continue
		}
		stars[r] = n
	}

	// Card badges written as "★ N".
	leadingStarTargets := []repoRef{
		{"IDisposable", "jellyfin-plugin-mindthegaps"},
		{"IDisposable", "jellyfin-plugin-subtitleocr"},
		{"IDisposable", "jellyfin-plugin-justwatch"},
		{"IDisposable", "dom-to-image-more"},
		{"IDisposable", "cloudfinger"},
		{"IDisposable", "IFilterExtractor"},
		{"IDisposable", "Dynamic"},
		{"IDisposable", "claude-transcript-plugin"},
		{"microsoft", "coreutils"},
	}
	for _, r := range leadingStarTargets {
		n, ok := stars[r]
		if !ok {
			continue
		}
		anchor := href(r)
		apply("stars "+r.String(), func(h string) (string, error) { return setLeadingStar(h, anchor, n) })
	}

	// Contribution-row badges written as "Nk★".
	for _, r := range []repoRef{{"rs", "zerolog"}, {"dotnet", "runtime"}} {
		n, ok := stars[r]
		if !ok {
			continue
		}
		anchor := href(r)
		apply("stars "+r.String(), func(h string) (string, error) { return setTrailingStar(h, anchor, n) })
	}

	// Module-head tags (no href of their own; located by their static prefix).
	if n, ok := stars[repoRef{"jellyfin", "jellyfin"}]; ok {
		apply("jellyfin module tag stars", func(h string) (string, error) {
			return setTrailingStar(h, "open-source media server · ", n)
		})
		apply("hero jellyfin star mention", func(h string) (string, error) { return setHeroStar(h, n) })
	}
	if n, ok := stars[repoRef{"jetkvm", "kvm"}]; ok {
		apply("jetkvm module tag stars", func(h string) (string, error) {
			return setTrailingStar(h, "open-source remote-KVM hardware · ", n)
		})
	}

	// Merged-PR counts, scoped to IDisposable as author.
	for _, r := range []repoRef{{"jellyfin", "jellyfin"}, {"jetkvm", "kvm"}, {"rs", "zerolog"}, {"dotnet", "runtime"}} {
		n, err := mergedPRs(r, ghLogin)
		if err != nil {
			log.Printf("merged %s: %v", r, err)
			continue
		}
		anchor := href(r)
		apply("merged "+r.String(), func(h string) (string, error) { return setMerged(h, anchor, n) })
	}

	// npm downloads, badge plus the two matching prose mentions.
	if dl, err := npmDownloadsLastMonth("dom-to-image-more"); err != nil {
		log.Printf("npm downloads: %v", err)
	} else {
		anchor := href(repoRef{"IDisposable", "dom-to-image-more"})
		apply("dom-to-image-more downloads badge", func(h string) (string, error) { return setDownloads(h, anchor, dl) })
		apply("hero/meta downloads mention", func(h string) (string, error) { return setTimesAMonth(h, dl) })
	}

	// User-level stat grid.
	if followers, publicRepos, err := userInfo(ghLogin); err != nil {
		log.Printf("user info: %v", err)
	} else {
		apply("followers stat", func(h string) (string, error) { return setStatN(h, "Followers", followers) })
		apply("public repos stat", func(h string) (string, error) { return setStatN(h, "Public repos", publicRepos) })
	}

	since := time.Now().UTC().AddDate(-1, 0, 0).Format("2006-01-02")
	if merged12mo, err := searchCount(fmt.Sprintf("is:pr is:merged author:%s merged:>=%s", ghLogin, since)); err != nil {
		log.Printf("PRs merged (12mo): %v", err)
	} else {
		apply("PRs merged stat", func(h string) (string, error) { return setStatN(h, "PRs merged", merged12mo) })
	}

	if c, err := yearlyContributions(ghLogin); err != nil {
		log.Printf("contributions: %v", err)
	} else {
		apply("commits stat", func(h string) (string, error) { return setStatN(h, "Commits", c.TotalCommitContributions) })
		apply("reviews given stat", func(h string) (string, error) {
			return setStatN(h, "Reviews given", c.TotalPullRequestReviewContributions)
		})
		apply("repos touched stat", func(h string) (string, error) { return setStatN(h, "Repos touched", reposTouched(c)) })
	}

	if html == string(raw) {
		log.Println("no changes")
		return
	}
	if err := os.WriteFile(htmlPath, []byte(html), 0o644); err != nil {
		log.Fatalf("write %s: %v", htmlPath, err)
	}
	log.Println("index.html updated")
}
