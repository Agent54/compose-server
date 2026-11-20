/*
   Copyright 2020 Docker Compose CLI authors

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

package compose

import (
	"io"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/streams"
	"github.com/spf13/cobra"
	"go.uber.org/mock/gomock"
	"gotest.tools/v3/assert"

	"github.com/docker/compose/v5/pkg/api"
	composepkg "github.com/docker/compose/v5/pkg/compose"
	"github.com/docker/compose/v5/pkg/mocks"
)

func TestNoClassicBuilderFlag(t *testing.T) {
	saveGlobalState(t)
	t.Setenv("NO_COLOR", "")
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "default", want: "the classic builder doesn't support privileged mode"},
		{name: "disabled", args: []string{"--no-classic-builder"}, want: "BuildKit is required by --no-classic-builder"},
		{name: "explicit false", args: []string{"--no-classic-builder=false"}, want: "the classic builder doesn't support privileged mode"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cli := mocks.NewMockCli(gomock.NewController(t))
			cli.EXPECT().Out().Return(streams.NewOut(io.Discard)).AnyTimes()
			cli.EXPECT().Err().Return(streams.NewOut(io.Discard)).AnyTimes()
			cli.EXPECT().In().Return(streams.NewIn(nil)).AnyTimes()
			cli.EXPECT().BuildKitEnabled().Return(false, nil)
			options := &BackendOptions{}
			root := RootCommand(cli, options)
			root.AddCommand(&cobra.Command{
				Use: "probe",
				RunE: func(cmd *cobra.Command, _ []string) error {
					backend, err := composepkg.NewComposeService(cli, options.Options...)
					assert.NilError(t, err)
					project := &types.Project{Name: "test", Services: types.Services{
						"app": {Name: "app", Build: &types.BuildConfig{Context: ".", Privileged: true}},
					}}
					return backend.Build(cmd.Context(), project, api.BuildOptions{})
				},
			})
			docker := &cobra.Command{Use: "docker", TraverseChildren: true, SilenceErrors: true, SilenceUsage: true}
			docker.AddCommand(root)
			args := append([]string{"compose", "--ansi", "always", "--progress", "quiet", "--project-directory", t.TempDir()}, test.args...)
			docker.SetArgs(append(args, "probe"))
			assert.ErrorContains(t, docker.ExecuteContext(t.Context()), test.want)
		})
	}
}

func TestFilterServices(t *testing.T) {
	p := &types.Project{
		Services: types.Services{
			"foo": {
				Name:  "foo",
				Links: []string{"bar"},
			},
			"bar": {
				Name: "bar",
				DependsOn: map[string]types.ServiceDependency{
					"zot": {},
				},
			},
			"zot": {
				Name: "zot",
			},
			"qix": {
				Name: "qix",
			},
		},
	}
	p, err := p.WithSelectedServices([]string{"bar"})
	assert.NilError(t, err)

	assert.Equal(t, len(p.Services), 2)
	_, err = p.GetService("bar")
	assert.NilError(t, err)
	_, err = p.GetService("zot")
	assert.NilError(t, err)
}
