// gendoc builds man.nein.ar from the manned.org database dump.
// It downloads the dump, filters it to the configured systems and locales,
// renders roff man pages to HTML with mandoc, and emits a static site.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// config holds the site configuration parsed from config.toml.
type config struct {
	Title     string
	BaseURL   string
	OutputDir string
	CacheDir  string
	Systems   []string
	Locales   []string
	DumpURL   string
}

// manPage describes one man page to render.
type manPage struct {
	PkgName   string
	Version   string
	ManName   string
	Section   string
	ContentID int
	Shorthash int
}

// renderedPage holds the final HTML for one man page.
type renderedPage struct {
	PkgName    string
	ManName    string
	Section    string
	Version    string
	HTML       string
	Shorthash  int
}

// pkgEntry groups man pages by package for index rendering.
type pkgEntry struct {
	name    string
	version string
	mans    []renderedPage
}

// searchEntry is one item in the search index.
type searchEntry struct {
	Name    string `json:"name"`
	Section string `json:"section"`
	Pkg     string `json:"pkg"`
	URL     string `json:"url"`
}

func main() {
	cfg := loadConfig("config.toml")

	dumpDir := resolveDumpDir(cfg.DumpURL)
	fmt.Printf("gendoc: dump dir %s\n", dumpDir)

	stampFile := filepath.Join(cfg.CacheDir, "dump.stamp")
	if stamp, err := os.ReadFile(stampFile); err == nil && strings.TrimSpace(string(stamp)) == dumpDir {
		fmt.Println("gendoc: dump unchanged, skipping rebuild")
		return
	}

	os.MkdirAll(cfg.CacheDir, 0o755)
	os.MkdirAll(cfg.OutputDir, 0o755)

	// Step 1: parse small reference TSVs
	fmt.Println("gendoc: parsing systems...")
	systemsTSV := downloadTSVToLines(cfg.DumpURL, dumpDir, "systems.tsv.zst")
	sysIDs := filterSystems(systemsTSV, cfg.Systems)
	if len(sysIDs) == 0 {
		fmt.Fprintln(os.Stderr, "gendoc: no systems matched, nothing to do")
		return
	}

	fmt.Println("gendoc: parsing packages...")
	packagesTSV := downloadTSVToLines(cfg.DumpURL, dumpDir, "packages.tsv.zst")
	pkgMap := filterPackages(packagesTSV, sysIDs) // map[packageID]packageName

	fmt.Println("gendoc: parsing package versions...")
	versionsTSV := downloadTSVToLines(cfg.DumpURL, dumpDir, "package_versions.tsv.zst")
	latestVers, pkgByVer := filterLatestVersions(versionsTSV, pkgMap)
	// latestVers: map[versionID]versionString
	// pkgByVer: map[versionID]packageID

	fmt.Println("gendoc: parsing mans...")
	mansTSV := downloadTSVToLines(cfg.DumpURL, dumpDir, "mans.tsv.zst")
	manMap := filterMans(mansTSV) // map[manID]{name, section}

	fmt.Println("gendoc: parsing locales...")
	localesTSV := downloadTSVToLines(cfg.DumpURL, dumpDir, "locales.tsv.zst")
	localeIDs := filterLocales(localesTSV, cfg.Locales)
	// locale 0 is the empty/unspecified locale; manned.org treats it as English
	// (is_english_locale). Many systems, including Alpine, store man pages
	// without an explicit locale tag.
	localeIDs[0] = struct{}{}
	if len(localeIDs) == 0 {
		fmt.Fprintln(os.Stderr, "gendoc: no locales matched, nothing to do")
		return
	}

	// Step 2: stream files.tsv.zst, collect needed content IDs
	fmt.Println("gendoc: collecting files (streaming)...")
	pagesByContent := collectFiles(cfg.DumpURL, dumpDir, latestVers, pkgByVer, pkgMap, manMap, localeIDs)
	// map[contentID][]manPage

	if len(pagesByContent) == 0 {
		fmt.Fprintln(os.Stderr, "gendoc: no man pages found, nothing to do")
		return
	}

	fmt.Printf("gendoc: %d unique contents to render\n", len(pagesByContent))

	// Step 3: stream contents.tsv.zst, extract needed roff
	fmt.Println("gendoc: collecting contents (streaming)...")
	contentsByID := collectContents(cfg.DumpURL, dumpDir, pagesByContent)
	// map[contentID]roffContent

	// Step 4: render each with mandoc
	fmt.Println("gendoc: rendering with mandoc...")
	rendered := renderAll(contentsByID, pagesByContent, cfg.CacheDir)

	// Step 5: emit HTML pages + search index
	fmt.Println("gendoc: emitting pages...")
	emitPages(rendered, cfg)

	// Record dump stamp so we skip if nothing changed
	os.WriteFile(stampFile, []byte(dumpDir), 0o644)

	fmt.Printf("gendoc: done, %d man pages rendered\n", len(rendered))
}

