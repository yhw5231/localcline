package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPersistedAccountsSurviveReload(t *testing.T) {
	dataDir := t.TempDir()
	accountsPath := filepath.Join(dataDir, "accounts.json")

	want := persistedAccounts{
		AdminUsername: "persisted-admin",
		AdminPassword: "persisted-password",
		ExtraUsers: map[string]string{
			"alice": "alice-password",
		},
	}
	if err := savePersistedAccounts(accountsPath, want); err != nil {
		t.Fatalf("savePersistedAccounts() error = %v", err)
	}

	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("ACCOUNTS_PATH", accountsPath)
	t.Setenv("USAGE_DB_PATH", "")

	got := loadConfig()
	if got.AdminUser != want.AdminUsername {
		t.Fatalf("AdminUser = %q, want %q", got.AdminUser, want.AdminUsername)
	}
	if got.AdminPass != want.AdminPassword {
		t.Fatalf("AdminPass = %q, want %q", got.AdminPass, want.AdminPassword)
	}
	if got.ExtraUsers["alice"] != "alice-password" {
		t.Fatalf("ExtraUsers[alice] = %q, want %q", got.ExtraUsers["alice"], "alice-password")
	}
	if !verifyLoginWithConfig(got, "persisted-admin", "persisted-password") {
		t.Fatal("persisted administrator credentials were not accepted")
	}
	if !verifyLoginWithConfig(got, "alice", "alice-password") {
		t.Fatal("persisted extra-user credentials were not accepted")
	}
}

func TestEnvironmentAccountsOverridePersistedAccounts(t *testing.T) {
	dataDir := t.TempDir()
	accountsPath := filepath.Join(dataDir, "accounts.json")
	if err := savePersistedAccounts(accountsPath, persistedAccounts{
		AdminUsername: "file-admin",
		AdminPassword: "file-password",
		ExtraUsers: map[string]string{
			"alice": "file-password",
			"bob":   "bob-password",
		},
	}); err != nil {
		t.Fatalf("savePersistedAccounts() error = %v", err)
	}

	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("ACCOUNTS_PATH", accountsPath)
	t.Setenv("ADMIN_USERNAME", "env-admin")
	t.Setenv("ADMIN_PASSWORD", "env-password")
	t.Setenv("EXTRA_USERS", "alice:env-password,charlie:charlie-password")
	t.Setenv("USAGE_DB_PATH", "")

	got := loadConfig()
	if got.AdminUser != "env-admin" || got.AdminPass != "env-password" {
		t.Fatalf("administrator = %q/%q, want env-admin/env-password", got.AdminUser, got.AdminPass)
	}
	if got.ExtraUsers["alice"] != "env-password" {
		t.Fatalf("environment user did not override persisted user: %q", got.ExtraUsers["alice"])
	}
	if got.ExtraUsers["bob"] != "bob-password" {
		t.Fatalf("persisted user bob was not retained: %q", got.ExtraUsers["bob"])
	}
	if got.ExtraUsers["charlie"] != "charlie-password" {
		t.Fatalf("environment user charlie was not added: %q", got.ExtraUsers["charlie"])
	}
}

func TestTokenSecretPersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token-secret")

	first, err := loadOrCreateTokenSecret(path)
	if err != nil {
		t.Fatalf("first loadOrCreateTokenSecret() error = %v", err)
	}
	second, err := loadOrCreateTokenSecret(path)
	if err != nil {
		t.Fatalf("second loadOrCreateTokenSecret() error = %v", err)
	}
	if first == "" {
		t.Fatal("generated token secret is empty")
	}
	if first != second {
		t.Fatalf("token secret changed across reload: %q != %q", first, second)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(token secret) error = %v", err)
	}
	if len(body) == 0 {
		t.Fatal("persisted token secret file is empty")
	}
}

func TestDataDirDerivesPersistentPaths(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("ACCOUNTS_PATH", filepath.Join(dataDir, "accounts.json"))
	t.Setenv("TOKEN_SECRET_PATH", filepath.Join(dataDir, "token-secret"))
	t.Setenv("USAGE_DB_PATH", filepath.Join(dataDir, "usage.db"))

	got := loadConfig()
	if got.DataDir != dataDir {
		t.Fatalf("DataDir = %q, want %q", got.DataDir, dataDir)
	}
	if got.AccountsPath != filepath.Join(dataDir, "accounts.json") {
		t.Fatalf("AccountsPath = %q", got.AccountsPath)
	}
	if got.TokenSecretPath != filepath.Join(dataDir, "token-secret") {
		t.Fatalf("TokenSecretPath = %q", got.TokenSecretPath)
	}
	if got.UsageDBPath != filepath.Join(dataDir, "usage.db") {
		t.Fatalf("UsageDBPath = %q", got.UsageDBPath)
	}
}

func verifyLoginWithConfig(config Config, username, password string) bool {
	old := cfg
	cfg = config
	defer func() { cfg = old }()
	return verifyLogin(username, password)
}
