package paths

import (
	"os"
	"path/filepath"
)

func BaseDir() string {
	if value := os.Getenv("APP_BASE_DIR"); value != "" {
		return filepath.Clean(value)
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	current := filepath.Clean(wd)
	for {
		if hasProjectMarker(current) {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(wd)
		}
		current = parent
	}
}

func hasProjectMarker(dir string) bool {
	entries := []string{
		filepath.Join(dir, "config.defaults.toml"),
		filepath.Join(dir, "web", "static"),
	}
	for _, entry := range entries {
		if _, err := os.Stat(entry); err == nil {
			return true
		}
	}
	return false
}

func resolve(path string) string {
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Clean(filepath.Join(BaseDir(), path))
}

func envOr(name, fallback string) string {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	return value
}

func DataDir() string {
	return resolve(envOr("DATA_DIR", "./data"))
}

func LogDir() string {
	return resolve(envOr("LOG_DIR", "./logs"))
}

func StaticDir() string {
	return filepath.Join(BaseDir(), "web", "static")
}

func ConfigDefaultsPath() string {
	return filepath.Join(BaseDir(), "config.defaults.toml")
}

func LocalConfigPath() string {
	return resolve(envOr("CONFIG_LOCAL_PATH", filepath.Join(DataDir(), "config.toml")))
}

func LocalAccountPath() string {
	return resolve(envOr("ACCOUNT_LOCAL_PATH", filepath.Join(DataDir(), "accounts.db")))
}

func ImageCacheDir() string {
	return filepath.Join(DataDir(), "files", "images")
}

func VideoCacheDir() string {
	return filepath.Join(DataDir(), "files", "videos")
}

func EnsureRuntimeDirs() error {
	dirs := []string{
		DataDir(),
		LogDir(),
		ImageCacheDir(),
		VideoCacheDir(),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}