// loadConfig reads config.toml with a minimal parser. Only the keys we use
// are supported; everything else is ignored.
func loadConfig(path string) *config {
	cfg := &config{
		OutputDir: "pub",
		CacheDir:  ".cache",
		DumpURL:   "https://dl.manned.org",
		Systems:   []string{"alpine-3.23"},
		Locales:   []string{"en"},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gendoc: cannot read %s: %v\n", path, err)
		return cfg
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		val = strings.Trim(val, "\"")
		switch key {
		case "title":
			cfg.Title = val
		case "base_url":
			cfg.BaseURL = val
		case "output_dir":
			cfg.OutputDir = val
		case "cache_dir":
			cfg.CacheDir = val
		case "dump_url":
			cfg.DumpURL = val
		case "systems":
			cfg.Systems = parseArray(val)
		case "locales":
			cfg.Locales = parseArray(val)
		}
	}
	return cfg
}

func parseArray(s string) []string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil
	}
	inner := s[1 : len(s)-1]
	if inner == "" {
		return nil
	}
	var out []string
	for _, item := range strings.Split(inner, ",") {
		item = strings.TrimSpace(item)
		item = strings.Trim(item, "\"")
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

// resolveDumpDir fetches the current pointer file and returns the latest dump date dir.
func resolveDumpDir(dumpURL string) string {
	resp, err := http.Get(dumpURL + "/current")
	if err != nil {
		fmt.Fprintf(os.Stderr, "gendoc: cannot fetch dump pointer: %v\n", err)
		return ""
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gendoc: cannot read dump pointer: %v\n", err)
		return ""
	}
	return strings.TrimSpace(string(data))
}

// downloadTSVToLines downloads a TSV.zst file, decompresses it, and returns all lines.
func downloadTSVToLines(dumpURL, dumpDir, filename string) []string {
	url := fmt.Sprintf("%s/%s/%s", dumpURL, dumpDir, filename)
	return streamTSV(url, nil)
}

// streamTSV downloads a .tsv.zst file, decompresses it with zstd, and calls
// handler for every line. When handler is nil, lines are collected and returned.
func streamTSV(url string, handler func(line string) error) []string {
	var collected []string
	if handler == nil {
		handler = func(line string) error {
			collected = append(collected, line)
			return nil
		}
	}

	tmp, err := os.CreateTemp("", "manned-*.tsv.zst")
	if err != nil {
		fmt.Fprintf(os.Stderr, "gendoc: cannot create temp file: %v\n", err)
		return collected
	}
	tmpPath := tmp.Name()

	resp, err := http.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gendoc: cannot download %s: %v\n", url, err)
		os.Remove(tmpPath)
		return collected
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "gendoc: %s returned %s\n", url, resp.Status)
		os.Remove(tmpPath)
		return collected
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		fmt.Fprintf(os.Stderr, "gendoc: cannot write %s: %v\n", tmpPath, err)
		tmp.Close()
		os.Remove(tmpPath)
		return collected
	}
	tmp.Close()

	cmd := exec.Command("zstd", "-dc", tmpPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gendoc: cannot create stdout pipe: %v\n", err)
		os.Remove(tmpPath)
		return collected
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "gendoc: cannot start zstd: %v\n", err)
		os.Remove(tmpPath)
		return collected
	}

	reader := bufio.NewReaderSize(stdout, 1024*1024)
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimRight(line, "\n")
		if line != "" {
			if err := handler(line); err != nil {
				break
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			fmt.Fprintf(os.Stderr, "gendoc: read error: %v\n", err)
			break
		}
	}
	cmd.Wait()
	os.Remove(tmpPath)
	return collected
}

