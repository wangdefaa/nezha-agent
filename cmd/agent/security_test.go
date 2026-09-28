package main

import (
	"archive/zip"
	"bytes"
	"context"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nezhahq/go-github-selfupdate/selfupdate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nezhahq/agent/model"
	pb "github.com/nezhahq/agent/proto"
)

// fillTaskSlots 以非阻塞方式占满剩余槽位（其它用例可能仍有任务在跑），返回占用数。
func fillTaskSlots() int {
	for n := 0; ; n++ {
		select {
		case taskSlots <- struct{}{}:
		default:
			return n
		}
	}
}

// 并发槽位占满时，新任务必须在接收循环内同步得到失败回复，而不是再起 goroutine。
func TestDispatchAgentTaskRejectsWhenFull(t *testing.T) {
	filled := fillTaskSlots()
	defer func() {
		for i := 0; i < filled; i++ {
			<-taskSlots
		}
	}()
	var got *pb.TaskResult
	send := func(r *pb.TaskResult) error { got = r; return nil }
	dispatchAgentTask(&pb.Task{Id: 7, Type: model.TaskTypeTCPPing, Data: "127.0.0.1:1"}, send, func() {})
	if got == nil || got.GetId() != 7 || got.GetSuccessful() || !strings.Contains(got.GetData(), "busy") {
		t.Fatalf("满载时应同步回复失败结果, got=%v", got)
	}
}

