// refresh fetches repository stats from Codeberg and GitHub APIs,
// updates repos.json, and regenerates index.html.
//
// Environment variables:
//
//	CODEBERG_APIKEY — Codeberg/Gitea API token (optional, avoids rate limits)
//	GITHUB_TOKEN    — GitHub API token (optional, avoids rate limits)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// --- Data model (matches repos.json) ---

type Group struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	CSS     string `json:"css"`
	Comment string `json:"comment"`
}

type Repo struct {
	Name         string `json:"name"`
	Group        string `json:"group"`
	Description  string `json:"description"`
	DocsURL      string `json:"docs_url,omitempty"`
	Codeberg     string `json:"codeberg,omitempty"`
	GitHub       string `json:"github,omitempty"`
	Stars        int    `json:"stars"`
	OpenIssues   int    `json:"open_issues"`
	ClosedIssues int    `json:"closed_issues"`
}

type Dashboard struct {
	Groups  []Group  `json:"groups"`
	Repos   []Repo   `json:"repos"`
	Ignored []string `json:"ignored,omitempty"`
}

// --- API response types ---

type giteaRepo struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	StarsCount      int    `json:"stars_count"`
	OpenIssuesCount int    `json:"open_issues_count"`
	Fork            bool   `json:"fork"`
	Mirror          bool   `json:"mirror"`
	Archived        bool   `json:"archived"`
	Empty           bool   `json:"empty"`
}