// filterSystems returns the set of system IDs matching the configured names.
func filterSystems(lines []string, names []string) map[int]struct{} {
	want := make(map[string]struct{}, len(names))
	for _, n := range names {
		want[n] = struct{}{}
	}
	out := make(map[int]struct{})
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) < 4 {
			continue
		}
		id, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		short := fields[3]
		if _, ok := want[short]; ok {
			out[id] = struct{}{}
		}
	}
	return out
}

// filterPackages returns map[packageID]packageName for packages in the given
// systems that have man pages and are not dead.
func filterPackages(lines []string, sysIDs map[int]struct{}) map[int]string {
	out := make(map[int]string)
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) < 5 {
			continue
		}
		id, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		sysID, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		if _, ok := sysIDs[sysID]; !ok {
			continue
		}
		dead := fields[3] == "t"
		if dead {
			continue
		}
		out[id] = fields[2]
	}
	return out
}

// filterLatestVersions returns map[versionID]versionString for the latest
// version of each package, plus map[versionID]packageID.
// package_versions.tsv columns: id, package, version, released, arch.
func filterLatestVersions(lines []string, pkgMap map[int]string) (map[int]string, map[int]int) {
	type verInfo struct {
		versionID int
		version   string
		released  string
	}
	best := make(map[int]verInfo) // packageID -> best
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) < 5 {
			continue
		}
		id, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		pkgID, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		if _, ok := pkgMap[pkgID]; !ok {
			continue
		}
		version := fields[2]
		released := fields[3]
		if cur, ok := best[pkgID]; !ok || released > cur.released {
			best[pkgID] = verInfo{id, version, released}
		}
	}
	latestVers := make(map[int]string, len(best))
	pkgByVer := make(map[int]int, len(best))
	for pkgID, v := range best {
		latestVers[v.versionID] = v.version
		pkgByVer[v.versionID] = pkgID
	}
	return latestVers, pkgByVer
}

// filterMans returns map[manID]manRef.
func filterMans(lines []string) map[int]manRef {
	out := make(map[int]manRef)
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}
		id, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		out[id] = manRef{name: fields[1], section: fields[2]}
	}
	return out
}

type manRef struct {
	name   string
	section string
}

// filterLocales returns the set of locale IDs matching the configured names.
func filterLocales(lines []string, names []string) map[int]struct{} {
	want := make(map[string]struct{}, len(names))
	for _, n := range names {
		want[n] = struct{}{}
	}
	out := make(map[int]struct{})
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		id, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		name := fields[1]
		if _, ok := want[name]; ok {
			out[id] = struct{}{}
		}
	}
	return out
}

