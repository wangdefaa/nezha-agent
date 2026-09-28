package model

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// install.sh 在 NZ_UUID 非空时写出 0644 配置且不会触发 Save；Read 必须自行收紧权限。
func TestReadTightensWorldReadableConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode semantics are unix-specific")
	}
	path := filepath.Join(t.TempDir(), "config.yml")
	seed := "server: dash:8008\nclient_secret: s3cr3t\nuuid: 5f0e4b52-4d9b-4a52-9d6a-0b8c3a9b7c11\n"
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	var c AgentConfig
	if err := c.Read(path); err != nil {
		t.Fatalf("Read: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("Read 后配置应为 0600, got %#o", fi.Mode().Perm())
	}
}
