package commands

import (
	"os"

	"github.com/nezhahq/service"
)

// Program 适配 service 库的生命周期回调，Run 为 agent 主循环（正常情况下不返回）。
type Program struct {
	Service service.Service
	Run     func()
}

func (p *Program) Start(s service.Service) error {
	go p.run()
	return nil
}

// Stop 交互模式（前台运行）下直接退出进程；服务模式下交由服务管理器结束。
// 原 Exit channel 只 close 不被读取，且重复 Stop 会 panic，已移除。
func (p *Program) Stop(s service.Service) error {
	if service.Interactive() {
		os.Exit(0)
	}
	return nil
}

func (p *Program) run() {
	defer func() {
		if service.Interactive() {
			p.Stop(p.Service)
		} else {
			p.Service.Stop()
		}
	}()
	p.Run()
}
