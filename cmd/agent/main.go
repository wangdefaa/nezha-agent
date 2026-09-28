package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blang/semver"
	ping "github.com/prometheus-community/pro-bing"
	utls "github.com/refraction-networking/utls"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/urfave/cli/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/resolver"

	"github.com/nezhahq/agent/cmd/agent/commands"
	"github.com/nezhahq/agent/model"
	"github.com/nezhahq/agent/pkg/logger"
	"github.com/nezhahq/agent/pkg/monitor"
	"github.com/nezhahq/agent/pkg/util"
	utlsx "github.com/nezhahq/agent/pkg/utls"
	pb "github.com/nezhahq/agent/proto"
)

var (
	version           = monitor.Version // 来自于 GoReleaser 的版本号
	arch              string
	executablePath    string
	defaultConfigPath = loadDefaultConfigPath()
	agentConfig       model.AgentConfig

	// 以下状态跨重连保留。重连前只在重连间隔内有限等待上一代守护协程退出，个别
	// 卡住的旧协程（例如仍在 FetchIP）可能与新一代短暂并存，因此一律用原子量读写。
	initialized           atomic.Bool
	prevDashboardBootTime atomic.Uint64 // 面板上次启动时间
	geoipReported         atomic.Bool   // 在面板重启后是否上报成功过 GeoIP
	lastReportHostInfo    atomicTime
	lastReportIPInfo      atomicTime

	hostStatus atomic.Bool
	ipStatus   atomic.Bool

	insecureTLSWarnOnce sync.Once

	// dnsResolver 使用系统 DNS（/etc/resolv.conf），供未自定义 dns 时的 ICMP/TCP 拨测解析内网域名
	dnsResolver = &net.Resolver{PreferGo: true}
	httpClient  = &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: time.Second * 30,
	}
)

var (
	println = logger.Println
	printf  = logger.Printf
)

const (
	delayWhenError = time.Second * 10 // Agent 重连间隔
	networkTimeOut = time.Second * 5  // 普通网络超时

	minUpdateInterval = 1440
	maxUpdateInterval = 2880

	binaryName = "nezha-agent"

	// maxConcurrentTasks 限制同时执行的任务数。每个 task 各起一个 goroutine，
	// 不设上限时面板（或冒充面板的中间人）洪泛任务即可耗尽内存与 fd。
	maxConcurrentTasks = 256
	// maxProbeBodyBytes 拨测只关心时延与状态码，响应体最多读 1MiB，避免被当作下载放大器。
	maxProbeBodyBytes = 1 << 20
	// maxRecvMsgBytes 面板下发的消息（Task/回执/GeoIP）都很小；gRPC 默认允许单条 4MiB，
	// 配合任务并发上限，恶意面板仍可让每个任务携带 4MiB 数据，这里收紧到 64KiB。
	maxRecvMsgBytes = 64 << 10
)

var (
	recvLimitOption = grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxRecvMsgBytes))

	taskSlots = make(chan struct{}, maxConcurrentTasks)
	upgrading atomic.Bool // 强制更新单飞，重复下发的 Upgrade 直接忽略
)

// 流操作限时：超时即取消整个连接会话并重连。面板每 20 秒下发一次 Keepalive，
// taskRecvTimeout 内收不到任何任务即视为连接已失活。声明为变量以便测试缩短。
var (
	taskRecvTimeout = time.Second * 30
	reportTimeout   = time.Second * 10
)

func setEnv() {
	resolver.SetDefaultScheme("passthrough")
	net.DefaultResolver.PreferGo = true // 使用 Go 内置的 DNS 解析器解析域名
	net.DefaultResolver.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		d := net.Dialer{
			Timeout: time.Second * 5,
		}
		dnsServers := util.DNSServersAll
		if len(agentConfig.DNS) > 0 {
			dnsServers = agentConfig.DNS
		}
		var conn net.Conn
		var err error
		for _, server := range util.RangeRnd(dnsServers) {
			conn, err = d.DialContext(ctx, "udp", server)
			if err == nil {
				return conn, nil
			}
		}
		return nil, err
	}
	headers := util.BrowserHeaders()
	http.DefaultClient.Timeout = time.Second * 30
	httpClient.Transport = utlsx.NewUTLSHTTPRoundTripperWithProxy(
		utls.HelloChrome_Auto, new(utls.Config),
		http.DefaultTransport, nil, headers,
	)
}

