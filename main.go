package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/pprof"
	"strings"

	toml "github.com/pelletier/go-toml"
	"github.com/pkg/errors"
	"github.com/remeh/sizedwaitgroup"
)

type lock struct {
	Projects []*project `toml:"projects"`
}

type project struct {
	Name     string   `toml:"name"`
	Branch   string   `toml:"branch,omitempty"`
	Revision string   `toml:"revision"`
	Version  string   `toml:"version,omitempty"`
	Source   string   `toml:"source,omitempty"`
	Packages []string `toml:"packages"`

	subdirTable map[string]bool
}

var (
	githubRegexp = regexp.MustCompile("github.com/(?P<user>[^/ \n]+)/(?P<repo>[^/ \n]+)")
	gopkgRegexp  = regexp.MustCompile("gopkg.in/(.+)")
	vendorDir    = ""
	fVerbose     = flag.Bool("v", false, "verbose output")
	fParallelism = flag.Int("p", 4, "parallelism of download")
	fCpuprofile  = flag.String("cpuprofile", "", "write cpu profile to file")
)

func (pj *project) dlGithub(user, repo string) error {
	tarballUrl := fmt.Sprintf("https://api.github.com/repos/%s/%s/tarball/%s", user, repo, pj.Revision)
	resp, err := http.Get(tarballUrl)
	if err != nil {
		return errors.WithStack(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errors.Errorf("failed getting tarball: status %s (URL: %s)", resp.Status, tarballUrl)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	buf := bytes.NewBuffer(body)

	gz, err := gzip.NewReader(buf)
	if err != nil {
		return errors.WithStack(err)
	}
	defer gz.Close()

	files := tar.NewReader(gz)

	baseDir := filepath.Join(vendorDir, pj.Name)
	if err := os.RemoveAll(baseDir); err != nil && !os.IsNotExist(err) {
		return errors.WithStack(err)
	}
	if err = os.MkdirAll(baseDir, 0777); err != nil {
		return errors.WithStack(err)
	}
	for {
		hdr, err := files.Next()

		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.WithStack(err)
		}

		nameDirs := strings.Split(hdr.Name, "/")
		if len(nameDirs) > 2 {
			pkg := filepath.Join(nameDirs[1 : len(nameDirs)-1]...)
			if !pj.subdirTable[pkg] {
				continue
			}
		}

		target := filepath.Join(baseDir, filepath.Join(nameDirs[1:]...))
		if target == baseDir {
			continue
		}

		if *fVerbose {
			fmt.Println("Writing:", target)
		}

		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			f, err := os.OpenFile(target, os.O_RDWR|os.O_CREATE, os.FileMode(hdr.Mode))
			if err != nil {
				return errors.WithStack(err)
			}
			defer f.Close()

			if _, err := io.Copy(f, files); err != nil {
				return errors.WithStack(err)
			}
			f.Close()
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0777); err != nil && !os.IsExist(err) {
				return errors.WithStack(err)
			}
		case tar.TypeSymlink:
			if err := os.Symlink(target, hdr.Linkname); err != nil {
				return errors.WithStack(err)
			}
		}
		os.Chtimes(target, hdr.AccessTime, hdr.ModTime)
	}

	return nil
}

func (pj *project) dlGit(path string) ([]byte, error) {
	if match := githubRegexp.FindStringSubmatch(path); match != nil {
		return nil, pj.dlGithub(match[1], match[2])
	}

	baseDir := filepath.Join(vendorDir, pj.Name)
	if err := os.MkdirAll(filepath.Dir(baseDir), 0777); err != nil && !os.IsExist(err) {
		return nil, errors.WithStack(err)
	}

	os.RemoveAll(baseDir)

	cloneCmd := exec.Command("git", "clone", path, baseDir)
	if buf, err := cloneCmd.Output(); err != nil {
		return buf, errors.WithStack(err)
	}

	resetCmd := exec.Command("git", "reset", "--hard", pj.Revision)
	resetCmd.Dir = baseDir
	if buf, err := resetCmd.Output(); err != nil {
		return buf, errors.WithStack(err)
	}

	return nil, nil
}

// Codes from: go/src/cmd/go/internal/get/discovery.go

// metaImport represents the parsed <meta name="go-import"
// content="prefix vcs reporoot" /> tags from HTML files.
type metaImport struct {
	Prefix, VCS, RepoRoot string
}

// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// charsetReader returns a reader for the given charset. Currently
// it only supports UTF-8 and ASCII. Otherwise, it returns a meaningful
// error which is printed by go get, so the user can find why the package
// wasn't downloaded if the encoding is not supported. Note that, in
// order to reduce potential errors, ASCII is treated as UTF-8 (i.e. characters
// greater than 0x7f are not rejected).
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(charset) {
	case "ascii":
		return input, nil
	default:
		return nil, fmt.Errorf("can't decode XML document using charset %q", charset)
	}
}

