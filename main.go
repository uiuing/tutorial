package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"unsafe"

	"github.com/alecthomas/chroma"
	"github.com/alecthomas/chroma/formatters/html"
	"github.com/alecthomas/chroma/lexers"
	"github.com/alecthomas/chroma/styles"
	"github.com/russross/blackfriday/v2"
)

var (
	footerTempl string
	exampleTmpl *template.Template
)

func init() {
	footerTempl = mustReadFile("templates/footer.tmpl")
	exampleTmpl = template.New("example")
	_, err := exampleTmpl.Parse(footerTempl)
	check(err)
	_, err = exampleTmpl.Parse(mustReadFile("templates/example.tmpl"))
	check(err)
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}

func mustReadFile(path string) string {
	bytes, err := os.ReadFile(path)
	check(err)
	return string(bytes)
}

// -----------------------------------------------------------------------------

type exampleCache struct {
	data []byte
}

type exampleIndex struct {
	Path  string
	Name  string
	Title string
	Prev  *exampleIndex
	Next  *exampleIndex
	cache *exampleCache
}

var (
	exampleIndexes map[string]*exampleIndex
)

func listTutorial(dir string) (names []string, err error) {
	fis, err := ioutil.ReadDir(dir)
	if err != nil {
		return
	}

	for _, fi := range fis {
		if fi.IsDir() {
			name := fi.Name()
			if len(name) > 3 && name[2] == '-' {
				if _, e := strconv.Atoi(name[:2]); e == nil {
					names = append(names, name)
				}
			}
		}
	}
	return
}

func renderIndex(tutorial []string) []byte {
	indexTmpl := template.New("index")
	_, err := indexTmpl.Parse(footerTempl)
	check(err)
	_, err = indexTmpl.Parse(mustReadFile("templates/index.tmpl"))
	check(err)

	var buf bytes.Buffer
	var examples = make([]*exampleIndex, len(tutorial))
	var indexes = make(map[string]*exampleIndex, len(tutorial))
	var prev *exampleIndex
	for i, name := range tutorial {
		title := name[3:]
		idx := &exampleIndex{
			Path:  "/" + strings.ToLower(title),
			Name:  name,
			Title: strings.ReplaceAll(title, "-", " "),
		}
		examples[i] = idx
		indexes[idx.Path] = idx
		if prev != nil {
			prev.Next = idx
		}
		idx.Prev = prev
		prev = idx
	}
	exampleIndexes = indexes
	err = indexTmpl.Execute(&buf, examples)
	check(err)
	return buf.Bytes()
}

// -----------------------------------------------------------------------------

// Seg is a segment of an example
type Seg struct {
	Docs         []string
	Code         []string
	DocsRendered string
	CodeRendered string
	CodeForJs    string
	CodeLeading  bool
	CodeRun      bool
}

var (
	goFileSuffixes = []string{".gop", ".go"}
)

func isGopFile(sourcePath string) bool {
	for _, suffix := range goFileSuffixes {
		if strings.HasSuffix(sourcePath, suffix) {
			return true
		}
	}
	return false
}

const (
	ltNone  = -1
	ltCode  = 0 // ltCode must be 0
	ltDoc   = 1
	ltBlank = 2
)

func checkLineType(line string) (doc string, lt int) {
	doc = strings.TrimSpace(line)
	if strings.HasPrefix(doc, "//") {
		return strings.TrimPrefix(doc[2:], " "), ltDoc
	}
	if strings.HasPrefix(doc, "#") {
		doc = "##" + doc
		lt = ltDoc
	} else if doc == "" {
		lt = ltBlank
	}
	return
}

func parseSegs(filecontent string) (segs []*Seg) {
	source := strings.Split(filecontent, "\n")
	lines := make([]string, len(source))
	for i, line := range source {
		// Convert tabs to spaces for uniform rendering.
		lines[i] = strings.ReplaceAll(line, "\t", "    ")
	}

	var lastSeg *Seg
	var lastSeen = ltNone
	for _, line := range lines {
		trimmed, lt := checkLineType(line)
		if lt == ltDoc || (lt == ltBlank && lastSeen == ltDoc) {
			if lastSeen == ltDoc {
				lastSeg.Docs = append(lastSeg.Docs, trimmed)
			} else {
				lastSeg = &Seg{Docs: []string{trimmed}}
				segs = append(segs, lastSeg)
			}
			lastSeen = ltDoc
		} else if lt == ltCode || lastSeen == ltCode {
			if lastSeg != nil {
				lastSeg.Code = append(lastSeg.Code, line)
			} else {
				lastSeg = &Seg{Code: []string{line}}
				segs = append(segs, lastSeg)
			}
			lastSeen = ltCode
		}
	}
	return
}