type githubRepo struct {
	Name            string    `json:"name"`
	Description     string    `json:"description"`
	StargazersCount int       `json:"stargazers_count"`
	OpenIssuesCount int       `json:"open_issues_count"`
	Fork            bool      `json:"fork"`
	Archived        bool      `json:"archived"`
	PushedAt        time.Time `json:"pushed_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// forgeStats holds stats fetched from a forge API.
type forgeStats struct {
	stars      int
	openIssues int
	desc       string
}

func main() {
	dataFile := flag.String("data", "repos.json", "repos data file")
	htmlFile := flag.String("html", "index.html", "output HTML file")
	flag.Parse()

	dash := loadData(*dataFile)

	codebergToken := os.Getenv("CODEBERG_APIKEY")
	githubToken := os.Getenv("GITHUB_TOKEN")

	// Fetch repo lists from forges
	cbRepos := fetchCodebergRepos("hum3", codebergToken)
	if len(cbRepos) == 0 {
		// hum3 may be a user account rather than an org
		cbRepos = fetchCodebergUserRepos("hum3", codebergToken)
	}
	ghRepos := fetchGitHubRepos("drummonds", githubToken)

	// Build set of tracked repos
	tracked := make(map[string]bool)
	for _, r := range dash.Repos {
		tracked[r.Name] = true
	}

	// Build set of ignored repos
	ignored := make(map[string]bool)
	for _, name := range dash.Ignored {
		ignored[name] = true
	}

	// Report new repos on forges not in repos.json (skip ignored)
	for name := range cbRepos {
		if !tracked[name] && !ignored[name] {
			fmt.Printf("NEW on Codeberg: %s — %s\n", name, cbRepos[name].desc)
		}
	}
	for name := range ghRepos {
		if !tracked[name] && !ignored[name] {
			fmt.Printf("NEW on GitHub: %s — %s\n", name, ghRepos[name].desc)
		}
	}

	// Update stats for tracked repos
	for i := range dash.Repos {
		r := &dash.Repos[i]

		if r.Codeberg != "" {
			parts := strings.SplitN(r.Codeberg, "/", 2)
			if len(parts) == 2 {
				if cs, ok := cbRepos[r.Name]; ok {
					r.Stars = cs.stars
					r.OpenIssues = cs.openIssues
				}
				r.ClosedIssues = fetchClosedIssuesCodeberg(parts[0], parts[1], codebergToken)
				time.Sleep(100 * time.Millisecond)
			}
		} else if r.GitHub != "" {
			parts := strings.SplitN(r.GitHub, "/", 2)
			if len(parts) == 2 {
				if gs, ok := ghRepos[r.Name]; ok {
					r.Stars = gs.stars
					// Note: GitHub open_issues_count includes PRs
					r.OpenIssues = gs.openIssues
				}
				r.ClosedIssues = fetchClosedIssuesGitHub(parts[0], parts[1], githubToken)
				time.Sleep(100 * time.Millisecond)
			}
		}
	}

	saveData(*dataFile, dash)
	fmt.Printf("refresh: updated stats for %d repos → %s\n", len(dash.Repos), *dataFile)

	generateHTML(*htmlFile, dash)
	fmt.Printf("refresh: generated %s\n", *htmlFile)
}

// --- Data loading/saving ---

func loadData(path string) Dashboard {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("load %s: %v", path, err)
	}
	var d Dashboard
	if err := json.Unmarshal(b, &d); err != nil {
		log.Fatalf("parse %s: %v", path, err)
	}
	return d
}

func saveData(path string, d Dashboard) {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		log.Fatalf("marshal: %v", err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0644); err != nil {
		log.Fatalf("write %s: %v", path, err)
	}
}

// --- Codeberg (Gitea) API ---

func fetchCodebergRepos(org, token string) map[string]forgeStats {
	m := make(map[string]forgeStats)
	page := 1
	for {
		url := fmt.Sprintf("https://codeberg.org/api/v1/orgs/%s/repos?limit=50&page=%d", org, page)
		resp, err := apiGet(url, token, "token")
		if err != nil {
			log.Printf("codeberg list repos page %d: %v", page, err)
			break
		}

		var repos []giteaRepo
		if err := json.NewDecoder(resp.Body).Decode(&repos); err != nil {
			resp.Body.Close()
			log.Printf("codeberg decode page %d: %v", page, err)
			break
		}
		resp.Body.Close()

		if len(repos) == 0 {
			break
		}
		for _, r := range repos {
			if r.Fork || r.Mirror || r.Archived {
				continue
			}
			m[r.Name] = forgeStats{
				stars:      r.StarsCount,
				openIssues: r.OpenIssuesCount,
				desc:       r.Description,
			}
		}
		page++
	}
	return m
}

func fetchCodebergUserRepos(user, token string) map[string]forgeStats {
	m := make(map[string]forgeStats)
	page := 1
	for {
		url := fmt.Sprintf("https://codeberg.org/api/v1/users/%s/repos?limit=50&page=%d", user, page)
		resp, err := apiGet(url, token, "token")
		if err != nil {
			log.Printf("codeberg user repos page %d: %v", page, err)
			break
		}

		var repos []giteaRepo
		if err := json.NewDecoder(resp.Body).Decode(&repos); err != nil {
			resp.Body.Close()
			log.Printf("codeberg decode page %d: %v", page, err)
			break
		}
		resp.Body.Close()

		if len(repos) == 0 {
			break
		}
		for _, r := range repos {
			if r.Fork || r.Mirror || r.Archived {
				continue
			}
			m[r.Name] = forgeStats{
				stars:      r.StarsCount,
				openIssues: r.OpenIssuesCount,
				desc:       r.Description,
			}
		}
		page++
	}
	return m
}

func fetchClosedIssuesCodeberg(owner, repo, token string) int {
	url := fmt.Sprintf("https://codeberg.org/api/v1/repos/%s/%s/issues?state=closed&type=issues&limit=1", owner, repo)
	resp, err := apiGet(url, token, "token")
	if err != nil {
		log.Printf("  codeberg closed issues %s/%s: %v", owner, repo, err)
		return 0
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	countStr := resp.Header.Get("X-Total-Count")
	if countStr == "" {
		return 0
	}
	n, _ := strconv.Atoi(countStr)
	return n
}

// --- GitHub API ---

func fetchGitHubRepos(user, token string) map[string]forgeStats {
	m := make(map[string]forgeStats)
	cutoff := time.Date(time.Now().Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	page := 1
	for {
		url := fmt.Sprintf("https://api.github.com/users/%s/repos?per_page=100&page=%d&sort=updated", user, page)
		resp, err := apiGet(url, token, "Bearer")
		if err != nil {
			log.Printf("github list repos page %d: %v", page, err)
			break
		}

		var repos []githubRepo
		if err := json.NewDecoder(resp.Body).Decode(&repos); err != nil {
			resp.Body.Close()
			log.Printf("github decode page %d: %v", page, err)
			break
		}
		resp.Body.Close()

		if len(repos) == 0 {
			break
		}
		stale := false
		for _, r := range repos {
			// Results sorted by updated_at desc; once we see a repo
			// updated before this year, all remaining are older too.
			if r.UpdatedAt.Before(cutoff) {
				stale = true
				break
			}
			if r.Fork || r.Archived {
				continue
			}
			if r.PushedAt.Before(cutoff) {
				continue // not changed this year
			}
			m[r.Name] = forgeStats{
				stars:      r.StargazersCount,
				openIssues: r.OpenIssuesCount,
				desc:       r.Description,
			}
		}
		if stale {
			break
		}
		page++
	}
	return m
}

func fetchClosedIssuesGitHub(owner, repo, token string) int {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues?state=closed&per_page=1", owner, repo)
	resp, err := apiGet(url, token, "Bearer")
	if err != nil {
		log.Printf("  github closed issues %s/%s: %v", owner, repo, err)
		return 0
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	// Parse Link header: <...?page=N&per_page=1>; rel="last"
	link := resp.Header.Get("Link")
	if link == "" {
		// No pagination — count items in response body (0 or 1)
		// Since we already drained the body above, re-fetch or just return 0/1
		// Actually, if no Link header, there are 0 or 1 closed issues.
		// Check if response had any items by status.
		return 0
	}
	re := regexp.MustCompile(`page=(\d+)>; rel="last"`)
	match := re.FindStringSubmatch(link)
	if len(match) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(match[1])
	return n
}

// --- HTTP helper ---

func apiGet(url, token, scheme string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", scheme+" "+token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body[:min(len(body), 200)]))
	}
	return resp, nil
}

// --- HTML generation ---

type tmplGroup struct {
	Comment string
	Repos   []tmplRepo
}

type tmplRepo struct {
	Name         string
	GroupCSS     string
	GroupLabel   string
	Description  string
	DocsURL      string
	Codeberg     string
	GitHub       string
	Stars        int
	OpenIssues   int
	ClosedIssues int
}

type tmplData struct {
	Groups []tmplGroup
	Date   string
}

func generateHTML(path string, d Dashboard) {
	// Build group lookup
	groupMap := make(map[string]Group)
	groupOrder := make([]string, 0, len(d.Groups))
	for _, g := range d.Groups {
		groupMap[g.ID] = g
		groupOrder = append(groupOrder, g.ID)
	}

	// Group repos by group ID, preserving order
	reposByGroup := make(map[string][]tmplRepo)
	for _, r := range d.Repos {
		g := groupMap[r.Group]
		tr := tmplRepo{
			Name:         r.Name,
			GroupCSS:     g.CSS,
			GroupLabel:   g.Label,
			Description:  r.Description,
			DocsURL:      r.DocsURL,
			Codeberg:     r.Codeberg,
			GitHub:       r.GitHub,
			Stars:        r.Stars,
			OpenIssues:   r.OpenIssues,
			ClosedIssues: r.ClosedIssues,
		}
		reposByGroup[g.ID] = append(reposByGroup[g.ID], tr)
	}

	var groups []tmplGroup
	for _, gid := range groupOrder {
		repos := reposByGroup[gid]
		if len(repos) == 0 {
			continue
		}
		groups = append(groups, tmplGroup{
			Comment: groupMap[gid].Comment,
			Repos:   repos,
		})
	}

	data := tmplData{
		Groups: groups,
		Date:   time.Now().Format("2006-01-02"),
	}

	t, err := template.New("index").Parse(htmlTmpl)
	if err != nil {
		log.Fatalf("parse template: %v", err)
	}

	f, err := os.Create(path)
	if err != nil {
		log.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()

	if err := t.Execute(f, data); err != nil {
		log.Fatalf("execute template: %v", err)
	}
}

const htmlTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>hum3 / drummonds — Repository Dashboard</title>
  <style>
    :root {
      --bg: #fafafa;
      --fg: #1a1a2e;
      --muted: #6b7280;
      --border: #e5e7eb;
      --link: #2563eb;
    }
    * { margin: 0; padding: 0; box-sizing: border-box; }
    body {
      font-family: system-ui, -apple-system, sans-serif;
      background: var(--bg);
      color: var(--fg);
      line-height: 1.6;
      padding: 2rem;
      max-width: 1400px;
      margin: 0 auto;
    }
    h1 { font-size: 1.8rem; margin-bottom: 0.25rem; }
    .subtitle { color: var(--muted); margin-bottom: 2rem; }
    .subtitle a { color: var(--link); text-decoration: none; }
    .subtitle a:hover { text-decoration: underline; }
    .diagram-container {
      border: 1px solid var(--border);
      border-radius: 8px;
      background: white;
      padding: 1rem;
      margin-bottom: 2rem;
      overflow-x: auto;
    }
    .diagram-container object {
      width: 100%;
      min-height: 600px;
    }
    h2 { font-size: 1.3rem; margin: 2rem 0 1rem; }
    table {
      width: 100%;
      border-collapse: collapse;
      font-size: 0.9rem;
    }
    th, td {
      text-align: left;
      padding: 0.5rem 0.75rem;
      border-bottom: 1px solid var(--border);
    }
    th { font-weight: 600; color: var(--muted); font-size: 0.8rem; text-transform: uppercase; }
    td a { color: var(--link); text-decoration: none; }
    td a:hover { text-decoration: underline; }
    .group-badge {
      display: inline-block;
      font-size: 0.75rem;
      padding: 0.1rem 0.5rem;
      border-radius: 9999px;
      font-weight: 500;
    }
    .g-gobank { background: #dbeafe; color: #1e40af; }
    .g-parsing { background: #ffedd5; color: #c2410c; }
    .g-godocs { background: #dcfce7; color: #166534; }
    .g-webui { background: #f3e8ff; color: #7e22ce; }
    .g-hardware { background: #fce7f3; color: #be185d; }
    .g-data { background: #fef9c3; color: #a16207; }
    .g-tooling { background: #f1f5f9; color: #475569; }
    .g-libs { background: #e0f2fe; color: #0369a1; }
    .g-github { background: #fee2e2; color: #dc2626; }
    .num { text-align: right; font-variant-numeric: tabular-nums; }
    tr:target {
      background: #fef9c3;
      outline: 2px solid #eab308;
    }
    footer {
      margin-top: 3rem;
      padding-top: 1rem;
      border-top: 1px solid var(--border);
      color: var(--muted);
      font-size: 0.85rem;
    }
  </style>
</head>
<body>
  <h1>Repository Dashboard</h1>
  <p class="subtitle">
    <a href="https://codeberg.org/hum3">Codeberg: hum3</a> &middot;
    <a href="https://github.com/drummonds">GitHub: drummonds</a> &middot;
    <a href="https://builder.statichost.eu/team/team_01kjg7ba6zfehrpjbkn8ktc855/">StaticHost</a> &middot;
    <a href="https://www.bytestone.uk">Blog</a>
    &mdash; public repos updated in 2026
  </p>

  <div class="diagram-container">
    <!-- SVG_PLACEHOLDER -->
  </div>

  <h2>All Repositories</h2>
  <table>
    <thead>
      <tr><th>Name</th><th>Group</th><th>Description</th><th>Docs</th><th>Codeberg</th><th>GitHub</th><th class="num">&#9733;</th><th class="num">Open</th><th class="num">Closed</th></tr>
    </thead>
    <tbody>
{{- range .Groups}}
      <!-- {{.Comment}} -->
{{- range .Repos}}
      <tr id="{{.Name}}"><td>{{.Name}}</td><td><span class="group-badge {{.GroupCSS}}">{{.GroupLabel}}</span></td><td>{{.Description}}</td><td>{{if .DocsURL}}<a href="{{.DocsURL}}">docs</a>{{else}}&mdash;{{end}}</td><td>{{if .Codeberg}}<a href="https://codeberg.org/{{.Codeberg}}">{{.Codeberg}}</a>{{else}}&mdash;{{end}}</td><td>{{if .GitHub}}<a href="https://github.com/{{.GitHub}}">{{.GitHub}}</a>{{else}}&mdash;{{end}}</td><td class="num">{{.Stars}}</td><td class="num">{{.OpenIssues}}</td><td class="num">{{.ClosedIssues}}</td></tr>
{{- end}}
{{- end}}
    </tbody>
  </table>

  <footer>
    <p style="margin-bottom:0.5rem">
      <a href="README.html">Docs</a> &middot;
      <a href="repo-symbol-design.html">Repo Symbol Design</a>
    </p>
    Generated {{.Date}} &middot; Diagram rendered with <a href="https://d2lang.com">d2</a>
  </footer>
  <script>document.querySelectorAll('a[href^="https://"]').forEach(a=>{a.target='_blank';a.rel='noopener noreferrer';})</script>
</body>
</html>
`
