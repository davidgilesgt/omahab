package projects

import (
	"net"
	"path/filepath"
	"time"
)

// Config controls deployment orchestration defaults for the service.
type Config struct {
	DataDir        string        // project data root; storage at DataDir/projects/<slug>/storage
	ProxyBind      string        // loopback ONCE proxy bind address (e.g. 127.0.0.1:8080)
	SecretsDir     string        // directory holding projected per-project secrets files
	HealthTimeout  time.Duration // total budget for post-deploy health probing
	HealthInterval time.Duration
	StaleDeployAge time.Duration // deploys locked longer than this are considered dead
}

// DefaultConfig returns the standard Omahab deployment configuration.
func DefaultConfig() Config {
	return Config{
		DataDir:        "/srv/omahab",
		ProxyBind:      "127.0.0.1:8080",
		SecretsDir:     "/var/lib/omahab/secrets/projects",
		HealthTimeout:  60 * time.Second,
		HealthInterval: 2 * time.Second,
		StaleDeployAge: 10 * time.Minute,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.DataDir == "" {
		c.DataDir = d.DataDir
	}
	if c.ProxyBind == "" {
		c.ProxyBind = d.ProxyBind
	}
	if c.SecretsDir == "" {
		c.SecretsDir = d.SecretsDir
	}
	if c.HealthTimeout == 0 {
		c.HealthTimeout = d.HealthTimeout
	}
	if c.HealthInterval == 0 {
		c.HealthInterval = d.HealthInterval
	}
	if c.StaleDeployAge == 0 {
		c.StaleDeployAge = d.StaleDeployAge
	}
	return c
}

func (c Config) validate() error {
	if !filepath.IsAbs(c.DataDir) {
		return invalidf("data_dir", "must be absolute")
	}
	if !filepath.IsAbs(c.SecretsDir) {
		return invalidf("secrets_dir", "must be absolute")
	}
	host, port, err := net.SplitHostPort(c.ProxyBind)
	if err != nil {
		return invalidf("proxy_bind", "invalid address %q: %v", c.ProxyBind, err)
	}
	if port == "" {
		return invalidf("proxy_bind", "must include a port")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return invalidf("proxy_bind", "must bind a loopback address, got %q", c.ProxyBind)
	}
	if c.HealthTimeout <= 0 {
		return invalidf("health_timeout", "must be positive")
	}
	if c.HealthInterval <= 0 {
		return invalidf("health_interval", "must be positive")
	}
	if c.HealthInterval >= c.HealthTimeout {
		return invalidf("health_interval", "must be shorter than health_timeout")
	}
	if c.StaleDeployAge <= 0 {
		return invalidf("stale_deploy_age", "must be positive")
	}
	return nil
}
