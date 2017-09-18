// Copyright 2017 orijtech Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"text/template"
	"time"

	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing/object"

	"github.com/odeke-em/semalim"
)

var regGo = regexp.MustCompile(".*\\.go$")

func goLikeFile(path string, fi os.FileInfo) bool {
	return fi != nil && fi.Mode().IsRegular() && regGo.Match([]byte(path)) && !strings.Contains(path, "vendor/") && !strings.HasSuffix(path, "doc.go")
}

var blankTime time.Time

func main() {
	log.SetFlags(0)
	var goRepo string
	var fixIt bool
	var copyrightHolder string
	var concurrency uint
	var tmplStr string

	flag.StringVar(&goRepo, "repo", "github.com/orijtech/apache2conform", "the go repo to use")
	flag.StringVar(&tmplStr, "tmpl", "apache2.0", "the license to use, options are: apache2.0, BSD")
	flag.BoolVar(&fixIt, "fix", false, "whether to add the headers")
	flag.StringVar(&copyrightHolder, "copyright-holder", "ACME", "the name of the copyright holder")
	flag.UintVar(&concurrency, "concurrency", 6, "controls how many files can be opened at once")
	flag.Parse()

	startTime := time.Now()
	defer func() {
		fmt.Printf("\nTimeSpent: %s\n", time.Now().Sub(startTime))
	}()

	var tmpl *template.Template
	switch strings.ToLower(tmplStr) {
	case "bsd":
		tmpl = shortBSDTempl
	default:
		tmpl = shortApache2Point0Templ
	}

	dirPath := os.ExpandEnv(filepath.Join("$GOPATH", "src", goRepo))
	repo, err := git.PlainOpen(dirPath)
	if err != nil {
		log.Fatal(err)
	}

	head, err := repo.Head()
	if err != nil {
		log.Fatal(err)
	}
	// First step here is to find the head hash
	refHash := head.Hash()
	// Start sifting through all the files
	headCommit, err := object.GetCommit(repo.Storer, refHash)
	if err != nil {
		log.Fatalf("failed to get headCommit: %v", err)
	}

	jobsChan := make(chan semalim.Job)
	go func() {
		defer close(jobsChan)
		goFiles := siftThroughFiles(dirPath, goLikeFile)
		for goFile := range goFiles {
			jobsChan <- &licenseConformer{
				dirPath:    dirPath,
				holder:     copyrightHolder,
				fixIt:      fixIt,
				filePath:   goFile,
				headCommit: headCommit,
				tmpl:       tmpl,
			}
		}
	}()

	resChan := semalim.Run(jobsChan, uint64(concurrency))
	nTotal := uint64(0)
	nGood := uint64(0)
	nBad := uint64(0)
	nAddLicense := uint64(0)
	for res := range resChan {
		added, err, path := res.Value().(bool), res.Err(), res.Id().(string)
		if added {
			nAddLicense += 1
		} else if err != nil {
			log.Printf("err:: %q: %v", path, err)
			nBad += 1
		} else {
			nGood += 1
		}
		nTotal += 1
		fmt.Printf("Total: %d:: AddedLicenses: %d AlreadyHaveLicenses: %d Errors: %d\r",
			nTotal, nAddLicense, nGood, nBad)

	}
}

type licenseConformer struct {
	holder     string
	dirPath    string
	filePath   string
	fixIt      bool
	headCommit *object.Commit
	tmpl       *template.Template
}

var _ semalim.Job = (*licenseConformer)(nil)

func (lc *licenseConformer) Id() interface{} { return lc.filePath }

func (lc *licenseConformer) Do() (res interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			stack := make([]byte, 1024*4)
			runtime.Stack(stack, true)
			err = fmt.Errorf("%s", stack)
		}
	}()
	res = false

	goFile := lc.filePath
	fixIt := lc.fixIt
	headCommit := lc.headCommit
	copyrightHolder := lc.holder
	dirPath := lc.dirPath

	sniff, f, potentiallyConformsToLicense, err := sniffIfHasLicense(goFile, containsALicense)
	if err != nil {
		if f != nil {
			f.Close()
		}
		return false, err
	}

	if potentiallyConformsToLicense || autoGenerated(sniff) {
		// Well good, move onto the next one
		f.Close()
		return false, nil
	}

	relToRootPath, _ := filepath.Rel(dirPath, goFile)
	if err != nil {
		return false, err
	}
	blame, err := git.Blame(headCommit, relToRootPath)
	if err != nil {
		return false, err
	}
	// Next step is to run gitBlame and figure out
	// the earliest date of addition of the file
	earliestTime := time.Now()
	for _, line := range blame.Lines {
		if commitTime := line.When; commitTime.After(blankTime) && commitTime.Before(earliestTime) {
			earliestTime = commitTime
		}
	}
	canEdit := fixIt && earliestTime.After(blankTime)
	if !canEdit {
		return false, nil
	}
	buf := new(bytes.Buffer)
	info := &copyright{
		Year: earliestTime.Year(),

		Holder: copyrightHolder,
	}
	if err := lc.tmpl.Execute(buf, info); err != nil {
		return false, err
	}
	// Next step is to concatenate the (license, sniff, rest)
	wholeFileWithLicense, err := ioutil.ReadAll(io.MultiReader(
		buf,
		bytes.NewReader(sniff),
		f,
	))
	_ = f.Close()
	if err != nil {
		return false, err
	}
	// Now write the properly licensed file to disk
	wf, err := os.Create(goFile)
	if err != nil {
		return false, err
	}
	wf.Write(wholeFileWithLicense)
	wf.Close()
	return true, nil
}

type copyright struct {
	Year int

	Holder string
}

var apacheLicenseURL = []byte("http://www.apache.org/licenses/LICENSE-2.0")
var doNotEdit = []byte("DO NOT EDIT!")
var allRightsReservedLower = []byte("all rights reserved")

func containsALicense(b []byte) bool {
	return bytes.Contains(bytes.ToLower(b), allRightsReservedLower) || bytes.Contains(b, apacheLicenseURL)
}

func autoGenerated(b []byte) bool { return bytes.Contains(b, doNotEdit) }

func sniffIfHasLicense(p string, contains func([]byte) bool) ([]byte, io.ReadCloser, bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, nil, false, err
	}

	headerBlob := make([]byte, approxShortHeaderSize)
	if _, err := io.ReadAtLeast(f, headerBlob, 1); err != nil {
		return nil, nil, false, err
	}
	return headerBlob, f, contains(headerBlob), nil
}

func siftThroughFiles(root string, match func(string, os.FileInfo) bool) chan string {
	filesChan := make(chan string)
	go func() {
		defer close(filesChan)
		filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
			if err == nil && match(path, fi) {
				filesChan <- path
			}
			return err
		})
	}()
	return filesChan
}

const approxShortHeaderSize = 624

var shortBSD = `// Copyright {{.Year}} {{.Holder}}. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

`

var shortApache2Point0 = `// Copyright {{.Year}} {{.Holder}}. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

`
var shortApache2Point0Templ = template.Must(template.New("apache2.0").Parse(shortApache2Point0))
var shortBSDTempl = template.Must(template.New("BSD").Parse(shortBSD))
