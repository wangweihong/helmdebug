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

package chartutil

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/ghodss/yaml"

	"k8s.io/helm/pkg/proto/hapi/chart"
)

// ApiVersionV1 is the API version number for version 1.
//
// This is ApiVersionV1 instead of APIVersionV1 to match the protobuf-generated name.
const ApiVersionV1 = "v1"

// UnmarshalChartfile takes raw Chart.yaml data and unmarshals it.
//解析Chart.yaml文件成chart的元数据
func UnmarshalChartfile(data []byte) (*chart.Metadata, error) {
	y := &chart.Metadata{}
	err := yaml.Unmarshal(data, y)
	if err != nil {
		return nil, err
	}
	return y, nil
}

// LoadChartfile loads a Chart.yaml file into a *chart.Metadata.
//加载Chart.yaml文件,并且解析成chart元数据
func LoadChartfile(filename string) (*chart.Metadata, error) {
	b, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	return UnmarshalChartfile(b)
}

// SaveChartfile saves the given metadata as a Chart.yaml file at the given path.
//
// 'filename' should be the complete path and filename ('foo/Chart.yaml')
//保存Chart元数据到指定的文件中
func SaveChartfile(filename string, cf *chart.Metadata) error {
	out, err := yaml.Marshal(cf)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(filename, out, 0644)
}

// IsChartDir validate a chart directory.
//
// Checks for a valid Chart.yaml.
//检测目录是否是Chart目录,里面的chart.yaml文件是否正确
func IsChartDir(dirName string) (bool, error) {
	if fi, err := os.Stat(dirName); err != nil {
		return false, err
	} else if !fi.IsDir() {
		return false, fmt.Errorf("%q is not a directory", dirName)
	}

	//确认Chart.yaml文件是否存在
	chartYaml := filepath.Join(dirName, "Chart.yaml")
	if _, err := os.Stat(chartYaml); os.IsNotExist(err) {
		return false, fmt.Errorf("no Chart.yaml exists in directory %q", dirName)
	}

	//读取chart.yaml文件内容
	chartYamlContent, err := ioutil.ReadFile(chartYaml)
	if err != nil {
		return false, fmt.Errorf("cannot read Chart.Yaml in directory %q", dirName)
	}

	//解析Chart.yaml文件内容
	chartContent, err := UnmarshalChartfile(chartYamlContent)
	if err != nil {
		return false, err
	}
	if chartContent == nil {
		return false, errors.New("chart metadata (Chart.yaml) missing")
	}
	if chartContent.Name == "" {
		return false, errors.New("invalid chart (Chart.yaml): name must not be empty")
	}

	return true, nil
}
