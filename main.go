package main

import (
	"encoding/json"
	"flag"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"

	"regexp"
	"strings"
	"time"

	"bufio"
	"bytes"
	"os/exec"

	"github.com/labstack/echo"
	"github.com/olahol/melody"
	"github.com/rjeczalik/notify"
	diff "github.com/sergi/go-diff/diffmatchpatch"
	"github.com/shurcooL/github_flavored_markdown"

	"github.com/gobuffalo/packr/v2"
)

var (
	extResources      = []string{".png", ".jpg", ".jpeg", ".svg", ".tiff", ".gif"}
	extMarkdown       = []string{".md", ".markdown", ".mdown", ".adoc", ".txt"}
	extExcluded       = []string{".git", ".idea", ".directory"}
	filenameBlacklist = []string{".directory"}

	// global vars
	dirPath     string
	currentFile string
	lastfile    string
	lasttext    string
	dmp         *diff.DiffMatchPatch

	// user preferences
	port           int
	noHeadTags     bool
	hardLineBreaks bool
	adoc           bool
	scrollSpy      bool
)

func init() {
	flag.IntVar(&port, "port", 3000, "port to serve on")
	flag.BoolVar(&noHeadTags, "topless", false, `remove file header tags matching '(?m)^---$(.|\n)*^---$'
	e.g.
	---
	name: Home
	category: documentation
	---`)
	flag.BoolVar(&hardLineBreaks, "n", false, "Enable hard line breaks.")
	flag.BoolVar(&scrollSpy, "s", true, "Enable or disable automatic scrolling to most recent change.")

	dmp = diff.New()
}

func main() {

	log.Println("running wmmd v1")
	flag.Parse()
	dirPath = mustMakeDirPath()
	mm := melody.New()

	watcher := make(chan notify.EventInfo, 1)

	currentFile = getLastUpdated(dirPath)

	mm.HandleConnect(func(s *melody.Session) {
		log.Println("session connected")
		curFile, e := getReadFile(getCurrentFile())
		if e != nil {
			log.Println(e)
		}
		sidebar, e := getReadFile(filepath.Join(dirPath, "_Sidebar.md"))
		if e != nil {
			log.Println(e)
		}
		footer, e := getReadFile(filepath.Join(dirPath, "_Footer.md"))
		if e != nil {
			log.Println(e)
		}
		for _, f := range []FileContent{sidebar, footer, curFile} {
			if (f == FileContent{}) {
				continue
			}
			j, e := json.Marshal(f)
			if e != nil {
				log.Println(e)
				continue
			}
			mm.Broadcast(j)
		}

		j, e := json.Marshal(curFile)
		if e != nil {
			log.Println(e)
			return
		}
		mm.Broadcast(j)
	})
	mm.HandleDisconnect(func(s *melody.Session) {
		log.Println("session disconnected")
	})

	go func() {
		for {
			select {
			case event := <-watcher:
				if ei, ee := os.Stat(event.Path()); ee != nil || (ei != nil && ei.IsDir()) {
					continue
				}
				if filepathMatches(event.Path(), extExcluded) {
					// log.Println("excluded path, continuing...")
					continue
				}
				// log.Println("event:", event)
				if !filepathMatches(event.Path(), extMarkdown) {
					// log.Println("not markdown file, continuing...")
					continue
				}
				for _, n := range filenameBlacklist {
					if strings.Contains(event.Path(), n) {
						continue
					}
				}
				f := getFilePathFromParam(event.Path())
				setCurrentFile(f)
				m, e := getReadFile(f)
				if e != nil {
					log.Println(e)
					continue
				}
				b, e := json.Marshal(m)
				if e != nil {
					log.Println(e)
					continue
				}
				log.Println("broadcasting", m.Title)
				mm.Broadcast(b)
			}
		}
	}()

	watch := func() error {
		if err := notify.Watch(filepath.Join(dirPath, "..."), watcher, notify.All); err != nil {
			return err
		}
		return nil
	}

	go func() {
		for {
			if err := watch(); err != nil {
				log.Println("watch err", err)
			}
		}
	}()

	defer notify.Stop(watcher)

	// Echo is polite because it prioritizes these paths, so they can be overlapping,
	// ie. ":filename" overlaps everything except /
	r := echo.New()

	// r.File("/", filepath.Join(os.Getenv("GOPATH"), "src", "github.com", "rotblauer", "wmmd", "index.html"))
	// Static assets.
	// r.Static("/assets", filepath.Join(os.Getenv("GOPATH"), "src", "github.com", "rotblauer", "wmmd", "assets"))
	// r.Static("/node_modules/primer-css/build", filepath.Join(os.Getenv("GOPATH"), "src", "github.com", "rotblauer", "wmmd", "node_modules/primer-css/build"))

	boxAssets := packr.New("assets", "./assets")

	r.GET("/", func(c echo.Context) error {
		b, err := boxAssets.Find("index.html")
		if err != nil {
			return err
		}
		return c.HTMLBlob(200, b)
	})

	r.GET("/assets/*", func(c echo.Context) error {
		log.Println(c.Path())
		log.Println(c.ParamValues())
		if len(c.ParamValues()) > 0 {
			b, err := boxAssets.Find(c.ParamValues()[0])
			if err != nil {
				log.Println("NOTOK")
				return err
			}
			log.Println("OK")
			return c.HTMLBlob(200, b)
		}
		return c.HTMLBlob(404, nil)
	})

	// Websocket.
	r.GET("/x/0", func(c echo.Context) error {
		mm.HandleRequest(c.Response(), c.Request())
		return nil
	})
	// Any other filename.
	r.Any("/*", func(c echo.Context) error { // :filename
		//p := c.Param("filename")
		p := dirPath
		for _, v := range c.ParamValues() {
			p = filepath.Join(p, v)
		}
		log.Println("path", p)
		if filepathMatches(p, extResources) {
			log.Println("resource request: filename:", p)
			e := c.File(p)
			if e != nil {
				log.Println("file error: ", e)
				if strings.Contains(p, "/wiki/") {
					p = strings.Replace(p, "/wiki", "", 1)
				}
				e = c.File(p)
				if e == nil {
					log.Println("found resource", p)
				}
			}
			return e
		}
		filename := getFilePathFromParam(p)
		setCurrentFile(filename)
		// It is important with all this same-file-yness to NOT allow cacheing.
		c.Response().Header().Set("Cache-Control: no-cache", "true")
		return c.File(filepath.Join(os.Getenv("GOPATH"), "src", "github.com", "rotblauer", "wmmd", "index.html"))
		// c.Redirect(http.StatusMovedPermanently, "/")
	})

	log.Println("Listening...", port)
	r.Logger.Fatal(r.Start(":" + strconv.Itoa(port)))
}