func loadDefaultConfigPath() string {
	var err error
	executablePath, err = os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(executablePath), "config.yml")
}

func preRun(configPath string) error {
	// init
	setEnv()

	if configPath == "" {
		configPath = defaultConfigPath
	}
	if err := checkWindowsArch(); err != nil {
		return err
	}
	if err := agentConfig.Read(configPath); err != nil {
		return fmt.Errorf("init config failed: %v", err)
	}
	warnPlaintextTransport(&agentConfig)

	monitor.InitConfig(&agentConfig)
	monitor.CustomEndpoints = agentConfig.CustomIPApi

	return nil
}

// checkWindowsArch 仅 Windows：校验二进制 arch 与系统 arch 是否匹配。
func checkWindowsArch() error {
	if runtime.GOOS != "windows" {
		return nil
	}
	hostArch, err := host.KernelArch()
	if err != nil {
		return err
	}
	switch hostArch {
	case "i386", "i686":
		hostArch = "386"
	case "x86_64":
		hostArch = "amd64"
	case "aarch64":
		hostArch = "arm64"
	}
	if arch != hostArch {
		return fmt.Errorf("与当前系统不匹配，当前运行 %s_%s, 需要下载 %s_%s", runtime.GOOS, arch, runtime.GOOS, hostArch)
	}
	return nil
}

// warnPlaintextTransport tls=false 时 client_secret 以明文随每次 RPC 发送，中间人可截获
// 并冒充面板下发任务（insecure_tls 的告警见 dialOptions）。用标准库 log 输出，
// 确保非 debug 模式下也能看到；面板在本机回环地址时不告警。
func warnPlaintextTransport(c *model.AgentConfig) {
	if c.TLS {
		return
	}
	host, _, _ := net.SplitHostPort(c.Server)
	if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
		return
	}
	log.Printf("警告: tls=false，client_secret 将以明文发送到 %s，中间人可截获并冒充面板；公网部署请启用 tls", c.Server)
}