// collectFiles streams files.tsv.zst and returns map[contentID][]manPage for
// entries matching the latest versions and configured locales.
// files.tsv columns: pkgver, man, content, shorthash, locale, encoding, filename.
func collectFiles(dumpURL, dumpDir string, latestVers map[int]string, pkgByVer map[int]int, pkgMap map[int]string, manMap map[int]manRef, localeIDs map[int]struct{}) map[int][]manPage {
	url := fmt.Sprintf("%s/%s/files.tsv.zst", dumpURL, dumpDir)
	out := make(map[int][]manPage)

	handler := func(line string) error {
		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			return nil
		}
		pkgver, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil
		}
		version, ok := latestVers[pkgver]
		if !ok {
			return nil
		}
		manID, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil
		}
		contentID, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil
		}
		shorthash, err := strconv.Atoi(fields[3])
		if err != nil {
			return nil
		}
		localeID, err := strconv.Atoi(fields[4])
		if err != nil {
			return nil
		}
		if _, ok := localeIDs[localeID]; !ok {
			return nil
		}
		manRef, ok := manMap[manID]
		if !ok {
			return nil
		}
		pkgID := pkgByVer[pkgver]
		pkgName := pkgMap[pkgID]
		mp := manPage{
			PkgName:   pkgName,
			Version:   version,
			ManName:   manRef.name,
			Section:   manRef.section,
			ContentID: contentID,
			Shorthash: shorthash,
		}
		out[contentID] = append(out[contentID], mp)
		return nil
	}
	streamTSV(url, handler)
	return out
}

// collectContents streams contents.tsv.zst and returns map[contentID]roffContent
// only for content IDs we need.
// contents.tsv columns: id, hash, content.
func collectContents(dumpURL, dumpDir string, needed map[int][]manPage) map[int]string {
	url := fmt.Sprintf("%s/%s/contents.tsv.zst", dumpURL, dumpDir)
	out := make(map[int]string, len(needed))

	handler := func(line string) error {
		tabIdx := strings.IndexByte(line, '\t')
		if tabIdx < 0 {
			return nil
		}
		idStr := line[:tabIdx]
		id, err := strconv.Atoi(idStr)
		if err != nil {
			return nil
		}
		if _, ok := needed[id]; !ok {
			return nil
		}
		rest := line[tabIdx+1:]
		tabIdx2 := strings.IndexByte(rest, '\t')
		if tabIdx2 < 0 {
			return nil
		}
		content := rest[tabIdx2+1:]
		out[id] = content
		return nil
	}
	streamTSV(url, handler)
	return out
}

