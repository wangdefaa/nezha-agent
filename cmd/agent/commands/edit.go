package commands

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/hashicorp/go-uuid"
	"github.com/shirou/gopsutil/v4/disk"
	psnet "github.com/shirou/gopsutil/v4/net"

	"github.com/nezhahq/agent/model"
)

type editAnswers struct {
	Nic   []string `mapstructure:"nic_allowlist" json:"nic_allowlist"`
	Disk  []string `mapstructure:"hard_drive_partition_allowlist" json:"hard_drive_partition_allowlist"`
	DNS   string   `mapstructure:"dns" json:"dns"`
	Debug bool     `mapstructure:"debug" json:"debug"`
	UUID  string   `mapstructure:"uuid" json:"uuid"`
}

// EditAgentConfig 交互式修改 Agent 要监控的网卡、硬盘分区、DNS、UUID 与调试开关。
func EditAgentConfig(configPath string, agentConfig *model.AgentConfig) {
	if err := agentConfig.Read(configPath); err != nil {
		fmt.Println("读取配置出错（仍可继续编辑）:", err)
	}
	qs, err := editQuestions(agentConfig)
	if err != nil {
		fmt.Println("获取网卡/磁盘信息失败:", err)
		return
	}
	var answers editAnswers
	if err := survey.Ask(qs, &answers, survey.WithValidator(survey.Required)); err != nil {
		fmt.Println("选择错误", err.Error())
		return
	}
	if err := applyEditAnswers(agentConfig, &answers); err != nil {
		fmt.Println(err)
		return
	}
	if err := agentConfig.Save(); err != nil {
		fmt.Println("保存配置失败:", err)
		return
	}
	fmt.Println("修改自定义配置成功，重启 Agent 后生效")
}

// editQuestions 构造交互问题，选项来自本机网卡与磁盘分区。
func editQuestions(c *model.AgentConfig) ([]*survey.Question, error) {
	nics, err := nicOptions()
	if err != nil {
		return nil, err
	}
	disks, err := diskOptions()
	if err != nil {
		return nil, err
	}
	id, err := uuid.GenerateUUID()
	if err != nil {
		return nil, err
	}
	return []*survey.Question{
		{Name: "nic", Prompt: &survey.MultiSelect{Message: "选择要监控的网卡", Options: nics}},
		{Name: "disk", Prompt: &survey.MultiSelect{Message: "选择要监控的硬盘分区", Options: disks}},
		{Name: "dns", Prompt: &survey.Input{
			Message: "自定义 DNS，可输入空格跳过，如 1.1.1.1:53,1.0.0.1:53",
			Default: strings.Join(c.DNS, ","),
		}},
		{Name: "uuid", Prompt: &survey.Input{
			Message: "输入 Agent UUID",
			Default: c.UUID,
			Suggest: func(string) []string { return []string{id} },
		}},
		{Name: "debug", Prompt: &survey.Confirm{Message: "是否开启调试模式？", Default: false}},
	}, nil
}

func nicOptions() ([]string, error) {
	nc, err := psnet.IOCounters(true)
	if err != nil {
		return nil, err
	}
	opts := make([]string, 0, len(nc))
	for _, v := range nc {
		opts = append(opts, v.Name)
	}
	return opts, nil
}

func diskOptions() ([]string, error) {
	parts, err := disk.Partitions(false)
	if err != nil {
		return nil, err
	}
	opts := make([]string, 0, len(parts))
	for _, p := range parts {
		opts = append(opts, fmt.Sprintf("%s\t%s\t%s", p.Mountpoint, p.Fstype, p.Device))
	}
	return opts, nil
}

// applyEditAnswers 把交互结果写回配置；磁盘选项只取挂载点一列。
func applyEditAnswers(c *model.AgentConfig, a *editAnswers) error {
	dns, err := parseDNSServers(a.DNS)
	if err != nil {
		return err
	}
	c.HardDrivePartitionAllowlist = []string{}
	for _, v := range a.Disk {
		c.HardDrivePartitionAllowlist = append(c.HardDrivePartitionAllowlist, strings.Split(v, "\t")[0])
	}
	c.NICAllowlist = make(map[string]bool)
	for _, v := range a.Nic {
		c.NICAllowlist[v] = true
	}
	c.DNS = dns
	c.Debug = a.Debug
	c.UUID = a.UUID
	return nil
}

// parseDNSServers 解析逗号分隔的 ip:port 列表，留空返回空列表。
func parseDNSServers(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return []string{}, nil
	}
	servers := strings.Split(s, ",")
	for _, server := range servers {
		host, _, err := net.SplitHostPort(server)
		if err == nil && net.ParseIP(host) == nil {
			err = errors.New("格式错误")
		}
		if err != nil {
			return nil, fmt.Errorf("自定义 DNS 格式错误：%s %v", server, err)
		}
	}
	return servers, nil
}
