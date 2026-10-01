package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Mode string

const (
	ModeDevelopment Mode = "dev"
	ModeProduction  Mode = "prod"
)

func (m Mode) String() string {
	return string(m)
}

type Config struct {
	Mode            Mode
	LogLevel        string
	HTTPTimeout     time.Duration
	PollInterval    time.Duration
	ShutdownTimeout time.Duration
	GitLab          GitLab
}

type GitLab struct {
	BaseURL string
	Token   string
}

func Load() (Config, error) {
	v := viper.New()
	v.SetDefault("app_mode", ModeProduction)
	v.SetDefault("log_level", "error")
	v.SetDefault("http_timeout_seconds", 30)
	v.SetDefault("poll_interval_seconds", 30)
	v.SetDefault("shutdown_timeout_seconds", 20)
	v.AutomaticEnv()

	if _, err := os.Stat(".env"); err == nil {
		v.SetConfigFile(".env")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("inspect .env: %w", err)
	}

	if err := v.ReadInConfig(); err != nil {
		if _, ok := errors.AsType[viper.ConfigFileNotFoundError](err); !ok {
			return Config{}, fmt.Errorf("read config: %w", err)
		}
	}

	mode, err := parseMode(v.GetString("app_mode"))
	if err != nil {
		return Config{}, err
	}

	httpTimeout, err := parsePositiveSeconds("HTTP_TIMEOUT_SECONDS", v.GetString("http_timeout_seconds"))
	if err != nil {
		return Config{}, err
	}
	pollInterval, err := parsePositiveSeconds("POLL_INTERVAL_SECONDS", v.GetString("poll_interval_seconds"))
	if err != nil {
		return Config{}, err
	}
	shutdownTimeout, err := parsePositiveSeconds("SHUTDOWN_TIMEOUT_SECONDS", v.GetString("shutdown_timeout_seconds"))
	if err != nil {
		return Config{}, err
	}

	gitLabBaseURL, err := parseBaseURL("GITLAB_BASE_URL", v.GetString("gitlab_base_url"))
	if err != nil {
		return Config{}, err
	}

	gitLabToken := strings.TrimSpace(v.GetString("gitlab_token"))
	if gitLabToken == "" {
		return Config{}, errors.New("GITLAB_TOKEN is required")
	}

	return Config{
		Mode:            mode,
		LogLevel:        v.GetString("log_level"),
		HTTPTimeout:     httpTimeout,
		PollInterval:    pollInterval,
		ShutdownTimeout: shutdownTimeout,
		GitLab: GitLab{
			BaseURL: gitLabBaseURL,
			Token:   gitLabToken,
		},
	}, nil
}

func parseMode(value string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(value))) {
	case ModeDevelopment:
		return ModeDevelopment, nil
	case ModeProduction:
		return ModeProduction, nil
	default:
		return "", fmt.Errorf("unsupported app mode %q", value)
	}
}

func parsePositiveSeconds(name, value string) (time.Duration, error) {
	seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	if seconds <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	const maxDuration = time.Duration(1<<63 - 1)
	if seconds > int64(maxDuration/time.Second) {
		return 0, fmt.Errorf("%s is too large", name)
	}

	return time.Duration(seconds) * time.Second, nil
}

func parseBaseURL(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}

	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", name, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("%s must use http or https", name)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("%s must include a host", name)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("%s must not include credentials", name)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%s must not include a query or fragment", name)
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = strings.TrimRight(parsed.RawPath, "/")

	return parsed.String(), nil
}