func main() {
	app := &cli.App{
		Usage:    "哪吒监控 Agent",
		Version:  version,
		Flags:    []cli.Flag{configFlag()},
		Action:   runAction,
		Commands: []*cli.Command{editCommand(), serviceCommand()},
	}
	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

// configFlag 各命令共用的 -c/--config 参数。
func configFlag() cli.Flag {
	return &cli.StringFlag{Name: "config", Aliases: []string{"c"}, Usage: "配置文件路径"}
}

// configPathOrDefault 返回 -c 指定的配置路径，留空时为二进制同目录的 config.yml。
func configPathOrDefault(c *cli.Context) string {
	if p := c.String("config"); p != "" {
		return p
	}
	return defaultConfigPath
}

// runAction 默认动作：读取配置后以前台或服务模式运行 agent（preRun 自行处理空路径）。
func runAction(c *cli.Context) error {
	if err := preRun(c.String("config")); err != nil {
		return err
	}
	runService("", "")
	return nil
}

// editCommand 交互式编辑配置。
func editCommand() *cli.Command {
	return &cli.Command{
		Name:  "edit",
		Usage: "编辑配置文件",
		Flags: []cli.Flag{configFlag()},
		Action: func(c *cli.Context) error {
			commands.EditAgentConfig(configPathOrDefault(c), &agentConfig)
			return nil
		},
	}
}

// serviceCommand 服务操作，配置路径转为绝对路径写入服务参数。
func serviceCommand() *cli.Command {
	return &cli.Command{
		Name:      "service",
		Usage:     "服务操作",
		UsageText: "<install/uninstall/start/stop/restart>",
		Flags:     []cli.Flag{configFlag()},
		Action: func(c *cli.Context) error {
			action := c.Args().Get(0)
			if action == "" {
				return cli.Exit("必须指定一个参数", 1)
			}
			ap, _ := filepath.Abs(configPathOrDefault(c))
			runService(action, ap)
			return nil
		},
	}
}

func run() {
	// 配置热重载已移除，agentConfig 在 run() 启动前由 preRun 一次性读入，运行期
	// 不再轮转。AuthHandler 闭包直接读 agentConfig.ClientSecret/UUID 即可——
	// 没有并发写入方，不存在 torn read 问题。
	auth := model.AuthHandler{
		Credentials: func() (string, string) {
			return agentConfig.ClientSecret, agentConfig.UUID
		},
		RequireTLS: func() bool {
			return agentConfig.TLS
		},
	}

	startSelfUpdate()

	for {
		conn, err := grpc.NewClient(agentConfig.Server, dialOptions(&auth)...)
		if err != nil {
			printf("与面板建立连接失败: %v", err)
			waitReconnect(nil, nil)
			continue
		}
		printf("Connection to %s established", agentConfig.Server)
		session := newAgentSession(pb.NewNezhaServiceClient(conn))
		// 各步骤失败原因已在 serveConnection 内记录，这里只负责断开重连
		_ = serveConnection(session)
		waitReconnect(conn, session)
	}
}

// startSelfUpdate 启动时检查一次更新，之后按 self_update_period（未配置则随机
// 1~2 天）定时检查；更新成功即退出进程，由服务管理器以新版本拉起。
func startSelfUpdate() {
	if _, err := semver.Parse(version); err != nil || agentConfig.DisableAutoUpdate {
		return
	}
	if doSelfUpdate(true) {
		os.Exit(1)
	}
	var interval time.Duration
	if agentConfig.SelfUpdatePeriod > 0 {
		interval = time.Duration(agentConfig.SelfUpdatePeriod) * time.Minute
	} else {
		interval = time.Duration(rand.Intn(maxUpdateInterval-minUpdateInterval)+minUpdateInterval) * time.Minute
	}
	go func() {
		for range time.Tick(interval) {
			if doSelfUpdate(true) {
				os.Exit(1)
			}
		}
	}()
}

// dialOptions 按配置构造传输安全与逐次调用鉴权选项。
func dialOptions(auth *model.AuthHandler) []grpc.DialOption {
	creds := insecure.NewCredentials()
	if agentConfig.TLS {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
		if agentConfig.InsecureTLS {
			// 跳过证书校验会暴露于中间人攻击，仅应在可信内网或自签证书场景显式开启
			insecureTLSWarnOnce.Do(func() {
				log.Println("WARNING: TLS certificate verification is disabled (insecure_tls=true). Use only in trusted environments.")
			})
			tlsConfig.InsecureSkipVerify = true // #nosec G402 -- 用户显式开启 insecure_tls，已告警
		}
		creds = credentials.NewTLS(tlsConfig)
	}
	return []grpc.DialOption{grpc.WithTransportCredentials(creds), grpc.WithPerRPCCredentials(auth), recvLimitOption}
}

// waitReconnect 关闭旧连接，并在 delayWhenError 的重连间隔内等待上一代守护协程
// 退出。关闭连接会让旧协程手上的流与 RPC 立即出错返回；个别仍卡在本地采集或
// FetchIP 的旧协程等不到也不阻塞重连（跨代共享状态均为原子量）。
func waitReconnect(conn *grpc.ClientConn, session *agentSession) {
	deadline := time.Now().Add(delayWhenError)
	initialized.Store(false)
	if conn != nil {
		conn.Close()
	}
	if session != nil && !session.waitDaemons(delayWhenError) {
		println("上一代守护协程仍未退出，继续重连")
	}
	time.Sleep(time.Until(deadline))
	println("Try to reconnect ...")
}

// newSerialTaskResultSender 用互斥锁串行化 RequestTask 流的 Send。dispatchAgentTask
// 为每个 task 起 goroutine，它们共享同一条流；gRPC Go 禁止并发 SendMsg，所有结果都
// 必须经此串行化，否则并发的拨测结果会损坏流。
func newSerialTaskResultSender(send func(*pb.TaskResult) error) func(*pb.TaskResult) error {
	var mu sync.Mutex
	return func(r *pb.TaskResult) error {
		mu.Lock()
		defer mu.Unlock()
		return send(r)
	}
}

// receiveTasksDaemon 循环接收面板任务。面板每 20 秒下发一次 Keepalive，超过
// taskRecvTimeout 收不到任何任务即取消会话触发重连（与原 doWithTimeout 语义一致）。
func receiveTasksDaemon(s *agentSession, tasks pb.NezhaService_RequestTaskClient) {
	send := newSerialTaskResultSender(tasks.Send)
	cancel := func() { s.cancel(errSendTaskResult) }
	for {
		task, err := callWithDeadline(s.cancel, taskRecvTimeout, tasks.Recv)
		if err != nil {
			printf("receiveTasks exit: %v", err)
			s.cancel(err)
			return
		}
		dispatchAgentTask(task, send, cancel)
	}
}

// dispatchAgentTask 以 goroutine 派发 task：拨测（HTTPGet/Ping）可能跑很久，
// 不能阻塞接收循环。并发数受 taskSlots 限制；满载时在接收循环内同步回复失败，
// 既不再新起 goroutine，也对洪泛方形成背压。
func dispatchAgentTask(task *pb.Task, send func(*pb.TaskResult) error, cancel context.CancelFunc) {
	select {
	case taskSlots <- struct{}{}:
		go func() {
			defer func() { <-taskSlots }()
			runAgentTask(task, send, cancel)
		}()
	default:
		rejectAgentTask(task, send, cancel)
	}
}

// rejectAgentTask 并发已满时回复失败结果（面板对 Keepalive/Upgrade 结果不做处理）。
func rejectAgentTask(task *pb.Task, send func(*pb.TaskResult) error, cancel context.CancelFunc) {
	result := &pb.TaskResult{Id: task.GetId(), Type: task.GetType(), Data: "agent busy: too many concurrent tasks"}
	if err := send(result); err != nil {
		printf("send task result exit: %v", err)
		cancel()
	}
}

func runAgentTask(task *pb.Task, send func(*pb.TaskResult) error, cancel context.CancelFunc) {
	defer func() {
		if err := recover(); err != nil {
			println("task panic", task, err)
		}
	}()
	result := doTask(task)
	if result == nil {
		return
	}
	if err := send(result); err != nil {
		printf("send task result exit: %v", err)
		cancel()
	}
}

func doTask(task *pb.Task) *pb.TaskResult {
	var result pb.TaskResult
	result.Id = task.GetId()
	result.Type = task.GetType()
	switch task.GetType() {
	case model.TaskTypeHTTPGet:
		handleHttpGetTask(task, &result)
	case model.TaskTypeICMPPing:
		handleIcmpPingTask(task, &result)
	case model.TaskTypeTCPPing:
		handleTcpPingTask(task, &result)
	case model.TaskTypeUpgrade:
		handleUpgradeTask(task, &result)
	case model.TaskTypeKeepalive:
	default:
		printf("不支持的任务: %v", task)
		return nil
	}
	return &result
}

// reportStateDaemon 向server上报状态信息
func reportStateDaemon(s *agentSession, stream pb.NezhaService_ReportSystemStateClient) {
	for {
		if err := reportState(s, stream); err != nil {
			printf("reportStateDaemon exit: %v", err)
			s.cancel(err)
			return
		}
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(time.Second * time.Duration(agentConfig.ReportDelay)):
		}
	}
}

