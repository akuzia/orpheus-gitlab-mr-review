package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStartupProcess(t *testing.T) {
	if os.Getenv("ORPHEUS_STARTUP_TEST") != "1" {
		return
	}
	main()
}

func TestFailuresEmitJSONAndExitOne(t *testing.T) {
	t.Parallel()

	gitLab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(gitLab.Close)

	tests := []struct {
		name         string
		env          []string
		configFile   string
		msg          string
		errorText    string
		instructions bool
	}{
		{
			name:      "missing configuration",
			msg:       "failed to load configuration",
			errorText: "GITLAB_BASE_URL is required",
		},
		{
			name:      "configuration failure with fatal log level",
			env:       []string{"LOG_LEVEL=fatal"},
			msg:       "failed to load configuration",
			errorText: "GITLAB_BASE_URL is required",
		},
		{
			name:       "malformed configuration file",
			configFile: "=invalid\n",
			msg:        "failed to load configuration",
			errorText:  "read config:",
		},
		{
			name:         "invalid logger configuration",
			env:          []string{"LOG_LEVEL=invalid"},
			msg:          "failed to initialize application",
			errorText:    "parse log level:",
			instructions: true,
		},
		{
			name:      "missing instructions",
			env:       []string{"LOG_LEVEL=fatal"},
			msg:       "failed to initialize application",
			errorText: "instructions.md",
		},
		{
			name:         "runtime failure",
			env:          []string{"LOG_LEVEL=debug"},
			msg:          "application failed",
			errorText:    "resolve authenticated GitLab reviewer:",
			instructions: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tt.configFile != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte(tt.configFile), 0o600))
			}
			if tt.instructions {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "instructions.md"), []byte("# Review policy\n"), 0o600))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			binary, err := os.Executable()
			require.NoError(t, err)
			cmd := exec.CommandContext(ctx, binary, "-test.run=^TestStartupProcess$")
			cmd.Dir = dir
			// Use only test configuration so local credentials and .env cannot affect the child.
			cmd.Env = []string{
				"ORPHEUS_STARTUP_TEST=1",
				// Coverage-instrumented binaries need a directory even when main calls os.Exit.
				"GOCOVERDIR=" + t.TempDir(),
			}
			if tt.msg != "failed to load configuration" {
				cmd.Env = append(cmd.Env,
					"GITLAB_BASE_URL="+gitLab.URL,
					"GITLAB_TOKEN=test-token",
					"ORPHEUS_BASE_URL=https://orpheus.example.com",
					"ORPHEUS_API_KEY=test-key",
					"ORPHEUS_AGENT_PROFILE=test-profile",
					"ORPHEUS_SANDBOX_TEMPLATE=test-template",
					"ORPHEUS_AGENT_INSTRUCTION_FILES=instructions.md",
					"HTTP_TIMEOUT_SECONDS=1",
					"SHUTDOWN_TIMEOUT_SECONDS=1",
				)
			}
			cmd.Env = append(cmd.Env, tt.env...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err = cmd.Run()
			require.NoError(t, ctx.Err(), "process timed out: %s", stdout.String())
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr)
			require.Equal(t, 1, exitErr.ExitCode())
			require.Empty(t, stderr.String())
			require.NotEmpty(t, stdout.String())
			require.True(t, strings.HasSuffix(stdout.String(), "\n"))

			var lastRecord map[string]any
			for line := range strings.SplitSeq(strings.TrimSuffix(stdout.String(), "\n"), "\n") {
				var record map[string]any
				require.NoError(t, json.Unmarshal([]byte(line), &record), "log record: %s", line)
				require.NotEmpty(t, record["timestamp"])
				require.NotEmpty(t, record["level"])
				require.NotEmpty(t, record["msg"])
				lastRecord = record
			}
			require.Equal(t, "error", lastRecord["level"])
			require.Equal(t, tt.msg, lastRecord["msg"])
			require.Contains(t, lastRecord["error"], tt.errorText)
		})
	}
}
