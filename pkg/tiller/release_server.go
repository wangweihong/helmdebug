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
	"bytes"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/technosophos/moniker"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"

	"k8s.io/helm/pkg/chartutil"
	"k8s.io/helm/pkg/proto/hapi/chart"
	"k8s.io/helm/pkg/proto/hapi/release"
	"k8s.io/helm/pkg/proto/hapi/services"
	relutil "k8s.io/helm/pkg/releaseutil"
	"k8s.io/helm/pkg/tiller/environment"
	"k8s.io/helm/pkg/timeconv"
	"k8s.io/helm/pkg/version"
)

// releaseNameMaxLen is the maximum length of a release name.
//
// As of Kubernetes 1.4, the max limit on a name is 63 chars. We reserve 10 for
// charts to add data. Effectively, that gives us 53 chars.
// See https://github.com/kubernetes/helm/issues/1528
const releaseNameMaxLen = 53

// NOTESFILE_SUFFIX that we want to treat special. It goes through the templating engine
// but it's not a yaml file (resource) hence can't have hooks, etc. And the user actually
// wants to see this file after rendering in the status command. However, it must be a suffix
// since there can be filepath in front of it.
const notesFileSuffix = "NOTES.txt"

var (
	// errMissingChart indicates that a chart was not provided.
	errMissingChart = errors.New("no chart provided")
	// errMissingRelease indicates that a release (name) was not provided.
	errMissingRelease = errors.New("no release provided")
	// errInvalidRevision indicates that an invalid release revision number was provided.
	errInvalidRevision = errors.New("invalid release revision")
	//errInvalidName indicates that an invalid release name was provided
	errInvalidName = errors.New("invalid release name, must match regex ^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])+$ and the length must not longer than 53")
)

// ListDefaultLimit is the default limit for number of items returned in a list.
var ListDefaultLimit int64 = 512

// ValidName is a regular expression for names.
//
// According to the Kubernetes help text, the regular expression it uses is:
//
//	(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?
//
// We modified that. First, we added start and end delimiters. Second, we changed
// the final ? to + to require that the pattern match at least once. This modification
// prevents an empty string from matching.
var ValidName = regexp.MustCompile("^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])+$")

// ReleaseServer implements the server-side gRPC endpoint for the HAPI services.
type ReleaseServer struct {
	ReleaseModule                          //interface,包含对K8s资源的创建/删除/更新的功能
	env           *environment.Environment //这里面包括kube client //k8s.io/helm/pkg/kube/client.go
	clientset     internalclientset.Interface
	Log           func(string, ...interface{})
}

// NewReleaseServer creates a new release server.
//创建release server,见k8s.io/helm/cmd/tiller/tiller.go
func NewReleaseServer(env *environment.Environment, clientset internalclientset.Interface, useRemote bool) *ReleaseServer {

	//远程Release server,目前仍处于实验性质
	var releaseModule ReleaseModule
	if useRemote {
		releaseModule = &RemoteReleaseModule{} //rudder api?
	} else {
		releaseModule = &LocalReleaseModule{
			clientset: clientset, // k8s的内部客户端
		}
	}

	return &ReleaseServer{
		env:           env,
		clientset:     clientset,
		ReleaseModule: releaseModule,
		Log:           func(_ string, _ ...interface{}) {},
	}
}