func reportState(s *agentSession, stream pb.NezhaService_ReportSystemStateClient) error {
	if err := stream.Context().Err(); err != nil {
		return err
	}
	if initialized.Load() {
		if err := sendState(s, stream); err != nil {
			return err
		}
	}
	// 每10分钟重新获取一次硬件信息
	if lastReportHostInfo.Load().Before(time.Now().Add(-10*time.Minute)) && reportHost(s) {
		lastReportHostInfo.Store(time.Now())
	}
	reportGeoIPIfDue(s)
	return nil
}

// sendState 采集并推送一次状态；推送与等待回执各限时 reportTimeout，超时取消会话。
// 采集本身不计入限时：本地采集变慢不应被当成连接失活。
func sendState(s *agentSession, stream pb.NezhaService_ReportSystemStateClient) error {
	monitor.TrackNetworkSpeed()
	state := monitor.GetState(agentConfig.SkipConnectionCount, agentConfig.SkipProcsCount).PB()
	if _, err := callWithDeadline(s.cancel, reportTimeout, func() (struct{}, error) {
		return struct{}{}, stream.Send(state)
	}); err != nil {
		return err
	}
	_, err := callWithDeadline(s.cancel, reportTimeout, stream.Recv)
	return err
}

// reportGeoIPIfDue 到达 ip_report_period，或面板重启后尚未上报过时更新 GeoIP。
func reportGeoIPIfDue(s *agentSession) {
	reported := geoipReported.Load()
	if reported && time.Since(lastReportIPInfo.Load()) <= time.Second*time.Duration(agentConfig.IPReportPeriod) {
		return
	}
	if reportGeoIP(s, agentConfig.UseIPv6CountryCode, !reported) {
		lastReportIPInfo.Store(time.Now())
		geoipReported.Store(true)
	}
}

