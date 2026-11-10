// Package cache 实现磁盘缓存仓库。
//
// 目录布局（缓存键 = "<源类型>/<请求相对路径>"）：
//
//	<cache_dir>/<源类型>/<文件名前2字符桶>/<请求路径中包含的子目录层>/…/<文件名><文件标记>
//
// 其中"子目录层"原样镜像了请求路径：对 apt/yum 这类源，天然包含发行版版本、
// 组件、架构等目录层（如 ubuntu/dists/jammy/main/binary-amd64/），因此
// 不同 Ubuntu 版本的同名文件（Packages.gz）会落到不同的子目录，互不冲突；
// 对 pip/npm/mvn 的扁平文件（无子目录），则退化为
// <cache_dir>/<源类型>/<前2字符>/<文件名><文件标记> 的两级结构。
//
// 文件标记 fileMarker（"~"）加在每个缓存文件的文件名之后，用于把"文件"
// 与"目录"的命名空间分开：npm 的包元数据 /npm/<pkg> 缓存为文件
// <桶>/<pkg>~，而 tarball /npm/<pkg>/-/x.tgz 需要目录 <桶>/<pkg>/-/，
// 若无标记两者会在 <桶>/<pkg> 处同名冲突（一边要当文件、一边要当目录），
// 导致 tarball 永远无法落盘。
//
// 该布局可被启动扫描无损还原：磁盘相对路径去掉第一段（桶）、去掉最后一段
// 的文件标记即得请求路径，无需任何旁路元数据即可重建内存索引。
package cache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Entry 内存索引中的单条缓存记录。
type Entry struct {
	Type        string // 源类型，如 apt
	Rest        string // 请求相对路径，如 ubuntu/dists/jammy/InRelease
	Path        string // 磁盘绝对路径
	Size        int64
	ModTime     time.Time
	ContentType string // 上游返回的 Content-Type（扫描恢复的条目为空，按规则推断）
}

// Key 返回缓存键：源类型/请求路径。
func Key(typeName, rest string) string { return typeName + "/" + rest }

// ScanStat 单个源类型的缓存统计。
type ScanStat struct {
	Files int64
	Bytes int64
}

// tempPrefix 临时（未提交）缓存文件的文件名前缀。扫描时会被删除。
const tempPrefix = ".__tmp"

// fileMarker 缓存文件名的固定后缀，用于区分磁盘布局中的"文件"与"目录"。
// 取单个 "~"——Unix 惯例中"缓存/备份副本"的后缀，可读性好（lodash~、
// Packages.gz~）。段编码会把裸 ~ 转义成 ~7E，因此任何目录段都不可能以
// ~ 结尾；文件名一律带此后缀，与目录段的命名空间完全不相交，从根上解决
// npm 包元数据 /npm/<pkg>（文件）与 tarball /npm/<pkg>/-/x.tgz（目录）
// 在 <桶>/<pkg> 路径上的同名冲突。
const fileMarker = "~"

var (
	// 段编码：把路径段中不适合出现在磁盘路径里的字符转义。
	// ~ 也需转义（→ ~7E）：保证任何编码后的目录段都不会以 fileMarker
	// （单个 ~）结尾，文件后缀标记才不会被目录段"模仿"。
	segEncoder = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23", "~", "~7E")
	typeRe     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)
)

// Store 磁盘缓存 + 内存索引。
type Store struct {
	root string

	mu      sync.RWMutex
	entries map[string]*Entry
}

// New 创建缓存仓库。root 不存在时将在首次写入时自动创建。
func New(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("缓存目录不能为空")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &Store{root: abs, entries: map[string]*Entry{}}, nil
}

// Root 返回缓存根目录（绝对路径）。
func (s *Store) Root() string { return s.root }

// CleanSegments 清洗请求相对路径：按 "/" 切分、去空段、拒绝 ".."。
func CleanSegments(rest string) ([]string, error) {
	if strings.ContainsRune(rest, 0) {
		return nil, errors.New("路径中包含非法字符")
	}
	parts := strings.Split(rest, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		switch p {
		case "", ".":
			continue
		case "..":
			return nil, errors.New("路径中不允许出现 ..")
		}
		if len(p) > 200 {
			return nil, fmt.Errorf("路径段过长（%d 字节）", len(p))
		}
		out = append(out, p)
	}
	return out, nil
}

// bucketOf 返回文件名前 2 个字符作为分桶名（不足 2 字符取整体）。
func bucketOf(filename string) string {
	if len(filename) >= 2 {
		return filename[:2]
	}
	return filename
}

func encodeSegment(seg string) string { return segEncoder.Replace(seg) }

