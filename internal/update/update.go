// Package update は GitHub Releases からの自動更新。
// リリースには wails build -nsis が生成する *-installer.exe を添付する前提。
// 更新手順: インストーラを DL → アプリ終了の猶予を置いてサイレント実行
// (/S) → 完了後にアプリを再起動、を cmd のワンライナーに委ねる。
package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const releasesLatest = "https://api.github.com/repos/ippoan/alc-gw/releases/latest"

type Release struct {
	Tag          string
	InstallerURL string
}

// Check は最新リリースを取得し、現在より新しければ返す。
// 新しくなければ nil を返す。
func Check(currentVersion string) (*Release, error) {
	req, err := http.NewRequest(http.MethodGet, releasesLatest, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "alc-gw-updater")
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: 15 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update: releases API %s", res.Status)
	}

	var body struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err = json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, err
	}

	if !newer(body.TagName, currentVersion) {
		return nil, nil
	}

	for _, a := range body.Assets {
		if strings.HasSuffix(a.Name, "-installer.exe") {
			return &Release{Tag: body.TagName, InstallerURL: a.URL}, nil
		}
	}
	return nil, errors.New("update: installer asset not found in " + body.TagName)
}

// Apply はインストーラを DL し、「本体の終了を待つ → サイレント実行 →
// アプリ再起動」を行う子プロセスを切り離して起動する。呼び出し側はこの後
// すみやかにアプリを終了させること。
//
// 旧実装の `cmd /C timeout /t 3 ...` は使わない: timeout.exe はコンソールが
// 無いと "Input redirection is not supported" で即終了し、待ち 0 秒で
// インストーラが exe ロックに衝突 → NSIS がエラー音を出して更新失敗して
// いた (v0.1.5 実機)。固定秒でなく Wait-Process で自 PID の終了を確実に待つ。
func Apply(r *Release) error {
	installer := filepath.Join(os.TempDir(), "alc-gw-"+r.Tag+"-installer.exe")
	if err := download(r.InstallerURL, installer); err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}

	script := fmt.Sprintf(
		`Wait-Process -Id %d -Timeout 30 -ErrorAction SilentlyContinue; `+
			`Start-Process -FilePath '%s' -ArgumentList '/S' -Wait; `+
			`Start-Process -FilePath '%s'`,
		os.Getpid(), installer, exe)
	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script)
	// CREATE_NO_WINDOW: 不可視のコンソールを持たせる (コンソール完全無しの
	// DETACHED_PROCESS だとコンソールアプリの挙動が壊れる — timeout の教訓)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return cmd.Start()
}

func download(url, dst string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "alc-gw-updater")

	client := &http.Client{Timeout: 5 * time.Minute}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("update: download %s", res.Status)
	}

	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, res.Body)
	return err
}

// newer は tag (v1.2.3) が current (1.2.0 / dev 等) より新しいか。
// current がパースできない場合 (dev ビルド) は false。
func newer(tag, current string) bool {
	t := parse(tag)
	c := parse(current)
	if t == nil || c == nil {
		return false
	}
	for i := 0; i < 3; i++ {
		if t[i] != c[i] {
			return t[i] > c[i]
		}
	}
	return false
}

func parse(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return nil
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil
		}
		out[i] = n
	}
	return out
}
