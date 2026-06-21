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
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blang/semver"
	"github.com/nezhahq/service"
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
	"github.com/nezhahq/agent/pkg/fsnotifyx"
	"github.com/nezhahq/agent/pkg/logger"
	"github.com/nezhahq/agent/pkg/monitor"
	"github.com/nezhahq/agent/pkg/util"
	utlsx "github.com/nezhahq/agent/pkg/utls"
	pb "github.com/nezhahq/agent/proto"
)

var (
	version               = monitor.Version // 来自于 GoReleaser 的版本号
	arch                  string
	executablePath        string
	defaultConfigPath     = loadDefaultConfigPath()
	client                pb.NezhaServiceClient
	initialized           bool
	agentConfig           model.AgentConfig
	prevDashboardBootTime uint64 // 面板上次启动时间
	geoipReported         bool   // 在面板重启后是否上报成功过 GeoIP
	lastReportHostInfo    time.Time
	lastReportIPInfo      time.Time

	hostStatus atomic.Bool
	ipStatus   atomic.Bool

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

	// windows环境处理
	if runtime.GOOS == "windows" {
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
	}

	if err := agentConfig.Read(configPath); err != nil {
		return fmt.Errorf("init config failed: %v", err)
	}

	monitor.InitConfig(&agentConfig)
	monitor.CustomEndpoints = agentConfig.CustomIPApi

	return nil
}

func main() {
	app := &cli.App{
		Usage:   "哪吒监控 Agent",
		Version: version,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "config", Aliases: []string{"c"}, Usage: "配置文件路径"},
		},
		Action: func(c *cli.Context) error {
			if path := c.String("config"); path != "" {
				if err := preRun(path); err != nil {
					return err
				}
			} else {
				if err := preRun(""); err != nil {
					return err
				}
			}
			runService("", "")
			return nil
		},
		Commands: []*cli.Command{
			{
				Name:  "edit",
				Usage: "编辑配置文件",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "config", Aliases: []string{"c"}, Usage: "配置文件路径"},
				},
				Action: func(c *cli.Context) error {
					if path := c.String("config"); path != "" {
						commands.EditAgentConfig(path, &agentConfig)
					} else {
						commands.EditAgentConfig(defaultConfigPath, &agentConfig)
					}
					return nil
				},
			},
			{
				Name:      "service",
				Usage:     "服务操作",
				UsageText: "<install/uninstall/start/stop/restart>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "config", Aliases: []string{"c"}, Usage: "配置文件路径"},
				},
				Action: func(c *cli.Context) error {
					if arg := c.Args().Get(0); arg != "" {
						if path := c.String("config"); path != "" {
							ap, _ := filepath.Abs(path)
							runService(arg, ap)
						} else {
							ap, _ := filepath.Abs(defaultConfigPath)
							runService(arg, ap)
						}
						return nil
					}
					return cli.Exit("必须指定一个参数", 1)
				},
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
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

	// 定时检查更新
	if _, err := semver.Parse(version); err == nil && !agentConfig.DisableAutoUpdate {
		if doSelfUpdate(true) {
			os.Exit(1)
		}
		go func() {
			var interval time.Duration
			if agentConfig.SelfUpdatePeriod > 0 {
				interval = time.Duration(agentConfig.SelfUpdatePeriod) * time.Minute
			} else {
				interval = time.Duration(rand.Intn(maxUpdateInterval-minUpdateInterval)+minUpdateInterval) * time.Minute
			}
			for range time.Tick(interval) {
				if doSelfUpdate(true) {
					os.Exit(1)
				}
			}
		}()
	}

	var err error
	var dashboardBootTimeReceipt *pb.Uint64Receipt
	var conn *grpc.ClientConn

	retry := func() {
		initialized = false
		if conn != nil {
			conn.Close()
		}
		time.Sleep(delayWhenError)
		println("Try to reconnect ...")
	}

	for {
		var securityOption grpc.DialOption
		if agentConfig.TLS {
			if agentConfig.InsecureTLS {
				securityOption = grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}))
			} else {
				securityOption = grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12}))
			}
		} else {
			securityOption = grpc.WithTransportCredentials(insecure.NewCredentials())
		}
		conn, err = grpc.NewClient(agentConfig.Server, securityOption, grpc.WithPerRPCCredentials(&auth))
		if err != nil {
			printf("与面板建立连接失败: %v", err)
			retry()
			continue
		}
		client = pb.NewNezhaServiceClient(conn)
		printf("Connection to %s established", agentConfig.Server)

		timeOutCtx, cancel := context.WithTimeout(context.Background(), networkTimeOut)
		dashboardBootTimeReceipt, err = client.ReportSystemInfo2(timeOutCtx, monitor.GetHost().PB())
		if err != nil {
			printf("上报系统信息失败: %v", err)
			cancel()
			retry()
			continue
		}
		cancel()

		geoipReported = geoipReported && prevDashboardBootTime > 0 && dashboardBootTimeReceipt.GetData() == prevDashboardBootTime
		prevDashboardBootTime = dashboardBootTimeReceipt.GetData()
		initialized = true

		wCtx, wCancel := context.WithCancel(context.Background())

		// 执行 Task
		tasks, err := doWithTimeout(func() (pb.NezhaService_RequestTaskClient, error) {
			return client.RequestTask(wCtx)
		}, networkTimeOut)
		if err != nil {
			printf("请求任务失败: %v", err)
			wCancel()
			retry()
			continue
		}
		go receiveTasksDaemon(tasks, wCancel)

		reportState, err := doWithTimeout(func() (pb.NezhaService_ReportSystemStateClient, error) {
			return client.ReportSystemState(wCtx)
		}, networkTimeOut)
		if err != nil {
			printf("上报状态信息失败: %v", err)
			wCancel()
			retry()
			continue
		}
		go reportStateDaemon(reportState, wCancel)

		<-wCtx.Done()
		println("Worker exit...")

		retry()
	}
}

