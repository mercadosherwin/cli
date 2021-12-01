package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"

	"github.com/MakeNowJust/heredoc"
	expect "github.com/Netflix/go-expect"
	"github.com/cli/cli/v2/pkg/cmdutil"
	"github.com/cli/safeexec"
	"github.com/hinshun/vt10x"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

func Test_printError(t *testing.T) {
	cmd := &cobra.Command{}

	type args struct {
		err   error
		cmd   *cobra.Command
		debug bool
	}
	tests := []struct {
		name    string
		args    args
		wantOut string
	}{
		{
			name: "generic error",
			args: args{
				err:   errors.New("the app exploded"),
				cmd:   nil,
				debug: false,
			},
			wantOut: "the app exploded\n",
		},
		{
			name: "DNS error",
			args: args{
				err: fmt.Errorf("DNS oopsie: %w", &net.DNSError{
					Name: "api.github.com",
				}),
				cmd:   nil,
				debug: false,
			},
			wantOut: `error connecting to api.github.com
check your internet connection or https://githubstatus.com
`,
		},
		{
			name: "Cobra flag error",
			args: args{
				err:   cmdutil.FlagErrorf("unknown flag --foo"),
				cmd:   cmd,
				debug: false,
			},
			wantOut: "unknown flag --foo\n\nUsage:\n\n",
		},
		{
			name: "unknown Cobra command error",
			args: args{
				err:   errors.New("unknown command foo"),
				cmd:   cmd,
				debug: false,
			},
			wantOut: "unknown command foo\n\nUsage:\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := &bytes.Buffer{}
			printError(out, tt.args.err, tt.args.cmd, tt.args.debug)
			if gotOut := out.String(); gotOut != tt.wantOut {
				t.Errorf("printError() = %q, want %q", gotOut, tt.wantOut)
			}
		})
	}
}

func Test_IntegrationRepoFork(t *testing.T) {
	// Set up temp dir
	tempDir := t.TempDir()
	oldWd, _ := os.Getwd()
	assert.NoError(t, os.Chdir(tempDir))
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	// Set up server to handle network requests
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		gitPath := regexp.MustCompile(`\/someone\/repo\.git\/.*`)
		forkPath := "/repos/owner/repo/forks"
		switch {
		case path == forkPath:
			forkResult := fmt.Sprintf(`{
				"node_id": "123",
				"name": "repo",
				"clone_url": "https://github.com/someone/repo.git",
				"created_at": "%s",
				"owner": {
					"login": "someone"
				}
			}`, time.Now().Format(time.RFC3339))
			_, _ = w.Write([]byte(forkResult))
		case gitPath.MatchString(path):
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(s.Close)
	os.Setenv("HTTP_PROXY", s.URL)
	t.Cleanup(func() { os.Unsetenv("HTTP_PROXY") })

	// Set up config directory and config files
	os.Setenv("GH_CONFIG_DIR", tempDir)
	t.Cleanup(func() { os.Unsetenv("GH_CONFIG_DIR") })
	hosts := heredoc.Doc(`
    github.localhost:
      user: monalisa
      oauth_token: TOKEN
      git_protocol: https
	`)
	err := ioutil.WriteFile("hosts.yml", []byte(hosts), 0771)
	assert.NoError(t, err)

	// Set up local git repo
	gitExe, err := safeexec.LookPath("git")
	assert.NoError(t, err)
	cmd := exec.Command(gitExe, "init", "--quiet", "repo")
	err = cmd.Run()
	assert.NoError(t, err)
	assert.NoError(t, os.Chdir("repo"))
	url := fmt.Sprintf("%s/owner/repo.git", "http://github.localhost")
	cmd2 := exec.Command(gitExe, "remote", "add", "origin", url)
	err = cmd2.Run()
	assert.NoError(t, err)
	// Add remote as resolved to skip graphql resolution
	cmd3 := exec.Command(gitExe, "config", "--add", "remote.origin.gh-resolved", "base")
	err = cmd3.Run()
	assert.NoError(t, err)
	// Set up server as git proxy
	cmd4 := exec.Command(gitExe, "config", "http.http://github.localhost.proxy", s.URL)
	err = cmd4.Run()
	assert.NoError(t, err)

	// Skip checking for updates
	os.Setenv("GH_NO_UPDATE_NOTIFIER", "1")
	t.Cleanup(func() { os.Unsetenv("GH_NO_UPDATE_NOTIFIER") })

	// Skip terminal colors
	os.Setenv("NO_COLOR", "1")
	t.Cleanup(func() { os.Unsetenv("NO_COLOR") })
	os.Setenv("CLICOLOR", "0")
	t.Cleanup(func() { os.Unsetenv("CLICOLOR") })

	// Set integration test environment variable
	os.Setenv("GH_INTEGRATION_TEST", "1")
	t.Cleanup(func() { os.Unsetenv("GH_INTEGRATION_TEST") })

	// Set up command and IO
	origArgs := os.Args
	origOut := os.Stdout
	origErr := os.Stderr
	t.Cleanup(func() {
		os.Args = origArgs
		os.Stdout = origOut
		os.Stderr = origErr
	})

	buf := &bytes.Buffer{}
	c, _, err := vt10x.NewVT10XConsole(expect.WithStdout(buf), expect.WithDefaultTimeout(time.Second))
	if err != nil {
		t.Error(err)
	}
	t.Cleanup(func() { c.Close() })

	os.Stdin = c.Tty()
	os.Stdout = c.Tty()
	os.Stderr = c.Tty()
	os.Args = []string{"gh", "repo", "fork"}

	donec := make(chan struct{})
	go func() {
		defer close(donec)
		_, _ = c.ExpectString("Would you like to add a remote for the fork?")
		_, _ = c.SendLine("Y")
		_, _ = c.ExpectString("FINISHED")
	}()

	// Execute command
	code := mainRun()
	<-donec

	assert.Equal(t, exitOK, code)

	// Strip ANSI escape codes from output
	ansi := "[\u001B\u009B][[\\]()#;?]*(?:(?:(?:[a-zA-Z\\d]*(?:;[a-zA-Z\\d]*)*)?\u0007)|(?:(?:\\d{1,4}(?:;\\d{0,4})*)?[\\dA-PRZcf-ntqry=><~]))"
	re := regexp.MustCompile(ansi)
	out := re.ReplaceAllString(buf.String(), "")

	assert.Regexp(t, "✓ Created fork someone/repo", out)
	assert.Regexp(t, "✓ Added remote origin", out)
}
