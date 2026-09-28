package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/nezhahq/agent/model"
	pb "github.com/nezhahq/agent/proto"
)

func TestCallWithDeadline_ReturnsResultWithinDeadline(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	v, err := callWithDeadline(cancel, time.Second, func() (int, error) { return 42, nil })

	if v != 42 || err != nil || ctx.Err() != nil {
		t.Fatalf("got v=%d err=%v ctxErr=%v, want 42/nil/nil", v, err, ctx.Err())
	}
}

// 阻塞中的流操作只有在会话被取消后才会返回；超时必须取消会话并带上超时原因。
func TestCallWithDeadline_CancelsSessionWhenCallBlocks(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())

	_, err := callWithDeadline(cancel, 20*time.Millisecond, func() (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	})

	if !errors.Is(err, errCallTimeout) || !errors.Is(context.Cause(ctx), errCallTimeout) {
		t.Fatalf("err=%v cause=%v, want both wrapping errCallTimeout", err, context.Cause(ctx))
	}
}

// 正常会话：握手上报、周期状态、硬件信息与 GeoIP 补报、拨测结果回传；面板侧结束
// 任务流后会话结束，关闭连接后两个守护协程都能退出。
func TestServeConnection_ReportsStateAndHandlesTasks(t *testing.T) {
	useSessionTestGlobals(t, time.Second, time.Second)
	dashboard := newFakeDashboard()
	dashboard.keepalive = 100 * time.Millisecond
	dashboard.tasks = []*pb.Task{{Id: 7, Type: model.TaskTypeTCPPing, Data: listenLocalTCP(t)}}
	conn := startFakeDashboard(t, dashboard)
	session, done := startSession(conn)

	result := waitTaskResult(t, dashboard, 5*time.Second)
	waitUntil(t, 5*time.Second, func() bool {
		return dashboard.states.Load() >= 2 && dashboard.hostReports.Load() >= 2 && dashboard.geoIPReports.Load() >= 1
	})
	close(dashboard.endTasks)
	err := waitSessionEnd(t, conn, session, done)

	if result.GetId() != 7 || !result.GetSuccessful() {
		t.Fatalf("TCP 拨测结果 = %+v，want id=7 successful", result)
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("会话结束原因 = %v，want io.EOF（面板结束任务流）", err)
	}
}

func TestServeConnection_CancelsWhenStateReceiptStalls(t *testing.T) {
	useSessionTestGlobals(t, time.Second, 200*time.Millisecond)
	dashboard := newFakeDashboard()
	dashboard.keepalive = 50 * time.Millisecond
	dashboard.silentReceipt = true
	conn := startFakeDashboard(t, dashboard)
	session, done := startSession(conn)

	err := waitSessionEnd(t, conn, session, done)

	if !errors.Is(err, errCallTimeout) {
		t.Fatalf("会话结束原因 = %v，want errCallTimeout（状态回执超时）", err)
	}
}

func TestServeConnection_CancelsWhenNoTaskArrives(t *testing.T) {
	useSessionTestGlobals(t, 200*time.Millisecond, time.Second)
	conn := startFakeDashboard(t, newFakeDashboard())
	session, done := startSession(conn)

	err := waitSessionEnd(t, conn, session, done)

	if !errors.Is(err, errCallTimeout) {
		t.Fatalf("会话结束原因 = %v，want errCallTimeout（任务流失活）", err)
	}
}

func TestHandleHttpGetTask_RejectsNonHTTPScheme(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "gopher://127.0.0.1:70/", "://bad"} {
		var result pb.TaskResult
		handleHttpGetTask(&pb.Task{Data: raw}, &result)
		if result.GetSuccessful() || !strings.Contains(result.GetData(), "only http and https") {
			t.Fatalf("%q: result = %+v，want 拒绝非 http(s) scheme", raw, &result)
		}
	}
}

func startSession(conn *grpc.ClientConn) (*agentSession, <-chan error) {
	session := newAgentSession(pb.NewNezhaServiceClient(conn))
	done := make(chan error, 1)
	go func() { done <- serveConnection(session) }()
	return session, done
}

// waitSessionEnd 等待会话结束，再像 waitReconnect 一样关闭连接并断言守护协程退出。
func waitSessionEnd(t *testing.T, conn *grpc.ClientConn, session *agentSession, done <-chan error) error {
	t.Helper()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		session.cancel(errors.New("test timeout"))
		t.Fatal("serveConnection 未在 5s 内结束")
	}
	conn.Close()
	if !session.waitDaemons(3 * time.Second) {
		t.Fatal("关闭连接后守护协程未在 3s 内退出")
	}
	return err
}

func waitTaskResult(t *testing.T, d *fakeDashboard, within time.Duration) *pb.TaskResult {
	t.Helper()
	select {
	case result := <-d.results:
		return result
	case <-time.After(within):
		t.Fatalf("%v 内未收到任务结果", within)
		return nil
	}
}

func waitUntil(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%v 内条件未满足", within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