// systemdScriptWithCapNetRaw 复刻 kardianos/service 默认 systemd 模板，仅在
// [Service] 段额外加入 AmbientCapabilities=CAP_NET_RAW，使以 nezha 低权限用户
// 运行的 agent 仍能执行 ICMP 拨测（raw socket 需要该 capability）。capability
// 写入 unit 文件，自更新覆写二进制不会丢失。
const systemdScriptWithCapNetRaw = `[Unit]
Description={{.Description}}
ConditionFileIsExecutable={{.Path|cmdEscape}}
{{range $i, $dep := .Dependencies}}
{{$dep}} {{end}}

[Service]
StartLimitInterval=5
StartLimitBurst=10
ExecStart={{.Path|cmdEscape}}{{range .Arguments}} {{.|cmd}}{{end}}
{{if .ChRoot}}RootDirectory={{.ChRoot|cmd}}{{end}}
{{if .WorkingDirectory}}WorkingDirectory={{.WorkingDirectory|cmdEscape}}{{end}}
{{if .UserName}}User={{.UserName}}{{end}}
AmbientCapabilities=CAP_NET_RAW
{{if .ReloadSignal}}ExecReload=/bin/kill -{{.ReloadSignal}} "$MAINPID"{{end}}
{{if .PIDFile}}PIDFile={{.PIDFile|cmd}}{{end}}
{{if and .LogOutput .HasOutputFileSupport -}}
StandardOutput=file:{{.LogDirectory}}/{{.Name}}.out
StandardError=file:{{.LogDirectory}}/{{.Name}}.err
{{- end}}
{{if gt .LimitNOFILE -1 }}LimitNOFILE={{.LimitNOFILE}}{{end}}
{{if .Restart}}Restart={{.Restart}}{{end}}
{{if .SuccessExitStatus}}SuccessExitStatus={{.SuccessExitStatus}}{{end}}
RestartSec=30
EnvironmentFile=-/etc/sysconfig/{{.Name}}

{{range $k, $v := .EnvVars -}}
Environment={{$k}}={{$v}}
{{end -}}

[Install]
WantedBy=multi-user.target
`

