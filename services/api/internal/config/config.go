// Package config loads application configuration from a YAML file
// using viper. The loaded config is exposed via the package-level
// AppConfig variable.
//
//	Usage:
//	  if err := config.Load("configs/config.yaml"); err != nil {
//	      log.Fatal(err)
//	  }
//	  port := config.AppConfig.Server.Port
package config

import (
	"fmt"

	"github.com/spf13/viper"
)

// Config is the root configuration object.
type Config struct {
	Server   ServerConfig   `mapstructure:"server"`
	Database DatabaseConfig `mapstructure:"database"`
	JWT      JWTConfig      `mapstructure:"jwt"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Port int `mapstructure:"port"`
}

// DatabaseConfig holds database connection settings.
type DatabaseConfig struct {
	Path string `mapstructure:"path"`
}

// JWTConfig holds JWT signing parameters.
type JWTConfig struct {
	Secret     string `mapstructure:"secret"`
	ExpireDays int    `mapstructure:"expire_days"`
}

// AppConfig is the globally accessible configuration instance,
// populated by Load(). It is safe to read after Load returns nil.
var AppConfig *Config

// Load reads the YAML config file at path, applies defaults for
// any missing fields, and populates AppConfig.
//
// Defaults applied:
//
//	server.port       -> 8080
//	database.path     -> /data/sqlite/app.db
//	jwt.secret        -> ""  (caller should validate non-empty)
//	jwt.expire_days   -> 365
func Load(path string) error {
	v := viper.New()

	// Defaults — keep the app runnable even if a field is omitted.
	v.SetDefault("server.port", 8080)
	v.SetDefault("database.path", "/data/sqlite/app.db")
	v.SetDefault("jwt.secret", "")
	v.SetDefault("jwt.expire_days", 365)

	v.SetConfigFile(path)

	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("read config %q: %w", path, err)
	}

	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return fmt.Errorf("parse config %q: %w", path, err)
	}

	AppConfig = cfg
	return nil
}
