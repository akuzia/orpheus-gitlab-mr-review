package config

import (
	"os"
	"path/filepath"
	"strings"
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
	t.Setenv("ORPHEUS_BASE_URL", " https://orpheus.example.com/api/ ")
	t.Setenv("ORPHEUS_WEB_BASE_URL", " https://orpheus.example.com/ui/ ")
	t.Setenv("ORPHEUS_API_KEY", " orpheus-token ")
	t.Setenv("ORPHEUS_AGENT_PROFILE", " review-profile ")
	t.Setenv("ORPHEUS_AGENT_MODEL", " review-model ")
	t.Setenv("ORPHEUS_SANDBOX_TEMPLATE", " review-sandbox ")
	t.Setenv("ORPHEUS_SERVICES", " gitlab, redmine ")
	t.Setenv("ORPHEUS_AGENT_INSTRUCTION_FILES", " /policies/compliance.md, /policies/project.md ")
	t.Setenv("RUN_TIMEOUT_SECONDS", "3600")
	t.Setenv("HOOK_TIMEOUT_SECONDS", "120")
	t.Setenv("MAX_SESSION_REQUEST_BYTES", "1048576")
	t.Setenv("RECONCILE_WORKER_COUNT", "6")
	t.Setenv("RECONCILE_QUEUE_CAPACITY", "48")
	t.Setenv("MAX_CONCURRENT_REVIEWS", "9")

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, ModeDevelopment, cfg.Mode)
	require.Equal(t, "debug", cfg.LogLevel)
	require.Equal(t, 12*time.Second, cfg.HTTPTimeout)
	require.Equal(t, 15*time.Second, cfg.PollInterval)
	require.Equal(t, 8*time.Second, cfg.ShutdownTimeout)
	require.Equal(t, time.Hour, cfg.RunTimeout)
	require.Equal(t, 2*time.Minute, cfg.HookTimeout)
	require.Equal(t, 1<<20, cfg.MaxSessionRequestBytes)
	require.Equal(t, 6, cfg.ReconcileWorkerCount)
	require.Equal(t, 48, cfg.ReconcileQueueCapacity)
	require.Equal(t, 9, cfg.MaxConcurrentReviews)
	require.Equal(t, "https://gitlab.example.com/root", cfg.GitLab.BaseURL)
	require.Equal(t, "token", cfg.GitLab.Token)
	require.Equal(t, "https://orpheus.example.com/api", cfg.Orpheus.BaseURL)
	require.Equal(t, "https://orpheus.example.com/ui", cfg.Orpheus.WebBaseURL)
	require.Equal(t, "orpheus-token", cfg.Orpheus.APIKey)
	require.Equal(t, "review-profile", cfg.Orpheus.AgentProfile)
	require.Equal(t, "review-model", cfg.Orpheus.AgentModel)
	require.Equal(t, "review-sandbox", cfg.Orpheus.SandboxTemplate)
	require.Equal(t, []string{"gitlab", "redmine"}, cfg.Orpheus.Services)
	require.Equal(t, []string{"/policies/compliance.md", "/policies/project.md"}, cfg.Orpheus.InstructionFiles)
}

func TestLoadRequiresOrpheusSettings(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GITLAB_BASE_URL", "https://gitlab.example.com")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("ORPHEUS_BASE_URL", "")

	_, err := Load()
	require.EqualError(t, err, "ORPHEUS_BASE_URL is required")
}

func TestLoadRequiresGitLabSettings(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GITLAB_BASE_URL", "")
	t.Setenv("GITLAB_TOKEN", "")

	_, err := Load()
	require.EqualError(t, err, "GITLAB_BASE_URL is required")
}