// openRCScriptWithCapNetRaw 复刻 nezhahq/service 默认 OpenRC 模板，额外加入
// command_user（以 nezha 低权限用户运行）与 capabilities="^cap_net_raw"（保留 ICMP
// 拨测所需的 raw socket 能力，^ 前缀使其进入 ambient set 供子进程继承）。capability
// 写入 service 脚本，自更新覆写二进制不会丢失。仅在使用 OpenRC 的发行版（如 Alpine）生效。
const openRCScriptWithCapNetRaw = `#!/sbin/openrc-run
supervisor=supervise-daemon
name="{{.DisplayName}}"
description="{{.Description}}"
command={{.Path|cmdEscape}}
{{- if .Arguments }}
command_args="{{range .Arguments}}{{.}} {{end}}"
{{- end }}
name=$(basename $(readlink -f $command))
{{if .WorkingDirectory}}directory="{{.WorkingDirectory}}"{{end}}
{{if .UserName}}command_user="{{.UserName}}"{{end}}
capabilities="^cap_net_raw"
supervise_daemon_args="--stdout {{.LogDirectory}}/${name}.log --stderr {{.LogDirectory}}/${name}.err"

{{range $k, $v := .EnvVars -}}
export {{$k}}={{$v}}
{{end -}}

{{- if .Dependencies }}
depend() {
{{- range $i, $dep := .Dependencies}}
{{"\t"}}{{$dep}}{{end}}
}
{{- end}}
`

func runService(action string, path string) {
	svcOption := map[string]interface{}{
		"OnFailure": "restart", // Windows 服务失败重启
	}

	args := []string{"-c", path}
	name := filepath.Base(executablePath)
	if path != defaultConfigPath && path != "" {
		hex := util.MD5Sum(path)[:7]
		name = fmt.Sprintf("%s-%s", name, hex)
	}

	svcConfig := &service.Config{
		Name:             name,
		DisplayName:      filepath.Base(executablePath),
		Arguments:        args,
		Description:      "哪吒监控 Agent",
		WorkingDirectory: filepath.Dir(executablePath),
		Option:           svcOption,
	}

	// 仅 Linux：以 nezha 低权限用户运行服务。kardianos/service 会据此在
	// systemd unit 写入 User=nezha。非 Linux 平台保持原行为（不设 UserName）。
	if runtime.GOOS == "linux" {
		svcConfig.UserName = "nezha"
		// systemd 与 OpenRC 模板均注入 CAP_NET_RAW，使低权限 nezha 用户仍可执行 ICMP
		// 拨测。service 库按机器实际 init 系统自动择一，无需在此探测。
		svcOption["SystemdScript"] = systemdScriptWithCapNetRaw
		svcOption["OpenRCScript"] = openRCScriptWithCapNetRaw
	}

	prg := &commands.Program{
		Exit: make(chan struct{}),
		Run:  run,
	}
	s, err := service.New(prg, svcConfig)
	if err != nil {
		printf("创建服务时出错，以普通模式运行: %v", err)
		run()
		return
	}
	prg.Service = s

	serviceLogger, err := logger.NewNezhaServiceLogger(s, nil)
	if err != nil {
		printf("获取 service logger 时出错: %+v", err)
		logger.InitDefaultLogger(agentConfig.Debug, service.ConsoleLogger)
	} else {
		logger.InitDefaultLogger(agentConfig.Debug, serviceLogger)
	}

	if action == "install" {
		initName := s.Platform()
		if err := agentConfig.Read(path); err != nil {
			log.Fatalf("init config failed: %v", err)
		}
		printf("Init system is: %s", initName)
		// Linux 下在写入 systemd unit 前创建 nezha 用户，并把程序目录归属给
		// nezha，使服务能以低权限运行的同时仍可自更新（覆盖写二进制）。
		if runtime.GOOS == "linux" {
			if err := ensureNezhaUser(); err != nil {
				log.Fatalf("创建 nezha 用户失败: %v", err)
			}
			if err := chownAgentDir(); err != nil {
				printf("chown 程序目录给 nezha 失败（自更新可能受影响）: %v", err)
			}
		}
	}

	if len(action) != 0 {
		err := service.Control(s, action)
		if err != nil {
			log.Fatal(err)
		}
		return
	}

	err = s.Run()
	if err != nil {
		logger.Error(err)
	}
}

