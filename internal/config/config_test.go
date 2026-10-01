package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoadGitLabConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("APP_MODE", "dev")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("HTTP_TIMEOUT_SECONDS", "12")
	t.Setenv("POLL_INTERVAL_SECONDS", "15")
	t.Setenv("SHUTDOWN_TIMEOUT_SECONDS", "8")
	t.Setenv("GITLAB_BASE_URL", " https://gitlab.example.com/root/ ")
	t.Setenv("GITLAB_TOKEN", " token ")

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, ModeDevelopment, cfg.Mode)
	require.Equal(t, "debug", cfg.LogLevel)
	require.Equal(t, 12*time.Second, cfg.HTTPTimeout)
	require.Equal(t, 15*time.Second, cfg.PollInterval)
	require.Equal(t, 8*time.Second, cfg.ShutdownTimeout)
	require.Equal(t, "https://gitlab.example.com/root", cfg.GitLab.BaseURL)
	require.Equal(t, "token", cfg.GitLab.Token)
}

func TestLoadRequiresGitLabSettings(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GITLAB_BASE_URL", "")
	t.Setenv("GITLAB_TOKEN", "")

	_, err := Load()
	require.EqualError(t, err, "GITLAB_BASE_URL is required")
}

func TestParseBaseURL(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		value   string
		want    string
		wantErr string
	}{
		"https":        {value: "https://gitlab.example.com/", want: "https://gitlab.example.com"},
		"subpath":      {value: "https://gitlab.example.com/root/", want: "https://gitlab.example.com/root"},
		"missing host": {value: "https://", wantErr: "must include a host"},
		"credentials":  {value: "https://user:secret@gitlab.example.com", wantErr: "must not include credentials"},
		"query":        {value: "https://gitlab.example.com?x=1", wantErr: "must not include a query or fragment"},
		"scheme":       {value: "ftp://gitlab.example.com", wantErr: "must use http or https"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := parseBaseURL("GITLAB_BASE_URL", test.value)
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				return
			}

			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}

func TestParsePositiveSeconds(t *testing.T) {
	t.Parallel()

	got, err := parsePositiveSeconds("HTTP_TIMEOUT_SECONDS", "2")
	require.NoError(t, err)
	require.Equal(t, 2*time.Second, got)

	_, err = parsePositiveSeconds("HTTP_TIMEOUT_SECONDS", "0")
	require.EqualError(t, err, "HTTP_TIMEOUT_SECONDS must be positive")

	_, err = parsePositiveSeconds("HTTP_TIMEOUT_SECONDS", "2s")
	require.ErrorContains(t, err, "parse HTTP_TIMEOUT_SECONDS")
}

func TestLoadRejectsInvalidPollInterval(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GITLAB_BASE_URL", "https://gitlab.example.com")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("POLL_INTERVAL_SECONDS", "0")

	_, err := Load()
	require.EqualError(t, err, "POLL_INTERVAL_SECONDS must be positive")
}

func TestLoadRejectsInvalidShutdownTimeout(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GITLAB_BASE_URL", "https://gitlab.example.com")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("SHUTDOWN_TIMEOUT_SECONDS", "0")

	_, err := Load()
	require.EqualError(t, err, "SHUTDOWN_TIMEOUT_SECONDS must be positive")
}
