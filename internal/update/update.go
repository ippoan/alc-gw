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

// Apply はインストーラを DL し、3 秒後にサイレント実行→アプリ再起動する
// 子プロセスを切り離して起動する。呼び出し側はこの後すみやかに
// アプリを終了させること。
func Apply(r *Release) error {
	installer := filepath.Join(os.TempDir(), "alc-gw-"+r.Tag+"-installer.exe")
	if err := download(r.InstallerURL, installer); err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}

	// timeout: アプリ終了 (exe のロック解放) を待つ猶予
	script := fmt.Sprintf(
		`timeout /t 3 /nobreak >nul & "%s" /S & start "" "%s"`,
		installer, exe)
	cmd := exec.Command("cmd", "/C", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000008} // DETACHED_PROCESS
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