// ensureNezhaUser 仅 Linux。创建系统用户 nezha（无登录、无家目录），幂等。
// install 需以 root 运行。优先用 GNU coreutils（useradd/groupadd，Debian/RHEL），
// 不可用时回退到 busybox（addgroup/adduser，Alpine）。
func ensureNezhaUser() error {
	if _, err := user.Lookup("nezha"); err == nil {
		return nil
	}
	shell := nologinShell()
	if err := addNezhaUserGNU(shell); err == nil {
		return nil
	}
	if err := addNezhaUserBusybox(shell); err != nil {
		return fmt.Errorf("创建 nezha 用户失败（useradd 与 adduser 均不可用）: %v", err)
	}
	return nil
}

// addNezhaUserGNU 用 GNU coreutils 创建 nezha 用户/组（Debian/RHEL 等）。
func addNezhaUserGNU(shell string) error {
	if _, err := exec.LookPath("useradd"); err != nil {
		return err
	}
	_ = exec.Command("groupadd", "--system", "nezha").Run() // 已存在则忽略
	out, err := exec.Command("useradd", "--system", "-g", "nezha",
		"-M", "-s", shell, "nezha").CombinedOutput()
	if err != nil {
		return fmt.Errorf("useradd nezha: %v: %s", err, out)
	}
	return nil
}

// addNezhaUserBusybox 用 busybox 创建 nezha 系统用户/组（Alpine）。
func addNezhaUserBusybox(shell string) error {
	_ = exec.Command("addgroup", "-S", "nezha").Run() // 已存在则忽略
	out, err := exec.Command("adduser", "-S", "-D", "-H",
		"-G", "nezha", "-s", shell, "nezha").CombinedOutput()
	if err != nil {
		return fmt.Errorf("adduser nezha: %v: %s", err, out)
	}
	return nil
}

// nologinShell 返回当前系统可用的 nologin shell 路径（含 Alpine 的 /bin/false 兜底）。
func nologinShell() string {
	for _, shell := range []string{"/usr/sbin/nologin", "/sbin/nologin", "/bin/false"} {
		if _, err := os.Stat(shell); err == nil {
			return shell
		}
	}
	return "/sbin/nologin"
}

// chownAgentDir 把程序目录（含二进制与配置）归属 nezha，保留自更新写权限。
func chownAgentDir() error {
	dir := filepath.Dir(executablePath)
	return exec.Command("chown", "-R", "nezha:nezha", dir).Run()
}

// newSerialTaskResultSender wraps the RequestTask stream's Send with a mutex.
// dispatchAgentTask runs most tasks in their own goroutine, and they all share
// one stream; gRPC Go forbids concurrent SendMsg, so every result must funnel
// through this serializer or overlapping MCP results corrupt the stream.
func newSerialTaskResultSender(send func(*pb.TaskResult) error) func(*pb.TaskResult) error {
	var mu sync.Mutex
	return func(r *pb.TaskResult) error {
		mu.Lock()
		defer mu.Unlock()
		return send(r)
	}
}

func receiveTasksDaemon(tasks pb.NezhaService_RequestTaskClient, cancel context.CancelFunc) {
	var task *pb.Task
	var err error
	send := newSerialTaskResultSender(tasks.Send)
	for {
		task, err = doWithTimeout(func() (*pb.Task, error) {
			return tasks.Recv()
		}, time.Second*30)
		if err != nil {
			printf("receiveTasks exit: %v", err)
			cancel()
			return
		}
		dispatchAgentTask(task, send, cancel)
	}
}