func TestLoadServicesFromDotEnv(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	t.Setenv("GITLAB_BASE_URL", "https://gitlab.example.com")
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("ORPHEUS_BASE_URL", "https://orpheus.example.com")
	t.Setenv("ORPHEUS_API_KEY", "orpheus-token")
	t.Setenv("ORPHEUS_AGENT_PROFILE", "review-profile")
	t.Setenv("ORPHEUS_SANDBOX_TEMPLATE", "review-sandbox")
	t.Setenv("ORPHEUS_AGENT_INSTRUCTION_FILES", "instructions.md")
	t.Setenv("ORPHEUS_SERVICES", "")
	require.NoError(t, os.Unsetenv("ORPHEUS_SERVICES"))

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, []string{"gitlab"}, cfg.Orpheus.Services)

	require.NoError(t, os.WriteFile(filepath.Join(directory, ".env"), []byte("ORPHEUS_SERVICES=gitlab,redmine\n"), 0o600))

	cfg, err = Load()
	require.NoError(t, err)
	require.Equal(t, []string{"gitlab", "redmine"}, cfg.Orpheus.Services)

	t.Setenv("ORPHEUS_SERVICES", "gitlab")
	cfg, err = Load()
	require.NoError(t, err)
	require.Equal(t, []string{"gitlab"}, cfg.Orpheus.Services)

	t.Setenv("ORPHEUS_SERVICES", "")
	cfg, err = Load()
	require.NoError(t, err)
	require.Nil(t, cfg.Orpheus.Services)

	require.NoError(t, os.Unsetenv("ORPHEUS_SERVICES"))
	require.NoError(t, os.WriteFile(filepath.Join(directory, ".env"), []byte("ORPHEUS_SERVICES=\n"), 0o600))
	cfg, err = Load()
	require.NoError(t, err)
	require.Nil(t, cfg.Orpheus.Services)

	t.Setenv("ORPHEUS_SERVICES", "gitlab,gitlab")
	_, err = Load()
	require.EqualError(t, err, `ORPHEUS_SERVICES contains duplicate value "gitlab"`)
}

func TestParseServices(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		want    []string
		wantErr string
	}{
		{name: "unset"},
		{name: "blank", value: " \t"},
		{name: "trimmed", value: " gitlab, redmine ", want: []string{"gitlab", "redmine"}},
		{name: "custom codes", value: "custom_service,gitlab-2", want: []string{"custom_service", "gitlab-2"}},
		{name: "empty entries", value: "gitlab, ,redmine,", want: []string{"gitlab", "redmine"}},
		{name: "duplicate", value: "gitlab, gitlab", wantErr: `ORPHEUS_SERVICES contains duplicate value "gitlab"`},
		{name: "no codes", value: " , ", wantErr: "ORPHEUS_SERVICES is required"},
		{name: "uppercase", value: "GitLab", wantErr: "invalid service code"},
		{name: "spaces", value: "git lab", wantErr: "invalid service code"},
		{name: "leading digit", value: "2gitlab", wantErr: "invalid service code"},
		{name: "punctuation", value: "gitlab.prod", wantErr: "invalid service code"},
		{name: "maximum length", value: strings.Repeat("a", 64), want: []string{strings.Repeat("a", 64)}},
		{name: "too long", value: strings.Repeat("a", 65), wantErr: "invalid service code"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseServices(test.value)
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
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

func TestParseOrderedList(t *testing.T) {
	t.Parallel()

	values, err := parseOrderedList("FILES", " first.md, ,second.md ")
	require.NoError(t, err)
	require.Equal(t, []string{"first.md", "second.md"}, values)

	_, err = parseOrderedList("FILES", "first.md, first.md")
	require.EqualError(t, err, `FILES contains duplicate value "first.md"`)
	_, err = parseOrderedList("FILES", " , ")
	require.EqualError(t, err, "FILES is required")
}

func TestParsePositiveIntEnforcesMaximum(t *testing.T) {
	t.Parallel()

	value, err := parsePositiveInt("LIMIT", "10", 10)
	require.NoError(t, err)
	require.Equal(t, 10, value)

	_, err = parsePositiveInt("LIMIT", "11", 10)
	require.EqualError(t, err, "LIMIT must not exceed 10")
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
