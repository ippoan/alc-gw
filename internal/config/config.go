// Package config は GW アプリの設定 (%LOCALAPPDATA%\alc-gw\config.json)。
// 環境変数 ALC_GW_RTSP_URL が設定されていれば常にそちらを優先する
// (開発時の上書き用)。
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Config struct {
	RTSPURL string `json:"rtsp_url"`
}

// Dir は設定・ログ置き場を返す (%LOCALAPPDATA%\alc-gw)。
func Dir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = "."
	}
	return filepath.Join(base, "alc-gw")
}

// Path は設定ファイルのフルパスを返す。
func Path() string {
	return filepath.Join(Dir(), "config.json")
}

// Load は設定ファイルを読む。無ければゼロ値。
// 環境変数 ALC_GW_RTSP_URL があれば RTSPURL を上書きする。
func Load() Config {
	var c Config
	if b, err := os.ReadFile(Path()); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if env := os.Getenv("ALC_GW_RTSP_URL"); env != "" {
		c.RTSPURL = env
	}
	return c
}

// Save は設定をファイルへ書く。
func Save(c Config) error {
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(Path(), b, 0o644)
}