// dispatchAgentTask 一律以 goroutine 派发 task：拨测（HTTPGet/Ping）可能跑很久
// 或永远不返回，不能阻塞接收循环。配置管理（ApplyConfig）原先需要同步处理的
// 特殊路径已随热重载机制一并移除。
func dispatchAgentTask(task *pb.Task, send func(*pb.TaskResult) error, cancel context.CancelFunc) {
	go runAgentTask(task, send, cancel)
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
func reportStateDaemon(stateClient pb.NezhaService_ReportSystemStateClient, cancel context.CancelFunc) {
	var err error
	for {
		lastReportHostInfo, lastReportIPInfo, err = reportState(stateClient, lastReportHostInfo, lastReportIPInfo)
		if err != nil {
			printf("reportStateDaemon exit: %v", err)
			cancel()
			return
		}
		time.Sleep(time.Second * time.Duration(agentConfig.ReportDelay))
	}
}

func reportState(statClient pb.NezhaService_ReportSystemStateClient, host, ip time.Time) (time.Time, time.Time, error) {
	if statClient.Context().Err() != nil {
		return host, ip, statClient.Context().Err()
	}
	if initialized {
		monitor.TrackNetworkSpeed()
		if _, err := doWithTimeout(func() (*pb.Receipt, error) {
			return nil, statClient.Send(monitor.GetState(agentConfig.SkipConnectionCount, agentConfig.SkipProcsCount).PB())
		}, time.Second*10); err != nil {
			return host, ip, err
		}
		_, err := doWithTimeout(statClient.Recv, time.Second*10)
		if err != nil {
			return host, ip, err
		}
	}
	// 每10分钟重新获取一次硬件信息
	if host.Before(time.Now().Add(-10 * time.Minute)) {
		if reportHost() {
			host = time.Now()
		}
	}
	// 更新IP信息
	if time.Since(ip) > time.Second*time.Duration(agentConfig.IPReportPeriod) || !geoipReported {
		if reportGeoIP(agentConfig.UseIPv6CountryCode, !geoipReported) {
			ip = time.Now()
			geoipReported = true
		}
	}
	return host, ip, nil
}

func reportHost() bool {
	if !hostStatus.CompareAndSwap(false, true) {
		return false
	}
	defer hostStatus.Store(false)
	if client != nil && initialized {
		receipt, err := doWithTimeout(func() (*pb.Uint64Receipt, error) {
			return client.ReportSystemInfo2(context.Background(), monitor.GetHost().PB())
		}, time.Second*10)
		if err != nil {
			printf("ReportSystemInfo2 error: %v", err)
			return false
		}
		geoipReported = geoipReported && prevDashboardBootTime > 0 && receipt.GetData() == prevDashboardBootTime
	}
	return true
}

func reportGeoIP(use6, forceUpdate bool) bool {
	if !ipStatus.CompareAndSwap(false, true) {
		return false
	}
	defer ipStatus.Store(false)

	if client == nil || !initialized {
		return false
	}

	pbg := monitor.FetchIP(use6)
	if pbg == nil {
		return false
	}

	if !monitor.GeoQueryIPChanged && !forceUpdate {
		return true
	}

	geoip, err := doWithTimeout(func() (*pb.GeoIP, error) {
		return client.ReportGeoIP(context.Background(), pbg)
	}, time.Second*10)
	if err != nil {
		return false
	}

	prevDashboardBootTime = geoip.GetDashboardBootTime()

	monitor.CachedCountryCode = geoip.GetCountryCode()
	monitor.GeoQueryIPChanged = false

	return true
}

// doSelfUpdate 执行更新检查 如果更新成功则会结束进程
func doSelfUpdate(useLocalVersion bool) (exit bool) {
	v := semver.MustParse("0.1.0")
	if useLocalVersion {
		vr, err := semver.Parse(version)
		if err != nil {
			printf("failed to parse current version string: %v", err)
			return
		}
		cmd := exec.Command(executablePath, "--version")
		vb, err := cmd.Output()
		if err != nil {
			printf("failed to retrieve current executable version: %v", err)
			return
		}
		vraw := strings.Split(strings.TrimSpace(string(vb)), " ")
		vstr := vraw[len(vraw)-1]
		v, err = semver.Parse(vstr)
		if err != nil {
			printf("failed to parse executable version string: %v", err)
			return
		}
		if !vr.Equals(v) {
			printf("executable version differs from current version, exiting to re-check update...")
			exit = true
			return
		}
	}

	execHash := util.MD5Sum(executablePath)[:7]
	statName := fmt.Sprintf("agent-%s.stat", execHash)
	tmpDir := filepath.Join(os.TempDir(), binaryName)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		printf("failed to create temp dir: %v", err)
		return
	}

	statFile := filepath.Join(tmpDir, statName)
	if _, err := os.Stat(statFile); err == nil {
		printf("found self-update stat file, waiting for another process to finish update...")
		if fErr := fsnotifyx.ExitOnDeleteFile(context.Background(), printf, statFile); fErr != nil {
			if errors.Is(fErr, fsnotifyx.ErrTimeout) {
				os.Remove(statFile) // try to remove stat file
			}
			printf("failed to monitor path of stat file: %v", fErr)
			return
		}
		exit = true
		return
	} else {
		if !errors.Is(err, os.ErrNotExist) {
			printf("failed to retrieve self-update stat at %s", statFile)
			return
		}
	}

	stat, err := os.OpenFile(statFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		printf("failed to create self-update stat file: %v", err)
		return
	}

	defer func() {
		stat.Close()
		if err := os.Remove(statFile); err != nil {
			printf("remove stat failed: %v", err)
		}
	}()

	printf("检查更新: %v", v)
	latest, err := updateFromSource("wangdefaa/nezha-agent", v)
	if err != nil {
		printf("更新失败: %v", err)
		return
	}

	if !latest.Version.Equals(v) {
		printf("已经更新至: %v, 正在结束进程", latest.Version)
		exit = true
	}
	return
}

