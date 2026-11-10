package proxy

import "sync"

// flightGroup 极简版 singleflight：合并对同一缓存键的并发回源请求。
//
// 与 golang.org/x/sync/singleflight 的差异：fn 在首个调用者的 goroutine 中
// 同步执行（因为首个请求者要边下载边把数据流给客户端），其余等待者在
// flight 完成后从缓存补发。
type flightGroup struct {
	mu    sync.Mutex
	calls map[string]*flightCall
}

type flightCall struct {
	wg  sync.WaitGroup
	val any
	err error
}

func newFlightGroup() *flightGroup {
	return &flightGroup{calls: map[string]*flightCall{}}
}

// Do 执行（或等待）key 对应的调用。返回值 shared=true 表示本次为共享结果
// （调用方未执行 fn，需要自行从缓存补发响应）。
func (g *flightGroup) Do(key string, fn func() (any, error)) (any, error, bool) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	}
	c := &flightCall{}
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	c.wg.Done()
	return c.val, c.err, false
}

// InFlight 当前正在回源的键数量。
func (g *flightGroup) InFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}
