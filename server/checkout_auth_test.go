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
	url := "https://GitHub.com:443/acme/private.git"
	assert.DeepEqual(t, checkoutGitArguments(url, "destination", 25), []string{
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		"clone", "--depth=25", "--", url, "destination",
	})
}

func TestCheckoutUsesConfiguredCredentialHelper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Git's credential-store helper is not consistently available on Windows")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is required for credential helper integration tests")
	}
	credentialStore := filepath.Join(t.TempDir(), "credentials")
	assert.NilError(t, os.WriteFile(credentialStore, []byte("https://checkout-user:checkout-test-token@github.com\n"), 0o600))
	configPath := filepath.Join(t.TempDir(), "gitconfig")
	assert.NilError(t, os.WriteFile(configPath, []byte("[credential]\n\thelper = store --file "+credentialStore+"\n"), 0o600))

	for _, scope := range []string{"global", "system"} {
		t.Run(scope, func(t *testing.T) {
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "0")
			t.Setenv("GIT_CONFIG_COUNT", "0")
			t.Setenv("GIT_ASKPASS", "")
			t.Setenv("SSH_ASKPASS", "")
			if scope == "global" {
				t.Setenv("GIT_CONFIG_GLOBAL", configPath)
			} else {
				t.Setenv("GIT_CONFIG_SYSTEM", configPath)
			}
			args := checkoutGitArguments("https://github.com/acme/private.git", "unused", 100)
			args = append(args[:slices.Index(args, "clone")], "credential", "fill")
			cmd := exec.CommandContext(t.Context(), gitPath, args...)
			cmd.Env = checkoutGitEnvironment()
			cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
			output, err := cmd.CombinedOutput()
			assert.NilError(t, err, string(output))
			assert.Assert(t, strings.Contains(string(output), "username=checkout-user"))
			assert.Assert(t, strings.Contains(string(output), "password=checkout-test-token"))
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
	assert.ErrorContains(t, err, "configure Git credentials for the user running compose-server")
}
