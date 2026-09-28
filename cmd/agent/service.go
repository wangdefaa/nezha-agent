package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"

	"github.com/nezhahq/service"

	"github.com/nezhahq/agent/cmd/agent/commands"
	"github.com/nezhahq/agent/pkg/logger"
	"github.com/nezhahq/agent/pkg/util"
)

// service.go：系统服务安装（systemd/OpenRC 模板、runService）与 Linux 低权限 nezha 用户、程序目录属主。

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

// runService 以 action（install/uninstall/start/stop/restart，留空为运行）操作系统服务。
func runService(action string, path string) {
	prg := &commands.Program{Run: run}
	s, err := service.New(prg, newServiceConfig(path))
	if err != nil {
		printf("创建服务时出错，以普通模式运行: %v", err)
		run()
		return
	}
	prg.Service = s
	initServiceLogger(s)
	if action == "install" {
		prepareInstall(s, path)
	}
	if len(action) != 0 {
		if err := service.Control(s, action); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := s.Run(); err != nil {
		logger.Error(err)
	}
}

// newServiceConfig 生成服务配置；自定义配置路径时服务名追加路径哈希，便于同机多实例。
func newServiceConfig(path string) *service.Config {
	svcOption := map[string]interface{}{
		"OnFailure": "restart", // Windows 服务失败重启
	}
	name := filepath.Base(executablePath)
	if path != defaultConfigPath && path != "" {
		name = fmt.Sprintf("%s-%s", name, util.MD5Sum(path)[:7])
	}
	cfg := &service.Config{
		Name:             name,
		DisplayName:      filepath.Base(executablePath),
		Arguments:        []string{"-c", path},
		Description:      "哪吒监控 Agent",
		WorkingDirectory: filepath.Dir(executablePath),
		Option:           svcOption,
	}
	// 仅 Linux：以 nezha 低权限用户运行（unit 写入 User=nezha），systemd 与 OpenRC
	// 模板注入 CAP_NET_RAW 以保留 ICMP 拨测；service 库按实际 init 系统择一。
	if runtime.GOOS == "linux" {
		cfg.UserName = "nezha"
		svcOption["SystemdScript"] = systemdScriptWithCapNetRaw
		svcOption["OpenRCScript"] = openRCScriptWithCapNetRaw
	}
	return cfg
}

// initServiceLogger 优先使用系统服务日志，获取失败时退回控制台。
func initServiceLogger(s service.Service) {
	serviceLogger, err := logger.NewNezhaServiceLogger(s, nil)
	if err != nil {
		printf("获取 service logger 时出错: %+v", err)
		logger.InitDefaultLogger(agentConfig.Debug, service.ConsoleLogger)
		return
	}
	logger.InitDefaultLogger(agentConfig.Debug, serviceLogger)
}

// prepareInstall install 前读取（必要时生成）配置；Linux 下在写入 unit 前创建 nezha 用户
// 并把程序目录交给它，使服务低权限运行的同时仍可自更新。此阶段 logger 尚未启用
// （debug 未读入），提示改用标准库 log 输出。
func prepareInstall(s service.Service, path string) {
	if err := agentConfig.Read(path); err != nil {
		log.Fatalf("init config failed: %v", err)
	}
	log.Printf("Init system is: %s", s.Platform())
	if runtime.GOOS != "linux" {
		return
	}
	if err := ensureNezhaUser(); err != nil {
		log.Fatalf("创建 nezha 用户失败: %v", err)
	}
	if err := chownAgentDir(path); err != nil {
		log.Printf("chown 给 nezha 失败（服务可能无法读取配置或自更新）: %v", err)
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
// 目录不是 agent 专用目录时（如 /usr/local/bin）拒绝递归 chown，否则 nezha 用户
// 将能替换目录内其它由 root 执行的程序；此时只把配置文件交给 nezha 保证可读。
func chownAgentDir(configPath string) error {
	uid, gid, err := lookupNezhaIDs()
	if err != nil {
		return err
	}
	dir := filepath.Dir(executablePath)
	if err := checkDedicatedDir(dir, filepath.Base(executablePath)); err != nil {
		log.Printf("跳过递归 chown（自更新将不可用）: %v", err)
	} else if err := chownDirFlat(dir, uid, gid); err != nil {
		return err
	}
	// 配置可能不在程序目录（-c 指定），单独交给 nezha 保证服务可读
	return os.Lchown(configPath, uid, gid)
}

// lookupNezhaIDs 返回 nezha 用户的 uid 与主组 gid。
func lookupNezhaIDs() (uid, gid int, err error) {
	u, err := user.Lookup("nezha")
	if err != nil {
		return 0, 0, err
	}
	if uid, err = strconv.Atoi(u.Uid); err != nil {
		return 0, 0, err
	}
	gid, err = strconv.Atoi(u.Gid)
	return uid, gid, err
}

// chownDirFlat 修改 dir 自身及其直接子项的属主（checkDedicatedDir 已保证没有子目录）。
// 不递归且只用 Lchown：子项即使被换成符号链接，也只改链接本身，不会把 root 的
// chown 引向系统文件（原 chown -R 在 busybox 下是否跟随链接因版本而异）。
func chownDirFlat(dir string, uid, gid int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Lchown(filepath.Join(dir, e.Name()), uid, gid); err != nil {
			return err
		}
	}
	return os.Lchown(dir, uid, gid)
}

// sharedSystemDirs 绝不能整体交给 nezha 用户的公共目录。
var sharedSystemDirs = []string{
	"/", "/bin", "/sbin", "/usr", "/usr/bin", "/usr/sbin", "/usr/local", "/usr/local/bin",
	"/usr/local/sbin", "/opt", "/etc", "/root", "/home", "/tmp", "/var", "/srv",
}

// checkDedicatedDir 校验 dir 为 agent 专用：不是公共系统目录，且只含 agent 自身文件。
func checkDedicatedDir(dir, exe string) error {
	if slices.Contains(sharedSystemDirs, filepath.Clean(dir)) {
		return fmt.Errorf("%s 是公共系统目录，请把 agent 放到独立目录（如 /opt/nezha/agent）", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !isAgentFile(e.Name(), exe) {
			return fmt.Errorf("%s 含非 agent 文件 %q，请把 agent 放到独立目录", dir, e.Name())
		}
	}
	return nil
}

// isAgentFile 判断文件是否属于 agent：二进制、go-update 的 .new/.old、自更新互斥文件与 yml 配置。
func isAgentFile(name, exe string) bool {
	switch name {
	case exe, "." + exe + ".new", "." + exe + ".old", "." + exe + ".stat":
		return true
	}
	ext := filepath.Ext(name)
	return ext == ".yml" || ext == ".yaml"
}
