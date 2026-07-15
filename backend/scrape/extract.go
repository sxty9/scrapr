package scrape

import (
	"bytes"
	neturl "net/url"
	"path"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// candidate is one enumerable target the LLM may pick by index: a follow-link or an asset.
type candidate struct {
	URL    string
	Anchor string
}

// pageData is a parsed page: its title, readable markdown, and the two candidate lists the
// decision prompt enumerates (links to follow, assets to download).
type pageData struct {
	URL       string
	Title     string
	Markdown  string
	Links     []candidate
	Downloads []candidate
}

const (
	maxLinks       = 150
	maxDownloads   = 80
	maxMarkdownLen = 24 << 10
)

// parseHTML walks the document once, extracting title + readable markdown and collecting
// follow-links (allowlisted pages) and download candidates (assets on public hosts). Relative
// URLs resolve against the page's final (post-redirect) URL. allowPrivate relaxes the asset
// host check (see isPublicHTTP).
func parseHTML(fetched *Fetched, allow *allowlist, allowPrivate bool) (*pageData, error) {
	root, err := html.Parse(bytes.NewReader(fetched.Body))
	if err != nil {
		return nil, err
	}
	base, err := neturl.Parse(fetched.FinalURL)
	if err != nil {
		return nil, err
	}

	pd := &pageData{URL: fetched.FinalURL}
	var md strings.Builder
	links := map[string]candidate{}
	linkOrder := []string{}
	downloads := map[string]candidate{}
	downloadOrder := []string{}

	addDownload := func(abs, anchor string) {
		if _, ok := downloads[abs]; !ok && len(downloadOrder) < maxDownloads {
			downloads[abs] = candidate{URL: abs, Anchor: clean(anchor)}
			downloadOrder = append(downloadOrder, abs)
		}
	}
	addLink := func(raw, anchor string) {
		abs := resolve(base, raw)
		if abs == "" {
			return
		}
		if looksDownloadable(abs) {
			if isPublicHTTP(abs, allowPrivate) {
				addDownload(abs, anchor)
			}
			return
		}
		if allow.allowedURL(abs) {
			if _, ok := links[abs]; !ok && len(linkOrder) < maxLinks {
				links[abs] = candidate{URL: abs, Anchor: clean(anchor)}
				linkOrder = append(linkOrder, abs)
			}
		}
	}
	addAsset := func(raw, anchor string) {
		abs := resolve(base, raw)
		if abs == "" || !isPublicHTTP(abs, allowPrivate) {
			return
		}
		addDownload(abs, anchor)
	}

	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			if t := strings.TrimSpace(n.Data); t != "" {
				md.WriteString(n.Data)
			}
			return
		}
		if n.Type != html.ElementNode {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			return
		}
		switch n.DataAtom {
		case atom.Script, atom.Style, atom.Noscript, atom.Template, atom.Svg, atom.Head,
			atom.Nav, atom.Header, atom.Footer, atom.Aside:
			return // skip non-content subtrees + navigational chrome (menus, sidebars, footers)
		case atom.Title:
			if pd.Title == "" {
				pd.Title = clean(textOf(n))
			}
			return
		case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
			level := int(n.DataAtom - atom.H1 + 1)
			md.WriteString("\n\n" + strings.Repeat("#", level) + " ")
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			md.WriteString("\n\n")
			return
		case atom.Br:
			md.WriteString("\n")
		case atom.P, atom.Li, atom.Blockquote, atom.Figcaption, atom.Dd, atom.Dt, atom.Tr:
			md.WriteString("\n")
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			md.WriteString("\n")
			return
		case atom.A:
			if href := attr(n, "href"); href != "" {
				addLink(href, textOf(n))
			}
		case atom.Img:
			addAsset(attr(n, "src"), attr(n, "alt"))
		case atom.Source, atom.Video, atom.Audio, atom.Embed:
			addAsset(attr(n, "src"), "")
		case atom.Object:
			addAsset(attr(n, "data"), "")
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)

	if pd.Title == "" {
		pd.Title = titleFromURL(fetched.FinalURL)
	}
	pd.Markdown = collapse(md.String())
	if len(pd.Markdown) > maxMarkdownLen {
		pd.Markdown = pd.Markdown[:maxMarkdownLen]
	}
	for _, k := range linkOrder {
		pd.Links = append(pd.Links, links[k])
	}
	for _, k := range downloadOrder {
		pd.Downloads = append(pd.Downloads, downloads[k])
	}
	return pd, nil
}

// resolve makes href absolute against base, returning "" for non-navigational schemes.
func resolve(base *neturl.URL, href string) string {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") {
		return ""
	}
	low := strings.ToLower(href)
	for _, p := range []string{"javascript:", "mailto:", "tel:", "data:", "blob:"} {
		if strings.HasPrefix(low, p) {
			return ""
		}
	}
	u, err := neturl.Parse(href)
	if err != nil {
		return ""
	}
	abs := base.ResolveReference(u)
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return ""
	}
	abs.Fragment = ""
	return abs.String()
}

var downloadableExt = map[string]bool{
	".pdf": true, ".doc": true, ".docx": true, ".ppt": true, ".pptx": true,
	".xls": true, ".xlsx": true, ".csv": true, ".txt": true, ".md": true,
	".rtf": true, ".odt": true, ".ods": true, ".odp": true, ".epub": true,
	".zip": true, ".tar": true, ".gz": true, ".7z": true, ".rar": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".svg": true, ".bmp": true, ".tiff": true, ".ico": true,
	".mp4": true, ".webm": true, ".mov": true, ".avi": true, ".mkv": true,
	".mp3": true, ".wav": true, ".ogg": true, ".flac": true, ".m4a": true,
	".json": true, ".xml": true,
}

// looksDownloadable reports whether raw's path has a file extension we treat as an asset
// (document/media) rather than a navigable page.
func looksDownloadable(raw string) bool {
	u, err := neturl.Parse(raw)
	if err != nil {
		return false
	}
	return downloadableExt[strings.ToLower(path.Ext(u.Path))]
}

// titleFromURL derives a human-ish title from a URL's last path segment.
func titleFromURL(raw string) string {
	u, err := neturl.Parse(raw)
	if err != nil {
		return raw
	}
	seg := path.Base(u.Path)
	if seg == "" || seg == "/" || seg == "." {
		return u.Host
	}
	if dec, err := neturl.PathUnescape(seg); err == nil {
		seg = dec
	}
	seg = strings.ReplaceAll(seg, "_", " ")
	return seg
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// textOf returns the concatenated text of a node subtree (skipping script/style).
func textOf(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			return
		}
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.Script, atom.Style, atom.Noscript:
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return b.String()
}

var (
	wsRun    = regexp.MustCompile(`[ \t]+`)
	blankRun = regexp.MustCompile(`\n[ \t]*\n[ \t]*(\n[ \t]*)+`)
)

// clean collapses inline whitespace in a short string (titles, anchors).
func clean(s string) string {
	return strings.TrimSpace(wsRun.ReplaceAllString(strings.ReplaceAll(s, "\n", " "), " "))
}

// collapse tidies extracted markdown: collapse inline whitespace and runs of blank lines.
func collapse(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight(wsRun.ReplaceAllString(ln, " "), " ")
	}
	out := strings.Join(lines, "\n")
	out = blankRun.ReplaceAllString(out, "\n\n")
	return strings.TrimSpace(out)
}
