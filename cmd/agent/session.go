package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nezhahq/agent/pkg/monitor"
	pb "github.com/nezhahq/agent/proto"
)

var (
	// errCallTimeout 标记 callWithDeadline 超时；它同时作为会话取消原因。
	errCallTimeout    = errors.New("call timed out")
	errSendTaskResult = errors.New("send task result failed")
)

// agentSession 对应一次 gRPC 连接：两个守护协程共用 ctx，任一方出错即以
// cancel(cause) 结束整个会话。client 随会话传递而非放在全局变量里，上一代
// 协程只会用到已关闭的旧连接，不会误用或并发读写新连接。
type agentSession struct {
	ctx     context.Context
	cancel  context.CancelCauseFunc
	client  pb.NezhaServiceClient
	daemons sync.WaitGroup
}

func newAgentSession(client pb.NezhaServiceClient) *agentSession {
	ctx, cancel := context.WithCancelCause(context.Background())
	return &agentSession{ctx: ctx, cancel: cancel, client: client}
}

// serveConnection 上报硬件信息、打开任务流与状态流并启动两个守护协程，阻塞到
// 会话结束，返回结束原因。守护协程的阻塞调用均绑定会话 ctx 或受 callWithDeadline
// 限时，会话取消、连接关闭后会自行退出，由 waitReconnect 在重连前等待。
func serveConnection(s *agentSession) error {
	defer s.cancel(context.Canceled)
	if err := reportHostOnConnect(s); err != nil {
		printf("上报系统信息失败: %v", err)
		return err
	}
	tasks, err := callWithDeadline(s.cancel, networkTimeOut, func() (pb.NezhaService_RequestTaskClient, error) {
		return s.client.RequestTask(s.ctx)
	})
	if err != nil {
		printf("请求任务失败: %v", err)
		return err
	}
	s.daemons.Go(func() { receiveTasksDaemon(s, tasks) })
	stream, err := callWithDeadline(s.cancel, networkTimeOut, func() (pb.NezhaService_ReportSystemStateClient, error) {
		return s.client.ReportSystemState(s.ctx)
	})
	if err != nil {
		printf("上报状态信息失败: %v", err)
		return err
	}
	s.daemons.Go(func() { reportStateDaemon(s, stream) })
	<-s.ctx.Done()
	printf("Worker exit: %v", context.Cause(s.ctx))
	return context.Cause(s.ctx)
}

// waitDaemons 最多等待 d，返回守护协程是否已全部退出。
func (s *agentSession) waitDaemons(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.daemons.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// reportHostOnConnect 连接建立后先上报硬件信息，并据面板启动时间判断面板是否
// 重启过（重启过则需重新上报 GeoIP）。
func reportHostOnConnect(s *agentSession) error {
	ctx, cancel := context.WithTimeout(s.ctx, networkTimeOut)
	defer cancel()
	receipt, err := s.client.ReportSystemInfo2(ctx, monitor.GetHost().PB())
	if err != nil {
		return err
	}
	bootTime := receipt.GetData()
	prevBootTime := prevDashboardBootTime.Swap(bootTime)
	geoipReported.Store(geoipReported.Load() && prevBootTime > 0 && bootTime == prevBootTime)
	initialized.Store(true)
	return nil
}

// callWithDeadline 同步执行 fn；若 d 内未返回，则以超时为由取消整个会话，使阻塞
// 中的 gRPC 流操作随之出错返回。取代原 doWithTimeout：后者另起 goroutine 执行 fn，
// 超时后读取仍会被该 goroutine 写入的返回值（数据竞争），并遗留继续运行的孤儿调用。
func callWithDeadline[T any](cancel context.CancelCauseFunc, d time.Duration, fn func() (T, error)) (T, error) {
	guard := time.AfterFunc(d, func() {
		cancel(fmt.Errorf("%w after %v", errCallTimeout, d))
	})
	v, err := fn()
	if !guard.Stop() && err != nil {
		err = fmt.Errorf("%w after %v: %v", errCallTimeout, d, err)
	}
	return v, err
}

// atomicTime 以纳秒时间戳原子保存时间；零值为 1970 年，语义同"从未上报"。
type atomicTime struct{ ns atomic.Int64 }

func (t *atomicTime) Load() time.Time { return time.Unix(0, t.ns.Load()) }

func (t *atomicTime) Store(v time.Time) { t.ns.Store(v.UnixNano()) }
