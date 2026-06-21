package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/blang/semver"
	update "github.com/inconshreveable/go-update"
	"github.com/nezhahq/go-github-selfupdate/selfupdate"
)

// updaterPublicKeyPEM 是自更新签名校验用的 ECDSA P-256 公钥（PKIX/PEM）。
// 对应私钥保存在发布流水线的 GitHub Secret AGENT_SIGNING_KEY 中，离线备份。
// 发布物（zip）在 CI 用私钥签名生成同名 .sig，Agent 下载后用本公钥验签，
// 防止上游/镜像发布渠道被篡改导致的自更新 RCE。
const updaterPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEpXK/obH2gt//xnixHrPrhYJioB6x
zXzMbcmL9rUdVtCruCWWVj1FbMCvfryS8kxJdDNqt0DMpu9uw3rn5+IxTQ==
-----END PUBLIC KEY-----`

var updaterPublicKey *ecdsa.PublicKey

// updaterHTTPClient 跟随重定向（GitHub release 资源下载会 302 到对象存储），
// 与全局 httpClient（禁止重定向）刻意区分。
var updaterHTTPClient = &http.Client{Timeout: 5 * time.Minute}

func init() {
	block, _ := pem.Decode([]byte(updaterPublicKeyPEM))
	if block == nil {
		panic("updater: 无法解析内置公钥 PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		panic(fmt.Sprintf("updater: 解析内置公钥失败: %v", err))
	}
	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		panic("updater: 内置公钥不是 ECDSA 公钥")
	}
	updaterPublicKey = ecPub
}

// httpGetBytes 下载 url 全部内容；非 200 视为失败。
func httpGetBytes(url string) ([]byte, error) {
	resp, err := updaterHTTPClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 %s 失败: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// downloadAndVerify 下载发布资源及其 .sig 签名，用内置公钥做 ECDSA 验签，
// 返回已验证的字节。后续必须对返回的同一份字节做应用，不可重新下载（防 TOCTOU）。
func downloadAndVerify(assetURL string) ([]byte, error) {
	data, err := httpGetBytes(assetURL)
	if err != nil {
		return nil, err
	}
	sig, err := httpGetBytes(assetURL + ".sig")
	if err != nil {
		return nil, fmt.Errorf("下载签名失败: %v", err)
	}
	v := &selfupdate.ECDSAValidator{PublicKey: updaterPublicKey}
	if err := v.Validate(data, sig); err != nil {
		return nil, fmt.Errorf("签名校验失败: %v", err)
	}
	return data, nil
}

// applyUpdate 解压并替换当前二进制，使用已验签的同一份字节。
func applyUpdate(data []byte, assetURL, cmdPath string) error {
	asset, err := selfupdate.UncompressCommand(bytes.NewReader(data), assetURL, binaryName)
	if err != nil {
		return err
	}
	return update.Apply(asset, update.Options{TargetPath: cmdPath})
}

// updateFromSource 检测最新发布，验签通过后应用更新。
// 无更新（未找到或版本相同）时返回 Version 等于当前版本的 Release。
func updateFromSource(slug string, v semver.Version) (*selfupdate.Release, error) {
	updater, err := selfupdate.NewUpdater(selfupdate.Config{BinaryName: binaryName})
	if err != nil {
		return nil, err
	}
	rel, found, err := updater.DetectLatest(slug)
	if err != nil {
		return nil, err
	}
	if !found || rel.Version.Equals(v) {
		return &selfupdate.Release{Version: v}, nil
	}
	data, err := downloadAndVerify(rel.AssetURL)
	if err != nil {
		return nil, err
	}
	if err := applyUpdate(data, rel.AssetURL, executablePath); err != nil {
		return nil, err
	}
	return rel, nil
}