func reportHost(s *agentSession) bool {
	if !hostStatus.CompareAndSwap(false, true) {
		return false
	}
	defer hostStatus.Store(false)
	if !initialized.Load() {
		return true
	}
	// 超时 ctx 派生自会话 ctx：会话取消即中止 RPC，不再遗留超时后仍在跑的孤儿调用
	ctx, cancel := context.WithTimeout(s.ctx, reportTimeout)
	defer cancel()
	receipt, err := s.client.ReportSystemInfo2(ctx, monitor.GetHost().PB())
	if err != nil {
		printf("ReportSystemInfo2 error: %v", err)
		return false
	}
	prevBootTime := prevDashboardBootTime.Load()
	geoipReported.Store(geoipReported.Load() && prevBootTime > 0 && receipt.GetData() == prevBootTime)
	return true
}

// reportGeoIP 的 monitor.GeoQueryIP* 等状态只在 ipStatus 互斥区内读写，无需额外加锁。
func reportGeoIP(s *agentSession, use6, forceUpdate bool) bool {
	if !ipStatus.CompareAndSwap(false, true) {
		return false
	}
	defer ipStatus.Store(false)
	if !initialized.Load() {
		return false
	}
	pbg := monitor.FetchIP(use6)
	if pbg == nil {
		return false
	}
	if !monitor.GeoQueryIPChanged && !forceUpdate {
		return true
	}
	ctx, cancel := context.WithTimeout(s.ctx, reportTimeout)
	defer cancel()
	geoip, err := s.client.ReportGeoIP(ctx, pbg)
	if err != nil {
		return false
	}
	prevDashboardBootTime.Store(geoip.GetDashboardBootTime())
	monitor.CachedCountryCode = geoip.GetCountryCode()
	monitor.GeoQueryIPChanged = false
	return true
}

func handleUpgradeTask(*pb.Task, *pb.TaskResult) {
	if agentConfig.DisableForceUpdate || !upgrading.CompareAndSwap(false, true) {
		return
	}
	defer upgrading.Store(false)
	if doSelfUpdate(false) {
		os.Exit(1)
	}
}

func handleTcpPingTask(task *pb.Task, result *pb.TaskResult) {
	if agentConfig.DisableSendQuery {
		result.Data = "This server has disabled query sending"
		return
	}

	host, port, err := net.SplitHostPort(task.GetData())
	if err != nil {
		result.Data = err.Error()
		return
	}
	ipAddr, err := lookupIP(host)
	if err != nil {
		result.Data = err.Error()
		return
	}
	addr := net.JoinHostPort(ipAddr, port)
	printf("TCP-Ping Task: Pinging %s", addr)
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, time.Second*10)
	if err != nil {
		result.Data = err.Error()
	} else {
		conn.Close()
		result.Delay = float32(time.Since(start).Microseconds()) / 1000.0
		result.Successful = true
	}
}

