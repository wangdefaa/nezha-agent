package util

import (
	"context"
	"crypto/md5"
	"fmt"
	"iter"
	"net"
	"net/http"
	"strings"
	"time"
)

// MacOSChromeUA 拨测与公网 IP 查询使用的 UA。名称为历史遗留，实际取值是 agent 自身标识；
// 上游测试直接引用此名，暂不改名。
const MacOSChromeUA = "nezha-agent/1.0"

func BrowserHeaders() http.Header {
	return http.Header{
		"Accept":          {"text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,image/apng,*/*;q=0.8"},
		"Accept-Language": {"en,zh-CN;q=0.9,zh;q=0.8"},
		"User-Agent":      {MacOSChromeUA},
	}
}

func ContainsStr(slice []string, str string) bool {
	if str != "" {
		for _, item := range slice {
			if strings.Contains(str, item) {
				return true
			}
		}
	}
	return false
}

func RotateQueue1(start, i, size int) int {
	return (start + i) % size
}

func RangeRnd[S ~[]E, E any](s S) iter.Seq2[int, E] {
	index := int(time.Now().Unix()) % len(s)
	return func(yield func(int, E) bool) {
		for i := range len(s) {
			r := RotateQueue1(index, i, len(s))
			if !yield(r, s[r]) {
				break
			}
		}
	}
}

// LookupIP looks up host using the local resolver.
// It returns a slice of that host's IPv4 and IPv6 addresses.
func LookupIP(host string) ([]net.IP, error) {
	defaultResolver := net.Resolver{PreferGo: true}
	addrs, err := defaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, len(addrs))
	for i, ia := range addrs {
		ips[i] = ia.IP
	}
	return ips, nil
}

func SubUintChecked[T Unsigned](a, b T) T {
	if a < b {
		return 0
	}

	return a - b
}

func MD5Sum(str string) string {
	return fmt.Sprintf("%x", md5.Sum([]byte(str)))
}

type Unsigned interface {
	~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64
}
