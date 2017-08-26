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

package tiller

import (
	"fmt"
	"log"
	"path"
	"strconv"
	"strings"

	"github.com/ghodss/yaml"

	"k8s.io/helm/pkg/chartutil"
	"k8s.io/helm/pkg/hooks"
	"k8s.io/helm/pkg/proto/hapi/release"
	util "k8s.io/helm/pkg/releaseutil"
)

var events = map[string]release.Hook_Event{
	hooks.PreInstall:         release.Hook_PRE_INSTALL,
	hooks.PostInstall:        release.Hook_POST_INSTALL,
	hooks.PreDelete:          release.Hook_PRE_DELETE,
	hooks.PostDelete:         release.Hook_POST_DELETE,
	hooks.PreUpgrade:         release.Hook_PRE_UPGRADE,
	hooks.PostUpgrade:        release.Hook_POST_UPGRADE,
	hooks.PreRollback:        release.Hook_PRE_ROLLBACK,
	hooks.PostRollback:       release.Hook_POST_ROLLBACK,
	hooks.ReleaseTestSuccess: release.Hook_RELEASE_TEST_SUCCESS,
	hooks.ReleaseTestFailure: release.Hook_RELEASE_TEST_FAILURE,
}

// manifest represents a manifest file, which has a name and some content.
//见mainfestFile.sort
type manifest struct {
	name    string           //manifest文件路径
	content string           //k8s资源完整描述
	head    *util.SimpleHead //k8s资源的通用头部,将yaml解析到该头部
}

type result struct {
	hooks   []*release.Hook //manifest中注册的钩子
	generic []manifest      //解析后的k8s资源
}

type manifestFile struct {
	entries map[string]string    //manifest文件获得的资源项.一个文件中可能有多个资源
	path    string               //manifest文件路径
	apis    chartutil.VersionSet //支持的k8s api版本号,用来检测解析后的k8s资源是否支持
}

// sortManifests takes a map of filename/YAML contents, splits the file
// by manifest entries, and sorts the entries into hook types.
//
// The resulting hooks struct will be populated with all of the generated hooks.
// Any file that does not declare one of the hook types will be placed in the
// 'generic' bucket.
//
// Files that do not parse into the expected format are simply placed into a map and
// returned.
func sortManifests(files map[string]string, apis chartutil.VersionSet, sort SortOrder) ([]*release.Hook, []manifest, error) {
	result := &result{}

	//遍历所有的manifest文件
	for filePath, c := range files {

		// Skip partials. We could return these as a separate map, but there doesn't
		// seem to be any need for that at this time.
		//过滤掉_helpers.tpl类似文件
		if strings.HasPrefix(path.Base(filePath), "_") {
			continue
		}
		// Skip empty files and log this.
		//过滤掉空文件
		if len(strings.TrimSpace(c)) == 0 {
			log.Printf("info: manifest %q is empty. Skipping.", filePath)
			continue
		}

		manifestFile := &manifestFile{
			entries: util.SplitManifests(c), //因为一个文件中可能有多个resource.比如secrets.yaml中有多个secret
			path:    filePath,               //resource文件路径
			apis:    apis,                   //支持的k8s api版本.如extensions/v1beta1等
		}

		if err := manifestFile.sort(result); err != nil {
			return result.hooks, result.generic, err
		}
	}

	return result.hooks, sortByKind(result.generic, sort), nil
}

// sort takes a manifestFile object which may contain multiple resource definition
// entries and sorts each entry by hook types, and saves the resulting hooks and
// generic manifests (or non-hooks) to the result struct.
//
// To determine hook type, it looks for a YAML structure like this:
//
//  kind: SomeKind
//  apiVersion: v1
// 	metadata:
//		annotations:
//			helm.sh/hook: pre-install
//
//遍历一份mainfest文件中所有的描述k8s资源的项,转换成简单k8s资源头部,检测该项资源是否被支持.
//并创建成manifest对象
func (file *manifestFile) sort(result *result) error {
	//遍历一个manifest文件中的所有资源
	for _, m := range file.entries {
		var entry util.SimpleHead
		//解压成k8s资源的简单头部
		err := yaml.Unmarshal([]byte(m), &entry)

		if err != nil {
			e := fmt.Errorf("YAML parse error on %s: %s", file.path, err)
			return e
		}

		//见则该项资源的k8s api版本是否被支持
		if entry.Version != "" && !file.apis.Has(entry.Version) {
			return fmt.Errorf("apiVersion %q in %s is not available", entry.Version, file.path)
		}

		//如果没有annotation,说明没有helm hook
		if !hasAnyAnnotation(entry) {
			//将manifest对象添加到结果中
			result.generic = append(result.generic, manifest{
				name:    file.path,
				content: m,
				head:    &entry,
			})
			continue
		}

		//检测有没有钩子
		hookTypes, ok := entry.Metadata.Annotations[hooks.HookAnno]
		if !ok {
			result.generic = append(result.generic, manifest{
				name:    file.path,
				content: m,
				head:    &entry,
			})
			continue
		}

		//计算钩子权重
		hw := calculateHookWeight(entry)

		h := &release.Hook{
			Name:     entry.Metadata.Name,
			Kind:     entry.Kind,
			Path:     file.path,
			Manifest: m,
			Events:   []release.Hook_Event{},
			Weight:   hw,
		}

		isKnownHook := false
		for _, hookType := range strings.Split(hookTypes, ",") {
			hookType = strings.ToLower(strings.TrimSpace(hookType))
			e, ok := events[hookType]
			if ok {
				isKnownHook = true
				h.Events = append(h.Events, e)
			}
		}

		if !isKnownHook {
			log.Printf("info: skipping unknown hook: %q", hookTypes)
			continue
		}

		result.hooks = append(result.hooks, h)
	}

	return nil
}

func hasAnyAnnotation(entry util.SimpleHead) bool {
	if entry.Metadata == nil ||
		entry.Metadata.Annotations == nil ||
		len(entry.Metadata.Annotations) == 0 {
		return false
	}

	return true
}

func calculateHookWeight(entry util.SimpleHead) int32 {
	hws, _ := entry.Metadata.Annotations[hooks.HookWeightAnno]
	hw, err := strconv.Atoi(hws)
	if err != nil {
		hw = 0
	}

	return int32(hw)
}
