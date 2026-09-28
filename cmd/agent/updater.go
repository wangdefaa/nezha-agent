package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

// maxUpdateAssetBytes 发布包（zip 约 7MiB）与签名的下载上限，防止异常源耗尽内存。
const maxUpdateAssetBytes = 64 << 20

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

// httpGetBytes 下载 url 全部内容；非 200 或超过 maxUpdateAssetBytes 视为失败。
func httpGetBytes(url string) ([]byte, error) {
	resp, err := updaterHTTPClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 %s 失败: HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxUpdateAssetBytes+1))
	if err == nil && len(data) > maxUpdateAssetBytes {
		return nil, fmt.Errorf("下载 %s 失败: 超过 %d 字节上限", url, maxUpdateAssetBytes)
	}
	return data, err
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

// updateBinaryName 返回发布包内可执行文件名。Windows 包内为 nezha-agent.exe；
// 直接用 binaryName 查找会失败，导致 Windows 上自更新/强制更新永远不生效。
func updateBinaryName(goos string) string {
	if goos == "windows" {
		return binaryName + ".exe"
	}
	return binaryName
}

// applyUpdate 解压并替换当前二进制，使用已验签的同一份字节。
func applyUpdate(data []byte, assetURL, cmdPath string) error {
	asset, err := selfupdate.UncompressCommand(bytes.NewReader(data), assetURL, updateBinaryName(runtime.GOOS))
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

// doSelfUpdate 执行更新检查 如果更新成功则会结束进程
func doSelfUpdate(useLocalVersion bool) (exit bool) {
	v := forcedUpdateBase(version)
	if useLocalVersion {
		var stop bool
		if v, stop, exit = checkLocalVersion(); stop {
			return exit
		}
	}
	unlock, exit, ok := acquireUpdateLock()
	if !ok {
		return exit
	}
	defer unlock()

	printf("检查更新: %v", v)
	latest, err := updateFromSource("wangdefaa/nezha-agent", v)
	if err != nil {
		printf("更新失败: %v", err)
		return false
	}
	if !latest.Version.Equals(v) {
		printf("已经更新至: %v, 正在结束进程", latest.Version)
		return true
	}
	return false
}

// forcedUpdateBase 强制更新的比较基准：能解析出当前版本时用当前版本，已是最新则不再
// 重复下载并重启（原先恒用 0.1.0，面板反复下发 Upgrade 即可让 agent 循环重启）；
// 本地构建等无版本号时仍用 0.1.0，保证总能更新到发布版。
func forcedUpdateBase(cur string) semver.Version {
	if v, err := semver.Parse(cur); err == nil {
		return v
	}
	return semver.MustParse("0.1.0")
}

// checkLocalVersion 比较运行中版本与磁盘上二进制的版本；stop=true 表示无需继续检查，
// exit=true 表示磁盘二进制已被（其它实例）更新，本进程应退出重启。
func checkLocalVersion() (v semver.Version, stop, exit bool) {
	vr, err := semver.Parse(version)
	if err != nil {
		printf("failed to parse current version string: %v", err)
		return v, true, false
	}
	vb, err := exec.Command(executablePath, "--version").Output()
	if err != nil {
		printf("failed to retrieve current executable version: %v", err)
		return v, true, false
	}
	vraw := strings.Split(strings.TrimSpace(string(vb)), " ")
	if v, err = semver.Parse(vraw[len(vraw)-1]); err != nil {
		printf("failed to parse executable version string: %v", err)
		return v, true, false
	}
	if !vr.Equals(v) {
		printf("executable version differs from current version, exiting to re-check update...")
		return v, true, true
	}
	return v, false, false
}

// updateStatFile 返回自更新互斥文件路径。放在程序目录（仅 agent 用户与 root 可写）而非
// 共享临时目录：/tmp 下的固定文件名可被同机任意用户预置（阻塞启动）或删除（迫使 agent 退出）。
func updateStatFile() string {
	return filepath.Join(filepath.Dir(executablePath), "."+filepath.Base(executablePath)+".stat")
}

// acquireUpdateLock 以 O_EXCL 创建互斥文件；已存在时等待持有者完成，对方完成后本进程应退出。
func acquireUpdateLock() (unlock func(), exit, ok bool) {
	statFile := updateStatFile()
	if _, err := os.Stat(statFile); err == nil {
		return nil, waitOtherUpdater(statFile), false
	} else if !errors.Is(err, os.ErrNotExist) {
		printf("failed to retrieve self-update stat at %s", statFile)
		return nil, false, false
	}
	stat, err := os.OpenFile(statFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		printf("failed to create self-update stat file: %v", err)
		return nil, false, false
	}
	return func() {
		stat.Close()
		if err := os.Remove(statFile); err != nil {
			printf("remove stat failed: %v", err)
		}
	}, false, true
}

// updateWaitTimeout 等待其它实例完成自更新的上限（沿用原 fsnotifyx 的 3 分钟）。
const updateWaitTimeout = 3 * time.Minute

// waitOtherUpdater 轮询等待另一进程删除互斥文件；返回 true 表示对方已完成更新。
// 原 pkg/fsnotifyx 依赖 fsnotify：文件在 Stat 与 Watch 之间被删会白等 3 分钟，
// watcher 关闭后事件协程还会读到 nil 错误并打印；每秒轮询即可，并去掉一个依赖。
func waitOtherUpdater(statFile string) bool {
	printf("found self-update stat file, waiting for another process to finish update...")
	for deadline := time.Now().Add(updateWaitTimeout); time.Now().Before(deadline); {
		if _, err := os.Stat(statFile); errors.Is(err, os.ErrNotExist) {
			return true
		}
		time.Sleep(time.Second)
	}
	os.Remove(statFile) // 超时视为上次更新中断的残留，清理后本次跳过
	printf("wait for self-update stat file timeout: %s", statFile)
	return false
}