// reuseValues copies values from the current release to a new release if the
// new release does not have any values.
//
// If the request already has values, or if there are no values in the current
// release, this does nothing.
//
// This is skipped if the req.ResetValues flag is set, in which case the
// request values are not altered.
func (s *ReleaseServer) reuseValues(req *services.UpdateReleaseRequest, current *release.Release) error {
	if req.ResetValues {
		// If ResetValues is set, we comletely ignore current.Config.
		s.Log("resetting values to the chart's original version")
		return nil
	}

	// If the ReuseValues flag is set, we always copy the old values over the new config's values.
	if req.ReuseValues {
		s.Log("reusing the old release's values")

		// We have to regenerate the old coalesced values:
		oldVals, err := chartutil.CoalesceValues(current.Chart, current.Config)
		if err != nil {
			err := fmt.Errorf("failed to rebuild old values: %s", err)
			s.Log("%s", err)
			return err
		}
		nv, err := oldVals.YAML()
		if err != nil {
			return err
		}
		req.Chart.Values = &chart.Config{Raw: nv}
		return nil
	}

	// If req.Values is empty, but current.Config is not, copy current into the
	// request.
	if (req.Values == nil || req.Values.Raw == "" || req.Values.Raw == "{}\n") &&
		current.Config != nil &&
		current.Config.Raw != "" &&
		current.Config.Raw != "{}\n" {
		s.Log("copying values from %s (v%d) to new release.", current.Name, current.Version)
		req.Values = current.Config
	}
	return nil
}

//生成唯一Release名?对
func (s *ReleaseServer) uniqName(start string, reuse bool) (string, error) {

	// If a name is supplied, we check to see if that name is taken. If not, it
	// is granted. If reuse is true and a deleted release with that name exists,
	// we re-grant it. Otherwise, an error is returned.
	//如果指定了release名
	if start != "" {

		//默认release名不超过53
		if len(start) > releaseNameMaxLen {
			return "", fmt.Errorf("release name %q exceeds max length of %d", start, releaseNameMaxLen)
		}

		//找到指定release名的历史
		h, err := s.env.Releases.History(start)
		//release不存在
		if err != nil || len(h) < 1 {
			//可以使用默认名
			return start, nil
		}
		relutil.Reverse(h, relutil.SortByRevision)
		rel := h[0]

		//如果reuse标志设置了,release已经删除,或者处于失败状态,则重用该release名
		if st := rel.Info.Status.Code; reuse && (st == release.Status_DELETED || st == release.Status_FAILED) {
			// Allowe re-use of names if the previous release is marked deleted.
			s.Log("name %s exists but is not in use, reusing name", start)
			return start, nil
		} else if reuse {
			return "", errors.New("cannot re-use a name that is still in use")
		}

		return "", fmt.Errorf("a release named %s already exists.\nRun: helm ls --all %s; to check the status of the release\nOr run: helm del --purge %s; to delete it", start, start, start)
	}

	//重复5次,随机生成release名
	//测试
	maxTries := 5
	for i := 0; i < maxTries; i++ {
		//利用moniker生成新的release名
		namer := moniker.New()
		name := namer.NameSep("-")
		if len(name) > releaseNameMaxLen {
			name = name[:releaseNameMaxLen]
		}
		//检测指定的release名是否已经存在
		if _, err := s.env.Releases.Get(name, 1); strings.Contains(err.Error(), "not found") {
			return name, nil
		}
		s.Log("info: generated name %s is taken. Searching again.", name)
	}
	s.Log("warning: No available release names found after %d tries", maxTries)
	return "ERROR", errors.New("no available release name found")
}

//获取模板引擎
func (s *ReleaseServer) engine(ch *chart.Chart) environment.Engine {
	//获得默认的模板引擎
	renderer := s.env.EngineYard.Default()
	//如果指定了模板引擎,替换默认的模板引擎
	if ch.Metadata.Engine != "" {
		if r, ok := s.env.EngineYard.Get(ch.Metadata.Engine); ok {
			renderer = r
		} else {
			s.Log("warning: %s requested non-existent template engine %s", ch.Metadata.Name, ch.Metadata.Engine)
		}
	}
	return renderer
}

