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

package serve

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

func TestCheckoutGitArguments(t *testing.T) {
	tests := []struct {
		name   string
		url    string
		ghPath string
		auth   bool
	}{
		{name: "GitHub", url: "https://github.com/acme/private.git", ghPath: "/bin/gh", auth: true},
		{name: "case insensitive", url: "https://GitHub.com/acme/private.git", ghPath: "/bin/gh", auth: true},
		{name: "HTTPS port", url: "https://github.com:443/acme/private.git", ghPath: "/bin/gh", auth: true},
		{name: "no CLI", url: "https://github.com/acme/public.git"},
		{name: "other host", url: "https://example.com/acme/private.git", ghPath: "/bin/gh"},
		{name: "lookalike", url: "https://github.com.example.com/acme/private.git", ghPath: "/bin/gh"},
		{name: "subdomain", url: "https://api.github.com/acme/private.git", ghPath: "/bin/gh"},
		{name: "other port", url: "https://github.com:8443/acme/private.git", ghPath: "/bin/gh"},
		{name: "HTTP", url: "http://github.com/acme/private.git", ghPath: "/bin/gh"},
		{name: "userinfo", url: "https://user@github.com/acme/private.git", ghPath: "/bin/gh"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := checkoutGitArguments(test.url, "destination", 25, test.ghPath)
			want := []string{"-c", "credential.helper=", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always"}
			if test.auth {
				want = append(want, "-c", "credential.https://github.com.helper=!'/bin/gh' auth git-credential")
				want = append(want, "clone", "--depth=25", "--", "https://github.com/acme/private.git", "destination")
			} else {
				want = append(want, "clone", "--depth=25", "--", test.url, "destination")
			}
			assert.DeepEqual(t, args, want)
		})
	}
}

func TestCheckoutGitHubCredentialHelper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX shell")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is required for credential helper integration tests")
	}
	// Exercise Git's actual helper protocol, including quoting paths with spaces
	// and apostrophes. No real credential store or network is used by this test.
	helperDir := filepath.Join(t.TempDir(), "GitHub CLI's directory")
	assert.NilError(t, os.Mkdir(helperDir, 0o700))
	ghPath := filepath.Join(helperDir, "gh")
	assert.NilError(t, os.WriteFile(ghPath, []byte(`#!/bin/sh
test "$1" = auth && test "$2" = git-credential && test "$3" = get || exit 1
test "$GH_PROMPT_DISABLED" = 1 && test "$GIT_TERMINAL_PROMPT" = 0 || exit 1
cat > "$CHECKOUT_HELPER_MARKER"
printf 'username=checkout-user\npassword=checkout-test-token\n'
`), 0o700))

	for _, test := range []struct {
		name     string
		protocol string
		host     string
		auth     bool
	}{
		{name: "GitHub", protocol: "https", host: "github.com", auth: true},
		{name: "HTTPS port", protocol: "https", host: "github.com:443", auth: true},
		{name: "other host", protocol: "https", host: "example.com"},
		{name: "lookalike", protocol: "https", host: "github.com.example.com"},
		{name: "other port", protocol: "https", host: "github.com:8443"},
		{name: "HTTP", protocol: "http", host: "github.com"},
	} {
		t.Run(test.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "helper-input")
			args := checkoutGitArguments("https://github.com/acme/private.git", "unused", 100, ghPath)
			args = append(args[:slices.Index(args, "clone")], "credential", "fill")
			cmd := exec.CommandContext(t.Context(), gitPath, args...)
			cmd.Dir = t.TempDir()
			cmd.Env = append(checkoutGitEnvironment(), "CHECKOUT_HELPER_MARKER="+marker)
			cmd.Stdin = strings.NewReader("protocol=" + test.protocol + "\nhost=" + test.host + "\n\n")
			output, err := cmd.CombinedOutput()
			if test.auth {
				assert.NilError(t, err, string(output))
				assert.Assert(t, strings.Contains(string(output), "password=checkout-test-token"))
				input, err := os.ReadFile(marker)
				assert.NilError(t, err)
				assert.Assert(t, strings.Contains(string(input), "host="+test.host))
			} else {
				assert.Assert(t, err != nil)
				assert.Assert(t, !strings.Contains(string(output), "checkout-test-token"))
				_, err = os.Stat(marker)
				assert.Assert(t, os.IsNotExist(err), "helper must not run for a different origin")
			}
		})
	}
}

func TestCheckoutGitAuthenticationHint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test Git executable uses a POSIX shell")
	}
	binDir := t.TempDir()
	assert.NilError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(`#!/bin/sh
echo "fatal: could not read Username for 'https://github.com': terminal prompts disabled" >&2
exit 128
`), 0o700))
	t.Setenv("PATH", binDir)
	err := cloneGitRepository(t.Context(), "https://github.com/acme/private.git", filepath.Join(t.TempDir(), "clone"), 100)
	assert.ErrorContains(t, err, "gh auth login --hostname github.com --git-protocol https")
}
