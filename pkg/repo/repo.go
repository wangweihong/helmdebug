/*
Copyright 2016 The Kubernetes Authors All rights reserved.

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

package repo // import "k8s.io/helm/pkg/repo"

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"time"

	"github.com/facebookgo/atomicfile"
	"github.com/ghodss/yaml"
)

// ErrRepoOutOfDate indicates that the repository file is out of date, but
// is fixable.
var ErrRepoOutOfDate = errors.New("repository file is out of date")

// RepoFile represents the repositories.yaml file in $HELM_HOME
type RepoFile struct {
	APIVersion   string    `json:"apiVersion"`   //api版本?有什么用?
	Generated    time.Time `json:"generated"`    //repo文件生成的日期
	Repositories []*Entry  `json:"repositories"` //每一项repo的信息
}

// NewRepoFile generates an empty repositories file.
//
// Generated and APIVersion are automatically set.
func NewRepoFile() *RepoFile {
	return &RepoFile{
		APIVersion:   APIVersionV1,
		Generated:    time.Now(),
		Repositories: []*Entry{},
	}
}

// LoadRepositoriesFile takes a file at the given path and returns a RepoFile object
//
// If this returns ErrRepoOutOfDate, it also returns a recovered RepoFile that
// can be saved as a replacement to the out of date file.
//读取指定路径上的repofile
func LoadRepositoriesFile(path string) (*RepoFile, error) {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	r := &RepoFile{}
	err = yaml.Unmarshal(b, r)
	if err != nil {
		return nil, err
	}

	// File is either corrupt, or is from before v2.0.0-Alpha.5
	if r.APIVersion == "" {
		m := map[string]string{}
		if err = yaml.Unmarshal(b, &m); err != nil {
			return nil, err
		}
		r := NewRepoFile()
		for k, v := range m {
			r.Add(&Entry{
				Name:  k,
				URL:   v,
				Cache: fmt.Sprintf("%s-index.yaml", k),
			})
		}
		return r, ErrRepoOutOfDate
	}

	return r, nil
}

// Add adds one or more repo entries to a repo file.
//添加指定项到repofile中
func (r *RepoFile) Add(re ...*Entry) {
	r.Repositories = append(r.Repositories, re...)
}

// Update attempts to replace one or more repo entries in a repo file. If an
// entry with the same name doesn't exist in the repo file it will add it.
func (r *RepoFile) Update(re ...*Entry) {
	for _, target := range re {
		found := false
		for j, repo := range r.Repositories {
			if repo.Name == target.Name {
				r.Repositories[j] = target
				found = true
				break
			}
		}
		if !found {
			r.Add(target)
		}
	}
}

// Has returns true if the given name is already a repository name.
//检测指定的repo名是否已经存在了
func (r *RepoFile) Has(name string) bool {
	for _, rf := range r.Repositories {
		if rf.Name == name {
			return true
		}
	}
	return false
}

// Remove removes the entry from the list of repositories.
//移除repos指定的repo信息
func (r *RepoFile) Remove(name string) bool {
	cp := []*Entry{}
	found := false
	for _, rf := range r.Repositories {
		if rf.Name == name {
			found = true
			continue
		}
		cp = append(cp, rf)
	}
	r.Repositories = cp
	return found
}

// WriteFile writes a repositories file to the given path.
//原子更改path路径文件上的内容为repo
func (r *RepoFile) WriteFile(path string, perm os.FileMode) error {
	//创建一个临时文件
	f, err := atomicfile.New(path, perm)
	if err != nil {
		return err
	}

	data, err := yaml.Marshal(r)
	if err != nil {
		return err
	}

	_, err = f.File.Write(data)
	if err != nil {
		return err
	}

	return f.Close()
}