func handleUpgradeTask(*pb.Task, *pb.TaskResult) {
	if agentConfig.DisableForceUpdate {
		return
	}
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
	pinger, err := ping.NewPinger(ipAddr)
	if err == nil {
		pinger.SetPrivileged(true)
		pinger.Count = 5
		pinger.Timeout = time.Second * 20
		err = pinger.Run() // Blocks until finished.
	}
	if err == nil {
		stat := pinger.Statistics()
		if stat.PacketsRecv == 0 {
			result.Data = "pockets recv 0"
			return
		}
		result.Delay = float32(stat.AvgRtt.Microseconds()) / 1000.0
		result.Successful = true
	} else {
		result.Data = err.Error()
	}
}

func handleHttpGetTask(task *pb.Task, result *pb.TaskResult) {
	if agentConfig.DisableSendQuery {
		result.Data = "This server has disabled query sending"
		return
	}
	start := time.Now()
	taskUrl := task.GetData()
	resp, err := httpClient.Get(taskUrl)
	printf("HTTP-GET Task: %s", taskUrl)
	if err == nil {
		defer resp.Body.Close()
		_, err = io.Copy(io.Discard, resp.Body)
	}
	if err == nil {
		// 检查 HTTP Response 状态
		result.Delay = float32(time.Since(start).Microseconds()) / 1000.0
		if resp.StatusCode > 399 || resp.StatusCode < 200 {
			err = errors.New("\n应用错误: " + resp.Status)
		}
	}
	if err == nil {
		// 检查 SSL 证书信息
		if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
			c := resp.TLS.PeerCertificates[0]
			result.Data = c.Issuer.CommonName + "|" + c.NotAfter.String()
		}
		result.Successful = true
	} else {
		// HTTP 请求失败
		result.Data = err.Error()
	}
}

func lookupIP(hostOrIp string) (string, error) {
	if net.ParseIP(hostOrIp) == nil {
		ips, err := dnsResolver.LookupIPAddr(context.Background(), hostOrIp)
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

func doWithTimeout[T any](fn func() (T, error), timeout time.Duration) (T, error) {
	timeoutCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var t T
	var err error
	go func() {
		defer cancel()
		t, err = fn()
	}()
	<-timeoutCtx.Done()
	if timeoutCtx.Err() != context.Canceled {
		return t, fmt.Errorf("context error: %v, fn err: %v", timeoutCtx.Err(), err)
	}
	return t, err
}
