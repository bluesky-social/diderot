package clientcmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/client"
)

func TestChooseHarnesses(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no harness is found
	t.Setenv("HOME", t.TempDir())
	var prompt bytes.Buffer
	person := &cliIO{stderr: &prompt, interactive: true}
	script := &cliIO{stderr: &prompt}

	got, err := chooseHarnesses(script, bufio.NewReader(strings.NewReader("")), setupOptions{harnesses: "codex, Cursor,codex", harnessesSet: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"codex", "cursor"}, got)

	got, err = chooseHarnesses(script, bufio.NewReader(strings.NewReader("")), setupOptions{harnesses: "none", harnessesSet: true})
	require.NoError(t, err)
	assert.Empty(t, got)

	_, err = chooseHarnesses(script, bufio.NewReader(strings.NewReader("")), setupOptions{harnesses: "vim", harnessesSet: true})
	assert.Equal(t, exitUsage, exitCode(err))

	_, err = chooseHarnesses(script, bufio.NewReader(strings.NewReader("codex\n")), setupOptions{})
	assert.Equal(t, exitUsage, exitCode(err), "off a terminal nothing is asked")
	assert.Empty(t, prompt.String())

	got, err = chooseHarnesses(person, bufio.NewReader(strings.NewReader("pi,opencode\n")), setupOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"pi", "opencode"}, got)
	assert.Contains(t, prompt.String(), "claude-code\n")
	assert.Contains(t, prompt.String(), "Which should use Diderot?")
}

func TestExitCodes(t *testing.T) {
	assert.Equal(t, exitSignIn, exitCode(&client.SignInError{Reason: "expired"}))
	assert.Equal(t, exitCancelled, exitCode(context.Canceled))
	assert.Equal(t, exitUsage, exitCode(usageError("bad")))
	assert.Equal(t, exitPartial, exitCode(&exitError{code: exitPartial, err: errors.New("partly")}))
	assert.Equal(t, exitFailure, exitCode(errors.New("other")))
}

func TestResolveVersion(t *testing.T) {
	assert.Equal(t, "v1.2.3", ResolveVersion("v1.2.3"), "a stamped version wins")
	// A test binary records no module version, so an unstamped one is dev.
	assert.Equal(t, "dev", ResolveVersion("dev"))
	assert.Equal(t, "dev", ResolveVersion(""))
}
