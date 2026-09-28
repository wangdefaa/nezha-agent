package monitor

import (
	"context"
	"math"
	"runtime"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"

	"github.com/nezhahq/agent/model"
	"github.com/nezhahq/agent/pkg/logger"
	"github.com/nezhahq/agent/pkg/monitor/conn"
	"github.com/nezhahq/agent/pkg/monitor/cpu"
	"github.com/nezhahq/agent/pkg/monitor/disk"
	"github.com/nezhahq/agent/pkg/monitor/load"
	"github.com/nezhahq/agent/pkg/monitor/nic"
	"github.com/nezhahq/agent/pkg/util"
)

var (
	Version     string
	agentConfig *model.AgentConfig

	printf = logger.Printf
)

var (
	netInSpeed, netOutSpeed, netInTransfer, netOutTransfer, lastUpdateNetStats uint64
	cachedBootTime                                                             time.Time
)

// 获取设备数据的最大尝试次数
const maxDeviceDataFetchAttempts = 3

const (
	CPU = iota + 1
	Load
)

// 获取主机数据的尝试次数，Key 为 Host 的属性名
var hostDataFetchAttempts = map[uint8]uint8{
	CPU: 0,
}

// 获取状态数据的尝试次数，Key 为 HostState 的属性名
var statDataFetchAttempts = map[uint8]uint8{
	CPU:  0,
	Load: 0,
}

// GetHost 可能被重连中的 run() 与上一代上报协程同时调用，采集状态统一加锁：
// hostLock 保护 hostDataFetchAttempts（map 并发写会直接 fatal，recover 兜不住），
// stateLock 保护 statDataFetchAttempts，metricLock 保护网速计数与开机时间。
var (
	hostLock   sync.Mutex
	stateLock  sync.Mutex
	metricLock sync.RWMutex
)

func InitConfig(cfg *model.AgentConfig) {
	agentConfig = cfg
}

// GetHost 获取主机硬件信息
func GetHost() *model.Host {
	var ret model.Host
	cpuType := fillHostInfo(&ret)
	ctxCpu := context.WithValue(context.Background(), cpu.CPUHostKey, cpuType)
	ret.CPU = tryHost(ctxCpu, CPU, cpu.GetHost)
	ret.DiskTotal = getDiskTotal()
	ret.MemTotal, ret.SwapTotal = getMemTotal()
	ret.Version = Version
	return &ret
}

// fillHostInfo 填充平台、架构、虚拟化与开机时间，返回 CPU 类型（Virtual/Physical）。
func fillHostInfo(ret *model.Host) string {
	hi, err := host.Info()
	if err != nil {
		printf("host.Info error: %v", err)
		return ""
	}
	cpuType := "Physical"
	if hi.VirtualizationRole == "guest" {
		cpuType = "Virtual"
		ret.Virtualization = hi.VirtualizationSystem
	}
	ret.Platform = hi.Platform
	ret.PlatformVersion = hi.PlatformVersion
	ret.Arch = hi.KernelArch
	ret.BootTime = hi.BootTime
	metricLock.Lock()
	cachedBootTime = time.Unix(int64(hi.BootTime), 0)
	metricLock.Unlock()
	return cpuType
}

// getMemTotal 返回内存与 swap 总量。
func getMemTotal() (memTotal, swapTotal uint64) {
	mv, err := mem.VirtualMemory()
	if err != nil {
		printf("mem.VirtualMemory error: %v", err)
	} else {
		memTotal = mv.Total
		if !swapFromSwapMemory() {
			swapTotal = mv.SwapTotal
		}
	}
	if swapFromSwapMemory() {
		ms, err := mem.SwapMemory()
		if err != nil {
			printf("mem.SwapMemory error: %v", err)
		} else {
			swapTotal = ms.Total
		}
	}
	return memTotal, swapTotal
}

func GetState(skipConnectionCount bool, skipProcsCount bool) *model.HostState {
	var ret model.HostState
	if cp := tryStat(context.Background(), CPU, cpu.GetState); len(cp) > 0 {
		ret.CPU = cp[0]
	}
	ret.MemUsed, ret.SwapUsed = getMemUsed()
	ret.DiskUsed = getDiskUsed()
	// tryStat 失败或已达重试上限时返回零值（nil 指针），必须判空再解引用
	if loadStat := tryStat(context.Background(), Load, load.GetState); loadStat != nil {
		ret.Load1, ret.Load5, ret.Load15 = loadStat.Load1, loadStat.Load5, loadStat.Load15
	}
	if !skipProcsCount {
		ret.ProcessCount = getProcessCount()
	}
	fillNetState(&ret)
	if !skipConnectionCount {
		ret.TcpConnCount, ret.UdpConnCount = getConns()
	}
	return &ret
}