// parseMetaGoImports returns meta imports from the HTML in r.
// Parsing ends at the end of the <head> section or the beginning of the <body>.
func parseMetaGoImports(r io.Reader) (imports []metaImport, err error) {
	d := xml.NewDecoder(r)
	d.CharsetReader = charsetReader
	d.Strict = false
	var t xml.Token
	for {
		t, err = d.RawToken()
		if err != nil {
			if err == io.EOF || len(imports) > 0 {
				err = nil
			}
			return
		}
		if e, ok := t.(xml.StartElement); ok && strings.EqualFold(e.Name.Local, "body") {
			return
		}
		if e, ok := t.(xml.EndElement); ok && strings.EqualFold(e.Name.Local, "head") {
			return
		}
		e, ok := t.(xml.StartElement)
		if !ok || !strings.EqualFold(e.Name.Local, "meta") {
			continue
		}

		// Check go-source for github link
		if attrValue(e.Attr, "name") == "go-source" && len(imports) == 1 {
			if match := githubRegexp.FindStringSubmatch(attrValue(e.Attr, "content")); match != nil {
				imports = []metaImport{{Prefix: imports[0].Prefix, VCS: "git", RepoRoot: match[0]}}
				continue
			}
		}

		if attrValue(e.Attr, "name") != "go-import" {
			continue
		}

		if f := strings.Fields(attrValue(e.Attr, "content")); len(f) == 3 {
			imports = append(imports, metaImport{
				Prefix:   f[0],
				VCS:      f[1],
				RepoRoot: f[2],
			})
		}
	}
}

// attrValue returns the attribute value for the case-insensitive key
// `name', or the empty string if nothing is found.
func attrValue(attrs []xml.Attr, name string) string {
	for _, a := range attrs {
		if strings.EqualFold(a.Name.Local, name) {
			return a.Value
		}
	}
	return ""
}

func getGoImports(path string) (*metaImport, error) {
	resp, err := http.Get(fmt.Sprintf("https://%s?go-get=1", path))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	imports, err := parseMetaGoImports(resp.Body)
	if err != nil {
		return nil, err
	}

	if len(imports) != 1 {
		return nil, fmt.Errorf("Too many imports: %v", imports)
	}
	return &imports[0], nil
}

func (pj *project) download(swg *sizedwaitgroup.SizedWaitGroup) {
	defer swg.Done()

	pj.subdirTable = make(map[string]bool, len(pj.Packages))
	for _, dir := range pj.Packages {
		if dir == "." {
			dir = ""
		}
		pj.subdirTable[dir] = true
	}

	src := pj.Source
	if len(src) == 0 {
		src = pj.Name
	}

	if match := githubRegexp.FindStringSubmatch(src); match != nil {
		fmt.Println("Downloading from github:", pj.Name, "(", src, pj.Revision, ")")
		if err := pj.dlGithub(match[1], match[2]); err != nil {
			panic(err)
		}
		return
	}

	if match := gopkgRegexp.FindStringSubmatch(src); match != nil {
		fmt.Println("Downloading from gopkg:", pj.Name, "(", src, pj.Revision, ")")
		var gitUrl string
		if !strings.HasPrefix(src, "https://") {
			gitUrl = "https://" + src
		}
		if log, err := pj.dlGit(gitUrl); err != nil {
			if log != nil {
				fmt.Fprintln(os.Stderr, string(log))
			}
			fmt.Fprintln(os.Stderr, pj.Name)
			fmt.Fprintf(os.Stderr, "%+v\n", err)
			panic(err)
		}
		return
	}

	meta, err := getGoImports(src)
	if err != nil {
		panic(err)
	}
	if strings.ToLower(meta.VCS) != "git" {
		panic(fmt.Errorf("Unsupported VCS type: %s", meta.VCS))
	}

	fmt.Println("Downloading from go-imports:", pj.Name, "(", meta.RepoRoot, pj.Revision, ")")
	if log, err := pj.dlGit(meta.RepoRoot); err != nil {
		if log != nil {
			fmt.Fprintln(os.Stderr, string(log))
		}
		fmt.Fprintln(os.Stderr, pj.Name)
		fmt.Fprintf(os.Stderr, "%+v\n", err)
		panic(err)
	}
}

func main() {
	flag.Parse()

	if *fCpuprofile != "" {
		f, err := os.Create(*fCpuprofile)
		if err != nil {
			panic(err)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			panic(err)
		}
		defer pprof.StopCPUProfile()
	}

	// target directory would be current directory
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	vendorDir = filepath.Join(wd, "vendor")

	// read Gopkg.lock
	lockfile, err := os.Open(filepath.Join(wd, "Gopkg.lock"))
	if err != nil {
		panic(err)
	}

	var lock lock
	if err := toml.NewDecoder(lockfile).Decode(&lock); err != nil {
		panic(err)
	}

	fmt.Println("Download start:")

	swg := sizedwaitgroup.New(*fParallelism)
	for _, pj := range lock.Projects {
		swg.Add()
		go func(pj *project, swg *sizedwaitgroup.SizedWaitGroup) {
			pj.download(swg)
		}(pj, &swg)
	}
	swg.Wait()

	fmt.Println("Download done.")
}
