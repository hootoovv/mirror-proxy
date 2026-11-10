package proxy

import (
	"sync"
	"sync/atomic"
)

// Stats 运行期计数器（/stats 接口的数据来源）。
type Stats struct {
	hits        atomic.Int64 // 本地缓存命中次数
	misses      atomic.Int64 // 未命中（触发回源）次数
	staleServed atomic.Int64 // 命中但已过期、先返回旧数据并后台再校验的次数
	refresh304  atomic.Int64 // 再校验返回 304（内容未变化）次数
	refresh200  atomic.Int64 // 再校验返回 200（内容更新）次数

	upMu sync.Mutex
	ups  map[string]*upCounters
}

type upCounters struct {
	ok  atomic.Int64 // 收到可用响应（2xx 且被采用）次数
	err atomic.Int64 // 网络错误 / 5xx / 404 次数
}

func newStats() *Stats {
	return &Stats{ups: map[string]*upCounters{}}
}

func (s *Stats) upstreamCounter(name string) *upCounters {
	s.upMu.Lock()
	defer s.upMu.Unlock()
	uc, ok := s.ups[name]
	if !ok {
		uc = &upCounters{}
		s.ups[name] = uc
	}
	return uc
}

func (s *Stats) upOK(name string)  { s.upstreamCounter(name).ok.Add(1) }
func (s *Stats) upErr(name string) { s.upstreamCounter(name).err.Add(1) }

// snapshotUps 返回上游计数快照。
func (s *Stats) snapshotUps() map[string]upCountersSnapshot {
	s.upMu.Lock()
	defer s.upMu.Unlock()
	out := make(map[string]upCountersSnapshot, len(s.ups))
	for name, uc := range s.ups {
		out[name] = upCountersSnapshot{OK: uc.ok.Load(), Err: uc.err.Load()}
	}
	return out
}

type upCountersSnapshot struct {
	OK  int64
	Err int64
}
