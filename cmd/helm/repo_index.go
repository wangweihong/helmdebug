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

package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"

	"k8s.io/helm/pkg/repo"
)

const repoIndexDesc = `
Read the current directory and generate an index file based on the charts found.

This tool is used for creating an 'index.yaml' file for a chart repository. To
set an absolute URL to the charts, use '--url' flag.

To merge the generated index with an existing index file, use the '--merge'
flag. In this case, the charts found in the current directory will be merged
into the existing index, with local charts taking priority over existing charts.
`

type repoIndexCmd struct {
	dir   string //通过参数指定一个目录
	url   string //通过选项--url指定一个url路径
	out   io.Writer
	merge string
}

func newRepoIndexCmd(out io.Writer) *cobra.Command {
	index := &repoIndexCmd{out: out}

	cmd := &cobra.Command{
		Use:   "index [flags] [DIR]",
		Short: "generate an index file given a directory containing packaged charts",
		Long:  repoIndexDesc,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkArgsLength(len(args), "path to a directory"); err != nil {
				return err
			}

			index.dir = args[0]

			return index.run()
		},
	}

	f := cmd.Flags()
	f.StringVar(&index.url, "url", "", "url of chart repository")
	f.StringVar(&index.merge, "merge", "", "merge the generated index into the given index")

	return cmd
}

func (i *repoIndexCmd) run() error {
	path, err := filepath.Abs(i.dir)
	if err != nil {
		return err
	}

	return index(path, i.url, i.merge)
}

//解析指定的chart压缩包,并更新成相应的index文件
func index(dir, url, mergeTo string) error {
	//指定index文件路径
	out := filepath.Join(dir, "index.yaml")

	//解析指定目录下的*.tgz文件为chart,并且添加到IndexFile对象中
	i, err := repo.IndexDirectory(dir, url)
	if err != nil {
		return err
	}
	//如果指定的文件不为空,则合并两个index文件
	if mergeTo != "" {
		i2, err := repo.LoadIndexFile(mergeTo)
		if err != nil {
			return fmt.Errorf("Merge failed: %s", err)
		}
		i.Merge(i2)
	}
	i.SortEntries()
	return i.WriteFile(out, 0755)
}
