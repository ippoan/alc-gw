package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// withLocalAppData points %LOCALAPPDATA% (and hence Dir/Path) at a temp dir
// for the duration of the test.
func withLocalAppData(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	return dir
}

func TestLoadMissingFileReturnsZeroValue(t *testing.T) {
	withLocalAppData(t)

	c := Load()
	if c != (Config{}) {
		t.Errorf("Load() on missing file = %+v, want zero value", c)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	withLocalAppData(t)

	want := Config{
		RTSPURL:     "rtsp://user:pass@192.168.1.50/stream1",
		WHIPRTSPURL: "rtsp://user:pass@192.168.1.50/stream2",
		WHIPURL:     "https://sfu.example.net/ingest/site1",
		WHIPToken:   "secret-token",
	}
	if err := Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got := Load()
	if got != want {
		t.Errorf("Load() = %+v, want %+v", got, want)
	}
}

// whip_url が設定ファイルに存在しない (未対応の旧バージョンで書かれた等)
// 場合、WHIP publish は無効のまま (ゼロ値) で他のフィールドは正しく読める。
func TestLoadWithoutWHIPFieldsStaysDisabled(t *testing.T) {
	dir := withLocalAppData(t)
	if err := os.MkdirAll(filepath.Join(dir, "alc-gw"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"rtsp_url": "rtsp://cam/stream1"})
	if err := os.WriteFile(Path(), b, 0o644); err != nil {
		t.Fatal(err)
	}

	c := Load()
	if c.RTSPURL != "rtsp://cam/stream1" {
		t.Errorf("RTSPURL = %q", c.RTSPURL)
	}
	if c.WHIPURL != "" {
		t.Errorf("WHIPURL = %q, want empty (publish disabled)", c.WHIPURL)
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	withLocalAppData(t)

	if err := Save(Config{
		RTSPURL:     "rtsp://file/stream1",
		WHIPRTSPURL: "rtsp://file/stream2",
		WHIPURL:     "https://file.example/ingest/site1",
		WHIPToken:   "file-token",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	t.Setenv("ALC_GW_RTSP_URL", "rtsp://env/stream1")
	t.Setenv("ALC_GW_WHIP_RTSP_URL", "rtsp://env/stream2")
	t.Setenv("ALC_GW_WHIP_URL", "https://env.example/ingest/site1")
	t.Setenv("ALC_GW_WHIP_TOKEN", "env-token")

	got := Load()
	want := Config{
		RTSPURL:     "rtsp://env/stream1",
		WHIPRTSPURL: "rtsp://env/stream2",
		WHIPURL:     "https://env.example/ingest/site1",
		WHIPToken:   "env-token",
	}
	if got != want {
		t.Errorf("Load() = %+v, want %+v", got, want)
	}
}