// decodeSegment 严格解码 encodeSegment 的输出；遇到非法转义返回 false。
func decodeSegment(seg string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(seg); {
		if seg[i] == '%' {
			if i+3 <= len(seg) {
				switch seg[i : i+3] {
				case "%25":
					b.WriteByte('%')
					i += 3
					continue
				case "%3F":
					b.WriteByte('?')
					i += 3
					continue
				case "%23":
					b.WriteByte('#')
					i += 3
					continue
				}
			}
			return "", false
		}
		if seg[i] == '~' {
			if i+3 <= len(seg) && seg[i:i+3] == "~7E" {
				b.WriteByte('~')
				i += 3
				continue
			}
			return "", false // 裸 ~ 不会出现在 encodeSegment 的输出里
		}
		b.WriteByte(seg[i])
		i++
	}
	return b.String(), true
}

// DiskRelPath 返回缓存文件相对缓存根目录的路径。
//
// 映射规则：<type>/<桶>/<编码后的请求路径目录段>/<编码后的文件名><文件标记>。
// 文件名（最后一段）额外加 fileMarker 后缀，与目录段空间分离。
func DiskRelPath(typeName, rest string) (string, error) {
	if !typeRe.MatchString(typeName) {
		return "", fmt.Errorf("非法的源类型 %q", typeName)
	}
	segs, err := CleanSegments(rest)
	if err != nil {
		return "", err
	}
	if len(segs) == 0 {
		return "", errors.New("请求路径为空")
	}
	dirs, file := segs[:len(segs)-1], segs[len(segs)-1]
	parts := make([]string, 0, len(segs)+2)
	parts = append(parts, typeName, encodeSegment(bucketOf(file)))
	for _, seg := range dirs {
		parts = append(parts, encodeSegment(seg))
	}
	parts = append(parts, encodeSegment(file)+fileMarker)
	return filepath.Join(parts...), nil
}

// Lookup 查询内存索引（优先本地命中的第一步）。
func (s *Store) Lookup(typeName, rest string) (Entry, bool) {
	e := s.get(Key(typeName, rest))
	if e == nil {
		return Entry{}, false
	}
	return *e, true
}

func (s *Store) get(key string) *Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.entries[key]
}

func (s *Store) put(e *Entry) {
	s.mu.Lock()
	s.entries[Key(e.Type, e.Rest)] = e
	s.mu.Unlock()
}

// Part 一个正在写入的缓存文件（先写临时文件，成功后原子提交）。
type Part struct {
	store    *Store
	f        *os.File
	tmpPath  string
	finalRel string // 相对缓存根目录
	typeName string
	rest     string
}