// Join concatenates the elements of its first argument to create a single string. The separator
// string sep is placed between elements in the resulting string.
func stringJoin(elems []string, sep string) []byte {
	switch len(elems) {
	case 0:
		return nil
	case 1:
		return []byte(elems[0])
	}
	n := len(sep) * (len(elems) - 1)
	for i := 0; i < len(elems); i++ {
		n += len(elems[i])
	}

	var b bytes.Buffer
	b.Grow(n)
	b.WriteString(elems[0])
	for _, s := range elems[1:] {
		b.WriteString(sep)
		b.WriteString(s)
	}
	return b.Bytes()
}

func markdown(docs []string) []byte {
	text := stringJoin(docs, "\n")
	return blackfriday.Run(text)
}

func chromaFormat(code string, filePath string) string {
	// Currently, Go+ source code will use syntax highlight rules that designed for Go
	if strings.HasSuffix(filePath, ".gop") {
		filePath = "main.go"
	}
	lexer := lexers.Get(filePath)
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)
	style := styles.Get("swapoff")
	if style == nil {
		style = styles.Fallback
	}
	formatter := html.New(html.WithClasses(true))
	iterator, err := lexer.Tokenise(nil, code)
	check(err)
	buf := new(bytes.Buffer)
	err = formatter.Format(buf, style, iterator)
	check(err)
	return buf.String()
}

func parseAndRenderSegs(sourcePath string) ([]*Seg, string) {
	filecontent := mustReadFile(sourcePath)
	segs := parseSegs(filecontent)
	for _, seg := range segs {
		if seg.Docs != nil {
			seg.DocsRendered = string(markdown(seg.Docs))
		}
		if seg.Code != nil {
			seg.CodeForJs = strings.Join(seg.Code, "\n")
			seg.CodeRendered = chromaFormat(seg.CodeForJs, sourcePath)
		}
	}
	// we are only interested in the 'Go+' code to pass to play.goplus.org
	if !isGopFile(sourcePath) {
		filecontent = ""
	}
	return segs, filecontent
}

// -----------------------------------------------------------------------------

type exampleFile struct {
	Segs   []*Seg
	GoCode string
}

type example struct {
	*exampleIndex
	Files []*exampleFile
}

func parseExample(dir string, idx *exampleIndex) *example {
	fis, err := ioutil.ReadDir(dir)
	check(err)
	example := &example{exampleIndex: idx}
	for _, fi := range fis {
		sourcePath := filepath.Join(dir, fi.Name())
		sourceSegs, filecontents := parseAndRenderSegs(sourcePath)
		file := &exampleFile{Segs: sourceSegs}
		if filecontents != "" {
			file.GoCode = filecontents
		}
		example.Files = append(example.Files, file)
	}
	return example
}

func renderExample(e *example) []byte {
	var buf bytes.Buffer
	err := exampleTmpl.Execute(&buf, e)
	check(err)
	return buf.Bytes()
}

func buildExampleCache(dir string, idx *exampleIndex) *exampleCache {
	example := parseExample(dir, idx)
	data := renderExample(example)
	return &exampleCache{data: data}
}

func handleExample(w http.ResponseWriter, req *http.Request, root, path string) {
	if idx, ok := exampleIndexes[path]; ok {
		cache := atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&idx.cache)))
		if cache == nil {
			cache = unsafe.Pointer(buildExampleCache(filepath.Join(root, idx.Name), idx))
			atomic.StorePointer((*unsafe.Pointer)(unsafe.Pointer(&idx.cache)), cache)
		}
		w.Write(((*exampleCache)(cache)).data)
		return
	}
	http.ServeFile(w, req, "./public/404.html")
}

// -----------------------------------------------------------------------------

func handle(root string) func(w http.ResponseWriter, req *http.Request) {
	var text []byte
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		names, err := listTutorial(root)
		if err != nil {
			log.Panicln(err)
		}
		text = renderIndex(names)
		wg.Done()
	}()
	return func(w http.ResponseWriter, req *http.Request) {
		urlPath := path.Clean(req.URL.Path)
		if !path.IsAbs(urlPath) {
			urlPath = "/404.html"
		}
		if urlPath == "/" {
			wg.Wait()
			w.Write(text)
			return
		}
		if path.Ext(urlPath) != "" {
			http.ServeFile(w, req, "./public"+urlPath)
			return
		}
		handleExample(w, req, root, urlPath)
	}
}

var (
	host = flag.String("host", "localhost:8000", "Serving host")
)

func main() {
	flag.Parse()
	fmt.Println("Serving Go+ tutorial at", *host)
	http.HandleFunc("/", handle("."))
	http.ListenAndServe(*host, nil)
}

// -----------------------------------------------------------------------------