func handleIcmpPingTask(task *pb.Task, result *pb.TaskResult) {
	if agentConfig.DisableSendQuery {
		result.Data = "This server has disabled query sending"
		return
	}
	ipAddr, err := lookupIP(task.GetData())
	printf("ICMP-Ping Task: Pinging %s(%s)", task.GetData(), ipAddr)
	if err != nil {
		result.Data = err.Error()
		return
	}
	stat, err := runPinger(ipAddr)
	if err != nil {
		result.Data = err.Error()
		return
	}
	if stat.PacketsRecv == 0 {
		result.Data = "packets recv 0"
		return
	}
	result.Delay = float32(stat.AvgRtt.Microseconds()) / 1000.0
	result.Successful = true
}

// runPinger 以特权模式（raw socket，需 CAP_NET_RAW）发 5 个 ICMP 包，总超时 20s。
func runPinger(ipAddr string) (*ping.Statistics, error) {
	pinger, err := ping.NewPinger(ipAddr)
	if err != nil {
		return nil, err
	}
	pinger.SetPrivileged(true)
	pinger.Count = 5
	pinger.Timeout = time.Second * 20
	if err := pinger.Run(); err != nil { // Blocks until finished.
		return nil, err
	}
	return pinger.Statistics(), nil
}

func handleHttpGetTask(task *pb.Task, result *pb.TaskResult) {
	if agentConfig.DisableSendQuery {
		result.Data = "This server has disabled query sending"
		return
	}
	taskUrl := task.GetData()
	if !isHTTPURL(taskUrl) {
		result.Data = "invalid URL: only http and https schemes are supported"
		return
	}
	printf("HTTP-GET Task: %s", taskUrl)
	certSummary, delay, err := httpGetProbe(taskUrl)
	result.Delay = delay
	if err != nil {
		// HTTP 请求失败
		result.Data = err.Error()
		return
	}
	// SSL 证书信息
	result.Data = certSummary
	result.Successful = true
}

// httpGetProbe 发起 GET 并读取响应体（至多 maxProbeBodyBytes），返回证书摘要与耗时
// （毫秒）；状态码不在 200~399 视为应用错误，此时仍返回耗时。
func httpGetProbe(taskUrl string) (string, float32, error) {
	start := time.Now()
	resp, err := httpClient.Get(taskUrl)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxProbeBodyBytes)); err != nil {
		return "", 0, err
	}
	delay := float32(time.Since(start).Microseconds()) / 1000.0
	if resp.StatusCode > 399 || resp.StatusCode < 200 {
		return "", delay, errors.New("\n应用错误: " + resp.Status)
	}
	return tlsCertSummary(resp), delay, nil
}

// isHTTPURL 仅放行 http/https，拨测目标来自面板下发，拒绝其它 scheme（对齐上游 CodeQL 修复）。
func isHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}

// tlsCertSummary 返回首张证书的签发者与到期时间（"Issuer|NotAfter"），非 TLS 响应返回空串。
func tlsCertSummary(resp *http.Response) string {
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		return ""
	}
	c := resp.TLS.PeerCertificates[0]
	return c.Issuer.CommonName + "|" + c.NotAfter.String()
}

func lookupIP(hostOrIp string) (string, error) {
	if net.ParseIP(hostOrIp) == nil {
		ips, err := probeResolver().LookupIPAddr(context.Background(), hostOrIp)
		if err != nil {
			return "", err
		}
		if len(ips) == 0 {
			return "", fmt.Errorf("无法解析 %s", hostOrIp)
		}
		return ips[0].IP.String(), nil
	}
	return hostOrIp, nil
}

// probeResolver 配置了 dns 时用 net.DefaultResolver（setEnv 已令其只拨这些服务器），
// 否则沿用系统 DNS。原实现 ICMP/TCP 拨测恒用 dnsResolver，自定义 dns 对其不生效。
func probeResolver() *net.Resolver {
	if len(agentConfig.DNS) > 0 {
		return net.DefaultResolver
	}
	return dnsResolver
}
