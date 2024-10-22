package node

import (
	"github.com/dobyte/due/v2/cluster"
	"github.com/dobyte/due/v2/utils/xcall"
	"sync"
	"sync/atomic"
)

type Creator func(actor *Actor, args ...any) Processor

const (
	unstart   int32 = iota // 未启动
	started                // 已启动
	destroyed              // 已销毁
)

type Actor struct {
	opts      *actorOptions                  // 配置项
	scheduler *Scheduler                     // 调度器
	state     atomic.Int32                   // 状态
	routes    map[int32]RouteHandler         // 路由处理器
	events    map[cluster.Event]EventHandler // 事件处理器
	processor Processor                      // 处理器
	rw        sync.RWMutex                   // 锁
	mailbox   chan Context                   // 邮箱
	fnChan    chan func()                    // 调用函数
	binds     sync.Map                       // 绑定的用户
}

// ID 获取Actor的ID
func (a *Actor) ID() string {
	return a.opts.id
}

// PID 获取Actor的唯一识别ID
func (a *Actor) PID() string {
	return a.Kind() + "/" + a.ID()
}

// Kind 获取Actor类型
func (a *Actor) Kind() string {
	return a.processor.Kind()
}

// Spawn 衍生出一个Actor
func (a *Actor) Spawn(creator Creator, opts ...ActorOption) (*Actor, error) {
	return a.scheduler.spawn(creator, opts...)
}

// Proxy 获取代理API
func (a *Actor) Proxy() *Proxy {
	return a.scheduler.node.proxy
}

// Invoke 调用函数（Actor内线程安全）
func (a *Actor) Invoke(fn func()) {
	a.rw.RLock()
	defer a.rw.RUnlock()

	if a.state.Load() != started {
		return
	}

	a.fnChan <- fn
}

// AddRouteHandler 添加路由处理器
func (a *Actor) AddRouteHandler(route int32, handler RouteHandler) {
	a.rw.RLock()
	defer a.rw.RUnlock()

	switch a.state.Load() {
	case unstart:
		a.routes[route] = handler
	case started:
		a.fnChan <- func() {
			a.routes[route] = handler
			a.scheduler.routes.Store(route, a.Kind())
		}
	default:
		// ignore
	}
}

// AddEventHandler 添加事件处理器
func (a *Actor) AddEventHandler(event cluster.Event, handler EventHandler) {
	a.rw.RLock()
	defer a.rw.RUnlock()

	switch a.state.Load() {
	case unstart:
		a.events[event] = handler
	case started:
		a.fnChan <- func() {
			a.events[event] = handler
		}
	default:
		// ignore
	}
}

// Next 投递消息到Actor中进行处理
func (a *Actor) Next(ctx Context) {
	a.rw.RLock()
	defer a.rw.RUnlock()

	if a.state.Load() != started {
		return
	}

	ctx.incrVersion()

	ctx.Cancel()

	a.mailbox <- ctx
}

// Deliver 投递消息到Actor中进行处理
func (a *Actor) Deliver(uid int64, message *cluster.Message) {
	req := a.scheduler.node.reqPool.Get().(*request)
	req.nid = a.scheduler.node.opts.id
	req.uid = uid
	req.message.Seq = message.Seq
	req.message.Route = message.Route
	req.message.Data = message.Data

	a.Next(req)
}

// Destroy 销毁Actor
func (a *Actor) Destroy() {
	if !a.state.CompareAndSwap(started, destroyed) {
		return
	}

	a.processor.Destroy()

	a.scheduler.batchUnbindActor(func(relations map[int64]map[string]*Actor) {
		a.binds.Range(func(uid, _ any) bool {
			delete(relations[uid.(int64)], a.Kind())
			return true
		})
	})

	a.rw.Lock()
	defer a.rw.Unlock()

	close(a.mailbox)

	close(a.fnChan)
}

// 绑定用户
func (a *Actor) bindUser(uid int64) {
	a.binds.Store(uid, struct{}{})
}

// 解绑用户
func (a *Actor) unbindUser(uid int64) bool {
	_, ok := a.binds.LoadAndDelete(uid)
	return ok
}

// 分发
func (a *Actor) dispatch() {
	go func() {
		for {
			select {
			case ctx, ok := <-a.mailbox:
				if !ok {
					return
				}

				version := ctx.loadVersion()

				if ctx.Kind() == Event {
					if handler, ok := a.events[ctx.Event()]; ok {
						xcall.Call(func() { handler(ctx) })
					}
				} else {
					if handler, ok := a.routes[ctx.Route()]; ok {
						xcall.Call(func() { handler(ctx) })
					}
				}

				ctx.compareVersionExecDefer(version)

				ctx.compareVersionRecycle(version)
			case handle, ok := <-a.fnChan:
				if !ok {
					return
				}
				xcall.Call(handle)
			}
		}
	}()
}