// capabilities builds a Capabilities from discovery information.
//获得k8s server的版本号,支持的api版本集,以及tiller版本号
func capabilities(disc discovery.DiscoveryInterface) (*chartutil.Capabilities, error) {
	//获得k8s server的版本号
	sv, err := disc.ServerVersion()
	if err != nil {
		return nil, err
	}
	//获得k8s服务器支持的api版本集
	vs, err := GetVersionSet(disc)
	if err != nil {
		return nil, fmt.Errorf("Could not get apiVersions from Kubernetes: %s", err)
	}
	return &chartutil.Capabilities{
		APIVersions:   vs,
		KubeVersion:   sv,
		TillerVersion: version.GetVersionProto(),
	}, nil
}

// GetVersionSet retrieves a set of available k8s API versions
//获取K8s支持api版本组
func GetVersionSet(client discovery.ServerGroupsInterface) (chartutil.VersionSet, error) {
	groups, err := client.ServerGroups() //???api组?
	if err != nil {
		return chartutil.DefaultVersionSet, err
	}

	// FIXME: The Kubernetes test fixture for cli appears to always return nil
	// for calls to Discovery().ServerGroups(). So in this case, we return
	// the default API list. This is also a safe value to return in any other
	// odd-ball case.
	if groups == nil {
		return chartutil.DefaultVersionSet, nil
	}

	versions := metav1.ExtractGroupVersions(groups)
	//创建新的版本集
	return chartutil.NewVersionSet(versions...), nil
}

//解析chart,得到chart钩子,manifest描述,注释
// 注意这里只检查了k8s 资源的版本是否被支持,并没有检测资源描述是否完全合法
func (s *ReleaseServer) renderResources(ch *chart.Chart, values chartutil.Values, vs chartutil.VersionSet) ([]*release.Hook, *bytes.Buffer, string, error) {
	// Guard to make sure Tiller is at the right version to handle this chart.
	//获得tiller当前的版本
	sver := version.GetVersion()
	if ch.Metadata.TillerVersion != "" &&
		!version.IsCompatibleRange(ch.Metadata.TillerVersion, sver) {
		return nil, nil, "", fmt.Errorf("Chart incompatible with Tiller %s", sver)
	}

	s.Log("rendering %s chart using values", ch.GetMetadata().Name)
	//获得模板引擎
	renderer := s.engine(ch)
	//利用模板引擎解析chart
	files, err := renderer.Render(ch, values)
	if err != nil {
		return nil, nil, "", err
	}

	// NOTES.txt gets rendered like all the other files, but because it's not a hook nor a resource,
	// pull it out of here into a separate file so that we can actually use the output of the rendered
	// text file. We have to spin through this map because the file contains path information, so we
	// look for terminating NOTES.txt. We also remove it from the files so that we don't have to skip
	// it in the sortHooks.
	notes := ""
	//移除注释的文件
	for k, v := range files {
		if strings.HasSuffix(k, notesFileSuffix) {
			// Only apply the notes if it belongs to the parent chart
			// Note: Do not use filePath.Join since it creates a path with \ which is not expected
			if k == path.Join(ch.Metadata.Name, "templates", notesFileSuffix) {
				notes = v
			}
			delete(files, k)
		}
	}

	// Sort hooks, manifests, and partials. Only hooks and manifests are returned,
	// as partials are not used after renderer.Render. Empty manifests are also
	// removed here.
	//vs是k8s server支持的api版本组,解析所有的描述k8s资源文件,解析成manifest对象
	hooks, manifests, err := sortManifests(files, vs, InstallOrder)
	if err != nil {
		// By catching parse errors here, we can prevent bogus releases from going
		// to Kubernetes.
		//
		// We return the files as a big blob of data to help the user debug parser
		// errors.
		b := bytes.NewBuffer(nil)
		for name, content := range files {
			if len(strings.TrimSpace(content)) == 0 {
				continue
			}
			b.WriteString("\n---\n# Source: " + name + "\n")
			b.WriteString(content)
		}
		return nil, b, "", err
	}

	// Aggregate all valid manifests into one big doc.
	//将manifest存放在buffer中
	b := bytes.NewBuffer(nil)
	for _, m := range manifests {
		b.WriteString("\n---\n# Source: " + m.name + "\n")
		b.WriteString(m.content)
	}

	return hooks, b, notes, nil
}

