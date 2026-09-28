package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/nezhahq/agent/model"
	"github.com/nezhahq/agent/pkg/monitor"
	pb "github.com/nezhahq/agent/proto"
)

const fakeBootTime = 1700000000

// fakeDashboard 是进程内的面板替身（bufconn 上的真实 gRPC 服务），按测试需要控制
// 各 RPC 行为，用于覆盖 serveConnection 的会话生命周期。
type fakeDashboard struct {
	pb.UnimplementedNezhaServiceServer
	tasks         []*pb.Task    // RequestTask 建立后依次下发
	keepalive     time.Duration // >0 时按此间隔持续下发 Keepalive，否则保持静默
	silentReceipt bool          // true 时收到状态后不回执，模拟上报流卡死
	endTasks      chan struct{} // 关闭后面板侧正常结束 RequestTask 流
	states        atomic.Int32
	hostReports   atomic.Int32
	geoIPReports  atomic.Int32
	results       chan *pb.TaskResult // 非 Keepalive 的任务结果
}

func newFakeDashboard() *fakeDashboard {
	return &fakeDashboard{endTasks: make(chan struct{}), results: make(chan *pb.TaskResult, 16)}
}

func (d *fakeDashboard) ReportSystemInfo2(context.Context, *pb.Host) (*pb.Uint64Receipt, error) {
	d.hostReports.Add(1)
	return &pb.Uint64Receipt{Data: fakeBootTime}, nil
}

func (d *fakeDashboard) ReportGeoIP(context.Context, *pb.GeoIP) (*pb.GeoIP, error) {
	d.geoIPReports.Add(1)
	return &pb.GeoIP{CountryCode: "aq", DashboardBootTime: fakeBootTime}, nil
}

func (d *fakeDashboard) RequestTask(stream grpc.BidiStreamingServer[pb.TaskResult, pb.Task]) error {
	go d.collectResults(stream)
	for _, task := range d.tasks {
		if err := stream.Send(task); err != nil {
			return err
		}
	}
	return d.keepAlive(stream)
}

func (d *fakeDashboard) collectResults(stream grpc.BidiStreamingServer[pb.TaskResult, pb.Task]) {
	for {
		result, err := stream.Recv()
		if err != nil {
			return
		}
		if result.GetType() == model.TaskTypeKeepalive {
			continue
		}
		select {
		case d.results <- result:
		default:
		}
	}
}

func (d *fakeDashboard) keepAlive(stream grpc.BidiStreamingServer[pb.TaskResult, pb.Task]) error {
	var tick <-chan time.Time
	if d.keepalive > 0 {
		ticker := time.NewTicker(d.keepalive)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		select {
		case <-d.endTasks:
			return nil
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-tick:
			if err := stream.Send(&pb.Task{Type: model.TaskTypeKeepalive}); err != nil {
				return err
			}
		}
	}
}

func (d *fakeDashboard) ReportSystemState(stream grpc.BidiStreamingServer[pb.State, pb.Receipt]) error {
	for {
		if _, err := stream.Recv(); err != nil {
			return err
		}
		d.states.Add(1)
		if d.silentReceipt {
			<-stream.Context().Done()
			return stream.Context().Err()
		}
		if err := stream.Send(&pb.Receipt{Proced: true}); err != nil {
			return err
		}
	}
}

// startFakeDashboard 在 bufconn 上启动面板替身并返回指向它的客户端连接。
func startFakeDashboard(t *testing.T, d *fakeDashboard) *grpc.ClientConn {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	pb.RegisterNezhaServiceServer(server, d)
	go func() { _ = server.Serve(listener) }()
	conn, err := grpc.NewClient("passthrough:///fake-dashboard",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial fake dashboard: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
		server.Stop()
	})
	return conn
}

// useSessionTestGlobals 为会话测试准备全局配置与跨重连状态，结束后还原。
// GeoIP 查询指向本地 HTTP 服务，测试不依赖外网。
func useSessionTestGlobals(t *testing.T, recvTimeout, stateTimeout time.Duration) {
	t.Helper()
	savedConfig, savedEndpoints := agentConfig, monitor.CustomEndpoints
	savedRecv, savedReport := taskRecvTimeout, reportTimeout
	agentConfig = model.AgentConfig{ReportDelay: 1, IPReportPeriod: 1800, SkipConnectionCount: true, SkipProcsCount: true}
	monitor.InitConfig(&agentConfig)
	monitor.CustomEndpoints = []string{startLocalIPAPI(t)}
	taskRecvTimeout, reportTimeout = recvTimeout, stateTimeout
	resetCrossSessionState()
	t.Cleanup(func() {
		agentConfig, monitor.CustomEndpoints = savedConfig, savedEndpoints
		taskRecvTimeout, reportTimeout = savedRecv, savedReport
		resetCrossSessionState()
	})
}

func resetCrossSessionState() {
	initialized.Store(false)
	geoipReported.Store(false)
	prevDashboardBootTime.Store(0)
	lastReportHostInfo.ns.Store(0)
	lastReportIPInfo.ns.Store(0)
}

func startLocalIPAPI(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "192.0.2.10")
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// listenLocalTCP 启动一个只接受连接的本地 TCP 服务，作为 TCP 拨测目标。
func listenLocalTCP(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	return listener.Addr().String()
}
