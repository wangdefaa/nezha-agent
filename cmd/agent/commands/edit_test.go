package commands

import (
	"testing"

	"github.com/nezhahq/agent/model"
)

func TestParseDNSServers(t *testing.T) {
	if got, err := parseDNSServers("  "); err != nil || len(got) != 0 {
		t.Fatalf("空输入应返回空列表, got %v %v", got, err)
	}
	if got, err := parseDNSServers("1.1.1.1:53,[2606:4700:4700::1111]:53"); err != nil || len(got) != 2 {
		t.Fatalf("合法输入应解析成功, got %v %v", got, err)
	}
	for _, bad := range []string{"1.1.1.1", "dns.google:53"} {
		if _, err := parseDNSServers(bad); err == nil {
			t.Fatalf("%q 应被拒绝", bad)
		}
	}
}

func TestApplyEditAnswers(t *testing.T) {
	var c model.AgentConfig
	a := &editAnswers{Nic: []string{"eth0"}, Disk: []string{"/\text4\t/dev/sda1"}, DNS: "1.1.1.1:53", UUID: "u", Debug: true}
	if err := applyEditAnswers(&c, a); err != nil {
		t.Fatal(err)
	}
	if !c.NICAllowlist["eth0"] || c.HardDrivePartitionAllowlist[0] != "/" || c.DNS[0] != "1.1.1.1:53" || !c.Debug || c.UUID != "u" {
		t.Fatalf("answers 未正确写回: %+v", c)
	}
}
