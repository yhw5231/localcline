package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// persistedAccounts 是 data/accounts.json 的持久化格式。
// 环境变量仍具有最高优先级，文件用于在容器重建后保留帐号配置。
type persistedAccounts struct {
	AdminUsername string            `json:"admin_username"`
	AdminPassword string            `json:"admin_password"`
	ExtraUsers    map[string]string `json:"extra_users,omitempty"`
}

// loadPersistedAccounts 读取持久化帐号。文件不存在时返回空配置。
func loadPersistedAccounts(path string) (persistedAccounts, error) {
	var accounts persistedAccounts
	if strings.TrimSpace(path) == "" {
		return accounts, nil
	}

	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return accounts, nil
	}
	if err != nil {
		return accounts, fmt.Errorf("read accounts file: %w", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return accounts, nil
	}
	if err := json.Unmarshal(body, &accounts); err != nil {
		return accounts, fmt.Errorf("decode accounts file: %w", err)
	}
	if accounts.ExtraUsers == nil {
		accounts.ExtraUsers = map[string]string{}
	}
	return accounts, nil
}

// savePersistedAccounts 原子写入帐号文件，避免进程中断留下半个 JSON 文件。
func savePersistedAccounts(path string, accounts persistedAccounts) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if accounts.ExtraUsers == nil {
		accounts.ExtraUsers = map[string]string{}
	}

	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create accounts directory: %w", err)
		}
	}

	body, err := json.MarshalIndent(accounts, "", "  ")
	if err != nil {
		return fmt.Errorf("encode accounts file: %w", err)
	}
	body = append(body, '\n')

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("write temporary accounts file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace accounts file: %w", err)
	}
	return nil
}

// loadOrCreateTokenSecret 返回可跨进程重启复用的 token 签名密钥。
// TOKEN_SECRET 由调用方优先处理；此函数只负责 data 中的密钥文件。
func loadOrCreateTokenSecret(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("token secret path is empty")
	}

	body, err := os.ReadFile(path)
	if err == nil {
		secret := strings.TrimSpace(string(body))
		if secret == "" {
			return "", errors.New("token secret file is empty")
		}
		return secret, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read token secret: %w", err)
	}

	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("create token secret directory: %w", err)
		}
	}

	secret := randomHex(32)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return "", fmt.Errorf("read concurrently created token secret: %w", readErr)
		}
		secret = strings.TrimSpace(string(body))
		if secret == "" {
			return "", errors.New("concurrently created token secret is empty")
		}
		return secret, nil
	}
	if err != nil {
		return "", fmt.Errorf("create token secret: %w", err)
	}

	if _, err := file.WriteString(secret + "\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("write token secret: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close token secret: %w", err)
	}
	return secret, nil
}