// 未满载时任务正常异步执行并释放槽位。
func TestDispatchAgentTaskReleasesSlot(t *testing.T) {
	before := len(taskSlots)
	done := make(chan *pb.TaskResult, 1)
	send := func(r *pb.TaskResult) error { done <- r; return nil }
	dispatchAgentTask(&pb.Task{Id: 8, Type: model.TaskTypeKeepalive}, send, func() {})
	select {
	case r := <-done:
		if r.GetId() != 8 {
			t.Fatalf("unexpected result %v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("keepalive 未回复")
	}
	for deadline := time.Now().Add(2 * time.Second); len(taskSlots) > before; {
		if time.Now().After(deadline) {
			t.Fatalf("任务结束后应释放槽位, 占用 %d -> %d", before, len(taskSlots))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Windows 发布包内为 nezha-agent.exe，按 updateBinaryName 查找才能解出。
func TestUpdateBinaryNameMatchesWindowsAsset(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("nezha-agent.exe")
	w.Write([]byte("MZ"))
	zw.Close()
	url := "https://example.com/nezha-agent_windows_amd64.zip"
	if _, err := selfupdate.UncompressCommand(bytes.NewReader(buf.Bytes()), url, updateBinaryName("windows")); err != nil {
		t.Fatalf("windows 包应能解出: %v", err)
	}
	if got := updateBinaryName("linux"); got != binaryName {
		t.Fatalf("linux 包内文件名应为 %s, got %s", binaryName, got)
	}
}

func TestCheckDedicatedDir(t *testing.T) {
	if err := checkDedicatedDir("/usr/local/bin", "nezha-agent"); err == nil {
		t.Fatal("公共系统目录必须拒绝")
	}
	dir := t.TempDir()
	for _, f := range []string{"nezha-agent", "config.yml", ".nezha-agent.old", ".nezha-agent.stat"} {
		os.WriteFile(filepath.Join(dir, f), nil, 0o600)
	}
	if err := checkDedicatedDir(dir, "nezha-agent"); err != nil {
		t.Fatalf("专用目录应放行: %v", err)
	}
	os.Mkdir(filepath.Join(dir, "evil.yml"), 0o755) // 名字合规的子目录也要拒绝
	if err := checkDedicatedDir(dir, "nezha-agent"); err == nil {
		t.Fatal("含子目录的目录必须拒绝")
	}
	os.Remove(filepath.Join(dir, "evil.yml"))
	os.WriteFile(filepath.Join(dir, "sudo"), nil, 0o755)
	if err := checkDedicatedDir(dir, "nezha-agent"); err == nil {
		t.Fatal("含非 agent 文件的目录必须拒绝")
	}
}

// 自更新互斥文件不能落在共享临时目录。
func TestUpdateStatFileInExecDir(t *testing.T) {
	got := updateStatFile()
	if filepath.Dir(got) != filepath.Dir(executablePath) {
		t.Fatalf("stat 文件应位于程序目录, got %s", got)
	}
	if strings.HasPrefix(got, filepath.Join(os.TempDir(), binaryName)) {
		t.Fatalf("stat 文件不应位于共享临时目录: %s", got)
	}
}

// 响应体只读 maxProbeBodyBytes，服务端推送大文件时不会被整段拉取。
func TestHTTPProbeBodyCapped(t *testing.T) {
	var sent atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := make([]byte, 64<<10)
		for i := 0; i < 1024; i++ { // 共 64MiB
			n, err := w.Write(chunk)
			sent.Add(int64(n))
			if err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	_, _, err := httpGetProbe(srv.URL)
	srv.CloseClientConnections()
	if err != nil {
		t.Fatalf("拨测应成功: %v", err)
	}
	if n := sent.Load(); n >= 64<<20 {
		t.Fatalf("响应体未被截断, 服务端写出 %d 字节", n)
	}
}

func TestWarnPlaintextTransport(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	warnPlaintextTransport(&model.AgentConfig{Server: "dash.example.com:8008"})
	warnPlaintextTransport(&model.AgentConfig{Server: "127.0.0.1:8008"})
	warnPlaintextTransport(&model.AgentConfig{Server: "localhost:8008"})
	warnPlaintextTransport(&model.AgentConfig{Server: "dash.example.com:443", TLS: true})
	if got := strings.Count(buf.String(), "警告"); got != 1 {
		t.Fatalf("应只对公网明文连接告警一次, got %d: %s", got, buf.String())
	}
}

// chownDirFlat 不能跟随目录内的符号链接去修改链接目标。
func TestChownDirFlatDoesNotFollowSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Lchown 在 Windows 上不可用，且该逻辑仅用于 Linux")
	}
	dir := t.TempDir()
	target := "/etc/hosts" // 属 root，若被跟随则 chown 会 EPERM
	if err := os.Symlink(target, filepath.Join(dir, "config.yml")); err != nil {
		t.Skip(err)
	}
	if err := chownDirFlat(dir, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("chownDirFlat 跟随了符号链接: %v", err)
	}
}

func TestForcedUpdateBase(t *testing.T) {
	if got := forcedUpdateBase("2.2.3"); got.String() != "2.2.3" {
		t.Fatalf("有版本号时应以当前版本为基准, got %s", got)
	}
	if got := forcedUpdateBase(""); got.String() != "0.1.0" {
		t.Fatalf("无版本号时应回退 0.1.0, got %s", got)
	}
}

// 面板下发超过 maxRecvMsgBytes 的消息时，客户端应拒收而不是整条读入内存。
func TestGRPCRecvLimit(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	pb.RegisterNezhaServiceServer(gs, &bigTaskDash{})
	go gs.Serve(ln)
	defer gs.Stop()
	saved := agentConfig
	defer func() { agentConfig = saved }()
	agentConfig.TLS = false
	auth := &model.AuthHandler{Credentials: func() (string, string) { return "s", "u" }}
	conn, err := grpc.NewClient(ln.Addr().String(), dialOptions(auth)...)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := pb.NewNezhaServiceClient(conn).RequestTask(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("超限任务应被拒收, got %v", err)
	}
}

type bigTaskDash struct {
	pb.UnimplementedNezhaServiceServer
}

func (bigTaskDash) RequestTask(s pb.NezhaService_RequestTaskServer) error {
	return s.Send(&pb.Task{Type: model.TaskTypeHTTPGet, Data: strings.Repeat("a", maxRecvMsgBytes+1)})
}