// getMemUsed 返回已用内存与 swap。
func getMemUsed() (memUsed, swapUsed uint64) {
	vm, err := mem.VirtualMemory()
	if err != nil {
		printf("mem.VirtualMemory error: %v", err)
	} else {
		if vm.Used > math.MaxInt64 && runtime.GOOS == "linux" {
			// alternative calculation method for lxc containers where `MemAvailable` can be larger than `MemTotal`
			memUsed = vm.Total - vm.Free
		} else {
			memUsed = vm.Used
		}
		if !swapFromSwapMemory() {
			swapUsed = vm.SwapTotal - vm.SwapFree
		}
	}
	if swapFromSwapMemory() {
		ms, err := mem.SwapMemory()
		if err != nil {
			printf("mem.SwapMemory error: %v", err)
		} else {
			swapUsed = ms.Used
		}
	}
	return memUsed, swapUsed
}

// swapFromSwapMemory 报告 swap 是否需经 SwapMemory 获取（gopsutil 在 Windows/Darwin 下如此）。
func swapFromSwapMemory() bool {
	return runtime.GOOS == "windows" || runtime.GOOS == "darwin"
}

func getProcessCount() uint64 {
	procs, err := process.Pids()
	if err != nil {
		printf("process.Pids error: %v", err)
		return 0
	}
	return uint64(len(procs))
}

// fillNetState 在 metricLock 下读取流量、网速与开机时长，避免与 TrackNetworkSpeed 并发读写。
func fillNetState(ret *model.HostState) {
	metricLock.RLock()
	defer metricLock.RUnlock()
	ret.NetInTransfer, ret.NetOutTransfer = netInTransfer, netOutTransfer
	ret.NetInSpeed, ret.NetOutSpeed = netInSpeed, netOutSpeed
	ret.Uptime = uint64(time.Since(cachedBootTime).Seconds())
}

// TrackNetworkSpeed NIC监控，统计流量与速度
func TrackNetworkSpeed() {
	ctx := context.WithValue(context.Background(), nic.NICKey, agentConfig.NICAllowlist)
	nc, err := nic.GetState(ctx)
	if err != nil {
		return
	}

	now := uint64(time.Now().Unix())
	metricLock.Lock()
	defer metricLock.Unlock()
	diff := util.SubUintChecked(now, lastUpdateNetStats)
	if diff > 0 {
		netInSpeed = util.SubUintChecked(nc[0], netInTransfer) / diff
		netOutSpeed = util.SubUintChecked(nc[1], netOutTransfer) / diff
	}
	netInTransfer, netOutTransfer = nc[0], nc[1]
	lastUpdateNetStats = now
}

func getDiskTotal() uint64 {
	ctx := context.WithValue(context.Background(), disk.DiskKey, agentConfig.HardDrivePartitionAllowlist)
	total, _ := disk.GetHost(ctx)

	return total
}

func getDiskUsed() uint64 {
	ctx := context.WithValue(context.Background(), disk.DiskKey, agentConfig.HardDrivePartitionAllowlist)
	used, _ := disk.GetState(ctx)

	return used
}

func getConns() (tcpConnCount, udpConnCount uint64) {
	connStat, err := conn.GetState(context.Background())
	if err != nil {
		return
	}

	if len(connStat) < 2 {
		return
	}

	return connStat[0], connStat[1]
}

type hostStateFunc[T any] func(context.Context) (T, error)

func tryHost[T any](ctx context.Context, typ uint8, f hostStateFunc[T]) T {
	var val T

	hostLock.Lock()
	defer hostLock.Unlock()

	if hostDataFetchAttempts[typ] < maxDeviceDataFetchAttempts {
		v, err := f(ctx)
		if err != nil {
			hostDataFetchAttempts[typ]++
			printf("monitor error: %v, type: %d, attempt: %d", err, typ, hostDataFetchAttempts[typ])
			return val
		} else {
			val = v
			hostDataFetchAttempts[typ] = 0
		}
	}
	return val
}

func tryStat[T any](ctx context.Context, typ uint8, f hostStateFunc[T]) T {
	var val T

	stateLock.Lock()
	defer stateLock.Unlock()

	if statDataFetchAttempts[typ] < maxDeviceDataFetchAttempts {
		v, err := f(ctx)
		if err != nil {
			statDataFetchAttempts[typ]++
			printf("monitor error: %v, type: %d, attempt: %d", err, typ, statDataFetchAttempts[typ])
			return val
		} else {
			val = v
			statDataFetchAttempts[typ] = 0
		}
	}
	return val
}