func getLastUpdated(path string) (filename string) {
	fs, fe := ioutil.ReadDir(path)
	if fe != nil {
		log.Println(fe)
		return ""
	}
	var latestMod time.Time
	var latestModFile string
	var found bool
outer:
	for _, ff := range fs {
		if ff.IsDir() {
			continue
		}
		for _, n := range filenameBlacklist {
			if strings.Contains(ff.Name(), n) {
				continue outer
			}
		}
		if !filepathMatches(ff.Name(), extMarkdown) {
			continue
		}
		if ff.ModTime().After(latestMod) {
			found = true
			latestMod = ff.ModTime()
			latestModFile = ff.Name()
		}
	}

	if !found {
	outera:
		for i := range fs {
			for _, n := range filenameBlacklist {
				if strings.Contains(fs[i].Name(), n) {
					continue outera
				}
			}
			return fs[i].Name()
		}
	}
	return latestModFile
}

func mustMakeDirPath() string {
	args := flag.Args()
	if len(args) == 0 {
		p, e := os.Getwd()
		if e != nil {
			panic(e)
		}
		return p
	}
	abs, e := filepath.Abs(args[0])
	if e != nil {
		panic(e)
	}
	di, de := os.Stat(abs)
	if de != nil {
		panic(de)
	}
	if !di.IsDir() {
		panic("path must be a dir")
	}
	return abs
}

func checkExistsOrAppend(filename string) (bool, string) {
	if fi, e := os.Stat(filename); e == nil && !fi.IsDir() {
		return true, filename
	}
	if ext := filepath.Ext(filename); ext != "" {
		return true, filename
	}
	for _, ext := range extMarkdown {
		fname := filename + ext
		if i, e := os.Stat(fname); e == nil && !i.IsDir() {
			return true, fname
		}
	}
	return false, ""
}

func getFilePathFromParam(param string) string {
	filename := param
	if filename == "" {
		return ""
	}
	if filepath.IsAbs(filename) {
		ok, gotname := checkExistsOrAppend(filename)
		if ok {
			return gotname
		}
	}
	filename = filepath.Join(dirPath, filename)
	ok, gotname := checkExistsOrAppend(filename)
	if ok {
		return gotname
	}
	return filename
}

