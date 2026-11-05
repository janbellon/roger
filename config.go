package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config is read once from ROGER_* environment variables at startup. Anything
// invalid stops the server: a typo in a limit must not silently remove it.
type Config struct {
	Port             int
	Password         string
	DataDir          string
	MaxConcurrent    int
	FileTTL          time.Duration
	DownloadTimeout  time.Duration
	MaxFilesize      int64
	AllowPrivateURLs bool
	YtDlpPath        string
	FfmpegPath       string
	YtDlpArgs        []string
	YtDlpUpdateEvery time.Duration
}

var sizePattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([KkMmGgTt]?)$`)

// parseSize reads sizes like 500M or 1.5G, in powers of 1024. Empty means no limit.
func parseSize(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	match := sizePattern.FindStringSubmatch(raw)
	if match == nil {
		return 0, fmt.Errorf("ROGER_MAX_FILESIZE: %q is not a size like 500M or 2G", raw)
	}
	value, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, fmt.Errorf("ROGER_MAX_FILESIZE: %w", err)
	}
	shift := strings.Index("KMGT", strings.ToUpper(match[2])) + 1 // 0 when there's no unit
	if match[2] == "" {
		shift = 0
	}
	return int64(value * float64(int64(1)<<(10*shift))), nil
}

func loadConfig(getenv func(string) string) (Config, error) {
	cfg := Config{
		Password:   getenv("ROGER_PASSWORD"),
		DataDir:    getenv("ROGER_DATA_DIR"),
		YtDlpPath:  getenv("ROGER_YTDLP_PATH"),
		FfmpegPath: getenv("ROGER_FFMPEG_PATH"),
		YtDlpArgs:  strings.Fields(getenv("ROGER_YTDLP_ARGS")),
	}
	if cfg.DataDir == "" {
		cfg.DataDir = filepath.Join(os.TempDir(), "roger")
	}
	if cfg.YtDlpPath == "" {
		cfg.YtDlpPath = "yt-dlp"
	}

	var err error
	if cfg.MaxFilesize, err = parseSize(getenv("ROGER_MAX_FILESIZE")); err != nil {
		return cfg, err
	}
	if cfg.Port, err = intEnv(getenv, "ROGER_PORT", 8080, 1, 65535); err != nil {
		return cfg, err
	}
	if cfg.MaxConcurrent, err = intEnv(getenv, "ROGER_MAX_CONCURRENT", 2, 1, 64); err != nil {
		return cfg, err
	}
	if cfg.FileTTL, err = durationEnv(getenv, "ROGER_FILE_TTL", 15*time.Minute, time.Minute); err != nil {
		return cfg, err
	}
	if cfg.DownloadTimeout, err = durationEnv(getenv, "ROGER_DOWNLOAD_TIMEOUT", time.Hour, time.Minute); err != nil {
		return cfg, err
	}
	if cfg.YtDlpUpdateEvery, err = durationEnv(getenv, "ROGER_YTDLP_UPDATE_INTERVAL", 0, 0); err != nil {
		return cfg, err
	}
	if raw := getenv("ROGER_ALLOW_PRIVATE_URLS"); raw != "" {
		if cfg.AllowPrivateURLs, err = strconv.ParseBool(raw); err != nil {
			return cfg, fmt.Errorf("ROGER_ALLOW_PRIVATE_URLS: %q is not a boolean", raw)
		}
	}
	return cfg, nil
}

func intEnv(getenv func(string) string, key string, fallback, low, high int) (int, error) {
	raw := getenv(key)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < low || value > high {
		return 0, fmt.Errorf("%s: %q is not a number between %d and %d", key, raw, low, high)
	}
	return value, nil
}

func durationEnv(getenv func(string) string, key string, fallback, low time.Duration) (time.Duration, error) {
	raw := getenv(key)
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value < low {
		return 0, fmt.Errorf("%s: %q is not a duration of at least %s (try 15m or 2h)", key, raw, low)
	}
	return value, nil
}
