package codexprovider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const outputSchema = `{"type":"object","properties":{"answer":{"type":"string"},"tool_calls":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string"},"arguments":{"type":"string"}},"required":["name","arguments"],"additionalProperties":false}}},"required":["answer","tool_calls"],"additionalProperties":false}`

const maxCodexOutputBytes = 2 << 20

func execRunner(binary, model string) runner {
	return func(ctx context.Context, prompt []byte, effort string) (turnResult, error) {
		var result turnResult
		dir, err := os.MkdirTemp("", "diderot-codex-")
		if err != nil {
			return result, errors.New("could not create a Codex working directory")
		}
		defer os.RemoveAll(dir)
		schema := filepath.Join(dir, "schema.json")
		answer := filepath.Join(dir, "answer.json")
		if err := os.WriteFile(schema, []byte(outputSchema), 0600); err != nil {
			return result, errors.New("could not write the Codex output schema")
		}
		args := []string{"exec", "--ephemeral", "--ignore-user-config", "--ignore-rules",
			"--skip-git-repo-check", "--sandbox", "read-only",
			"--disable", "shell_tool", "--disable", "unified_exec", "--disable", "hooks",
			"--disable", "apps", "--disable", "plugins",
			"-c", `approval_policy="never"`, "-c", `web_search="disabled"`,
			"--model", model, "--output-schema", schema, "--output-last-message", answer, "--json"}
		if effort != "" {
			args = append(args, "-c", "model_reasoning_effort="+strconv.Quote(effort))
		}
		cmd := exec.CommandContext(ctx, binary, append(args, "-")...)
		cmd.Dir = dir
		cmd.WaitDelay = 2 * time.Second
		cmd.Env = codexEnvironment(os.Environ())
		cmd.Stdin = bytes.NewReader(prompt)
		events := &boundedBuffer{}
		cmd.Stdout = events
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			return result, errors.New("Codex execution failed; check its login, model and version from the adapter account")
		}
		output, err := readAnswer(answer)
		if err != nil || json.Unmarshal(output, &result) != nil {
			return result, errors.New("Codex returned invalid structured output")
		}
		scanner := bufio.NewScanner(bytes.NewReader(events.buffer.Bytes()))
		scanner.Buffer(make([]byte, 4096), maxCodexOutputBytes)
		for scanner.Scan() {
			var event struct {
				Type  string `json:"type"`
				Usage usage  `json:"usage"`
			}
			if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Type == "turn.completed" {
				result.Usage = event.Usage
				return result, nil
			}
		}
		return result, errors.New("Codex did not report a completed turn")
	}
}

func codexEnvironment(environ []string) []string {
	var result []string
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "PATH", "HOME", "CODEX_HOME", "LANG", "LC_ALL", "SSL_CERT_FILE", "SSL_CERT_DIR":
			result = append(result, entry)
		}
	}
	return result
}

func readAnswer(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxCodexOutputBytes+1))
	if len(body) > maxCodexOutputBytes {
		return nil, errors.New("Codex answer is too large")
	}
	return body, err
}

type boundedBuffer struct{ buffer bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > maxCodexOutputBytes {
		return 0, errors.New("Codex event output is too large")
	}
	return b.buffer.Write(p)
}