// NewPart 创建目标目录与临时文件。
func (s *Store) NewPart(typeName, rest string) (*Part, error) {
	rel, err := DiskRelPath(typeName, rest)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(s.root, filepath.Dir(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, tempPrefix+"-*")
	if err != nil {
		return nil, err
	}
	return &Part{
		store:    s,
		f:        f,
		tmpPath:  f.Name(),
		finalRel: rel,
		typeName: typeName,
		rest:     rest,
	}, nil
}

// File 返回临时文件句柄，调用方直接向其写入数据。
func (p *Part) File() *os.File { return p.f }

// Commit 原子提交缓存文件：设置 mtime、rename 到最终路径、更新内存索引。
func (p *Part) Commit(size int64, modTime time.Time, contentType string) (Entry, error) {
	if p.f != nil {
		_ = p.f.Close()
		p.f = nil
	}
	mt := modTime
	if mt.IsZero() {
		mt = time.Now()
	}
	if err := os.Chtimes(p.tmpPath, mt, mt); err != nil {
		_ = os.Remove(p.tmpPath)
		return Entry{}, err
	}
	final := filepath.Join(p.store.root, p.finalRel)
	if err := os.Rename(p.tmpPath, final); err != nil {
		_ = os.Remove(p.tmpPath)
		return Entry{}, err
	}
	e := &Entry{
		Type:        p.typeName,
		Rest:        p.rest,
		Path:        final,
		Size:        size,
		ModTime:     mt,
		ContentType: contentType,
	}
	p.store.put(e)
	return *e, nil
}

// Abort 丢弃本次写入（删除临时文件，不影响已有缓存）。
func (p *Part) Abort() {
	if p.f != nil {
		_ = p.f.Close()
		p.f = nil
	}
	_ = os.Remove(p.tmpPath)
}

// SaveBytes 将一段内存数据写入缓存（用于较小的可重写响应）。
func (s *Store) SaveBytes(typeName, rest string, data []byte, modTime time.Time, contentType string) (Entry, error) {
	p, err := s.NewPart(typeName, rest)
	if err != nil {
		return Entry{}, err
	}
	if _, err := p.f.Write(data); err != nil {
		p.Abort()
		return Entry{}, err
	}
	return p.Commit(int64(len(data)), modTime, contentType)
}

// Touch 刷新条目的 mtime（用于 304 再校验后延长 TTL）。
func (s *Store) Touch(typeName, rest string) bool {
	key := Key(typeName, rest)
	s.mu.RLock()
	e := s.entries[key]
	s.mu.RUnlock()
	if e == nil {
		return false
	}
	now := time.Now()
	_ = os.Chtimes(e.Path, now, now)
	s.mu.Lock()
	e.ModTime = now
	s.mu.Unlock()
	return true
}

// PurgePrefix 按前缀删除某源类型的缓存。prefix 为空表示清空该源全部缓存。
// 返回删除的文件数与释放的字节数。
func (s *Store) PurgePrefix(typeName, prefix string) (int, int64, error) {
	if !typeRe.MatchString(typeName) {
		return 0, 0, fmt.Errorf("非法的源类型 %q", typeName)
	}
	segs, err := CleanSegments(prefix)
	if err != nil {
		return 0, 0, err
	}
	normPrefix := ""
	if len(segs) > 0 {
		normPrefix = strings.Join(segs, "/") + "/"
	}

	var removed int
	var freed int64
	s.mu.Lock()
	for k, e := range s.entries {
		if e.Type != typeName {
			continue
		}
		if normPrefix != "" && !strings.HasPrefix(e.Rest+"/", normPrefix) {
			continue
		}
		if err := os.Remove(e.Path); err == nil || os.IsNotExist(err) {
			removed++
			freed += e.Size
		}
		delete(s.entries, k)
	}
	s.mu.Unlock()
	return removed, freed, nil
}

// Scan 启动时扫描缓存目录，重建内存索引：
//   - 磁盘相对路径去掉第一段（桶）、去掉最后一段的文件标记即还原请求路径；
//   - 不带文件标记的文件视为无关文件（旧布局遗留或外部写入），跳过；
//   - 残留的临时文件（进程中断遗留）被直接删除。
func (s *Store) Scan() (map[string]ScanStat, error) {
	stats := map[string]ScanStat{}
	loaded := map[string]*Entry{}

	rootEntries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return stats, nil // 首次启动，缓存目录尚不存在
		}
		return nil, err
	}

	for _, te := range rootEntries {
		if !te.IsDir() {
			continue
		}
		typeName := te.Name()
		if !typeRe.MatchString(typeName) {
			continue // 跳过缓存根目录下的无关目录
		}
		typeDir := filepath.Join(s.root, typeName)
		_ = filepath.WalkDir(typeDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil // 容错：跳过不可读项
			}
			base := d.Name()
			if strings.HasPrefix(base, tempPrefix) {
				_ = os.Remove(path) // 中断残留的临时文件
				return nil
			}
			rel, err := filepath.Rel(typeDir, path)
			if err != nil {
				return nil
			}
			segs := strings.Split(filepath.ToSlash(rel), "/")
			if len(segs) < 2 { // 至少应为 <桶>/<文件>
				return nil
			}
			// 最后一段是缓存文件名，必须以文件标记结尾；否则不是本布局的文件。
			last := segs[len(segs)-1]
			if !strings.HasSuffix(last, fileMarker) {
				return nil
			}
			segs[len(segs)-1] = last[:len(last)-len(fileMarker)]
			decoded := make([]string, 0, len(segs)-1)
			for _, seg := range segs[1:] { // 第一段是桶，跳过
				ds, ok := decodeSegment(seg)
				if !ok {
					return nil // 命名不符合本程序约定，跳过
				}
				decoded = append(decoded, ds)
			}
			if len(decoded) == 0 {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			rest := strings.Join(decoded, "/")
			loaded[Key(typeName, rest)] = &Entry{
				Type:    typeName,
				Rest:    rest,
				Path:    path,
				Size:    fi.Size(),
				ModTime: fi.ModTime(),
			}
			st := stats[typeName]
			st.Files++
			st.Bytes += fi.Size()
			stats[typeName] = st
			return nil
		})
	}

	s.mu.Lock()
	s.entries = loaded
	s.mu.Unlock()
	return stats, nil
}

// TypeStats 返回某源类型当前的缓存统计（基于内存索引）。
func (s *Store) TypeStats(typeName string) ScanStat {
	var st ScanStat
	s.mu.RLock()
	for _, e := range s.entries {
		if e.Type == typeName {
			st.Files++
			st.Bytes += e.Size
		}
	}
	s.mu.RUnlock()
	return st
}

// Count 返回内存索引中的缓存条目总数。
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}