func (s *ReleaseServer) recordRelease(r *release.Release, reuse bool) {
	if reuse {
		if err := s.env.Releases.Update(r); err != nil {
			s.Log("warning: Failed to update release %s: %s", r.Name, err)
		}
	} else if err := s.env.Releases.Create(r); err != nil {
		s.Log("warning: Failed to record release %s: %s", r.Name, err)
	}
}

func (s *ReleaseServer) execHook(hs []*release.Hook, name, namespace, hook string, timeout int64) error {
	kubeCli := s.env.KubeClient
	code, ok := events[hook]
	if !ok {
		return fmt.Errorf("unknown hook %s", hook)
	}

	s.Log("executing %d %s hooks for %s", len(hs), hook, name)
	executingHooks := []*release.Hook{}
	for _, h := range hs {
		for _, e := range h.Events {
			if e == code {
				executingHooks = append(executingHooks, h)
			}
		}
	}

	executingHooks = sortByHookWeight(executingHooks)

	for _, h := range executingHooks {

		b := bytes.NewBufferString(h.Manifest)
		if err := kubeCli.Create(namespace, b, timeout, false); err != nil {
			s.Log("warning: Release %s %s %s failed: %s", name, hook, h.Path, err)
			return err
		}
		// No way to rewind a bytes.Buffer()?
		b.Reset()
		b.WriteString(h.Manifest)
		if err := kubeCli.WatchUntilReady(namespace, b, timeout, false); err != nil {
			s.Log("warning: Release %s %s %s could not complete: %s", name, hook, h.Path, err)
			return err
		}
		h.LastRun = timeconv.Now()
	}

	s.Log("hooks complete for %s %s", hook, name)
	return nil
}

//检测解析后的k8s resource manifest是否有效
func validateManifest(c environment.KubeClient, ns string, manifest []byte) error {
	r := bytes.NewReader(manifest)
	info, err := c.BuildUnstructured(ns, r)
	//输出信息如
	/*name:
	needled-hamster-wordpress
	namespace:
	default
	source:

	versionObject:
	 &{map[kind:Service metadata:map[labels:map[app:needled-hamster-wordpress chart:wordpress-0.6.8 heritage:Tiller release:needled-hamster] name:needled-hamster-wordpress namespace:default] spec:map[ports:[map[name:http port:80 targetPort:http] map[targetPort:https name:https port:443]] selector:map[app:needled-hamster-wordpress] type:LoadBalancer] apiVersion:v1]}
	 object:
	  &{map[kind:Service metadata:map[labels:map[heritage:Tiller release:needled-hamster app:needled-hamster-wordpress chart:wordpress-0.6.8] name:needled-hamster-wordpress namespace:default] spec:map[ports:[map[name:http port:80 targetPort:http] map[name:https port:443 targetPort:https]] selector:map[app:needled-hamster-wordpress] type:LoadBalancer] apiVersion:v1]}
	*/
	fmt.Println("\n\n===================validate Manifests==============")
	for _, v := range info {
		fmt.Println("name:\n" + v.Name)
		fmt.Println("namespace:\n" + v.Namespace)
		fmt.Println("source:\n" + v.Source)
		fmt.Println("versionObject:\n", v.VersionedObject)
		fmt.Println("object:\n", v.Object)
		fmt.Println("\n\n=================================")
	}
	return err
}

//检测release名是否有效
func validateReleaseName(releaseName string) error {

	if releaseName == "" {
		return errMissingRelease
	}

	//检测名字是否正则匹配以及长度未超过
	if !ValidName.MatchString(releaseName) || (len(releaseName) > releaseNameMaxLen) {
		return errInvalidName
	}

	return nil
}
