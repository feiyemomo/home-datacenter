package main

import (
	"os"
	"testing"

	"home-datacenter-api/internal/security"
)

func TestCmd_BootGuardIntegration(t *testing.T) {
	origAppEnv := os.Getenv("APP_ENV")
	defer func() {
		os.Setenv("APP_ENV", origAppEnv)
	}()

	os.Setenv("APP_ENV", "test")
	// In test mode, boot guard must never block
	err := security.ValidateBootGuard(security.IsProductionEnvironment(), "short", "@Momo324")
	if err != nil {
		t.Fatalf("Boot guard should not block in test environment: %v", err)
	}

	os.Setenv("APP_ENV", "production")
	// In production mode, weak secrets must be rejected
	err = security.ValidateBootGuard(security.IsProductionEnvironment(), "short", "@Momo324")
	if err == nil {
		t.Fatalf("Boot guard must reject weak secrets in production environment")
	}

	// Valid production secret must pass
	strongJWT := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	strongPass := "StrongProductionPassword9988!"
	err = security.ValidateBootGuard(security.IsProductionEnvironment(), strongJWT, strongPass)
	if err != nil {
		t.Fatalf("Boot guard failed on strong production credentials: %v", err)
	}
}
