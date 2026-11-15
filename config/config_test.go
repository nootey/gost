package config

import "testing"

// Guards the defaults-as-registry contract: a key must be in setDefaults to be
// overridable by env var, and the env name is the key with dots replaced by
// underscores.
func TestLoadEnvOverrides(t *testing.T) {
	t.Setenv("DATABASE_HOST", "db")
	t.Setenv("DATABASE_NAME", "gost")
	t.Setenv("APP_ENVIRONMENT", "production")
	t.Setenv("SEED_ROOT_USER", "seeded@example.com")
	t.Setenv("REDIS_HOST", "redis")
	t.Setenv("REDIS_PORT", "6380")
	t.Setenv("REDIS_PASSWORD", "redis-secret")
	t.Setenv("SESSION_TTL_HOURS", "48")
	t.Setenv("CORS_ALLOWED_ORIGINS", "https://a.example,https://b.example")
	t.Setenv("UPLOAD_DIR", "/data/uploads")
	t.Setenv("UPLOAD_MAX_SIZE_MB", "10")
	t.Setenv("DOWNLOAD_TIMEOUT_SECONDS", "120")
	t.Setenv("DOWNLOAD_MAX_ZIP_MB", "500")

	if err := Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	c := Get()

	if c.Database.Host != "db" {
		t.Errorf("Database.Host = %q, want %q", c.Database.Host, "db")
	}
	if c.Database.DBName != "gost" {
		t.Errorf("Database.DBName = %q, want %q", c.Database.DBName, "gost")
	}
	if c.App.Environment != "production" {
		t.Errorf("App.Environment = %q, want %q", c.App.Environment, "production")
	}
	if c.Seed.RootUser != "seeded@example.com" {
		t.Errorf("Seed.RootUser = %q, want %q", c.Seed.RootUser, "seeded@example.com")
	}
	if c.Redis.Host != "redis" || c.Redis.Port != 6380 {
		t.Errorf("Redis = %+v, want host redis port 6380", c.Redis)
	}
	if c.Session.TTLHours != 48 {
		t.Errorf("Session.TTLHours = %d, want 48", c.Session.TTLHours)
	}
	if c.Upload.Dir != "/data/uploads" || c.Upload.MaxSizeMB != 10 {
		t.Errorf("Upload = %+v, want dir /data/uploads max 10 MB", c.Upload)
	}
	if c.Download.TimeoutSeconds != 120 || c.Download.MaxZipMB != 500 {
		t.Errorf("Download = %+v, want timeout 120s max zip 500 MB", c.Download)
	}
	want := []string{"https://a.example", "https://b.example"}
	if len(c.Cors.AllowedOrigins) != len(want) {
		t.Fatalf("Cors.AllowedOrigins = %#v, want %#v", c.Cors.AllowedOrigins, want)
	}
	for i, o := range want {
		if c.Cors.AllowedOrigins[i] != o {
			t.Errorf("Cors.AllowedOrigins[%d] = %q, want %q", i, c.Cors.AllowedOrigins[i], o)
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	if err := Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	c := Get()

	if c.Server.Port != "8080" {
		t.Errorf("Server.Port = %q, want %q", c.Server.Port, "8080")
	}
	if c.Redis.Port != 6379 || c.Redis.DB != 0 {
		t.Errorf("Redis = %+v, want port 6379 db 0", c.Redis)
	}
	if c.Session.TTLHours != 24 {
		t.Errorf("Session.TTLHours = %d, want 24", c.Session.TTLHours)
	}
	if c.Session.RememberMeTTLHours != 720 {
		t.Errorf("Session.RememberMeTTLHours = %d, want 720", c.Session.RememberMeTTLHours)
	}
	if c.Upload.Dir != "./storage/uploads" || c.Upload.MaxSizeMB != 50 || c.Upload.TimeoutSeconds != 300 {
		t.Errorf("Upload = %+v, want dir ./storage/uploads max 50 MB timeout 300s", c.Upload)
	}
	if c.Download.TimeoutSeconds != 3600 || c.Download.MaxZipMB != 10240 {
		t.Errorf("Download = %+v, want timeout 3600s max zip 10240 MB", c.Download)
	}
}

func TestLoadProductionRejectsEmptyRedisPassword(t *testing.T) {
	t.Setenv("APP_ENVIRONMENT", "production")

	err := Load()

	if err == nil {
		t.Fatal("Load() error = nil, want error for empty redis password in production")
	}
}

func TestLoadProductionAcceptsRedisPassword(t *testing.T) {
	t.Setenv("APP_ENVIRONMENT", "production")
	t.Setenv("REDIS_PASSWORD", "redis-secret")

	if err := Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
}
