/* Copyright 2017 The Bazel Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package golang

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/rule"
	toml "github.com/pelletier/go-toml"
)

const (
	defaultGoProxyBase = "https://proxy.golang.org"
)

type depLockFile struct {
	Projects []depProject `toml:"projects"`
}

type depProject struct {
	Name     string `toml:"name"`
	Revision string `toml:"revision"`
	Source   string `toml:"source"`
}

func importReposFromDep(args language.ImportReposArgs) language.ImportReposResult {
	goProxyBase := args.GoProxy
	if goProxyBase == "" {
		goProxyBase = defaultGoProxyBase
	}

	data, err := ioutil.ReadFile(args.Path)
	if err != nil {
		return language.ImportReposResult{Error: err}
	}
	var file depLockFile
	if err := toml.Unmarshal(data, &file); err != nil {
		return language.ImportReposResult{Error: err}
	}

	gen := make([]*rule.Rule, len(file.Projects))
	var wg sync.WaitGroup
	for i, p := range file.Projects {
		wg.Add(1)
		go func(i int, p depProject) {
			gen[i] = rule.NewRule("go_repository", label.ImportPathToBazelRepoName(p.Name))
			gen[i].SetAttr("importpath", p.Name)
			if ok, err := path.Match(args.GoPrivate, p.Name); ok || err != nil {
				gen[i].SetAttr("commit", p.Revision)
				if p.Source != "" {
					// TODO(#411): Handle source directives correctly. It may be an import
					// path, or a URL. In the case of an import path, we should resolve it
					// to the correct remote and vcs. In the case of a URL, we should
					// correctly determine what VCS to use (the URL will usually start
					// with "https://", which is used by multiple VCSs).
					gen[i].SetAttr("remote", p.Source)
					gen[i].SetAttr("vcs", "git")
				}
			} else {
				// Goproxy sometimes returns 410 even though the commit exists. Retry a few
				// times for the fetch to succeed.
				var err error
				for attempt := 0; attempt < 5; attempt++ {
					err = ruleUsingGoProxy(goProxyBase, p, gen[i])
					if err == nil {
						break
					}
					if attempt == 4 {
						panic(err)
					}
					time.Sleep(5 * time.Second)
				}
			}
			wg.Done()
		}(i, p)
	}
	wg.Wait()
	sort.SliceStable(gen, func(i, j int) bool {
		return gen[i].Name() < gen[j].Name()
	})

	return language.ImportReposResult{Gen: gen}
}

func ruleUsingGoProxy(goProxyBase string, project depProject, r *rule.Rule) error {
	name := project.Name
	if project.Source != "" {
		name = project.Source
		if strings.HasSuffix(name, ".git") {
			name = name[:len(name)-len(".git")]
		}
		if strings.HasPrefix(name, "https://") {
			name = name[len("https://"):]
		}
	}
	name = strings.ToLower(name)

	infoURL := fmt.Sprintf("%s/%s/@v/%s.info", goProxyBase, name, project.Revision)
	resp, err := http.Get(infoURL)
	if err != nil {
		return fmt.Errorf("failed to fetch info for %s@%s: %v", name, project.Revision, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to fetch info for %s@%s: %v", name, project.Revision, resp.Status)
	}

	info := struct{ Version string }{}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return fmt.Errorf("failed to decode response for %s@%s: %v", name, project.Revision, err)
	}

	zipURL := fmt.Sprintf("%s/%s/@v/%s.zip", goProxyBase, name, info.Version)
	resp, err = http.Get(zipURL)
	if err != nil {
		return fmt.Errorf("failed to fetch zip for %s@%s: %v", name, project.Revision, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to fetch zip for %s@%s: %v", name, project.Revision, resp.Status)
	}

	h := sha256.New()
	if _, err := io.Copy(h, resp.Body); err != nil {
		return fmt.Errorf("failed to hash zip for %s@%s: %v", name, project.Revision, err)
	}
	fmt.Printf("%s@%s: %x\n", name, project.Revision, h.Sum(nil))

	r.SetAttr("urls", []string{zipURL})
	r.SetAttr("sha256", fmt.Sprintf("%x", h.Sum(nil)))
	r.SetAttr("strip_prefix", fmt.Sprintf("%s@%s", name, info.Version))

	return nil
}