func getReadFile(path string) (FileContent, error) {
	fileBytes, e := ioutil.ReadFile(path)
	if hardLineBreaks {
		blanknewlinere := regexp.MustCompile(`\n\n`)
		fileBytes = blanknewlinere.ReplaceAll(fileBytes, []byte("<p class='an-newline'><span class='hidden-newline'></span></p>\n"))
		fString := string(fileBytes)
		fString = strings.Replace(fString, "\n", "\n\n", -1)
		fileBytes = []byte(fString)
	}
	if e != nil {
		log.Println(e)
		return FileContent{}, e
	}
	changeI := 0
	if lasttext == "" {
		lasttext = string(fileBytes)
		lastfile = filepath.Base(path)
		log.Println("initializing diff")
	} else {
		log.Println("filepath.base:", filepath.Base(path), lastfile, lasttext == string(fileBytes))
		var ffs string
		if lastfile == filepath.Base(path) && lasttext != string(fileBytes) {
			log.Println("could update diff")
			if fbn := filepath.Base(path); !strings.Contains(fbn, "Sidebar") && !strings.Contains(fbn, "Footer") {
				if scrollSpy {
					log.Println("updating diff")

					ffs = string(fileBytes)
					hiddenChangeTag := `<span class="suffix-change">CHANGED</span>`

					changeI = getCommSuffixI(ffs)
					if changeI > 1 && len(ffs)-1 != changeI {
						log.Println("comm suffix: ", changeI)
						ffs = ffs[:len(ffs)-changeI] + hiddenChangeTag + ffs[len(ffs)-changeI:]
					} else {
						changeI = getCommPrefix(ffs)
						log.Println("comm prefix: ", changeI)
						ffs = ffs[:changeI] + hiddenChangeTag + ffs[changeI:]
					}

					// Order matters here.
					lasttext = string(fileBytes)
					fileBytes = []byte(ffs)
				}
			}
		}
		lastfile = filepath.Base(path)
	}
	if noHeadTags {
		fileBytes = stripHeaderTagMetadata(fileBytes)
	}
	rp, e := filepath.Rel(dirPath, path)
	if e != nil {
		log.Println(e)
		rp = path
	}

	var body string
	if !adoc {
		body = string(github_flavored_markdown.Markdown(fileBytes))
	} else {
		body = getAsciidocContent(fileBytes)
	}

	return FileContent{
		Title: rp, // TODO parse File-Name.md syntax => File Name
		// Body:    emoji.Emojitize(string(github_flavored_markdown.Markdown(fileBytes))),
		Body:    body,
		ChangeI: changeI,
	}, nil
}

// hack: this is a weird ugly way to to it
// problem was matching only the first occurrence of a regex group pattern...
func stripHeaderTagMetadata(infile []byte) []byte {
	outfile := infile
	re := regexp.MustCompile(`(?m)^---$(.|\n)*^---$`) // has header tag regex
	reDashes := regexp.MustCompile(`^---$`)           // header tag sep regex
	if found := re.Find(infile); found != nil {
		log.Println("Found top to take off...")
		reader := bytes.NewReader(infile)
		scanner := bufio.NewScanner(reader)
		buf := bytes.Buffer{}
		writer := bufio.NewWriter(&buf)
		lnum := 0
		c := 0 // count how many header tag seps we find. only want file text AFTER the second match
		e := 0 // tally how many lines we are after second match... we want to only include >= matchline+1
		for scanner.Scan() {
			lnum++
			if reDashes.Match(scanner.Bytes()) {
				c++
				log.Printf("Found head delimiter match at line: %d", lnum)
			}
			if c >= 2 {
				e++
			}
			if c >= 2 && e > 1 {
				//log.Printf("Appending line: %d", lnum) // debug
				bs := append(scanner.Bytes(), []byte("\n")...)
				if _, err := writer.Write(bs); err != nil {
					log.Println("ERROR WRITE BUF BYTES", err)
				} else {
					writer.Flush()
				}
				//log.Printf("%s", string(scanner.Bytes())) // debug
			}
		}
		if err := scanner.Err(); err != nil {
			log.Println("SCANNER ERROR: %v", err)
		}
		outfile = buf.Bytes()
	} else {
		log.Println("no matching tags found, continuing")
	}
	return outfile
}

// getAsciidocContent calls asciidoctor or asciidoc as an external helper
// to convert AsciiDoc content to HTML.
// https://github.com/gohugoio/hugo/pull/826/files
func getAsciidocContent(content []byte) string {
	cleanContent := content // bytes.Replace(content, SummaryDivider, []byte(""), 1)

	path, err := exec.LookPath("asciidoctor")
	if err != nil {
		path, err = exec.LookPath("asciidoc")
		if err != nil {
			log.Println("asciidoctor / asciidoc not found in $PATH: Please install.\n",
				"                 Leaving AsciiDoc content unrendered.")
			return (string(content))
		}
	}

	log.Println("Rendering with", path, "...")
	cmd := exec.Command(path, "--safe", "-")
	cmd.Stdin = bytes.NewReader(cleanContent)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		log.Println(err)
	}

	asciidocLines := strings.Split(out.String(), "\n")
	for i, line := range asciidocLines {
		if strings.HasPrefix(line, "<body") {
			asciidocLines = (asciidocLines[i+1 : len(asciidocLines)-3])
		}
	}
	return strings.Join(asciidocLines, "\n")
}

func setCurrentFile(path string) {
	currentFile = path
}

func getCurrentFile() string {
	return currentFile
}