// renderAll renders every collected content with mandoc, using the shorthash
// cache to avoid re-rendering unchanged pages.
func renderAll(contentsByID map[int]string, pagesByContent map[int][]manPage, cacheDir string) []renderedPage {
	var out []renderedPage
	renderCacheDir := filepath.Join(cacheDir, "render")
	os.MkdirAll(renderCacheDir, 0o755)

	ids := make([]int, 0, len(contentsByID))
	for id := range contentsByID {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	for _, id := range ids {
		roff := contentsByID[id]
		pages := pagesByContent[id]
		if len(pages) == 0 {
			continue
		}
		shorthash := pages[0].Shorthash

		cachePath := filepath.Join(renderCacheDir, fmt.Sprintf("%d.html", shorthash))
		var html string
		if data, err := os.ReadFile(cachePath); err == nil {
			html = string(data)
		} else {
			html = renderManpage(roff)
			if html == "" {
				continue
			}
			os.WriteFile(cachePath, []byte(html), 0o644)
		}

		for _, p := range pages {
			rp := renderedPage{
				PkgName:   p.PkgName,
				ManName:   p.ManName,
				Section:   p.Section,
				Version:   p.Version,
				HTML:      html,
				Shorthash: shorthash,
			}
			out = append(out, rp)
		}
	}
	return out
}

// renderManpage converts roff source to HTML using mandoc.
func renderManpage(roff string) string {
	cmd := exec.Command("mandoc", "-Thtml", "-Ostyle=man.css")
	cmd.Stdin = strings.NewReader(roff)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	return out.String()
}

// emitPages writes HTML package index pages, man page pages, search index,
// and copies static assets to the output directory.
func emitPages(pages []renderedPage, cfg *config) {
	pkgMap := make(map[string]*pkgEntry)
	var pkgOrder []string
	for _, p := range pages {
		if p.PkgName == "" {
			continue
		}
		entry, ok := pkgMap[p.PkgName]
		if !ok {
			entry = &pkgEntry{name: p.PkgName, version: p.Version}
			pkgMap[p.PkgName] = entry
			pkgOrder = append(pkgOrder, p.PkgName)
		}
		entry.mans = append(entry.mans, p)
	}
	sort.Strings(pkgOrder)

	for _, pkgName := range pkgOrder {
		entry := pkgMap[pkgName]
		dir := filepath.Join(cfg.OutputDir, pkgName)
		os.MkdirAll(dir, 0o755)
		indexHTML := renderPackageIndex(entry)
		os.WriteFile(filepath.Join(dir, "index.html"), []byte(indexHTML), 0o644)

		for _, p := range entry.mans {
			fileName := fmt.Sprintf("%s.%s.html", p.ManName, p.Section)
			os.WriteFile(filepath.Join(dir, fileName), []byte(p.HTML), 0o644)
		}
	}

	var entries []searchEntry
	for _, pkgName := range pkgOrder {
		entry := pkgMap[pkgName]
		for _, p := range entry.mans {
			entries = append(entries, searchEntry{
				Name:    p.ManName,
				Section: p.Section,
				Pkg:     p.PkgName,
				URL:     fmt.Sprintf("/%s/%s.%s.html", p.PkgName, p.ManName, p.Section),
			})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Pkg != entries[j].Pkg {
			return entries[i].Pkg < entries[j].Pkg
		}
		return entries[i].Name < entries[j].Name
	})
	searchData, _ := json.MarshalIndent(entries, "", "  ")
	os.WriteFile(filepath.Join(cfg.OutputDir, "search.json"), append(searchData, '\n'), 0o644)

	homeHTML := renderHome(pkgOrder)
	os.WriteFile(filepath.Join(cfg.OutputDir, "index.html"), []byte(homeHTML), 0o644)

	os.WriteFile(filepath.Join(cfg.OutputDir, "robots.txt"), []byte("User-agent: *\nAllow: /\n"), 0o644)
}

func renderHome(pkgs []string) string {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"UTF-8\">\n<meta name=\"viewport\" content=\"width=device-width, initial-scale=1.0\">\n<title>man.nein.ar</title>\n<style>\nbody { font-family: monospace; background: #0a0000; color: #f5d4d2; margin: 0; padding: 2rem; }\na { color: #ffc9c5; }\nul { list-style: none; padding: 0; }\nli { margin: 0.5rem 0; }\n</style>\n</head>\n<body>\n<h1>man.nein.ar</h1>\n<p>man pages for alpine linux.</p>\n<ul>\n")
	for _, pkg := range pkgs {
		fmt.Fprintf(&b, "<li><a href=\"/%s/\">%s</a></li>\n", html.EscapeString(pkg), html.EscapeString(pkg))
	}
	b.WriteString("</ul>\n</body>\n</html>\n")
	return b.String()
}

func renderPackageIndex(entry *pkgEntry) string {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"UTF-8\">\n<meta name=\"viewport\" content=\"width=device-width, initial-scale=1.0\">\n<title>")
	b.WriteString(html.EscapeString(entry.name))
	b.WriteString("</title>\n<style>\nbody { font-family: monospace; background: #0a0000; color: #f5d4d2; margin: 0; padding: 2rem; }\na { color: #ffc9c5; }\nul { list-style: none; padding: 0; }\nli { margin: 0.5rem 0; }\n</style>\n</head>\n<body>\n<h1>")
	b.WriteString(html.EscapeString(entry.name))
	b.WriteString("</h1>\n<p>version ")
	b.WriteString(html.EscapeString(entry.version))
	b.WriteString("</p>\n<ul>\n")
	for _, p := range entry.mans {
		fmt.Fprintf(&b, "<li><a href=\"%s.%s.html\">%s(%s)</a></li>\n", html.EscapeString(p.ManName), html.EscapeString(p.Section), html.EscapeString(p.ManName), html.EscapeString(p.Section))
	}
	b.WriteString("</ul>\n</body>\n</html>\n")
	return b.String()
}
