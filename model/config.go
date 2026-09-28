package model

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/hashicorp/go-uuid"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/v2"
	"sigs.k8s.io/yaml"
)

// AgentConfig 为 agent 配置；YAML 文件与 NZ_ 前缀环境变量共同生效，后者优先。
type AgentConfig struct {
	Debug bool `koanf:"debug" json:"debug"`

	Server       string `koanf:"server" json:"server"`               // 服务器地址
	ClientSecret string `koanf:"client_secret" json:"client_secret"` // 客户端密钥
	UUID         string `koanf:"uuid" json:"uuid"`

	HardDrivePartitionAllowlist []string        `koanf:"hard_drive_partition_allowlist" json:"hard_drive_partition_allowlist,omitempty"`
	NICAllowlist                map[string]bool `koanf:"nic_allowlist" json:"nic_allowlist,omitempty"`
	DNS                         []string        `koanf:"dns" json:"dns,omitempty"`
	SkipConnectionCount         bool            `koanf:"skip_connection_count" json:"skip_connection_count"` // 跳过连接数检查
	SkipProcsCount              bool            `koanf:"skip_procs_count" json:"skip_procs_count"`           // 跳过进程数量检查
	DisableAutoUpdate           bool            `koanf:"disable_auto_update" json:"disable_auto_update"`     // 关闭自动更新
	DisableForceUpdate          bool            `koanf:"disable_force_update" json:"disable_force_update"`   // 关闭强制更新
	ReportDelay                 uint32          `koanf:"report_delay" json:"report_delay"`                   // 报告间隔
	TLS                         bool            `koanf:"tls" json:"tls"`                                     // 是否使用TLS加密传输至服务端
	InsecureTLS                 bool            `koanf:"insecure_tls" json:"insecure_tls"`                   // 是否禁用证书检查
	UseIPv6CountryCode          bool            `koanf:"use_ipv6_country_code" json:"use_ipv6_country_code"` // 默认优先展示IPv6旗帜
	DisableSendQuery            bool            `koanf:"disable_send_query" json:"disable_send_query"`       // 关闭发送TCP/ICMP/HTTP请求
	IPReportPeriod              uint32          `koanf:"ip_report_period" json:"ip_report_period"`           // IP上报周期
	SelfUpdatePeriod            uint32          `koanf:"self_update_period" json:"self_update_period"`       // 自动更新周期
	CustomIPApi                 []string        `koanf:"custom_ip_api" json:"custom_ip_api,omitempty"`       // 自定义 IP API                      // 重载间隔

	k        *koanf.Koanf `json:"-"`
	filePath string       `json:"-"`
}

// Read 从给定的文件目录加载配置文件
func (c *AgentConfig) Read(path string) error {
	c.k = koanf.New("")
	c.filePath = path
	saveOnce := sync.OnceFunc(c.saveOrWarn)

	if fi, err := os.Stat(path); err == nil {
		tightenConfigPerm(path, fi)
		if err := c.k.Load(fileProvider(path), new(kubeyaml)); err != nil {
			return err
		}
	} else {
		defer saveOnce()
	}
	if err := c.loadEnvAndUnmarshal(); err != nil {
		return err
	}
	if c.UUID == "" {
		id, err := uuid.GenerateUUID()
		if err != nil {
			return fmt.Errorf("generate UUID failed: %v", err)
		}
		c.UUID = id
		defer saveOnce()
	}
	return ValidateConfig(c)
}

// saveOrWarn 写回自动补全的配置；失败（如只读挂载）时提示，否则 uuid 每次重启都会变化，
// 面板上会不断出现新服务器（原实现静默忽略该错误）。
func (c *AgentConfig) saveOrWarn() {
	if err := c.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "保存配置 %s 失败，自动生成的 uuid 不会持久化: %v\n", c.filePath, err)
	}
}

// loadEnvAndUnmarshal 叠加 NZ_ 前缀的环境变量后反序列化到结构体。
func (c *AgentConfig) loadEnvAndUnmarshal() error {
	err := c.k.Load(env.Provider("NZ_", "", func(s string) string {
		return strings.ToLower(strings.TrimPrefix(s, "NZ_"))
	}), nil)
	if err != nil {
		return err
	}
	return c.k.Unmarshal("", c)
}

// tightenConfigPerm 配置含 client_secret：对组/其他用户开放权限时收紧为 0600。
// install.sh 按默认 umask 写出、或手工创建的 0644 配置在未触发 Save 时会一直全员可读；
// 这里在每次读取时收紧（Windows 不适用，失败时忽略，如只读挂载）。
func tightenConfigPerm(path string, fi os.FileInfo) {
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(path, 0o600)
	}
}

func (c *AgentConfig) Save() error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}

	dir := filepath.Dir(c.filePath)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}

	if err := os.WriteFile(c.filePath, data, 0600); err != nil {
		return err
	}
	// os.WriteFile only applies the perm argument on CREATE; an existing
	// 0644 file keeps its permissions even though we asked for 0600.
	// Explicit Chmod tightens it so the new client_secret never sits in a
	// world-readable file (a same-host attacker could otherwise read it
	// and impersonate the agent against the dashboard).
	return os.Chmod(c.filePath, 0600)
}

// ValidateConfig 补齐默认值并校验必填项。原 isRemoteEdit 参数服务于已删除的
// 配置下发功能，调用方恒传 false，已去掉。
func ValidateConfig(c *AgentConfig) error {
	if c.ReportDelay == 0 {
		c.ReportDelay = 3
	}
	if c.IPReportPeriod == 0 {
		c.IPReportPeriod = 1800
	} else if c.IPReportPeriod < 30 {
		c.IPReportPeriod = 30
	}
	if c.ReportDelay < 1 || c.ReportDelay > 4 {
		return errors.New("report-delay ranges from 1-4")
	}
	if c.Server == "" {
		return errors.New("server address should not be empty")
	}
	if c.ClientSecret == "" {
		return errors.New("client_secret must be specified")
	}
	_, err := uuid.ParseUUID(c.UUID)
	return err
}

// fileProvider 以 koanf.Provider 形式读取配置文件。替代 koanf/providers/file：
// 后者为 Watch 热重载引入 fsnotify，而本 fork 已移除热重载。
type fileProvider string

func (f fileProvider) ReadBytes() ([]byte, error) { return os.ReadFile(string(f)) }

func (f fileProvider) Read() (map[string]interface{}, error) {
	return nil, errors.New("fileProvider 需配合 Parser 使用")
}

type kubeyaml struct{}

// Unmarshal parses the given YAML bytes.
func (k *kubeyaml) Unmarshal(b []byte) (map[string]interface{}, error) {
	var out map[string]interface{}
	if err := yaml.Unmarshal(b, &out); err != nil {
		return nil, err
	}

	return out, nil
}

// Marshal marshals the given config map to YAML bytes.
func (k *kubeyaml) Marshal(o map[string]interface{}) ([]byte, error) {
	return yaml.Marshal(o)
}
