// svgpost post-processes a d2-generated SVG to make appendix icons clickable.
//
// For each repo node d2 emits two appendix-icon <g> elements:
//   - chain icon (no <title>) → wrapped with link to origin forge
//   - info icon (has <title>)  → wrapped with link to docs page
//
// URLs are extracted from the HTML table in index.html.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
)

type repoInfo struct {
	DocsURL     string
	CodebergURL string
	GitHubURL   string
}

func main() {
	svgPath := flag.String("svg", "docs/dashboard.svg", "path to d2-generated SVG")
	htmlPath := flag.String("html", "index.html", "path to index.html with link table")
	flag.Parse()

	urls := parseHTML(*htmlPath)
	nodeOrder := parseSVGNodeOrder(*svgPath)
	svgData := mustRead(*svgPath)
	result := linkIcons(svgData, nodeOrder, urls)
	mustWrite(*svgPath, result)

	fmt.Printf("svgpost: linked %d icon pairs in %s\n", len(nodeOrder), *svgPath)
}

// parseHTML extracts per-repo URLs from the <tr id="name"> rows in index.html.
func parseHTML(path string) map[string]repoInfo {
	data := mustRead(path)
	m := make(map[string]repoInfo)

	// Match <tr id="NAME">...<td>...<a href="DOCS">...
	trRe := regexp.MustCompile(`<tr id="([^"]+)">(.*?)</tr>`)
	hrefRe := regexp.MustCompile(`<a href="([^"]+)">`)

	for _, match := range trRe.FindAllStringSubmatch(data, -1) {
		name := match[1]
		row := match[2]

		// Split into <td> cells
		cells := splitTDs(row)
		if len(cells) < 6 {
			continue
		}

		info := repoInfo{}

		// Cell 3 (0-indexed) = Docs
		if hrefs := hrefRe.FindStringSubmatch(cells[3]); hrefs != nil {
			info.DocsURL = hrefs[1]
		}
		// Cell 4 = Codeberg
		if hrefs := hrefRe.FindStringSubmatch(cells[4]); hrefs != nil {
			info.CodebergURL = hrefs[1]
		}
		// Cell 5 = GitHub
		if hrefs := hrefRe.FindStringSubmatch(cells[5]); hrefs != nil {
			info.GitHubURL = hrefs[1]
		}

		m[name] = info
	}

	return m
}

// splitTDs splits an HTML row into its <td>...</td> segments.
func splitTDs(row string) []string {
	var cells []string
	for {
		start := strings.Index(row, "<td>")
		if start == -1 {
			break
		}
		end := strings.Index(row[start:], "</td>")
		if end == -1 {
			break
		}
		cells = append(cells, row[start:start+end+5])
		row = row[start+end+5:]
	}
	return cells
}

// parseSVGNodeOrder returns repo names in the order their <a href="#name"> nodes
// appear in the SVG.
func parseSVGNodeOrder(path string) []string {
	data := mustRead(path)
	re := regexp.MustCompile(`<a href="#([^"]+)"`)
	matches := re.FindAllStringSubmatch(data, -1)

	var names []string
	for _, m := range matches {
		names = append(names, m[1])
	}
	return names
}

// linkIcons wraps each appendix-icon <g> in an <a> tag pointing to the
// appropriate URL based on whether it's a chain or info icon.
func linkIcons(svg string, nodeOrder []string, urls map[string]repoInfo) string {
	const marker = `class="appendix-icon"`

	// Icons come in pairs: info (has <title>), chain (no <title>), for each node.
	// Find all icon positions.
	type iconPos struct {
		start int // start of <g transform=...
		end   int // position after closing </g>
	}

	var icons []iconPos
	searchFrom := 0
	for {
		idx := strings.Index(svg[searchFrom:], marker)
		if idx == -1 {
			break
		}
		abs := searchFrom + idx

		// Walk back to find the start of this <g
		gStart := strings.LastIndex(svg[:abs], "<g ")
		if gStart == -1 {
			searchFrom = abs + len(marker)
			continue
		}

		// Find matching </g> by tracking nesting depth.
		gEnd := findClosingG(svg, abs)
		if gEnd == -1 {
			searchFrom = abs + len(marker)
			continue
		}

		icons = append(icons, iconPos{start: gStart, end: gEnd})
		searchFrom = gEnd
	}

	if len(icons) != len(nodeOrder)*2 {
		log.Printf("svgpost: warning: expected %d icons, found %d", len(nodeOrder)*2, len(icons))
	}

	// Process from end to start so indices stay valid.
	for i := len(icons) - 1; i >= 0; i-- {
		pairIdx := i / 2 // which repo
		if pairIdx >= len(nodeOrder) {
			continue
		}
		name := nodeOrder[pairIdx]
		info, ok := urls[name]
		if !ok {
			continue
		}

		isInfoIcon := (i % 2) == 0 // first of each pair is info icon
		fragment := svg[icons[i].start:icons[i].end]

		var url string
		if isInfoIcon {
			// Info icon → docs
			if hasTitle(fragment) {
				url = info.DocsURL
			}
		} else {
			// Chain icon → origin forge (Codeberg preferred, GitHub fallback)
			if !hasTitle(fragment) {
				if info.CodebergURL != "" {
					url = info.CodebergURL
				} else {
					url = info.GitHubURL
				}
			}
		}

		// Swap assignment if our assumption about ordering is wrong.
		// d2 emits info first, then chain. But let's verify by checking <title>.
		if isInfoIcon && !hasTitle(fragment) {
			// This is actually the chain icon — d2 order might vary.
			// Re-check: if this one has no <title>, it's chain; use forge URL.
			if info.CodebergURL != "" {
				url = info.CodebergURL
			} else {
				url = info.GitHubURL
			}
		} else if !isInfoIcon && hasTitle(fragment) {
			// This is actually the info icon.
			url = info.DocsURL
		}

		if url == "" {
			continue
		}

		wrapped := fmt.Sprintf(`<a href="%s" xlink:href="%s" target="_blank">%s</a>`,
			url, url, fragment)
		svg = svg[:icons[i].start] + wrapped + svg[icons[i].end:]
	}

	return svg
}

// hasTitle checks if a fragment contains a <title> element.
func hasTitle(fragment string) bool {
	return strings.Contains(fragment, "<title>")
}

// findClosingG finds the position after the closing </g> that matches the <g>
// containing the marker at position markerPos.
func findClosingG(svg string, markerPos int) int {
	// Start scanning from the '<g' that contains the marker.
	gStart := strings.LastIndex(svg[:markerPos], "<g ")
	if gStart == -1 {
		return -1
	}

	depth := 0
	pos := gStart
	for pos < len(svg) {
		// Look for next <g or </g>
		nextOpen := strings.Index(svg[pos:], "<g")
		nextClose := strings.Index(svg[pos:], "</g>")

		if nextClose == -1 {
			return -1
		}

		if nextOpen != -1 && pos+nextOpen < pos+nextClose {
			// Check it's actually a <g tag and not <glyph etc.
			absOpen := pos + nextOpen
			if absOpen+2 < len(svg) {
				after := svg[absOpen+2]
				if after == ' ' || after == '>' || after == '\n' || after == '\t' {
					depth++
					pos = absOpen + 2
					continue
				}
			}
			// Not a <g> tag, skip past it.
			pos = absOpen + 2
			continue
		}

		// Found </g>
		depth--
		absClose := pos + nextClose + 4 // past "</g>"
		if depth == 0 {
			return absClose
		}
		pos = absClose
	}
	return -1
}

func mustRead(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func mustWrite(path, data string) {
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		log.Fatalf("write %s: %v", path, err)
	}
}
