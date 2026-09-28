package monitor

import (
	"sync"
	"testing"

	"github.com/nezhahq/agent/model"
)

// 重连时 run() 主循环调用 GetHost，而上一代 reportStateDaemon 可能仍在调用
// GetHost/GetState/TrackNetworkSpeed。这些入口共享 hostDataFetchAttempts、
// 网速计数与 cachedBootTime，必须在 -race 下并发调用无数据竞争。
func TestMonitorConcurrentCollectionIsRaceFree(t *testing.T) {
	InitConfig(&model.AgentConfig{})
	const workers, rounds = 4, 3
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				GetHost()
				TrackNetworkSpeed()
				GetState(true, true)
			}
		}()
	}
	wg.Wait()
}

// load 采集连续失败达到上限后 tryStat 返回 nil 指针（Windows 下 PDH 取值出错即会
// 走到这里），GetState 不得因此 panic——上报协程里的 panic 会直接打挂整个进程。
func TestGetStateToleratesUnavailableLoadStat(t *testing.T) {
	InitConfig(&model.AgentConfig{})
	stateLock.Lock()
	saved := statDataFetchAttempts[Load]
	statDataFetchAttempts[Load] = maxDeviceDataFetchAttempts
	stateLock.Unlock()
	t.Cleanup(func() {
		stateLock.Lock()
		statDataFetchAttempts[Load] = saved
		stateLock.Unlock()
	})

	state := GetState(true, true)

	if state.Load1 != 0 || state.Load5 != 0 || state.Load15 != 0 {
		t.Fatalf("load 不可用时应上报 0，got %v/%v/%v", state.Load1, state.Load5, state.Load15)
	}
}
