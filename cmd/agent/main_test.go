package main

import (
	"fmt"
	"net"
	"os"
	"testing"
)

func TestLookupIP(t *testing.T) {
	if ci := os.Getenv("CI"); ci != "" { // skip if test on CI
		return
	}

	ip, err := lookupIP("www.google.com")
	fmt.Printf("ip: %v, err: %v\n", ip, err)
	if err != nil {
		t.Errorf("lookupIP failed: %v", err)
	}
	_, err = net.ResolveIPAddr("ip", "www.google.com")
	if err != nil {
		t.Errorf("ResolveIPAddr failed: %v", err)
	}

	ip, err = lookupIP("ipv6.google.com")
	fmt.Printf("ip: %v, err: %v\n", ip, err)
	if err != nil {
		t.Errorf("lookupIP failed: %v", err)
	}
	_, err = net.ResolveIPAddr("ip", "ipv6.google.com")
	if err != nil {
		t.Errorf("ResolveIPAddr failed: %v", err)
	}
}

// 自定义 dns 时 ICMP/TCP 拨测也要走 net.DefaultResolver（已按 dns 配置拨号）。
func TestProbeResolverHonorsDNSConfig(t *testing.T) {
	defer func(old []string) { agentConfig.DNS = old }(agentConfig.DNS)
	agentConfig.DNS = nil
	if probeResolver() != dnsResolver {
		t.Fatal("未配置 dns 时应使用系统 DNS")
	}
	agentConfig.DNS = []string{"1.1.1.1:53"}
	if probeResolver() != net.DefaultResolver {
		t.Fatal("配置 dns 后应使用 net.DefaultResolver")
	}
}
